package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 감사 14: WebSocket 경로의 DB 호출마다 시한(DEF-71)을 거는 c.db 를 단계 하나씩 걷어 내도(G1~G5) 테스트가 통과했다.
// 중계기 E2E 는 전달(느린 길)·저장·권한 확인·세션 재확인만 지났다. 여기서는 저장소 대역이 한 단계의 DB 호출만 멈추게 해서
// 그 단계에 시한이 걸려 있음을 관찰 결과(일시 오류 프레임·다음 시도·WARN 의 단계 이름)로 본다.
// 대역이 멈추는 모양은 죽은 DB 연결과 같다 — 호출 ctx 가 끝날 때까지 답하지 않는다. 시한이 없으면 연결이 끝날 때까지 묶인다.

// stallStore 는 실제 저장소 앞의 대역이다. 이름을 붙인 호출만 멈추거나(hang) 곧바로 실패하게(fail) 한다.
type stallStore struct {
	*pgstore.Store
	mu    sync.Mutex
	modes map[string]stallMode
	// metaCalls 는 대화 행 조회(느린 길) 횟수다. 빠른 길로 전달했는지 가른다.
	metaCalls atomic.Int32
}

type stallMode int

const (
	stallOff stallMode = iota
	stallHang
	stallFail
)

// leakErr 는 DB 가 낸 오류의 대역이다. 원문에 접속 정보가 들어 있고 SQLSTATE 를 가진다(pgx 의 PgError 와 같은 모양).
type leakErr struct{}

func (leakErr) Error() string {
	return "failed to connect to `user=chat database=chat_leak`: 127.0.0.1:56432 (127.0.0.1): server error: FATAL: terminating connection due to administrator command"
}
func (leakErr) SQLState() string { return "57P01" }

func newStallStore(shared *pgstore.Store) *stallStore {
	return &stallStore{Store: shared, modes: map[string]stallMode{}}
}

func (s *stallStore) set(call string, m stallMode) {
	s.mu.Lock()
	s.modes[call] = m
	s.mu.Unlock()
}

func (s *stallStore) gate(ctx context.Context, call string) error {
	s.mu.Lock()
	m := s.modes[call]
	s.mu.Unlock()
	switch m {
	case stallOff:
	case stallHang:
		<-ctx.Done()
		return ctx.Err()
	case stallFail:
		return leakErr{}
	}
	return nil
}

func (s *stallStore) ReadStates(ctx context.Context, convID string) ([]core.ReadState, error) {
	if err := s.gate(ctx, "ReadStates"); err != nil {
		return nil, err
	}
	return s.Store.ReadStates(ctx, convID)
}

func (s *stallStore) MarkRead(ctx context.Context, convID string, ev core.LiveEvent, seq int64, internal bool) (int64, error) {
	if err := s.gate(ctx, "MarkRead"); err != nil {
		return 0, err
	}
	return s.Store.MarkRead(ctx, convID, ev, seq, internal)
}

func (s *stallStore) PublishLive(ctx context.Context, ev core.LiveEvent) error {
	if err := s.gate(ctx, "PublishLive"); err != nil {
		return err
	}
	return s.Store.PublishLive(ctx, ev)
}

func (s *stallStore) ListMessages(ctx context.Context, convID string, afterSeq int64, limit int, internal bool) ([]core.Message, error) {
	if err := s.gate(ctx, "ListMessages"); err != nil {
		return nil, err
	}
	return s.Store.ListMessages(ctx, convID, afterSeq, limit, internal)
}

func (s *stallStore) ConversationTags(ctx context.Context, convID string) ([]core.Tag, error) {
	if err := s.gate(ctx, "ConversationTags"); err != nil {
		return nil, err
	}
	return s.Store.ConversationTags(ctx, convID)
}

func (s *stallStore) ConversationMeta(ctx context.Context, convID string) (core.Conversation, error) {
	s.metaCalls.Add(1)
	return s.Store.ConversationMeta(ctx, convID)
}

