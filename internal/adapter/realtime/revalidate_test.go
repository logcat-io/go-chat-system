package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// DEF-02(실사용 v0.0.2 F31-22·B329): 열린 연결의 세션 재확인 시각에 DB 가 잠깐 끊기자 "세션이 만료됐다. 다시 로그인해라" 를 보내고
// 1000 으로 닫았다. 로그인 개념이 없는 위젯 고객도 이 안내를 받았고, 서버 로그에는 INFO ws.closed 만 남았다.
// 재현: 서버의 저장소를 DB 앞의 TCP 중계기 뒤에 둔다. 중계기를 끊으면 세션 조회가 진짜 DB 연결 오류로 실패한다.
// 훅은 httpapi 의 것과 같다 — Subject 의 오류를 그대로 돌려준다.
func TestSession_세션재확인이DB오류로실패하면연결을끊지않고프레임도보내지않으며WARN을남긴다_DB가돌아오면다음확인에서정상이면그대로이어지고세션이없으면그때끊는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	direct := app.New(shared)
	token, err := direct.Commands.IssueCustomerSession(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}

	relay := newCutRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	svc := app.New(store)
	logs := &syncBuffer{}
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))
	srv := NewServer(svc, NewHub(), 10*time.Millisecond, log)
	srv.connTick, srv.sessionRecheck = 50*time.Millisecond, 0 // 틱마다 다시 확인한다

	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handle(w, r, actor, func(ctx context.Context) error {
			_, err := svc.Queries.Subject(ctx, token)
			return err
		})
	}))
	t.Cleanup(hs.Close)

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	type frame struct{ Type, Code, Message, Body string }
	frames := make(chan frame, 64)
	closed := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				closed <- err
				return
			}
			var f frame
			_ = json.Unmarshal(data, &f)
			frames <- f
		}
	}()
	if f := <-frames; f.Type != frameReady {
		t.Fatalf("첫 프레임이 %+v 다", f)
	}
	// 이미 있는 줄(있다면)을 받아 둔다
	for drained := false; !drained; {
		select {
		case <-frames:
		case <-time.After(300 * time.Millisecond):
			drained = true
		}
	}
	quiet := func(d time.Duration, why string) {
		t.Helper()
		select {
		case f := <-frames:
			t.Fatalf("%s: 프레임 %+v 를 받았다", why, f)
		case err := <-closed:
			t.Fatalf("%s: 연결이 닫혔다(close %d): %v", why, websocket.CloseStatus(err), err)
		case <-time.After(d):
		}
	}
	revalidateWarns := func() []map[string]any {
		var out []map[string]any
		for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var l map[string]any
			if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "ws.revalidate_failed" {
				out = append(out, l)
			}
		}
		return out
	}

	// 1) DB 가 끊긴 동안 열 번 넘게 다시 확인해도 끊지 않고 아무 프레임도 보내지 않는다. WARN 은 연속 실패의 첫 번만이다
	relay.cut()
	quiet(700*time.Millisecond, "DB 가 끊긴 동안")
	warns := revalidateWarns()
	if len(warns) != 1 {
		t.Fatalf("ws.revalidate_failed 가 %d 줄이다. 연속 실패는 첫 번 한 줄이어야 한다:\n%s", len(warns), logs.String())
	}
	w := warns[0]
	if w["level"] != "WARN" || w["reason"] != "error" || w["conn_id"] == nil || w["actor_id"] != custID {
		t.Errorf("WARN 의 내용이 틀렸다(수준·종류·연결·주체 id): %v", w)
	}
	// 로그에는 id 와 종류만 싣는다(D30). 오류 문구에는 접속 문자열(사용자·DB 이름·주소)이 들어간다.
	for _, key := range []string{"err", "error", "token"} {
		if _, ok := w[key]; ok {
			t.Errorf("ws.revalidate_failed 에 %q 가 실렸다: %v", key, w)
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Error("로그에 토큰이 실렸다")
	}

	// 2) DB 가 돌아오면 다음 확인에서 정상이고 연결은 그대로 이어진다 — 그 사이 쓴 글이 이 연결로 온다
	relay.restore()
	if _, err := shared.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
		SenderCustomer: custID, Visibility: core.Public, Body: "DB 가 돌아온 뒤 쓴 글",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.Type != frameMessage || f.Body != "DB 가 돌아온 뒤 쓴 글" {
			t.Fatalf("DB 가 돌아온 뒤 첫 프레임이 %+v 다. 메시지여야 한다", f)
		}
	case err := <-closed:
		t.Fatalf("DB 가 돌아온 뒤 연결이 닫혔다(close %d): %v", websocket.CloseStatus(err), err)
	case <-time.After(5 * time.Second):
		t.Fatal("DB 가 돌아온 뒤 메시지가 오지 않는다")
	}
	quiet(300*time.Millisecond, "DB 가 돌아온 뒤 세션이 살아 있는데")

	// 3) 다시 끊기면 새 장애다. 한 줄 더 남는다(확인에 성공하면 연속이 끝난다)
	relay.cut()
	quiet(400*time.Millisecond, "두 번째로 DB 가 끊긴 동안")
	relay.restore()
	quiet(300*time.Millisecond, "두 번째 장애 뒤")
	if n := len(revalidateWarns()); n != 2 {
		t.Errorf("두 번째 장애 뒤 ws.revalidate_failed 가 %d 줄이다. 2 줄이어야 한다", n)
	}

	// 4) DB 가 돌아온 뒤 세션이 없으면 그때 끊는다 — unauthorized 와 1000
	if err := direct.Commands.EndSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	var last frame
	for {
		select {
		case f := <-frames:
			last = f
			continue
		case err := <-closed:
			if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
				t.Fatalf("세션이 없을 때 close 코드가 %d 다(%v). 1000 이어야 한다", code, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("세션이 없어졌는데 연결을 끊지 않는다")
		}
		break
	}
	if last.Type != frameError || last.Code != "unauthorized" {
		t.Errorf("닫기 전 마지막 프레임이 %+v 다. unauthorized 여야 한다", last)
	}
}

