package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ---- 배포·축소로 태스크가 내려간다 (D32) ----
// 아래 테스트는 run 이 쓰는 serve 로 띄우고, ctx 를 끊는 것으로 종료 신호(SIGTERM)를 흉내 낸다.

// served 는 serve 로 띄운 서버다. stop 이 종료 신호다.
type served struct {
	*assembled
	addr string
	stop context.CancelFunc
	// done 은 serve 가 돌아오면 닫힌다. 그때 err 에 serve 의 결과가 있다.
	done chan struct{}
	err  error
}

func serveFor(t *testing.T, drain time.Duration) *served {
	t.Helper()
	s := assembleFor(t, "local")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := s.cfg
	cfg.ShutdownDrain = drain
	out := &served{assembled: s, addr: ln.Addr().String(), stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(out.done)
		out.err = serve(ctx, ln, s.store, s.a, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-out.done:
		case <-time.After(30 * time.Second):
			t.Error("serve 가 30초 안에 끝나지 않았다")
		}
	})
	return out
}

// wsFrame 은 테스트가 읽는 서버 프레임의 필요한 부분이다.
type wsFrame struct {
	Type        string `json:"type"`
	Seq         int64  `json:"seq"`
	ID          string `json:"id"`
	ClientMsgID string `json:"client_msg_id"`
	Code        string `json:"code"`
}

// dialSubscribed 는 WebSocket 으로 붙어 대화를 구독하고 ready 를 받을 때까지 읽는다.
func dialSubscribed(t *testing.T, addr, token, convID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws?token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	writeFrame(t, ws, map[string]any{"type": "subscribe", "conversation_id": convID, "after_seq": 0})
	for {
		f, err := readFrame(ctx, ws)
		if err != nil {
			t.Fatalf("ready 를 받기 전에 끊겼다: %v", err)
		}
		if f.Type == "ready" {
			return ws
		}
	}
}

func writeFrame(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func readFrame(ctx context.Context, ws *websocket.Conn) (wsFrame, error) {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return wsFrame{}, err
	}
	var f wsFrame
	_ = json.Unmarshal(data, &f)
	return f, nil
}

// readUntilClosed 는 연결이 끝날 때까지 프레임을 모으고 끝난 이유를 돌려준다.
func readUntilClosed(ws *websocket.Conn, within time.Duration) ([]wsFrame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var frames []wsFrame
	for {
		f, err := readFrame(ctx, ws)
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
	}
}

