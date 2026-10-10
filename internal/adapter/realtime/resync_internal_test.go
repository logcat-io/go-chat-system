package realtime

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// fakeChanges 는 정해 둔 변경 목록을 커서 뒤부터 주는 저장소다. 안전망의 기억만 보려고 DB 없이 돈다.
type fakeChanges struct {
	changes []core.ChangedConv
}

func (f *fakeChanges) ChangedSince(_ context.Context, cur core.ChangeCursor, limit int) ([]core.ChangedConv, core.ChangeCursor, bool, error) {
	sort.Slice(f.changes, func(i, j int) bool { return f.changes[i].At.Before(f.changes[j].At) })
	var out []core.ChangedConv
	next := cur
	for _, c := range f.changes {
		if c.At.After(cur.At) || (c.At.Equal(cur.At) && c.ID > cur.ID) {
			out = append(out, c)
			next = core.ChangeCursor{At: c.At, ID: c.ID}
			if len(out) == limit {
				return out, next, true, nil
			}
		}
	}
	return out, next, false, nil
}

func (f *fakeChanges) Now(context.Context) (time.Time, error) { return time.Time{}, nil }

// 안전망은 겹침 구간 안에서 이미 알린 변경을 기억한다. 구간 밖으로 밀려난 변경은 다시 훑지 않으니 기억에서 지운다 —
// 안 지우면 서버가 떠 있는 동안 바뀐 적 있는 모든 대화가 쌓인다.
func TestChangeWatcher_안전망의기억은겹침구간밖으로밀려난변경을지운다(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	src := &fakeChanges{}
	w := NewChangeWatcher(src, NewHub(), time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st := &watchState{last: core.ChangeCursor{At: t0}, notified: map[string]core.ChangedConv{}}
	ctx := context.Background()

	src.changes = append(src.changes, core.ChangedConv{ID: "a", At: t0.Add(time.Second), UpdatedAt: t0.Add(time.Second)})
	if woken := w.poll(ctx, st); woken != 1 {
		t.Fatalf("첫 변경을 %d번 깨웠다", woken)
	}
	// 겹침 구간(2초) 안이라 a 는 다시 훑이지만 기억에 있어 다시 깨우지 않는다.
	src.changes = append(src.changes, core.ChangedConv{ID: "b", At: t0.Add(10 * time.Second), UpdatedAt: t0.Add(10 * time.Second)})
	if woken := w.poll(ctx, st); woken != 1 {
		t.Fatalf("둘째 주기에 %d번 깨웠다. 새 변경 b 하나여야 한다", woken)
	}
	// 이제 구간은 b 의 2초 앞에서 시작한다. a 는 구간 밖이다.
	w.poll(ctx, st)
	if _, ok := st.notified["a"]; ok || len(st.notified) != 1 {
		t.Errorf("구간 밖으로 밀려난 변경이 기억에 남았다: %d개 %v", len(st.notified), st.notified)
	}
}

// brokenChanges 는 변경 조회가 죽은 연결처럼 답하지 않거나(hang) 접속 정보를 담은 오류로 실패하는 저장소다.
type brokenChanges struct{ hang bool }

func (b brokenChanges) ChangedSince(ctx context.Context, cur core.ChangeCursor, _ int) ([]core.ChangedConv, core.ChangeCursor, bool, error) {
	if b.hang {
		<-ctx.Done()
		return nil, cur, false, ctx.Err()
	}
	return nil, cur, false, leakErr{}
}

func (brokenChanges) Now(context.Context) (time.Time, error) { return time.Time{}, nil }

// DEF-76: 안전망의 변경 조회에 시한이 없어 죽은 DB 연결을 붙잡으면 안전망 전체가 멈췄고, 실패 WARN 은 오류 원문(접속 정보)을 실었다.
func TestChangeWatcher_변경조회는5초시한을넘기면그주기를접고WARN에오류원문없이종류만싣는다(t *testing.T) {
	ctx := context.Background()
	t.Run("시한", func(t *testing.T) {
		logs := &syncBuffer{}
		w := NewChangeWatcher(brokenChanges{hang: true}, NewHub(), time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		st := &watchState{last: core.ChangeCursor{At: time.Now()}, notified: map[string]core.ChangedConv{}}
		start := time.Now()
		done := make(chan int, 1)
		go func() { done <- w.poll(ctx, st) }()
		select {
		case <-done:
		case <-time.After(3 * policyReadTimeout):
			t.Fatal("변경 조회가 시한 없이 기다린다 — 안전망이 멈춘다")
		}
		if took := time.Since(start); took < policyReadTimeout-100*time.Millisecond || took > policyReadTimeout+time.Second {
			t.Errorf("변경 조회가 %v 만에 포기했다. 시한은 %v 다", took, policyReadTimeout)
		}
		if l := logLines(logs, "resync.query_failed"); len(l) != 1 || l[0]["err_kind"] != "timeout" || l[0]["level"] != "WARN" {
			t.Errorf("resync.query_failed 는 err_kind=timeout 인 WARN 한 줄이어야 한다: %v", l)
		}
	})
	t.Run("오류 원문", func(t *testing.T) {
		logs := &syncBuffer{}
		w := NewChangeWatcher(brokenChanges{}, NewHub(), time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
		w.poll(ctx, &watchState{last: core.ChangeCursor{At: time.Now()}, notified: map[string]core.ChangedConv{}})
		l := logLines(logs, "resync.query_failed")
		if len(l) != 1 || l[0]["err_kind"] != "57P01" || l[0]["err"] != nil {
			t.Errorf("resync.query_failed 는 err_kind=57P01 만 실어야 한다: %v", l)
		}
		for _, leak := range []string{"user=", "database=", "chat_leak", "127.0.0.1"} {
			if strings.Contains(logs.String(), leak) {
				t.Errorf("로그에 접속 정보 %q 가 실렸다:\n%s", leak, logs.String())
			}
		}
	})
}

// clockBroken 은 DB 시계를 읽지 못하는 저장소다. 변경은 없다.
type clockBroken struct{ fakeChanges }

func (clockBroken) Now(context.Context) (time.Time, error) { return time.Time{}, leakErr{} }

// 안전망이 시작할 때 DB 시계를 못 읽으면 앱 시계로 시작한다. 그 경고도 오류 원문 없이 종류만 싣는다(감사 15 L7 — 이 줄을 지키는 테스트가 없었다).
func TestChangeWatcher_DB시계를못읽어앱시계로시작하는경고에는오류원문없이종류만싣는다(t *testing.T) {
	logs := &syncBuffer{}
	w := NewChangeWatcher(&clockBroken{}, NewHub(), time.Hour, slog.New(slog.NewJSONHandler(logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)
	l := logLines(logs, "resync.clock_fallback")
	if len(l) != 1 || l[0]["err_kind"] != "57P01" || l[0]["err"] != nil || l[0]["level"] != "WARN" {
		t.Fatalf("시계 경고는 err_kind=57P01 만 실은 WARN 한 줄이어야 한다: %v", l)
	}
	for _, leak := range []string{"user=", "database=", "chat_leak", "127.0.0.1"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("로그에 접속 정보 %q 가 실렸다:\n%s", leak, logs.String())
		}
	}
}
