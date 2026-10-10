package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

func panicLines(buf *bytes.Buffer) []map[string]any {
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "panic.recovered" {
			out = append(out, l)
		}
	}
	return out
}

// D47: 끝나지 않아야 하는 백그라운드 루프(알림 리스너·안전망)가 패닉으로 끝나면 프로세스를 죽이지 않고 panic.recovered 를 남긴 뒤 다시 띄운다.
// 다시 띄우지 않으면 그 서버의 실시간 전달이 안전망 없이 멈춘다.
func TestSupervise_백그라운드루프의패닉은남기고다시띄운다(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, log, "listener", func(ctx context.Context) {
			if runs.Add(1) == 1 {
				panic("리스너 패닉 원문")
			}
			<-ctx.Done()
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if runs.Load() < 2 {
		t.Fatalf("패닉 뒤 루프를 다시 띄우지 않았다(실행 %d 번)", runs.Load())
	}
	l := panicLines(&buf)
	if len(l) != 1 || l[0]["where"] != "listener" || l[0]["panic_type"] != "string" {
		t.Fatalf("panic.recovered 는 where=listener panic_type=string 한 줄이어야 한다: %v", l)
	}
	if strings.Contains(buf.String(), "리스너 패닉 원문") {
		t.Errorf("panic 값의 원문이 실렸다:\n%s", buf.String())
	}
}

// 주기 작업 한 번의 패닉은 그 한 번만 끝낸다. 다음 주기는 그대로 돈다.
func TestGuarded_주기작업한번의패닉은그한번만끝내고다음주기는돈다(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	var runs atomic.Int32
	job := guarded(periodicJob{name: "보류 해제", key: "wake_snoozed", run: func(context.Context) {
		if runs.Add(1) == 1 {
			panic(42)
		}
	}}, log)
	job(context.Background()) // 패닉이 밖으로 새면 테스트 프로세스가 끝난다
	job(context.Background())
	if runs.Load() != 2 {
		t.Fatalf("두 번째 주기가 돌지 않았다: %d", runs.Load())
	}
	if l := panicLines(&buf); len(l) != 1 || l[0]["where"] != "job.wake_snoozed" || l[0]["panic_type"] != "int" {
		t.Fatalf("panic.recovered 는 where=job.wake_snoozed panic_type=int 한 줄이어야 한다: %v", l)
	}
}

// D47(감사 23 N1·N2·N14): supervise·guarded 함수만 따로 보면 startBackground 가 그것을 지나지 않고 리스너·주기 작업을 바로 띄워도
// 테스트가 통과한다. 운영 조립(assemble)으로 만든 것을 startBackground 로 띄우고 리스너·안전망·주기 작업 안에서 패닉을 일으킨다.
// 배선이 빠지면 패닉이 테스트 프로세스를 끝낸다. 리스너는 1초 뒤 다시 붙고(listener.connected 두 번), 안전망은 다시 돌고, 주기 작업은 다음 주기에 돈다.
func TestStartBackground_조립한리스너와안전망과주기작업의패닉은프로세스를살리고다시돈다(t *testing.T) {
	s := assembleFor(t, "local")
	var buf syncBuf
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	var faults sync.Map // where → *atomic.Int32
	hook := func(where string) {
		n, _ := faults.LoadOrStore(where, new(atomic.Int32))
		if n.(*atomic.Int32).Add(1) == 1 {
			panic("배선 시험 패닉 " + where)
		}
	}
	faultHook.Store(&hook)
	t.Cleanup(func() { faultHook.Store(nil) })
	var jobRuns atomic.Int32
	a := s.a
	a.jobs = []periodicJob{{name: "시험", key: "test", period: 20 * time.Millisecond, run: func(context.Context) {
		if jobRuns.Add(1) == 1 {
			panic(7)
		}
	}}}
	count := func(where string) int32 {
		if n, ok := faults.Load(where); ok {
			return n.(*atomic.Int32).Load()
		}
		return 0
	}
	stop := startBackground(s.store, a, s.cfg, log)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (count("listener") < 2 || count("watcher") < 2 || jobRuns.Load() < 2) {
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if count("listener") < 2 || count("watcher") < 2 || jobRuns.Load() < 2 {
		t.Fatalf("패닉 뒤 다시 돌지 않았다: listener %d, watcher %d, job %d", count("listener"), count("watcher"), jobRuns.Load())
	}
	wheres := map[any]int{}
	for _, l := range linesOf(buf.String(), "panic.recovered") {
		wheres[l["where"]]++
	}
	if wheres["listener"] != 1 || wheres["watcher"] != 1 || wheres["job.test"] != 1 {
		t.Errorf("panic.recovered 는 listener·watcher·job.test 하나씩이어야 한다: %v", wheres)
	}
	if n := len(linesOf(buf.String(), "listener.connected")); n < 2 {
		t.Errorf("리스너가 다시 붙지 않았다(listener.connected %d 번)", n)
	}
	if strings.Contains(buf.String(), "배선 시험 패닉") {
		t.Errorf("panic 값의 원문이 실렸다")
	}
}

// supervise 는 패닉 뒤 1초를 쉬고 다시 띄운다. 같은 패닉이 되풀이돼도 로그가 초당 한 줄을 넘지 않는다(감사 23 N14 — 간격 0 이 살아남았다).
func TestSupervise_패닉뒤다시띄우기전에1초를쉰다(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var starts []time.Time
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, log, "watcher", func(context.Context) {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			panic("늘 패닉")
		})
	}()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("1.5초 동안 %d 번 띄웠다. 1초 간격이면 두 번이어야 한다", len(starts))
	}
	if gap := starts[1].Sub(starts[0]); gap < time.Second {
		t.Errorf("다시 띄우기까지 %v 를 쉬었다. 1초여야 한다", gap)
	}
}

// syncBuf 는 여러 고루틴이 쓰는 로그 버퍼다.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func linesOf(out, msg string) []map[string]any {
	var res []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == msg {
			res = append(res, l)
		}
	}
	return res
}