func readyz(t *testing.T, addr string) int {
	t.Helper()
	res, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestShutdown_readyz(t *testing.T) {
	t.Run("종료 신호를 받으면 readyz 가 곧바로(500ms 안에) 503 이다", func(t *testing.T) {
		// 대기를 2초로 둬서 그동안 서버가 요청을 받는 채로 남게 한다.
		s := serveFor(t, 2*time.Second)
		if code := readyz(t, s.addr); code != http.StatusOK {
			t.Fatalf("종료 전 readyz 가 %d 다", code)
		}

		signaled := time.Now()
		s.stop()

		for {
			code := readyz(t, s.addr)
			if code == http.StatusServiceUnavailable {
				break
			}
			if time.Since(signaled) > 500*time.Millisecond {
				t.Fatalf("종료 신호 뒤 500ms 가 지나도 readyz 가 %d 다", code)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if elapsed := time.Since(signaled); elapsed > 500*time.Millisecond {
			t.Errorf("readyz 가 503 이 되기까지 %v 걸렸다. 500ms 안이어야 한다", elapsed)
		}
	})
}

func TestShutdown_종료할때열린WebSocket은1001로닫힌다(t *testing.T) {
	s := serveFor(t, 0)
	token, convID := s.customer()
	ws := dialSubscribed(t, s.addr, token, convID)

	s.stop()

	_, err := readUntilClosed(ws, 5*time.Second)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close 코드가 %d 다(%v). 1001 이어야 한다", code, err)
	}
}

func TestShutdown_종료중에도이미시작한메시지쓰기는커밋되고ack가1001보다먼저가며재전송하면같은메시지로처리된다(t *testing.T) {
	s := serveFor(t, 0)
	token, convID := s.customer()
	ws := dialSubscribed(t, s.addr, token, convID)
	ctx := context.Background()

	// 대화 행을 잠가 저장을 번호 발급에서 멈춰 둔다. 종료 신호가 저장 도중에 오게 하려는 것이다.
	tx, err := s.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM conversations WHERE id = $1 FOR UPDATE", convID); err != nil {
		t.Fatal(err)
	}
	const clientMsgID = "shutdown-inflight-1"
	writeFrame(t, ws, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": clientMsgID, "body": "내려가는 중에 보낸 문의"})
	waitForLockWaiter(t, s.assembled)

	s.stop()
	select {
	case <-s.done:
		t.Fatalf("저장이 끝나기 전에 종료가 끝났다: %v", s.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	frames, err := readUntilClosed(ws, 10*time.Second)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close 코드가 %d 다(%v). 1001 이어야 한다", code, err)
	}
	var ack wsFrame
	for _, f := range frames {
		if f.Type == "ack" && f.ClientMsgID == clientMsgID {
			ack = f
		}
	}
	if ack.Seq == 0 || ack.ID == "" {
		t.Fatalf("1001 전에 ack 가 오지 않았다: %+v", frames)
	}
	if n := s.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = $2", convID, clientMsgID); n != 1 {
		t.Fatalf("메시지가 %d행이다. 커밋돼서 1행이어야 한다", n)
	}

	// 다른 태스크로 다시 붙어 같은 메시지를 다시 보낸다. ack 를 못 받았다고 여긴 클라이언트가 하는 일이다.
	other, err := assemble(ctx, s.cfg, s.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	next := httptest.NewServer(other.handler)
	t.Cleanup(next.Close)
	ws2 := dialSubscribed(t, strings.TrimPrefix(next.URL, "http://"), token, convID)
	writeFrame(t, ws2, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": clientMsgID, "body": "내려가는 중에 보낸 문의"})
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		f, err := readFrame(readCtx, ws2)
		if err != nil {
			t.Fatalf("재전송의 ack 를 받지 못했다: %v", err)
		}
		if f.Type == "error" {
			t.Fatalf("재전송이 거절됐다: %+v", f)
		}
		if f.Type == "ack" && f.ClientMsgID == clientMsgID {
			if f.Seq != ack.Seq || f.ID != ack.ID {
				t.Errorf("재전송이 다른 메시지가 됐다: 처음 %+v, 다시 %+v", ack, f)
			}
			break
		}
	}
	if n := s.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = $2", convID, clientMsgID); n != 1 {
		t.Errorf("재전송 뒤 메시지가 %d행이다. 1행이어야 한다", n)
	}
}

// waitForLockWaiter 는 이 테스트 DB 에서 잠금을 기다리는 쿼리가 생길 때까지 기다린다.
func waitForLockWaiter(t *testing.T, s *assembled) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.count("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'") > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("저장이 대화 행 잠금에서 기다리지 않는다")
}

// 닫기 단계가 지나간 뒤에 받은 연결은 아무도 닫지 않아 프로세스와 함께 1006 으로 끊긴다. 받지 않고 돌려보낸다.
func TestShutdown_WebSocket을닫기시작한뒤에붙는연결은업그레이드하지않고503이다(t *testing.T) {
	s := assembleFor(t, "local")
	token, _ := s.customer()
	if closed, left := s.a.rt.GoAway(context.Background()); closed != 0 || left != 0 {
		t.Fatalf("열린 연결이 없는데 닫은 수 %d, 남은 수 %d 다", closed, left)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.srv.URL, "http")+"/ws?token="+token, nil)
	if err == nil {
		_ = ws.CloseNow()
		t.Fatal("닫기 시작한 서버가 WebSocket 을 받았다")
	}
	if res == nil || res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("응답이 503 이 아니다: %v %v", res, err)
	}
}

