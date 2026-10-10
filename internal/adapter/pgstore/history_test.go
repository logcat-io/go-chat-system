package pgstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func TestReviseMessage_원문과본문(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	_, other := testsupport.Conversation(t, store)
	sent, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "원문"))
	if err != nil {
		t.Fatal(err)
	}
	mine := core.ReviseParams{ConversationID: conv.ID, MessageID: sent.Message.ID, ActorKind: core.SenderCustomer, ActorCustomer: customerID}

	t.Run("고치면 번호를 하나 받는 이벤트 행이 생기고 원문은 이력에 남는다", func(t *testing.T) {
		p := mine
		p.Body = "고친 본문"
		res, err := store.ReviseMessage(ctx, core.KindEdit, p)
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Seq != sent.Message.Seq+1 || res.Message.RefMessageID != sent.Message.ID {
			t.Errorf("이벤트 행: seq=%d ref=%s", res.Message.Seq, res.Message.RefMessageID)
		}
		revs, err := store.Revisions(ctx, conv.ID, sent.Message.ID)
		if err != nil || len(revs) != 1 || revs[0].Body != "원문" {
			t.Errorf("이력: %+v %v", revs, err)
		}
	})

	t.Run("이력은 그 대화의 메시지일 때만 나온다", func(t *testing.T) {
		revs, err := store.Revisions(ctx, other.ID, sent.Message.ID)
		if err != nil || len(revs) != 0 {
			t.Errorf("다른 대화 id 로 이력이 나왔다: %+v %v", revs, err)
		}
	})

	t.Run("다른 대화 id 로는 고칠 수 없다", func(t *testing.T) {
		p := mine
		p.ConversationID, p.Body = other.ID, "x"
		if _, err := store.ReviseMessage(ctx, core.KindEdit, p); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})

	t.Run("지우면 원 메시지와 앞선 수정 이벤트 행의 본문이 모두 비고, 다시 지울 수 없다", func(t *testing.T) {
		if _, err := store.ReviseMessage(ctx, core.KindDelete, mine); err != nil {
			t.Fatal(err)
		}
		var withBody int
		err := store.Pool().QueryRow(ctx,
			"SELECT count(*) FROM messages WHERE conversation_id = $1 AND body <> ''", conv.ID).Scan(&withBody)
		if err != nil || withBody != 0 {
			t.Errorf("본문이 남은 행 %d개 (%v)", withBody, err)
		}
		if _, err := store.ReviseMessage(ctx, core.KindDelete, mine); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("이미 지운 메시지: %v", err)
		}
		seqs, _ := store.MessageSeqs(ctx, conv.ID)
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("번호에 빈칸: %v", seqs)
			}
		}
	})
}

