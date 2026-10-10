package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func agentReply(convID, agentID, body string, reopen bool) core.AppendParams {
	return core.AppendParams{
		ConversationID: convID, ClientMsgID: id.New(), SenderKind: core.SenderAgent, SenderAgentID: agentID,
		Visibility: core.Public, Body: body, Reopen: reopen,
	}
}

type response struct {
	waitedFrom, answeredAt time.Time
	first                  bool
}

func responsesOf(t *testing.T, convID string) []response {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(),
		"SELECT waited_from, answered_at, first FROM conversation_responses WHERE conversation_id = $1 ORDER BY message_seq", convID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []response
	for rows.Next() {
		var r response
		if err := rows.Scan(&r.waitedFrom, &r.answeredAt, &r.first); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// 정책: 고객 글과 상담원 답이 몇 ms 안에 겹쳐도 답은 기다리던 고객에게 간 응답으로 남는다(DEF-103).
// 고객 글의 트랜잭션이 대화 행을 쥔 채 커밋하지 않은 동안 상담원 답이 그 행을 기다리게 하고, 고객 글을 커밋한 뒤 답의 판정을 본다.
// 다시 여는 답(Reopen)은 전에도 행을 먼저 잠가 맞았고, 다시 열지 않는 답(AI 응대·앞으로의 길)은 기다리기 전 값을 봐서 틀렸다.
func TestAppendMessage_고객글과답이겹쳐도답은기다리던고객에게간응답으로남는다(t *testing.T) {
	store := testsupport.Store(t)
	agent := testsupport.Agent(t, store)
	for _, reopen := range []bool{false, true} {
		name := "다시 열지 않는 답"
		if reopen {
			name = "다시 여는 답"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			customerID, conv := testsupport.Conversation(t, store)
			// 출발점: 첫 차례는 이미 답을 받았다 — 고객은 기다리고 있지 않다.
			if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "첫 문의")); err != nil {
				t.Fatal(err)
			}
			a1, err := store.AppendMessage(ctx, agentReply(conv.ID, agent.ID, "첫 답", false))
			if err != nil {
				t.Fatal(err)
			}

			conn, err := pgx.Connect(ctx, testsupport.URL())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			q2, err := pgstore.AppendInTx(ctx, tx, customerMessage(conv.ID, customerID, id.New(), "또 문의"))
			if err != nil {
				t.Fatal(err)
			}

			type result struct {
				res core.AppendResult
				err error
			}
			done := make(chan result, 1)
			go func() {
				res, err := store.AppendMessage(ctx, agentReply(conv.ID, agent.ID, "겹친 답", reopen))
				done <- result{res, err}
			}()
			waitForLockWait(t, conn)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var r result
			select {
			case r = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("답이 끝나지 않는다")
			}
			if r.err != nil {
				t.Fatal(r.err)
			}
			if !r.res.Answered || !r.res.Responded {
				t.Errorf("기다리던 고객에게 간 답이다 — 업무 기록의 응답(answered)과 응답 기록(responded)이 남아야 한다: %+v", r.res)
			}
			rs := responsesOf(t, conv.ID)
			if len(rs) != 2 || rs[0].first == rs[1].first || !rs[0].first {
				t.Fatalf("차례 둘, 첫 응답은 앞의 것: %+v", rs)
			}
			// 차례의 시작은 그 고객 글이 번호를 받은 순간이다 — 첫 답 뒤이고 그 글의 시각(같은 트랜잭션의 INSERT)보다 늦지 않다.
			if w := rs[1].waitedFrom; w.Before(a1.Message.CreatedAt) || w.After(q2.Message.CreatedAt) {
				t.Errorf("둘째 차례는 겹친 고객 글에서 시작한다: %v (첫 답 %v, 고객 글 %v)", w, a1.Message.CreatedAt, q2.Message.CreatedAt)
			}
		})
	}
}

// waitForLockWait 는 이 DB 에서 잠금을 기다리는 다른 세션(겹친 답)이 생길 때까지 기다린다. 이 패키지 테스트는 하나씩 돈다(-p 1).
func waitForLockWait(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := conn.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("답이 대화 행을 기다리지 않는다 — 겹침을 만들지 못했다")
}

// 정책: 만족도는 그 상담의 고객만 낸다 — 저장소도 잠근 행의 고객과 맞춰 본다(앱의 권한 검사가 빠져도 남의 상담에 들어가지 않는다).
func TestPutRating_저장소는그상담의고객이아니면없는상담으로답한다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	other, _ := testsupport.Conversation(t, store)
	if _, err := store.SetConversationState(ctx, conv.ID, core.StateClosed, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := store.PutRating(ctx, core.RatingInput{ConversationID: conv.ID, CustomerID: other, Score: 1, Window: time.Hour})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("남의 상담: %v", err)
	}
	st, err := store.PutRating(ctx, core.RatingInput{ConversationID: conv.ID, CustomerID: customerID, Score: 5, Window: time.Hour})
	if err != nil || st.Rating == nil || st.Rating.Score != 5 || st.Rating.Handler != core.HandlerUnanswered {
		t.Errorf("그 상담의 고객: %+v %v", st, err)
	}
}

// 감사 20 B10: 가린(삭제한) 상담 조건을 빼도 통과했다. 상담원이 가린 상담은 고객에게도 없는 상담이다 — 고객이 그 상담 id 로 평가를 내도 받지 않는다.
func TestPutRating_저장소는가린상담의평가를없는상담으로답한다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	if _, err := store.SetConversationState(ctx, conv.ID, core.StateClosed, nil, nil); err != nil {
		t.Fatal(err)
	}
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET deleted_at = now() WHERE id = $1", conv.ID)
	if _, err := store.PutRating(ctx, core.RatingInput{ConversationID: conv.ID, CustomerID: customerID, Score: 5, Window: time.Hour}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("가린 상담에 평가를 받았다: %v", err)
	}
	var n int
	if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM conversation_ratings WHERE conversation_id = $1", conv.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("가린 상담의 평가 행 %d개(%v)", n, err)
	}
}
