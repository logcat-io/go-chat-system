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

// 고객이 상담을 직접 종료한다(D48). 종료와 상담원의 답·고객의 글이 같은 대화에서 겹치는 순간을 실제로 만든다 —
// 한쪽 트랜잭션이 대화 행을 쥔 채 커밋하지 않은 동안 다른 쪽이 그 행을 기다리게 하고, 커밋 뒤의 결과를 본다.

// customerSend 는 위젯이 보내는 고객 글이다. 앱은 고객 글에 Reopen 을 켠다(appendParams).
func customerSend(convID, customerID, body string) core.AppendParams {
	p := customerMessage(convID, customerID, id.New(), body)
	p.Visibility = core.Public
	p.Reopen = true
	return p
}

type closure struct {
	handler  string
	closedBy string
	state    string
}

func closureOf(t *testing.T, store *pgstore.Store, convID string) closure {
	t.Helper()
	var c closure
	err := store.Pool().QueryRow(context.Background(), `
		SELECT COALESCE(cl.handler, ''), COALESCE(c.closed_by, ''), c.state
		  FROM conversations c LEFT JOIN conversation_closures cl ON cl.conversation_id = c.id
		 WHERE c.id = $1`, convID).Scan(&c.handler, &c.closedBy, &c.state)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// held 는 테스트가 쥔 트랜잭션이다. 대화 행을 잠근 채 커밋을 미룬다.
func held(t *testing.T) (pgx.Tx, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx, conn
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s 이(가) 끝나지 않는다", what)
	}
	var zero T
	return zero
}

type appendOut struct {
	res core.AppendResult
	err error
}

// 정책: 고객 종료와 상담원 답이 겹쳐 답이 먼저 커밋되면 처리 주체는 상담원이다
func TestCloseByCustomer_고객종료와상담원답이겹쳐답이먼저커밋되면처리주체는상담원이다(t *testing.T) {
	store := testsupport.Store(t)
	agent := testsupport.Agent(t, store)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	if _, err := store.AppendMessage(ctx, customerSend(conv.ID, customerID, "문의")); err != nil {
		t.Fatal(err)
	}

	tx, conn := held(t)
	if _, err := pgstore.AppendInTx(ctx, tx, agentReply(conv.ID, agent.ID, "답입니다", true)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.CloseByCustomer(ctx, conv.ID, customerID, nil)
		done <- err
	}()
	waitForLockWait(t, conn)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done, "고객 종료"); err != nil {
		t.Fatal(err)
	}
	if got := closureOf(t, store, conv.ID); got != (closure{handler: "AGENT", closedBy: "CUSTOMER", state: "CLOSED"}) {
		t.Errorf("답이 먼저 커밋됐다 — 종료가 그 답을 보고 처리 주체는 상담원, 닫은 쪽은 고객이다: %+v", got)
	}
}

// 정책: 고객 종료가 먼저 커밋되면 겹친 상담원 답은 지금 규칙대로 상담을 다시 연다(종료된 상담에 쓴 상담원 공개 글은 다시 연다)
func TestCloseByCustomer_고객종료가먼저커밋되면겹친상담원답은지금규칙대로상담을다시연다(t *testing.T) {
	store := testsupport.Store(t)
	agent := testsupport.Agent(t, store)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	if _, err := store.AppendMessage(ctx, customerSend(conv.ID, customerID, "문의")); err != nil {
		t.Fatal(err)
	}

	tx, conn := held(t)
	if _, err := pgstore.CloseByCustomerInTx(ctx, tx, conv.ID, customerID); err != nil {
		t.Fatal(err)
	}
	done := make(chan appendOut, 1)
	go func() {
		res, err := store.AppendMessage(ctx, agentReply(conv.ID, agent.ID, "늦은 답", true))
		done <- appendOut{res, err}
	}()
	waitForLockWait(t, conn)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	out := await(t, done, "상담원 답")
	if out.err != nil || !out.res.Created {
		t.Fatalf("상담원의 공개 답은 닫힌 상담에도 저장된다: %+v %v", out.res, out.err)
	}
	got := closureOf(t, store, conv.ID)
	if got.state != "OPEN" || got.handler != "SELF" {
		t.Errorf("종료가 먼저다 — 그때의 처리 주체는 고객 자체 해결이고 상담원 답이 상담을 다시 연다: %+v", got)
	}
	// 다시 열렸으니 고객 입력도 다시 열린다.
	if _, err := store.AppendMessage(ctx, customerSend(conv.ID, customerID, "다시 열린 뒤의 글")); err != nil {
		t.Errorf("상담원 답으로 다시 열린 상담에는 고객이 쓴다: %v", err)
	}
}