// 세션을 확인하지 못한 틱은 확인 시각을 갱신하지 않는다 — DB 가 돌아오면 한 주기(60초)를 더 기다리지 않고 다음 틱에 바로 다시 묻는다.
// 연결별 틱이 짧은 위 테스트로는 이 차이가 안 보여서 판정 함수를 직접 부른다.
func TestSession_세션을확인하지못하면확인시각을그대로둬다음틱에다시묻고WARN에는오류문구없이종류만싣는다(t *testing.T) {
	logs := &syncBuffer{}
	srv := &Server{log: slog.New(slog.NewJSONHandler(logs, nil))}
	c := &conn{log: srv.log}
	d := &delivery{lastAuth: time.Now().Add(-time.Hour)}
	before := d.lastAuth
	dbErr := errors.New("failed to connect: connection refused")
	for range 3 {
		if !srv.recheckSession(context.Background(), c, d, func(context.Context) error { return dbErr }) {
			t.Fatal("확인 못 함인데 연결을 끊는다")
		}
	}
	if !d.lastAuth.Equal(before) {
		t.Error("확인 못 함인데 마지막 확인 시각을 갱신했다 — DB 가 돌아와도 한 주기를 더 기다린다")
	}
	if n := strings.Count(logs.String(), `"msg":"ws.revalidate_failed"`); n != 1 {
		t.Errorf("연속 실패 세 번에 WARN 이 %d 줄이다", n)
	}
	if strings.Contains(logs.String(), "connection refused") {
		t.Errorf("오류 문구가 로그에 실렸다: %s", logs.String())
	}
	timeout := fmt.Errorf("조회: %w", context.DeadlineExceeded)
	d2 := &delivery{}
	srv.recheckSession(context.Background(), c, d2, func(context.Context) error { return timeout })
	if !strings.Contains(logs.String(), `"reason":"timeout"`) {
		t.Errorf("시한 초과의 종류는 timeout 이다: %s", logs.String())
	}
	if !srv.recheckSession(context.Background(), c, d, func(context.Context) error { return nil }) || d.lastAuth.Equal(before) {
		t.Error("살아 있으면 이어 가고 확인 시각을 갱신한다")
	}
}

