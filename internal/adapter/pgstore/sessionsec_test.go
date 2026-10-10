package pgstore_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 토큰 보안 묶음(D46)의 저장소 쪽 정책.

// 티켓 쓰기는 DELETE … RETURNING 한 문장이다. 동시에 몇 번을 써도 행을 얻는 것은 하나다 — 앱이 먼저 읽고 지우면 둘 다 통과한다.
func TestRedeemWSTicket_같은티켓을동시에여러번써도하나만주체를얻는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	custID, _ := testsupport.Conversation(t, store)
	token, err := store.CreateSession(ctx, "CUSTOMER", "", custID, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.SubjectByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		ticket, _, err := store.IssueWSTicket(ctx, sub.Session, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				got, err := store.RedeemWSTicket(ctx, ticket)
				switch {
				case err == nil && got.CustomerID == custID:
					wins.Add(1)
				case err != nil && !errors.Is(err, core.ErrNotFound):
					t.Errorf("일시 오류가 아닌 답이어야 한다: %v", err)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("한 티켓으로 %d번 주체를 얻었다", wins.Load())
		}
	}
}

// 세션 행이 지워지면 트리거가 알린다. 리스너는 그 세션 참조를 넘긴다 — 열린 연결이 곧바로 세션을 다시 묻는 근거다.
// 한 문장으로 여러 세션을 지워도(비활성·만료 정리) 모두 온다.
func TestListener_세션행을지우면지운세션참조가모두알림으로온다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	want := map[core.SessionRef]bool{}
	for range 3 {
		tok, err := store.CreateSession(ctx, "AGENT", agent.ID, "", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := store.SubjectByToken(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		want[sub.Session] = true
	}
	got := make(chan core.SessionRef, 16)
	l := pgstore.NewListener(testsupport.URL(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.OnSessionsEnded = func(refs []core.SessionRef) {
		for _, r := range refs {
			got <- r
		}
	}
	connected := make(chan struct{}, 1)
	l.OnResync = func() { connected <- struct{}{} }
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { l.Run(lctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("리스너가 붙지 않았다")
	}

	// 비활성은 그 상담원의 세션을 한 문장으로 지운다.
	if err := store.SetAgentActive(ctx, core.DefaultWorkspaceID, agent.ID, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case r := <-got:
			delete(want, r)
		case <-deadline:
			t.Fatalf("알림이 오지 않은 세션 %d개", len(want))
		}
	}
}

func TestPruneCustomerSessions_최신keep개만남기고오래된것부터지운다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	custID, _ := testsupport.Conversation(t, store)
	var tokens []string
	for range 5 {
		tok, err := store.CreateSession(ctx, "CUSTOMER", "", custID, time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
		time.Sleep(5 * time.Millisecond) // 만든 순서가 시각에 남게
	}
	n, err := store.PruneCustomerSessions(ctx, custID, 2)
	if err != nil || n != 3 {
		t.Fatalf("지운 수 %d, %v", n, err)
	}
	for i, tok := range tokens {
		_, err := store.SubjectByToken(ctx, tok)
		if alive := err == nil; alive != (i >= 3) {
			t.Errorf("%d번째 세션 alive=%v", i, alive)
		}
	}
}