// 정책: 고객 종료가 먼저 커밋되면 겹친 고객 글은 거절되고 번호에 빈칸이 없다(closed_by_customer)
func TestCloseByCustomer_고객종료가먼저커밋되면겹친고객글은거절되고번호에빈칸이없다(t *testing.T) {
	store := testsupport.Store(t)
	agent := testsupport.Agent(t, store)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	first, err := store.AppendMessage(ctx, customerSend(conv.ID, customerID, "문의"))
	if err != nil {
		t.Fatal(err)
	}

	tx, conn := held(t)
	if _, err := pgstore.CloseByCustomerInTx(ctx, tx, conv.ID, customerID); err != nil {
		t.Fatal(err)
	}
	done := make(chan appendOut, 1)
	go func() {
		res, err := store.AppendMessage(ctx, customerSend(conv.ID, customerID, "겹친 글"))
		done <- appendOut{res, err}
	}()
	waitForLockWait(t, conn)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if out := await(t, done, "고객 글"); !errors.Is(out.err, core.ErrClosedByCustomer) {
		t.Fatalf("고객이 닫아 둔 상담의 고객 글은 closed_by_customer 다: %+v %v", out.res, out.err)
	}
	if got := closureOf(t, store, conv.ID); got.state != "CLOSED" || got.closedBy != "CUSTOMER" {
		t.Errorf("거절된 글은 상담을 다시 열지 않는다: %+v", got)
	}
	// 거절된 글의 번호 증가는 되돌아갔다 — 다음 글(상담원 답)이 바로 다음 번호다(I1·I9).
	next, err := store.AppendMessage(ctx, agentReply(conv.ID, agent.ID, "다시 열고 답합니다", true))
	if err != nil {
		t.Fatal(err)
	}
	if next.PublicSeq != first.PublicSeq+1 {
		t.Errorf("공개 번호에 빈칸이 생겼다: 첫 글 %d, 다음 %d", first.PublicSeq, next.PublicSeq)
	}
}

// 정책: 고객 글이 먼저 커밋되면 글은 남고 종료는 그 뒤에 닫는다
func TestCloseByCustomer_고객글이먼저커밋되면글은남고종료는그뒤에닫는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)

	tx, conn := held(t)
	sent, err := pgstore.AppendInTx(ctx, tx, customerSend(conv.ID, customerID, "닫기 직전의 글"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.CloseByCustomer(ctx, conv.ID, customerID, nil)
		done <- err
	}()
	waitForLockWait(t, conn)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done, "고객 종료"); err != nil {
		t.Fatal(err)
	}
	msgs, err := store.ListMessages(ctx, conv.ID, 0, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != sent.Message.ID {
		t.Errorf("종료 앞에 커밋된 글은 남는다: %+v", msgs)
	}
	if got := closureOf(t, store, conv.ID); got != (closure{handler: "SELF", closedBy: "CUSTOMER", state: "CLOSED"}) {
		t.Errorf("답 없이 고객이 닫았다 — 고객 자체 해결: %+v", got)
	}
}

// 정책: 저장소는 그 상담의 고객이 아니면 고객 종료를 없는 상담으로 답한다
func TestCloseByCustomer_저장소는그상담의고객이아니면고객종료를없는상담으로답한다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	_, conv := testsupport.Conversation(t, store)
	other, _ := testsupport.Conversation(t, store)
	if _, err := store.CloseByCustomer(ctx, conv.ID, other, nil); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("남의 상담은 닫지 않는다(앱의 권한 검사가 빠져도): %v", err)
	}
	if got := closureOf(t, store, conv.ID); got.state != "OPEN" {
		t.Errorf("상태가 바뀌었다: %+v", got)
	}
}