// stallServer 는 대역 저장소로 서버를 띄운다. 연결별 안전망은 돌지 않는다 — 다시 묻는 것은 시한과 다시 깨움뿐이다.
func stallServer(shared *pgstore.Store) (*Server, *stallStore, *syncBuffer) {
	st := newStallStore(shared)
	logs := &syncBuffer{}
	srv := NewServer(app.New(st), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	srv.connTick = time.Hour
	return srv, st, logs
}

// awaitFrame 은 keep 이 참인 첫 프레임을 기다린다. 받은함 신호처럼 상관없는 프레임은 건너뛴다.
func awaitFrame(t *testing.T, frames <-chan testFrame, closed <-chan error, within time.Duration, keep func(testFrame) bool, why string) testFrame {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f := <-frames:
			if keep(f) {
				return f
			}
		case err := <-closed:
			t.Fatalf("연결이 닫혔다(close %d): %v", websocket.CloseStatus(err), err)
		case <-deadline:
			t.Fatalf("%v 안에 오지 않는다 — %s", within, why)
		}
	}
}

func writeFrame(t *testing.T, ctx context.Context, ws *websocket.Conn, f map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(f)
	if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

func notInbox(f testFrame) bool { return f.Type != frameInboxChanged }

func TestDBTimeout_구독의읽음지점조회와읽음표시와입력중은5초시한을넘기면일시오류프레임을받고WARN에그단계이름을남긴다(t *testing.T) {
	shared := testsupport.Store(t)
	cases := []struct {
		step, call string
		frame      func(convID string) map[string]any
	}{
		{"read_states", "ReadStates", func(c string) map[string]any {
			return map[string]any{"type": "subscribe", "conversation_id": c, "after_seq": 0}
		}},
		{"mark_read", "MarkRead", func(c string) map[string]any {
			return map[string]any{"type": "read", "conversation_id": c, "seq": 1}
		}},
		{"typing", "PublishLive", func(c string) map[string]any {
			return map[string]any{"type": "typing", "conversation_id": c}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.step, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			custID, conv := testsupport.Conversation(t, shared)
			srv, st, logs := stallServer(shared)
			ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
			defer ws.CloseNow()

			st.set(tc.call, stallHang) // 이 단계의 DB 호출만 죽은 연결처럼 답하지 않는다
			sent := time.Now()
			writeFrame(t, ctx, ws, tc.frame(conv.ID))
			f := awaitFrame(t, frames, closed, 3*policyReadTimeout, notInbox, tc.step+" 단계가 시한 없이 기다린다")
			took := time.Since(sent)
			if f.Type != frameError || f.Code != "internal" || f.ClientMsgID != "" {
				t.Fatalf("시한을 넘긴 %s 의 답이 %+v 다. 글과 무관한 일시 오류(internal)여야 한다 — 클라이언트가 다시 한다", tc.step, f)
			}
			if took < policyReadTimeout-300*time.Millisecond || took > policyReadTimeout+2*time.Second {
				t.Errorf("%s 가 %v 만에 포기했다. 시한은 %v 다", tc.step, took, policyReadTimeout)
			}
			if l := logLines(logs, "ws.db_timeout"); len(l) != 1 || l[0]["step"] != tc.step || l[0]["level"] != "WARN" {
				t.Errorf("ws.db_timeout 은 step=%s 인 WARN 한 줄이어야 한다: %v", tc.step, l)
			}
			if strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Errorf("시한 초과를 ERROR 로 남겼다:\n%s", logs.String())
			}

			// DB 가 돌아오면 다음 시도는 된다. 구독이면 ready 가 온다
			st.set(tc.call, stallOff)
			writeFrame(t, ctx, ws, tc.frame(conv.ID))
			if tc.step == "read_states" {
				awaitFrame(t, frames, closed, 5*time.Second, notInbox, "다시 구독해도 ready 가 오지 않는다")
				return
			}
			select {
			case f := <-frames:
				if f.Type == frameError {
					t.Errorf("DB 가 돌아왔는데 다시 보낸 %s 가 실패했다: %+v", tc.step, f)
				}
			case <-time.After(time.Second):
			}
		})
	}
}