// cutRelay 는 테스트 DB 앞의 TCP 중계기다. cut 하면 열린 연결을 모두 끊고 새 연결도 받자마자 끊는다 — DB 가 잠깐 사라진 것과 같다.
type cutRelay struct {
	url   string
	down  atomic.Bool
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (p *cutRelay) cut() {
	p.down.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *cutRelay) restore() { p.down.Store(false) }

func (p *cutRelay) track(cs ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down.Load() {
		return false
	}
	for _, c := range cs {
		p.conns[c] = struct{}{}
	}
	return true
}

func newCutRelay(t *testing.T) *cutRelay {
	t.Helper()
	target, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutRelay{conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close(); p.cut() })
	upstream := target.Host
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			if p.down.Load() {
				client.Close()
				continue
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			if !p.track(client, server) {
				client.Close()
				server.Close()
				continue
			}
			go func() { _, _ = io.Copy(client, server); client.Close() }()
			go func() { _, _ = io.Copy(server, client); server.Close() }()
		}
	}()
	target.Host = ln.Addr().String()
	p.url = target.String()
	return p
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 감사 20: 운영은 연결 틱과 재확인 주기가 같다(60초). 틱이 확인 시각보다 몇 ms 일찍 깨면 "아직 60초가 안 됐다" 로 그 틱을 건너뛰어
// 실제 재확인 간격이 120초가 됐다(exp 70초 회원 세션의 연결이 180초에야 닫혔다). 문서가 말하는 "최대 60초" 와 맞아야 한다.
// 같은 비율을 줄여서 본다 — 틱 = 재확인 = 200ms 면 2초 동안 약 10번 묻고, 두 확인 사이는 한 주기(±틱의 절반)를 넘지 않는다.
func TestSession_열린연결의세션재확인은주기마다돈다_틱과주기가같아도한주기를건너뛰지않는다(t *testing.T) {
	store := testsupport.Store(t)
	custID, conv := testsupport.Conversation(t, store)
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const period = 200 * time.Millisecond
	srv.connTick, srv.sessionRecheck = period, period
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var asked []time.Time
	ws, _, _ := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID,
		func(context.Context) error {
			time.Sleep(5 * time.Millisecond) // DB 왕복만큼 늦게 답한다
			mu.Lock()
			asked = append(asked, time.Now())
			mu.Unlock()
			return nil
		})
	defer ws.CloseNow()
	start := time.Now()
	time.Sleep(10*period + period/2)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 9 {
		t.Fatalf("%s 동안 세션을 %d번 물었다 — 주기(%s)마다 묻지 않고 틱을 건너뛴다", time.Since(start).Round(time.Millisecond), len(asked), period)
	}
	for i := 1; i < len(asked); i++ {
		if gap := asked[i].Sub(asked[i-1]); gap > period+period/2 {
			t.Errorf("%d번째와 %d번째 확인 사이가 %s — 한 주기(%s)를 건너뛰었다", i, i+1, gap.Round(time.Millisecond), period)
		}
	}
}

// 감사 21 G3: 확인 시각을 답을 받은 때로 찍어도 위 테스트가 통과했다(DB 왕복 5ms 는 틱의 절반 여유에 묻힌다).
// DB 가 틱의 절반보다 늦게 답하면 그 늦음만큼 다음 확인이 밀려 한 주기를 건너뛴다. 확인 시각은 묻기 시작한 때다.
func TestSession_세션재확인의답이늦어도확인시각은묻기시작한때라다음확인이한주기를건너뛰지않는다(t *testing.T) {
	store := testsupport.Store(t)
	custID, conv := testsupport.Conversation(t, store)
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const period = 400 * time.Millisecond
	const slow = 300 * time.Millisecond // 틱의 절반(200ms)보다 늦은 답
	srv.connTick, srv.sessionRecheck = period, period
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var mu sync.Mutex
	var asked []time.Time
	ws, _, _ := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID,
		func(context.Context) error {
			mu.Lock()
			asked = append(asked, time.Now())
			mu.Unlock()
			time.Sleep(slow)
			return nil
		})
	defer ws.CloseNow()
	time.Sleep(10*period + period/2)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 8 {
		t.Fatalf("%s 동안 세션을 %d번 물었다 — 답이 늦은 만큼 다음 확인이 밀렸다", 10*period+period/2, len(asked))
	}
	for i := 1; i < len(asked); i++ {
		if gap := asked[i].Sub(asked[i-1]); gap > period+period/2 {
			t.Errorf("%d번째와 %d번째 확인 사이가 %s — 한 주기(%s)를 건너뛰었다", i, i+1, gap.Round(time.Millisecond), period)
		}
	}
}