func TestOpenMemberConversation_상담마다대화하나(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, first := testsupport.Conversation(t, store)

	t.Run("열린 상담이 있으면 그 방을 돌려주고 아무것도 만들지 않는다", func(t *testing.T) {
		conv, opened, err := store.OpenMemberConversation(ctx, first.WorkspaceID, customerID)
		if err != nil {
			t.Fatal(err)
		}
		if opened != core.OpenedExisting || conv.ID != first.ID {
			t.Errorf("opened=%v id=%s", opened, conv.ID)
		}
	})

	t.Run("종료된 상담은 다시 열지 않는다 — 동시에 열 번 요청해도 새 대화 하나만 생긴다", func(t *testing.T) {
		if _, err := store.Pool().Exec(ctx, "UPDATE conversations SET state = 'CLOSED' WHERE id = $1", first.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		created, ids := 0, map[string]bool{}
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conv, opened, err := store.OpenMemberConversation(ctx, first.WorkspaceID, customerID)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				ids[conv.ID] = true
				if opened == core.OpenedNew {
					created++
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if created != 1 || len(ids) != 1 || ids[first.ID] {
			t.Errorf("새 대화 %d개, 받은 방 %v", created, ids)
		}
		old, err := store.Conversation(ctx, first.ID)
		if err != nil || old.State != core.StateClosed {
			t.Errorf("종료된 상담은 닫힌 채여야 한다: %s (%v)", old.State, err)
		}
		if n, err := store.CustomerConversationCount(ctx, customerID); err != nil || n != 2 {
			t.Errorf("이 고객의 상담은 2건: %d (%v)", n, err)
		}
	})

	t.Run("상담이 없는 고객에게 동시에 열 번 요청해도 방은 하나다", func(t *testing.T) {
		fresh, err := store.CreateCustomer(ctx, first.WorkspaceID, "새 회원")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := store.OpenMemberConversation(ctx, first.WorkspaceID, fresh); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if n, err := store.CustomerConversationCount(ctx, fresh); err != nil || n != 1 {
			t.Errorf("방 %d개 (%v)", n, err)
		}
	})

	t.Run("다른 워크스페이스의 고객 id 로는 열 수 없다", func(t *testing.T) {
		if _, _, err := store.OpenMemberConversation(ctx, id.New(), customerID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})
}

func TestListCustomerHistory_이전상담을이어읽는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, first := testsupport.Conversation(t, store)
	say := func(convID, body string) core.Message {
		t.Helper()
		res, err := store.AppendMessage(ctx, customerMessage(convID, customerID, id.New(), body))
		if err != nil {
			t.Fatal(err)
		}
		return res.Message
	}
	next := func() core.Conversation {
		t.Helper()
		c, err := store.CreateConversation(ctx, first.WorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	bodies := func(list []core.Message) (out []string) {
		for _, m := range list {
			out = append(out, m.Body)
		}
		return out
	}
	say(first.ID, "1-가")
	edited := say(first.ID, "1-나")
	say(first.ID, "1-다")
	second := next()
	say(second.ID, "2-가")
	say(second.ID, "2-나")
	third := next()
	say(third.ID, "3-가")
	mine := core.ReviseParams{ConversationID: first.ID, MessageID: edited.ID, ActorKind: core.SenderCustomer, ActorCustomer: customerID, Body: "1-나(고침)"}
	if _, err := store.ReviseMessage(ctx, core.KindEdit, mine); err != nil {
		t.Fatal(err)
	}

	t.Run("지금 대화보다 먼저 열린 상담들의 마지막 줄부터 오래된 순으로 준다", func(t *testing.T) {
		got, err := store.ListCustomerHistory(ctx, first.WorkspaceID, customerID, third.ID, core.PriorCursor{ConversationID: third.ID}, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b := bodies(got); len(b) != 3 || b[0] != "1-다" || b[1] != "2-가" || b[2] != "2-나" {
			t.Fatalf("직전 3줄: %v", b)
		}
		if got[0].ConversationID != first.ID || got[2].ConversationID != second.ID {
			t.Errorf("줄마다 어느 상담의 것인지 담는다: %s %s", got[0].ConversationID, got[2].ConversationID)
		}
	})

	t.Run("받은 첫 줄을 커서로 주면 그 앞이 이어지고, 수정 이벤트 행 없이 고친 본문이 나온다", func(t *testing.T) {
		got, err := store.ListCustomerHistory(ctx, first.WorkspaceID, customerID, third.ID, core.PriorCursor{ConversationID: first.ID, Seq: 3}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if b := bodies(got); len(b) != 2 || b[0] != "1-가" || b[1] != "1-나(고침)" {
			t.Errorf("그 앞 2줄: %v", b)
		}
	})

	t.Run("지금 대화와 그 뒤에 열린 상담은 나오지 않는다", func(t *testing.T) {
		got, err := store.ListCustomerHistory(ctx, first.WorkspaceID, customerID, second.ID, core.PriorCursor{ConversationID: second.ID}, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			if m.ConversationID != first.ID {
				t.Errorf("먼저 열린 상담만 나와야 한다: %s %q", m.ConversationID, m.Body)
			}
		}
		if len(got) != 3 {
			t.Errorf("첫 상담의 메시지 3줄: %v", bodies(got))
		}
	})

	t.Run("커서를 뒤에 열린 상담으로 줘도 지금 대화와 그 뒤의 상담은 나오지 않는다", func(t *testing.T) {
		got, err := store.ListCustomerHistory(ctx, first.WorkspaceID, customerID, second.ID, core.PriorCursor{ConversationID: third.ID, Seq: 99}, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got {
			if m.ConversationID != first.ID {
				t.Errorf("지금 대화(둘째)보다 앞의 것만 나와야 한다: %q", m.Body)
			}
		}
		if len(got) != 3 {
			t.Errorf("첫 상담의 3줄: %v", bodies(got))
		}
	})

	t.Run("다른 고객의 대화를 커서로 주거나 워크스페이스가 다르면 아무것도 나오지 않는다", func(t *testing.T) {
		_, stranger := testsupport.Conversation(t, store)
		got, err := store.ListCustomerHistory(ctx, first.WorkspaceID, customerID, third.ID, core.PriorCursor{ConversationID: stranger.ID}, 50)
		if err != nil || len(got) != 0 {
			t.Errorf("남의 대화 커서: %v (%v)", bodies(got), err)
		}
		got, err = store.ListCustomerHistory(ctx, id.New(), customerID, third.ID, core.PriorCursor{ConversationID: third.ID}, 50)
		if err != nil || len(got) != 0 {
			t.Errorf("다른 워크스페이스: %v (%v)", bodies(got), err)
		}
	})
}

func TestAgentReplyPoint_지운답변은답이아니다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "질문")); err != nil {
		t.Fatal(err)
	}
	reply, err := store.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Body: "답",
	})
	if err != nil {
		t.Fatal(err)
	}
	if point, err := store.AgentReplyPoint(ctx, conv.ID); err != nil || point != 2 {
		t.Fatalf("답한 지점은 고객 번호 2: %d %v", point, err)
	}
	if _, err := store.ReviseMessage(ctx, core.KindDelete, core.ReviseParams{
		ConversationID: conv.ID, MessageID: reply.Message.ID, ActorKind: core.SenderAgent, ActorAgentID: agent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if point, err := store.AgentReplyPoint(ctx, conv.ID); err != nil || point != 0 {
		t.Errorf("답을 지웠으면 답한 지점이 없다: %d %v", point, err)
	}
}

func TestReviseMessage_덮인수정본은이벤트행에남지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	sent, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "A"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"중간본 B", "최종본 C"} {
		if _, err := store.ReviseMessage(ctx, core.KindEdit, core.ReviseParams{
			ConversationID: conv.ID, MessageID: sent.Message.ID, ActorKind: core.SenderCustomer, ActorCustomer: customerID, Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var stale int
	if err := store.Pool().QueryRow(ctx,
		"SELECT count(*) FROM messages WHERE conversation_id = $1 AND body = '중간본 B'", conv.ID).Scan(&stale); err != nil || stale != 0 {
		t.Errorf("덮인 수정본이 로그에 남았다: %d (%v)", stale, err)
	}
	var latest int
	if err := store.Pool().QueryRow(ctx,
		"SELECT count(*) FROM messages WHERE conversation_id = $1 AND body = '최종본 C'", conv.ID).Scan(&latest); err != nil || latest != 2 {
		t.Errorf("최종본은 원 메시지와 마지막 이벤트 행에 있다: %d (%v)", latest, err)
	}
	revs, _ := store.Revisions(ctx, conv.ID, sent.Message.ID)
	if len(revs) != 2 || revs[0].Body != "A" || revs[1].Body != "중간본 B" {
		t.Errorf("이력에는 고치기 전 본문이 차례로 남는다: %+v", revs)
	}
}

// 수정·삭제는 새 메시지가 아니다. 받은함 순서와 안 읽은 수는 그대로고, 유실 안전망은 그 변화를 잡아야 한다.
func TestReviseMessage_받은함과안전망(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	first, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "첫 글"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "둘째 글"))
	if err != nil {
		t.Fatal(err)
	}
	unread := func() int {
		t.Helper()
		list, err := store.ListConversations(ctx, core.InboxFilter{WorkspaceID: conv.WorkspaceID, ReaderID: agent.ID, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list {
			if c.ID == conv.ID {
				return int(c.Unread)
			}
		}
		t.Fatal("받은함에 대화가 없다")
		return -1
	}
	lastMessageAt := func() (at time.Time) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx, "SELECT last_message_at FROM conversations WHERE id = $1", conv.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if got := unread(); got != 2 {
		t.Fatalf("안 읽은 글 2개: %d", got)
	}
	before := lastMessageAt()
	var cursor time.Time
	if err := store.Pool().QueryRow(ctx, "SELECT clock_timestamp()").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	mine := core.ReviseParams{ConversationID: conv.ID, MessageID: first.Message.ID, ActorKind: core.SenderCustomer, ActorCustomer: customerID, Body: "고친 첫 글"}
	if _, err := store.ReviseMessage(ctx, core.KindEdit, mine); err != nil {
		t.Fatal(err)
	}

	t.Run("고쳐도 받은함의 마지막 메시지 시각은 그대로다", func(t *testing.T) {
		if after := lastMessageAt(); !after.Equal(before) {
			t.Errorf("수정이 대화를 받은함 위로 올렸다: %v → %v", before, after)
		}
	})

	t.Run("고쳐도 상담원의 안 읽은 수는 늘지 않는다", func(t *testing.T) {
		if got := unread(); got != 2 {
			t.Errorf("수정 이벤트가 안 읽은 글로 세졌다: %d", got)
		}
	})

	t.Run("알림을 놓쳐도 안전망이 수정을 잡는다", func(t *testing.T) {
		changed, _, _, err := store.ChangedSince(ctx, core.ChangeCursor{At: cursor}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range changed {
			if c.ID == conv.ID {
				if c.Seq != 3 {
					t.Errorf("이벤트 행의 번호까지 알려야 한다: %d", c.Seq)
				}
				return
			}
		}
		t.Error("수정한 대화가 바뀐 대화 목록에 없다")
	})

	t.Run("지운 글은 안 읽은 수에서 빠진다", func(t *testing.T) {
		gone := core.ReviseParams{ConversationID: conv.ID, MessageID: second.Message.ID, ActorKind: core.SenderCustomer, ActorCustomer: customerID}
		if _, err := store.ReviseMessage(ctx, core.KindDelete, gone); err != nil {
			t.Fatal(err)
		}
		if got := unread(); got != 1 {
			t.Errorf("지운 글이 안 읽은 수에 남았다: %d", got)
		}
	})
}

// 집계 SQL 은 업무 기록 종류를 문자열로 센다. 상수 값과 SQL 의 문자열이 어긋나면 숫자가 조용히 0 이 된다.
// 배정·응답·종료는 E2E 업무 기록 테스트가 앱을 거쳐 이미 센다. 거기서 안 나오는 종류만 상수로 넣고 센다.
func TestWorkSummary_업무기록종류상수를센다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	_, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	for _, kind := range []core.EventKind{core.EventTransferred, core.EventSnoozed} {
		if err := store.RecordEvent(ctx, core.ConversationEvent{ConversationID: conv.ID, AgentID: agent.ID, Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.WorkSummary(ctx, core.DefaultWorkspaceID, time.Time{}, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rows {
		if w.AgentID != agent.ID {
			continue
		}
		if w.Assigned != 1 {
			t.Errorf("core.EventTransferred 는 배정으로 센다: %d", w.Assigned)
		}
		if w.Snoozed != 1 {
			t.Errorf("core.EventSnoozed 는 보류로 센다: %d", w.Snoozed)
		}
		return
	}
	t.Fatal("집계에 이 상담원의 줄이 없다")
}

// 새 상담을 여는 트랜잭션은 회원 잠금을 기다릴 수 있다. 변경 시각이 트랜잭션 시작(now())이면 기다린 만큼 과거라서,
// 안전망 겹침 구간(2초)보다 오래 기다린 새 상담은 알림을 놓치는 순간 영영 안 잡힌다. 변경 시각은 잠금을 얻은 뒤의 문장 시각이다.
func TestOpenMemberConversation_회원잠금을기다려늦게커밋된새상담도안전망이잡는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, err := store.CreateCustomer(ctx, core.DefaultWorkspaceID, "잠금 대기 회원")
	if err != nil {
		t.Fatal(err)
	}

	holder, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	// 키 숫자는 정책이라 리터럴로 고정한다(TestAdvisoryLocks_같은줄에선다).
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock(7302, hashtext($1::text))", customerID); err != nil {
		t.Fatal(err)
	}
	type opened struct {
		conv core.Conversation
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		conv, _, err := store.OpenMemberConversation(ctx, core.DefaultWorkspaceID, customerID)
		done <- opened{conv, err}
	}()
	time.Sleep(500 * time.Millisecond)
	if len(done) != 0 {
		t.Fatal("회원 잠금을 기다리지 않고 끝났다")
	}
	// 안전망이 이 시각까지 훑었다고 본다. 잠금을 놓은 뒤에 커밋되는 새 상담은 이 뒤에서 잡혀야 한다.
	var cursor time.Time
	if err := holder.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var got opened
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("잠금이 풀린 뒤에도 상담이 안 열렸다")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}

	changed, _, _, err := store.ChangedSince(ctx, core.ChangeCursor{At: cursor}, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changed {
		if c.ID == got.conv.ID {
			return
		}
	}
	t.Errorf("잠금을 기다려 늦게 커밋된 새 상담이 그 직전 시각 뒤의 변경 목록에 없다(변경 시각 %v, 커서 %v)", got.conv.UpdatedAt, cursor)
}
