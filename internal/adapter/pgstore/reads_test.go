package pgstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func TestMarkRead_단조증가(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	for i := 0; i < 5; i++ {
		if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "m")); err != nil {
			t.Fatal(err)
		}
	}
	agentReader := core.LiveEvent{ActorKind: string(core.SenderAgent), ActorID: agent.ID, ActorName: agent.Name}

	t.Run("처음 보고한 지점이 저장된다", func(t *testing.T) {
		got, err := store.MarkRead(ctx, conv.ID, agentReader, 3, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != 3 {
			t.Errorf("3 이어야 한다: %d", got)
		}
	})

	t.Run("더 작은 지점을 보고하면 무시하고 기존 값을 돌려준다", func(t *testing.T) {
		got, err := store.MarkRead(ctx, conv.ID, agentReader, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != 3 {
			t.Errorf("읽음 지점이 되돌아갔다: %d", got)
		}
	})

	t.Run("더 큰 지점은 올라간다", func(t *testing.T) {
		got, err := store.MarkRead(ctx, conv.ID, agentReader, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != 5 {
			t.Errorf("5 여야 한다: %d", got)
		}
	})

	t.Run("고객과 상담원의 읽음 지점은 따로 쌓인다", func(t *testing.T) {
		custReader := core.LiveEvent{ActorKind: string(core.SenderCustomer), ActorID: customerID, ActorName: "테스트고객"}
		if _, err := store.MarkRead(ctx, conv.ID, custReader, 2, false); err != nil {
			t.Fatal(err)
		}
		states, err := store.ReadStates(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 2 {
			t.Fatalf("읽음 지점이 둘이어야 한다: %+v", states)
		}
		byKind := map[string]int64{}
		for _, s := range states {
			byKind[s.ReaderKind] = s.LastReadSeq
		}
		if byKind["AGENT"] != 5 || byKind["CUSTOMER"] != 2 {
			t.Errorf("지점이 섞였다: %+v", byKind)
		}
	})

	t.Run("주체별 조회가 같은 값을 준다", func(t *testing.T) {
		got, err := store.ReadSeqOf(ctx, conv.ID, string(core.SenderAgent), agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != 5 {
			t.Errorf("5 여야 한다: %d", got)
		}
	})

	t.Run("읽은 적 없는 주체는 0 이다", func(t *testing.T) {
		other := testsupport.Agent(t, store)
		got, err := store.ReadSeqOf(ctx, conv.ID, string(core.SenderAgent), other.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("0 이어야 한다: %d", got)
		}
	})
}

func TestListConversations_안읽은수(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	for i := 0; i < 4; i++ {
		if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "문의")); err != nil {
			t.Fatal(err)
		}
	}

	find := func(t *testing.T, readerID string) core.Conversation {
		t.Helper()
		convs, err := store.ListConversations(ctx, core.InboxFilter{WorkspaceID: core.DefaultWorkspaceID,
			State: "OPEN", ReaderID: readerID, Limit: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range convs {
			if c.ID == conv.ID {
				return c
			}
		}
		t.Fatalf("목록에서 대화를 찾지 못했다")
		return core.Conversation{}
	}

	t.Run("읽은 적 없으면 전부 안 읽은 수다", func(t *testing.T) {
		if got := find(t, agent.ID).Unread; got != 4 {
			t.Errorf("4 여야 한다: %d", got)
		}
	})

	t.Run("읽은 만큼 줄어든다", func(t *testing.T) {
		reader := core.LiveEvent{ActorKind: string(core.SenderAgent), ActorID: agent.ID}
		if _, err := store.MarkRead(ctx, conv.ID, reader, 3, true); err != nil {
			t.Fatal(err)
		}
		if got := find(t, agent.ID).Unread; got != 1 {
			t.Errorf("1 이어야 한다: %d", got)
		}
	})

	t.Run("다른 상담원의 안 읽은 수는 영향받지 않는다", func(t *testing.T) {
		other := testsupport.Agent(t, store)
		if got := find(t, other.ID).Unread; got != 4 {
			t.Errorf("4 여야 한다: %d", got)
		}
	})

	t.Run("읽는 주체를 안 주면 전부 안 읽은 것으로 센다", func(t *testing.T) {
		if got := find(t, "").Unread; got != 4 {
			t.Errorf("4 여야 한다: %d", got)
		}
	})
}

func TestPublishLive_흔적을남기지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)

	t.Run("타이핑 신호는 메시지도 읽음 행도 만들지 않는다", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			err := store.PublishLive(ctx, core.LiveEvent{
				Kind: core.LiveTyping, ConversationID: conv.ID,
				ActorKind: string(core.SenderCustomer), ActorID: customerID, ActorName: "테스트고객",
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		seqs, err := store.MessageSeqs(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(seqs) != 0 {
			t.Errorf("메시지가 생겼다: %v", seqs)
		}
		states, err := store.ReadStates(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 0 {
			t.Errorf("읽음 행이 생겼다: %+v", states)
		}
	})
}

// 읽음 갱신의 실시간 알림도 커밋 뒤에 5초까지만 기다린다. 읽음 지점은 이미 커밋됐고, 놓친 알림은 재접속 때 ready 가 맞춘다 —
// 그래서 시한을 넘기면 읽음은 성공으로 돌려주고 notify.failed 한 줄에 대화 id 만 남긴다(D30). 5초는 정책이라 리터럴로 고정한다.
// 재현은 TestAppendMessage_커밋뒤알림은5초를넘겨기다리지않고저장은성공으로돌려준다 와 같은 중계기(notifyStaller)로 한다.
func TestMarkRead_읽음알림은5초를넘겨기다리지않고읽음은성공으로돌려준다(t *testing.T) {
	const notifyLimit = 5 * time.Second
	testsupport.Store(t) // 스키마 준비. DB 가 없으면 건너뛴다
	proxy := newNotifyStaller(t)
	ctx := context.Background()
	store, err := pgstore.OpenWithPool(ctx, proxy.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "읽음 알림이 막혀도 읽음은 남는다")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(logctx.NewHandler(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	proxy.armed.Store(true)
	type outcome struct {
		seq int64
		err error
	}
	marked := make(chan outcome, 1)
	reader := core.LiveEvent{ActorKind: string(core.SenderAgent), ActorID: agent.ID, ActorName: agent.Name, WorkspaceID: core.DefaultWorkspaceID}
	start := time.Now()
	go func() {
		seq, err := store.MarkRead(ctx, conv.ID, reader, 1, true)
		marked <- outcome{seq, err}
	}()
	var got outcome
	select {
	case got = <-marked:
	case <-time.After(15 * time.Second):
		proxy.release() // 붙든 연결을 끊어 저장소를 닫을 수 있게 한다
		<-marked
		t.Fatal("읽음 알림이 시한 없이 기다린다 — 15초가 지나도 읽음 갱신이 돌아오지 않았다")
	}
	elapsed := time.Since(start)
	proxy.armed.Store(false)
	proxy.release()

	if proxy.stalled.Load() == 0 {
		t.Fatal("알림 문장을 붙들지 못했다 — 막힌 알림을 재현하지 못했다")
	}
	if got.err != nil || got.seq != 1 {
		t.Fatalf("읽음은 성공이어야 한다: %d %v", got.seq, got.err)
	}
	if elapsed < notifyLimit-200*time.Millisecond || elapsed > notifyLimit+2*time.Second {
		t.Errorf("알림은 %v 에서 포기해야 한다: %v 걸렸다", notifyLimit, elapsed)
	}
	states, err := testsupport.Store(t).ReadStates(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	for _, s := range states {
		if s.ReaderID == agent.ID && s.LastReadSeq == 1 {
			committed = true
		}
	}
	if !committed {
		t.Errorf("읽음 지점이 커밋돼 있어야 한다: %+v", states)
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "notify.failed" {
			line = l
		}
	}
	if line == nil {
		t.Fatalf("notify.failed 가 없다:\n%s", out)
	}
	if line["conversation_id"] != conv.ID || line["reason"] != "timeout" || line["level"] != "WARN" {
		t.Errorf("내용이 틀렸다: %v", line)
	}
	// 로그에는 id 만 적는다(D30). 알림 본문(이름 등)이나 오류 문구를 싣지 않는다.
	for key := range line {
		switch key {
		case "time", "level", "msg", "conversation_id", "reason":
		default:
			t.Errorf("notify.failed 에 id 가 아닌 %q 가 실렸다: %v", key, line)
		}
	}
	if strings.Contains(out, agent.Name) {
		t.Errorf("로그에 상담원 이름이 실렸다:\n%s", out)
	}
}