// waitNotReady 는 종료 신호 뒤 readyz 가 503 이 될 때까지 기다린다. 그 뒤로는 종료 순서 안에 있다.
func waitNotReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for readyz(t, addr) != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("종료 신호 뒤 2초가 지나도 readyz 가 503 이 아니다")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 알림 리스너·안전망은 종료 신호가 아니라 종료 순서의 끝에서 멈춘다. 신호와 함께 멈추면 기다리는 동안(drain)과 닫는 동안
// 이 서버에 붙은 고객은 다른 서버로 쓴 메시지를 받지 못한다 — 다시 붙어 이어 받기 전까지 실시간이 끊긴다.
func TestShutdown_한서버가내려가며기다리는동안다른서버로쓴메시지도그서버의WebSocket에1001전에전달된다(t *testing.T) {
	s := serveFor(t, 3*time.Second)
	token, convID := s.customer()
	ws := dialSubscribed(t, s.addr, token, convID)

	// 같은 DB 를 쓰는 다른 태스크.
	other, err := assemble(context.Background(), s.cfg, s.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	next := httptest.NewServer(other.handler)
	t.Cleanup(next.Close)

	s.stop()
	waitNotReady(t, s.addr)
	const clientMsgID = "drain-other-server-1"
	if code, _, raw := s.call(http.MethodPost, next.URL+"/api/conversations/"+convID+"/messages", token,
		map[string]any{"client_msg_id": clientMsgID, "body": "다른 서버로 보낸 문의"}); code != http.StatusCreated {
		t.Fatalf("다른 서버의 저장이 %d 다: %s", code, raw)
	}

	frames, err := readUntilClosed(ws, 10*time.Second)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close 코드가 %d 다(%v). 1001 이어야 한다", code, err)
	}
	for _, f := range frames {
		if f.Type == "message" && f.ClientMsgID == clientMsgID {
			return
		}
	}
	t.Fatalf("내려가는 서버의 WebSocket 에 다른 서버로 쓴 메시지가 1001 전에 오지 않았다: %+v", frames)
}

// 3단계(WebSocket 을 닫는 동안)에도 HTTP 는 열려 있다. HTTP 정리(4단계)가 먼저 오면 그 사이의 REST 저장이 연결 거절로 끝난다.
func TestShutdown_WebSocket을닫는단계까지REST로보낸메시지는저장된다(t *testing.T) {
	s := serveFor(t, 0)
	token, convID := s.customer()
	ws := dialSubscribed(t, s.addr, token, convID)
	restToken, restConv := s.customer()
	ctx := context.Background()

	// WebSocket 저장을 번호 발급에서 멈춰 둔다. 그동안 닫는 단계(GoAway)가 그 연결을 기다린다.
	tx, err := s.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM conversations WHERE id = $1 FOR UPDATE", convID); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, ws, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": "closing-ws-1", "body": "닫히는 연결"})
	waitForLockWaiter(t, s.assembled)

	s.stop()
	waitNotReady(t, s.addr)
	time.Sleep(200 * time.Millisecond) // 닫는 단계에 들어가 저장을 기다리는 중이다

	code, _, raw := s.call(http.MethodPost, "http://"+s.addr+"/api/conversations/"+restConv+"/messages", restToken,
		map[string]any{"client_msg_id": "closing-rest-1", "body": "닫는 동안 REST 로 보낸 문의"})
	if code != http.StatusCreated {
		t.Fatalf("닫는 동안의 REST 저장이 %d 다: %s", code, raw)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := readUntilClosed(ws, 10*time.Second); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close 코드가 1001 이 아니다: %v", err)
	}
	if n := s.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = 'closing-rest-1'", restConv); n != 1 {
		t.Errorf("REST 로 보낸 메시지가 %d행이다. 1행이어야 한다", n)
	}
}