// 정책: 0020 은 닫힌 상담의 닫은 쪽을 상담원 종료 기록이 있으면 상담원 없으면 시스템으로 채운다
func TestMigration0020_닫힌상담의닫은쪽을상담원종료기록이있으면상담원없으면시스템으로채운다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	_, byAgent := testsupport.Conversation(t, store)
	_, bySystem := testsupport.Conversation(t, store)
	_, open := testsupport.Conversation(t, store)

	// 0020 전의 모양으로 되돌린다: closed_by 가 없고 처리 주체에 SELF 가 없다.
	testsupport.Exec(t, dbURL, `
		ALTER TABLE conversations DROP COLUMN closed_by;
		ALTER TABLE conversation_closures DROP CONSTRAINT conversation_closures_handler_check;
		ALTER TABLE conversation_closures ADD CONSTRAINT conversation_closures_handler_check CHECK (handler IN ('AGENT', 'AI', 'UNANSWERED'));
		DELETE FROM schema_migrations WHERE name = '0020_customer_close.sql'`)
	testsupport.Exec(t, dbURL, `UPDATE conversations SET state = 'CLOSED' WHERE id IN ($1, $2)`, byAgent.ID, bySystem.ID)
	testsupport.Exec(t, dbURL, `
		INSERT INTO conversation_events (workspace_id, conversation_id, customer_id, agent_id, kind, detail)
		SELECT workspace_id, id, customer_id, $2, 'closed', '{}' FROM conversations WHERE id = $1`, byAgent.ID, agent.ID)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("0020 이 0019 상태 위에서 돌아야 한다: %v", err)
	}
	for _, tc := range []struct {
		conv string
		want string
	}{{byAgent.ID, "AGENT"}, {bySystem.ID, "SYSTEM"}, {open.ID, ""}} {
		if got := closureOf(t, store, tc.conv); got.closedBy != tc.want {
			t.Errorf("%s: 닫은 쪽 %q, 기대 %q", tc.conv, got.closedBy, tc.want)
		}
	}
}

// 정책: 열린 채 가렸던 지난 상담을 되살리면 종료로 돌리고 닫은 쪽은 시스템이며 이미 닫혀 있던 상담은 닫은 쪽을 그대로 둔다
func TestRestoreConversation_열린채가렸던지난상담을되살리면종료로돌리고닫은쪽은시스템이며이미닫혀있던상담은닫은쪽을그대로둔다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	_, openPast := testsupport.Conversation(t, store)
	_, closedPast := testsupport.Conversation(t, store)
	_, openCurrent := testsupport.Conversation(t, store)
	pool := store.Pool()
	if _, err := pool.Exec(ctx, `UPDATE conversations SET past_for_customer = true, deleted_at = now() WHERE id = $1`, openPast.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET past_for_customer = true, deleted_at = now(), state = 'CLOSED', closed_by = 'CUSTOMER' WHERE id = $1`, closedPast.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET deleted_at = now() WHERE id = $1`, openCurrent.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{openPast.ID, closedPast.ID, openCurrent.ID} {
		if err := store.RestoreConversation(ctx, c); err != nil {
			t.Fatalf("되살리기: %v", err)
		}
	}
	for _, tc := range []struct {
		name, conv, state, closedBy string
	}{
		{"열린 채 가렸던 지난 상담", openPast.ID, "CLOSED", "SYSTEM"},
		{"고객이 닫은 뒤 가렸던 지난 상담", closedPast.ID, "CLOSED", "CUSTOMER"},
		{"지난 상담이 아닌 열린 상담", openCurrent.ID, "OPEN", ""},
	} {
		if got := closureOf(t, store, tc.conv); got.state != tc.state || got.closedBy != tc.closedBy {
			t.Errorf("%s: state=%s closed_by=%q, 기대 state=%s closed_by=%q", tc.name, got.state, got.closedBy, tc.state, tc.closedBy)
		}
	}
}