// 알림(NOTIFY)으로 깨운 전달은 대화 행을 다시 읽지 않는 빠른 길을 탄다(Deliver). 그 조회도 죽은 연결을 붙잡으면 5초 시한 뒤 버리고,
// 0.5초 뒤 같은 알림으로 다시 깨워 다시 묻는다. 전에는 다시 깨움에 힌트가 없어 "보낼 것이 없다" 로 끝났고, 다시 묻는 것은 안전망뿐이었다.
func TestDBTimeout_알림으로깨운빠른길전달도5초시한을넘기면버리고같은알림으로다시조회해상담원답이시한과한번의재시도안에온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := stallServer(shared)
	listen(t, ctx, srv)

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	st.set("ListMessages", stallHang)
	metaBefore := st.metaCalls.Load()
	appended := time.Now()
	if _, err := shared.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
		SenderAgentID: agent.ID, Visibility: core.Public, Body: "빠른 길이 죽은 동안 쓴 상담원 답",
	}); err != nil {
		t.Fatal(err)
	}
	// 시한이 지나 첫 조회를 버린 뒤 DB 가 돌아온다
	waitLog(t, logs, "ws.db_timeout", 3*policyReadTimeout)
	timedOut := time.Since(appended)
	st.set("ListMessages", stallOff)
	f := awaitFrame(t, frames, closed, 5*time.Second, notInbox, "시한을 넘긴 빠른 길 전달을 다시 묻지 않는다")
	took := time.Since(appended)
	if f.Type != frameMessage || f.Body != "빠른 길이 죽은 동안 쓴 상담원 답" {
		t.Fatalf("첫 프레임이 %+v 다. 상담원 답이어야 한다", f)
	}
	if timedOut < policyReadTimeout-300*time.Millisecond || timedOut > policyReadTimeout+time.Second {
		t.Errorf("빠른 길 조회가 %v 만에 포기했다. 시한은 %v 다", timedOut, policyReadTimeout)
	}
	// 시한 + 다시 깨움(0.5초) + 여유
	if limit := policyReadTimeout + drainRetryPolicy + time.Second; took > limit {
		t.Errorf("상담원 답이 %v 만에 왔다. %v 안이어야 한다", took, limit)
	}
	if n := st.metaCalls.Load() - metaBefore; n != 0 {
		t.Errorf("대화 행을 %d번 읽었다. 알림이 번호를 들고 왔으니 처음도 다시 묻기도 빠른 길이어야 한다", n)
	}
	if l := logLines(logs, "ws.db_timeout"); len(l) != 1 || l[0]["step"] != "deliver" {
		t.Errorf("ws.db_timeout 은 step=deliver 한 줄이어야 한다: %v", l)
	}
	if l := logLines(logs, "ws.deliver_failed"); len(l) != 1 || l[0]["reason"] != "timeout" {
		t.Errorf("ws.deliver_failed 는 reason=timeout 한 줄이어야 한다: %v", l)
	}
}

// DEF-74 의 "조회가 성공하면 다시 센다" 는 빠른 길에도 걸린다. 빠른 길의 성공이 표식을 풀지 않으면 다음 장애는 로그에 안 남는다(감사 14 G13).
func TestDelivery_빠른길전달이실패하면같은알림으로다시조회하고성공하면실패표식을풀어다음장애의WARN을다시남긴다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := stallServer(shared)
	listen(t, ctx, srv)

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()
	metaBefore := st.metaCalls.Load()

	for round := 1; round <= 2; round++ {
		st.set("ListMessages", stallFail)
		body := "장애 " + string(rune('0'+round)) + " 동안 쓴 상담원 답"
		if _, err := shared.AppendMessage(ctx, core.AppendParams{
			ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
			SenderAgentID: agent.ID, Visibility: core.Public, Body: body,
		}); err != nil {
			t.Fatal(err)
		}
		waitLog(t, logs, "ws.deliver_failed", 5*time.Second)
		time.Sleep(3 * drainRetryPolicy) // 다시 묻기가 몇 번 더 실패한다. 연속 실패라 줄이 늘지 않는다
		if n := len(logLines(logs, "ws.deliver_failed")); n != round {
			t.Fatalf("%d번째 장애 뒤 ws.deliver_failed 가 %d 줄이다. %d 줄이어야 한다(장애마다 첫 번만)", round, n, round)
		}
		st.set("ListMessages", stallOff)
		f := awaitFrame(t, frames, closed, 3*time.Second, notInbox, "실패한 빠른 길 전달을 다시 묻지 않는다")
		if f.Type != frameMessage || f.Body != body {
			t.Fatalf("%d번째 장애 뒤 첫 프레임이 %+v 다", round, f)
		}
	}
	if n := st.metaCalls.Load() - metaBefore; n != 0 {
		t.Errorf("대화 행을 %d번 읽었다. 빠른 길만 탔어야 한다", n)
	}
	for _, l := range logLines(logs, "ws.deliver_failed") {
		if l["reason"] != "error" {
			t.Errorf("reason 이 %v 다. error 여야 한다", l["reason"])
		}
	}
	if s := logs.String(); strings.Contains(s, "user=") || strings.Contains(s, "database=") {
		t.Errorf("로그에 접속 정보가 실렸다(D30):\n%s", s)
	}
}