// 닫기 시작한 뒤에 읽은 프레임은 처리하지 않는다. 저장하면 ack 를 못 보낸 채 닫히고, 클라이언트는 ack 가 없으니 다시 보낸다 —
// 결과는 같지만(멱등) 내려가는 서버에 새 일을 얹지 않는다.
func TestShutdown_WebSocket을닫기시작한뒤에읽은프레임은처리하지않는다(t *testing.T) {
	s := serveFor(t, 0)
	token, convID := s.customer()
	ws := dialSubscribed(t, s.addr, token, convID)
	ctx := context.Background()

	tx, err := s.store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1 FROM conversations WHERE id = $1 FOR UPDATE", convID); err != nil {
		t.Fatal(err)
	}
	writeFrame(t, ws, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": "inflight-1", "body": "처리 중인 프레임"})
	waitForLockWaiter(t, s.assembled)
	// 처리 중인 프레임 뒤에 쌓인 프레임. 읽기 루프가 앞 프레임을 끝낸 뒤에 읽는다.
	writeFrame(t, ws, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": "after-closing-1", "body": "닫는 동안 온 프레임"})

	s.stop()
	waitNotReady(t, s.addr)
	time.Sleep(200 * time.Millisecond)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	frames, err := readUntilClosed(ws, 10*time.Second)
	if code := websocket.CloseStatus(err); code != websocket.StatusGoingAway {
		t.Fatalf("close 코드가 %d 다(%v). 1001 이어야 한다", code, err)
	}
	for _, f := range frames {
		if f.ClientMsgID == "after-closing-1" {
			t.Errorf("닫기 시작한 뒤의 프레임에 답이 왔다: %+v", f)
		}
	}
	if n := s.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = 'inflight-1'", convID); n != 1 {
		t.Errorf("처리 중이던 메시지가 %d행이다. 커밋돼 1행이어야 한다", n)
	}
	// 저장은 비동기로 끝날 수 있다. 서버가 다 내려간 뒤에 센다.
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve 가 끝나지 않았다")
	}
	if n := s.count("SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = 'after-closing-1'", convID); n != 0 {
		t.Errorf("닫기 시작한 뒤에 읽은 프레임의 메시지가 %d행 저장됐다. 처리하지 않아야 한다", n)
	}
}

// closeLog 는 ws.closed 를 셀 때마다 잠깐 붙잡는 로그 핸들러다. 연결의 끝 정리가 늦는 상황을 만든다.
type closeLog struct {
	delay  time.Duration
	mu     sync.Mutex
	closed int
}

func (h *closeLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *closeLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *closeLog) WithGroup(string) slog.Handler            { return h }
func (h *closeLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "ws.closed" {
		time.Sleep(h.delay)
		h.mu.Lock()
		h.closed++
		h.mu.Unlock()
	}
	return nil
}

func (h *closeLog) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// GoAway 가 돌아오면 다음은 HTTP 정리와 프로세스 종료다. 연결의 끝 정리(닫힘 로그·연결 수)가 그 뒤로 밀리면 사라진다.
func TestShutdown_WebSocket을닫는단계는연결마다닫힘로그를남기고연결수에서빠진뒤에끝난다(t *testing.T) {
	logs := &closeLog{delay: 300 * time.Millisecond}
	s := assembleWith(t, "local", slog.New(logs), nil)
	addr := strings.TrimPrefix(s.srv.URL, "http://")
	const n = 3
	for range n {
		token, convID := s.customer()
		ws := dialSubscribed(t, addr, token, convID)
		// 클라이언트는 읽고 있어야 close 에 답한다.
		go func() { _, _ = readUntilClosed(ws, 10*time.Second) }()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closed, left := s.a.rt.GoAway(ctx)

	if closed != n || left != 0 {
		t.Fatalf("닫은 수 %d, 남은 수 %d 다. %d, 0 이어야 한다", closed, left, n)
	}
	if got := logs.count(); got != n {
		t.Errorf("GoAway 가 돌아왔을 때 닫힘 로그가 %d 줄이다. %d 줄이어야 한다", got, n)
	}
	if got := s.a.rt.Connections(); got != 0 {
		t.Errorf("GoAway 가 돌아왔을 때 연결 수가 %d 다. 0 이어야 한다", got)
	}
}
