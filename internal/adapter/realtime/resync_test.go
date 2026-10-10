package realtime_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/realtime"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 먼저 시작해 늦게 커밋한 변경은 커서보다 과거 시각을 갖는다. 안전망이 매 주기 겹침 구간을 다시 훑지 않으면 영영 못 본다.
func TestChangeWatcher_늦게커밋한변경을놓치지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	custA, convA := testsupport.Conversation(t, store)
	custB, convB := testsupport.Conversation(t, store)

	hub := realtime.NewHub()
	sub := hub.Join("")
	defer hub.Leave(sub)
	hub.Bind(sub, convA.ID)
	<-sub.Wake() // Bind 가 한 번 깨운다. 그건 센다.

	const interval = 200 * time.Millisecond
	go realtime.NewChangeWatcher(store, hub, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	time.Sleep(interval) // 워처가 DB 시계로 시작점을 잡을 시간

	// A 의 변경: 번호 발급 문장은 지금, 커밋은 한참 뒤.
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET last_seq = last_seq + 1, last_public_seq = last_public_seq + 1,
		last_customer_seq = last_seq + 1, last_message_at = clock_timestamp() WHERE id = $1`, convA.ID); err != nil {
		t.Fatal(err)
	}
	// 그동안 B 가 커밋해 커서가 A 의 시각을 지나간다.
	for i := 0; i < 3; i++ {
		if _, err := store.AppendMessage(ctx, core.AppendParams{
			ConversationID: convB.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
			SenderCustomer: custB, Visibility: core.Public, Body: "B",
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(interval)
	}
	// 지금까지의 깨우기(A 를 만든 변경 등)는 버린다. 아래에서 받는 깨우기는 늦은 커밋에서 와야 한다.
	select {
	case <-sub.Wake():
	default:
	}
	if _, err := tx.Exec(ctx, `INSERT INTO messages (conversation_id, seq, public_seq, id, client_msg_id, sender_kind, sender_customer, visibility, body)
		SELECT $1, last_seq, last_public_seq, $2, $3, 'CUSTOMER', $4, 'PUBLIC', 'A 늦은 커밋' FROM conversations WHERE id = $1`,
		convA.ID, id.New(), id.New(), custA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case <-sub.Wake():
	case <-time.After(10 * interval):
		t.Fatal("늦게 커밋한 A 의 변경을 안전망이 깨우지 않았다")
	}
}

// 안전망은 겹침 구간을 매 주기 다시 훑는다. 그 구간의 변경을 매번 다시 알리면 쉬는 동안에도 받은함 구독자(데스크)가
// 주기마다 깨어 받은함을 다시 조회한다 — 그 부하가 상담원 수에 비례한다. 이미 알린 변경은 다시 알리지 않고, 새 변경은 알린다.
func TestChangeWatcher_변경이없으면이미알린대화를다시깨우지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cust, conv := testsupport.Conversation(t, store)

	hub := realtime.NewHub()
	inbox := hub.Join(conv.WorkspaceID)
	defer hub.Leave(inbox)
	sub := hub.Join("")
	defer hub.Leave(sub)
	hub.Bind(sub, conv.ID)
	<-sub.Wake()

	const interval = 100 * time.Millisecond
	go realtime.NewChangeWatcher(store, hub, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	drain := func(ch <-chan struct{}) {
		select {
		case <-ch:
		default:
		}
	}
	// 대화를 만든 변경은 겹침 구간 안이라 첫 주기에 한 번 알린다. 그 깨우기는 버린다.
	time.Sleep(3 * interval)
	drain(sub.Wake())

	appendOne := func() {
		t.Helper()
		if _, err := store.AppendMessage(ctx, core.AppendParams{
			ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
			SenderCustomer: cust, Visibility: core.Public, Body: "한 번",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 이미 알린 대화라도 새 변경은 알린다. 겹침 구간 안에서 연달아 바뀌어도 매번 알린다.
	for i := range 2 {
		appendOne()
		select {
		case <-sub.Wake():
		case <-time.After(20 * interval):
			t.Fatalf("%d번째 새 변경을 안전망이 깨우지 않았다", i+1)
		}
	}
	// 겹침 구간(2초)과 시작 직전의 다른 테스트 변경이 지나가길 기다린 뒤부터 센다.
	time.Sleep(2*time.Second + 3*interval)
	drain(sub.Wake())
	drain(inbox.InboxWake())

	quiet := time.After(10 * interval)
	wakes, inboxWakes := 0, 0
	for done := false; !done; {
		select {
		case <-sub.Wake():
			wakes++
		case <-inbox.InboxWake():
			inboxWakes++
		case <-quiet:
			done = true
		}
	}
	if wakes != 0 || inboxWakes != 0 {
		t.Errorf("변경이 없는데 10주기 동안 대화를 %d번, 받은함을 %d번 깨웠다", wakes, inboxWakes)
	}

	// 새 변경은 여전히 알린다.
	appendOne()
	select {
	case <-sub.Wake():
	case <-time.After(20 * interval):
		t.Fatal("이미 알린 대화의 새 변경을 안전망이 깨우지 않았다")
	}
}

// 새 상담이 열리면 앞선 상담은 그 회원에게 지난 상담이 된다. 옛 탭은 메타 변화로 깨어나 권한 검사에서 떨어져야 한다.
// 그 알림을 놓쳐도 안전망이 잡으려면 지난 상담으로 바꾸는 쓰기가 변경 시각을 올려야 한다.
func TestChangeWatcher_알림을놓쳐도지난상담으로바뀐대화를안전망이다음주기에깨운다(t *testing.T) {
	store := testsupport.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cust, old := testsupport.Conversation(t, store)
	if _, err := store.SetConversationState(ctx, old.ID, core.StateClosed, nil, nil); err != nil {
		t.Fatal(err)
	}

	// 알림 리스너는 붙이지 않는다. 깨우기는 안전망에서만 온다.
	hub := realtime.NewHub()
	sub := hub.Join("")
	defer hub.Leave(sub)
	hub.Bind(sub, old.ID)
	<-sub.Wake()

	const interval = 100 * time.Millisecond
	go realtime.NewChangeWatcher(store, hub, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	// 닫은 변경은 겹침 구간 안이라 한 번 알린다. 그 깨우기는 버린다.
	time.Sleep(3 * interval)
	select {
	case <-sub.Wake():
	default:
	}

	fresh, result, err := store.OpenMemberConversation(ctx, core.DefaultWorkspaceID, cust)
	if err != nil {
		t.Fatal(err)
	}
	if result != core.OpenedNew || fresh.ID == old.ID {
		t.Fatalf("새 상담이 열려야 한다: %v %s", result, fresh.ID)
	}
	var past bool
	if err := store.Pool().QueryRow(ctx, "SELECT past_for_customer FROM conversations WHERE id = $1", old.ID).Scan(&past); err != nil || !past {
		t.Fatalf("앞선 상담이 지난 상담이 아니다: %v %v", past, err)
	}

	select {
	case <-sub.Wake():
	case <-time.After(3 * interval):
		t.Fatal("지난 상담으로 바뀐 대화를 안전망이 다음 주기에 깨우지 않았다")
	}
}

// 안전망은 변경 시각으로만 바뀐 대화를 찾는다(D4). 구독자에게 보이는 대화 변화가 시각을 안 올리면 알림을 놓친 순간 영영 못 깨운다.
// 리스너 없이, 같은 대화에 겹침 구간(2초) 안에서 연달아 일어난 쓰기를 하나씩 해 보고 매번 대화 구독자와 받은함 구독자가 깨는지 본다.
// 메시지 밖의 변화(상태·담당자·태그·보류·가리기·되살리기·지난 상담)는 메타 힌트를 실어야 연결이 대화 행을 다시 읽는다.
func TestChangeWatcher_알림이없어도대화를바꾸는모든쓰기는안전망이구독자와받은함을깨운다(t *testing.T) {
	store := testsupport.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cust, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	tag, err := store.EnsureTag(ctx, core.DefaultWorkspaceID, "safety-net-"+id.New()[:8])
	if err != nil {
		t.Fatal(err)
	}

	hub := realtime.NewHub()
	inbox := hub.Join(conv.WorkspaceID)
	defer hub.Leave(inbox)
	sub := hub.Join("")
	defer hub.Leave(sub)
	hub.Bind(sub, conv.ID)
	<-sub.Wake()
	sub.TakeHint()

	const interval = 100 * time.Millisecond
	go realtime.NewChangeWatcher(store, hub, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	drain := func(ch <-chan struct{}) {
		select {
		case <-ch:
		default:
		}
	}
	// 대화를 만든 변경은 첫 주기에 알린다. 그 깨우기는 버린다.
	time.Sleep(3 * interval)
	drain(sub.Wake())
	drain(inbox.InboxWake())
	sub.TakeHint()

	// awaitWake 는 안전망이 대화 구독자를 깨울 때까지 기다리고 그 힌트를 준다.
	awaitWake := func(name string) (realtime.Hint, bool) {
		t.Helper()
		select {
		case <-sub.Wake():
			return sub.TakeHint(), true
		case <-time.After(20 * interval):
			t.Errorf("%s: 안전망이 대화 구독자를 깨우지 않았다", name)
			return realtime.Hint{}, false
		}
	}

	var firstMsg string
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	steps := []struct {
		name string
		meta bool
		do   func() error
	}{
		{"메시지", false, func() error {
			r, err := store.AppendMessage(ctx, core.AppendParams{ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
				SenderCustomer: cust, Visibility: core.Public, Body: "첫 글"})
			firstMsg = r.Message.ID
			return err
		}},
		{"닫기", true, func() error {
			_, err := store.SetConversationState(ctx, conv.ID, core.StateClosed, nil, nil)
			return err
		}},
		{"다시 열기", true, func() error { _, err := store.SetConversationState(ctx, conv.ID, core.StateOpen, nil, nil); return err }},
		{"배정", true, func() error { _, err := store.AssignConversation(ctx, conv.ID, agent.ID, nil, nil); return err }},
		{"배정 해제", true, func() error { _, err := store.AssignConversation(ctx, conv.ID, "", nil, nil); return err }},
		{"태그 붙이기", true, func() error { return store.AddConversationTag(ctx, conv.ID, tag.ID) }},
		{"태그 떼기", true, func() error { return store.RemoveConversationTag(ctx, conv.ID, tag.ID) }},
		{"보류", true, func() error {
			_, err := store.SetConversationState(ctx, conv.ID, core.StateSnoozed, &future, nil)
			return err
		}},
		{"보류에서 열기", true, func() error { _, err := store.SetConversationState(ctx, conv.ID, core.StateOpen, nil, nil); return err }},
		{"글 고치기", true, func() error {
			_, err := store.ReviseMessage(ctx, core.KindEdit, core.ReviseParams{ConversationID: conv.ID, MessageID: firstMsg,
				ActorKind: core.SenderCustomer, ActorCustomer: cust, Body: "고친 첫 글"})
			return err
		}},
		{"내부 메모", false, func() error {
			_, err := store.AppendMessage(ctx, core.AppendParams{ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
				SenderAgentID: agent.ID, Visibility: core.Private, Body: "메모"})
			return err
		}},
		{"보류 시각이 지나 저절로 열림", true, func() error {
			if _, err := store.SetConversationState(ctx, conv.ID, core.StateSnoozed, &past, nil); err != nil {
				return err
			}
			// 보류 자체도 한 번 깨운다. 그 깨우기는 버리고 저절로 열리는 쓰기를 본다.
			awaitWake("지난 시각으로 보류")
			drain(inbox.InboxWake())
			_, err := store.WakeSnoozed(ctx)
			return err
		}},
		{"가리기", true, func() error { return store.DeleteConversation(ctx, conv.ID, agent.ID, "정책 테스트") }},
		{"되살리기", true, func() error { return store.RestoreConversation(ctx, conv.ID) }},
		{"지난 상담 전환", true, func() error {
			if _, err := store.SetConversationState(ctx, conv.ID, core.StateClosed, nil, nil); err != nil {
				return err
			}
			awaitWake("지난 상담 앞의 닫기")
			drain(inbox.InboxWake())
			_, _, err := store.OpenMemberConversation(ctx, core.DefaultWorkspaceID, cust)
			return err
		}},
	}
	for _, s := range steps {
		drain(inbox.InboxWake())
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if h, ok := awaitWake(s.name); ok && h.Meta != s.meta {
			if s.meta {
				t.Errorf("%s: 메시지 밖의 변화인데 힌트에 메타가 없다: %+v", s.name, h)
			} else {
				t.Errorf("%s: 메시지만 바뀌었는데 힌트에 메타가 켜졌다: %+v", s.name, h)
			}
		}
		select {
		case <-inbox.InboxWake():
		case <-time.After(20 * interval):
			t.Errorf("%s: 안전망이 받은함 구독자를 깨우지 않았다", s.name)
		}
	}
}

func TestChangeWatcher_겹침구간안에늦게커밋된상태변경도메타를싣는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, x := testsupport.Conversation(t, store)
	hub := realtime.NewHub()
	sub := hub.Join("")
	defer hub.Leave(sub)
	hub.Bind(sub, x.ID)
	<-sub.Wake()
	sub.TakeHint()
	const interval = 100 * time.Millisecond
	go realtime.NewChangeWatcher(store, hub, interval, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	// 다른 대화 Y 의 변경으로 커서를 X 의 생성 시각보다 겹침 구간 넘게 앞세운다. X 는 기억에서 빠진다.
	time.Sleep(2500 * time.Millisecond)
	ycust, y := testsupport.Conversation(t, store)
	time.Sleep(5 * interval)
	select {
	case <-sub.Wake():
	default:
	}
	sub.TakeHint()

	// X 의 닫기는 시각을 먼저 받고 커밋은 뒤로 미룬다. 그동안 Y 가 커밋해 커서가 닫기 시각을 지나간다.
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "UPDATE conversations SET state = 'CLOSED', updated_at = clock_timestamp() WHERE id = $1", x.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := store.AppendMessage(ctx, core.AppendParams{ConversationID: y.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
		SenderCustomer: ycust, Visibility: core.Public, Body: "y"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * interval)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Wake():
		if h := sub.TakeHint(); !h.Meta {
			t.Fatalf("늦게 커밋된 닫기인데 힌트에 메타가 없다: %+v", h)
		}
	case <-time.After(20 * interval):
		t.Fatal("늦게 커밋된 변경을 안전망이 깨우지 않았다")
	}
}