// 상담원 화면의 대화 프레임은 태그를 읽어 싣는다. 태그 조회가 죽은 연결을 붙잡아도 5초 시한 뒤 tags 필드 없이("모름", DEF-83) 보내고 시한 초과를 남긴다.
func TestDBTimeout_태그조회가5초시한을넘기면대화프레임을tags없이보내고WARN에tags단계를남긴다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := stallServer(shared)

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorAgent, AgentID: agent.ID, WorkspaceID: agent.WorkspaceID, Role: agent.Role}, conv.ID, nil)
	defer ws.CloseNow()

	st.set("ConversationTags", stallHang)
	// 대화 메타가 바뀌었다(updated_at 이 오르면 상태·담당·태그 중 무엇이든 바뀐 것이다)
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET updated_at = clock_timestamp() WHERE id = $1", conv.ID)
	changed := time.Now()
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	f := awaitFrame(t, frames, closed, 3*policyReadTimeout, notInbox, "태그 조회가 시한 없이 기다려 대화 프레임이 나가지 않는다")
	took := time.Since(changed)
	if f.Type != frameConversation {
		t.Fatalf("첫 프레임이 %+v 다. 대화 프레임이어야 한다", f)
	}
	if took < policyReadTimeout-300*time.Millisecond || took > policyReadTimeout+2*time.Second {
		t.Errorf("대화 프레임이 %v 만에 나갔다. 태그 조회 시한은 %v 다", took, policyReadTimeout)
	}
	if l := logLines(logs, "ws.db_timeout"); len(l) != 1 || l[0]["step"] != "tags" || l[0]["level"] != "WARN" {
		t.Errorf("ws.db_timeout 은 step=tags 인 WARN 한 줄이어야 한다: %v", l)
	}
}

// DEF-77: 처리 실패 ERROR 가 오류 원문을 실었다. DB 연결 오류면 user=·database= 가 로그에 남는다. 종류(SQLSTATE)만 싣는다.
func TestConn_WebSocket처리실패ERROR에는오류원문없이종류만싣는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, st, logs := stallServer(shared)
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	st.set("MarkRead", stallFail)
	writeFrame(t, ctx, ws, map[string]any{"type": "read", "conversation_id": conv.ID, "seq": 1})
	if f := awaitFrame(t, frames, closed, 5*time.Second, notInbox, "실패한 읽음의 답"); f.Type != frameError || f.Code != "internal" {
		t.Fatalf("답이 %+v 다. 일시 오류(internal)여야 한다", f)
	}
	l := logLines(logs, "ws.failed")
	if len(l) != 1 || l[0]["level"] != "ERROR" || l[0]["err_kind"] != "57P01" {
		t.Fatalf("ws.failed 는 err_kind=57P01 인 ERROR 한 줄이어야 한다: %v", l)
	}
	if _, ok := l[0]["err"]; ok {
		t.Errorf("오류 원문을 실었다: %v", l[0])
	}
	for _, leak := range []string{"user=", "database=", "chat_leak", "127.0.0.1"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("로그에 접속 정보 %q 가 실렸다:\n%s", leak, logs.String())
		}
	}
}

// ── 도구 ──────────────────────────────────────────────────

// drainRetryPolicy 는 전달 조회가 실패한 뒤 스스로 다시 깨우는 간격이다(정책 리터럴).
const drainRetryPolicy = 500 * time.Millisecond

// listen 은 실제 LISTEN 연결로 srv 를 깨운다. 리스너는 대역을 거치지 않는다 — 알림 연결은 살아 있고 조회 연결만 죽은 경우다.
func listen(t *testing.T, ctx context.Context, srv *Server) {
	t.Helper()
	ln := pgstore.NewListener(testsupport.URL(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ready := make(chan struct{})
	var once sync.Once
	ln.OnChange = srv.OnChange
	ln.OnResync = func() { once.Do(func() { close(ready) }) }
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { ln.Run(lctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("리스너가 붙지 않는다")
	}
}

// waitLog 는 msg 사건이 한 줄 이상 남을 때까지 기다린다.
func waitLog(t *testing.T, logs *syncBuffer, msg string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for len(logLines(logs, msg)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%v 안에 %s 가 남지 않는다:\n%s", within, msg, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
