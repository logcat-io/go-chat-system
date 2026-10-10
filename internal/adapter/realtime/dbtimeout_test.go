package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// DEF-71(감사 13): 장애 조치 뒤 옛 DB 연결의 패킷이 거절되지 않고 사라지면, 그 연결을 쥔 WebSocket 의 전달이 TCP 재전송이 포기할 때까지 멈췄다.
// 상담원 답은 저장됐는데 위젯에는 45초 넘게 오지 않았고 위젯은 "연결됨" 이었다. 고루틴은 pgxpool.Acquire → Ping 에서 멈춰 있었다.
// 재현: 서버의 저장소를 DB 앞의 중계기 뒤에 두고, 열려 있던 연결만 데이터를 버리게 한다(새 연결은 정상 — 장애 조치 뒤와 같다).
// 읽기 시한 5초와 저장 시한 10초는 정책이라 리터럴로 고정한다.

const (
	policyReadTimeout  = 5 * time.Second
	policyWriteTimeout = 10 * time.Second
)

func TestDBTimeout_DB연결이조용히죽어도전달조회는5초시한을넘기면버리고다시조회해상담원답이시한과한틱안에고객에게온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)

	relay := newHoleRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	t.Cleanup(relay.closeAll) // 저장소를 닫기 전에 죽은 연결을 끊는다. 안 그러면 닫기가 그 연결의 종료 인사를 기다린다
	logs := &syncBuffer{}
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	const tick = 500 * time.Millisecond
	srv.connTick = tick

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()
	time.Sleep(3 * tick) // 연결별 안전망이 몇 번 돌아 풀의 연결이 데워진다

	relay.holeOld()
	holed := time.Now()
	if _, err := shared.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
		SenderAgentID: agent.ID, Visibility: core.Public, Body: "연결이 죽은 동안 쓴 상담원 답",
	}); err != nil {
		t.Fatal(err)
	}
	// 시한(5초) + 한 틱 + 풀에 남은 죽은 연결(최대 3개)을 꺼낼 때 Ping 시한(1초)으로 걸러 내는 시간 + 새 연결을 맺는 여유 1초
	limit := policyReadTimeout + tick + 3*time.Second + time.Second
	select {
	case f := <-frames:
		if f.Type != frameMessage || f.Body != "연결이 죽은 동안 쓴 상담원 답" {
			t.Fatalf("첫 프레임이 %+v 다. 상담원 답이어야 한다", f)
		}
		if took := time.Since(holed); took > limit {
			t.Errorf("상담원 답이 %v 만에 왔다. %v 안이어야 한다", took, limit)
		}
	case err := <-closed:
		t.Fatalf("연결이 닫혔다(close %d): %v", websocket.CloseStatus(err), err)
	case <-time.After(3 * limit):
		t.Fatalf("상담원 답이 %v 안에 오지 않는다 — 전달 조회가 죽은 DB 연결을 시한 없이 기다린다", 3*limit)
	}

	timeouts := logLines(logs, "ws.db_timeout")
	if len(timeouts) != 1 || timeouts[0]["step"] != "deliver" || timeouts[0]["level"] != "WARN" || timeouts[0]["conn_id"] == nil {
		t.Fatalf("ws.db_timeout 은 step=deliver 인 WARN 한 줄이어야 한다: %v", timeouts)
	}
	// 시한은 5초다 — 그보다 이르면 느린 DB 를 끊고, 늦으면 전달이 그만큼 묶인다.
	at, _ := time.Parse(time.RFC3339Nano, timeouts[0]["time"].(string))
	if d := at.Sub(holed); d < policyReadTimeout-300*time.Millisecond || d > policyReadTimeout+tick+300*time.Millisecond {
		t.Errorf("전달 조회가 %v 만에 포기했다. 시한은 %v 다", d, policyReadTimeout)
	}
	failed := logLines(logs, "ws.deliver_failed")
	if len(failed) != 1 || failed[0]["reason"] != "timeout" || failed[0]["conversation_id"] != conv.ID {
		t.Errorf("ws.deliver_failed 는 reason=timeout 한 줄이어야 한다: %v", failed)
	}
	for _, l := range append(timeouts, failed...) {
		for _, key := range []string{"err", "error"} {
			if _, ok := l[key]; ok {
				t.Errorf("%s 에 오류 문구(%q)가 실렸다(D30): %v", l["msg"], key, l)
			}
		}
	}
}

func TestDBTimeout_저장이10초시한을넘기면일시오류프레임을받고같은client_msg_id로다시보내면한번만저장된다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)

	relay := newHoleRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	t.Cleanup(relay.closeAll)
	logs := &syncBuffer{}
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	srv.connTick = time.Hour // 저장만 본다. 연결별 안전망은 돌지 않는다

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	relay.holeOld()
	cid := id.New()
	send, _ := json.Marshal(map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": cid, "body": "죽은 연결로 저장한 글"})
	sent := time.Now()
	if err := ws.Write(ctx, websocket.MessageText, send); err != nil {
		t.Fatal(err)
	}
	var got testFrame
	select {
	case got = <-frames:
	case err := <-closed:
		t.Fatalf("연결이 닫혔다(close %d): %v", websocket.CloseStatus(err), err)
	case <-time.After(3 * policyWriteTimeout):
		t.Fatal("저장이 시한 없이 기다린다")
	}
	took := time.Since(sent)
	if got.Type != frameError || got.Code != "internal" || got.ClientMsgID != cid {
		t.Fatalf("시한을 넘긴 저장의 답이 %+v 다. 그 글의 일시 오류(internal)여야 한다 — 클라이언트가 큐에 남겨 다시 보낸다", got)
	}
	if took < policyWriteTimeout-300*time.Millisecond || took > policyWriteTimeout+2*time.Second {
		t.Errorf("저장이 %v 만에 포기했다. 시한은 %v 다", took, policyWriteTimeout)
	}

	// 클라이언트가 같은 client_msg_id 로 다시 보낸다. 시한을 넘긴 연결은 버려졌고 새 연결로 저장된다
	if err := ws.Write(ctx, websocket.MessageText, send); err != nil {
		t.Fatal(err)
	}
	for acked := false; !acked; {
		select {
		case f := <-frames:
			if f.Type == frameError {
				t.Fatalf("다시 보낸 글이 또 실패했다: %+v", f)
			}
			acked = f.Type == frameAck && f.ClientMsgID == cid
		case err := <-closed:
			t.Fatalf("연결이 닫혔다: %v", err)
		case <-time.After(policyWriteTimeout + 2*time.Second):
			t.Fatal("다시 보낸 글의 ack 가 오지 않는다")
		}
	}
	var n int
	if err := shared.Pool().QueryRow(ctx, "SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = $2", conv.ID, cid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("그 글이 %d 번 저장됐다. 한 번이어야 한다", n)
	}
	timeouts := logLines(logs, "ws.db_timeout")
	if len(timeouts) != 1 || timeouts[0]["step"] != "send" {
		t.Errorf("ws.db_timeout 은 step=send 한 줄이어야 한다: %v", timeouts)
	}
	// 시한 초과는 서버 잘못이 아니다. ERROR 로 남기지 않고 오류 문구도 싣지 않는다(D30)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("시한 초과를 ERROR 로 남겼다:\n%s", logs.String())
	}
}

// 감사 14: 저장이 실제로 커밋됐는데 COMMIT 의 응답만 사라지면, 서버는 저장 시한(10초) 뒤 그 글의 일시 오류를 보낸다. 글은 이미 DB 에 있다.
// 클라이언트가 같은 client_msg_id 로 다시 보내면 새 행을 만들지 않고 처음 저장한 번호로 ack 를 받아야 한다(I3). 위 테스트는 커밋 전에 막힌 경우다.
func TestDBTimeout_커밋은됐는데응답만사라진저장도일시오류프레임을받고같은client_msg_id로다시보내면처음번호로한번만저장된다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)

	relay := newCommitRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	t.Cleanup(relay.closeAll)
	logs := &syncBuffer{}
	logger := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))
	// 저장 결과(message.appended 의 created)는 app 이 기본 로거로 남긴다
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, logger)
	srv.connTick = time.Hour

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	cid := id.New()
	send, _ := json.Marshal(map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": cid, "body": "커밋 뒤 응답이 사라진 글"})
	relay.armed.Store(true)
	if err := ws.Write(ctx, websocket.MessageText, send); err != nil {
		t.Fatal(err)
	}
	var got testFrame
	select {
	case got = <-frames:
	case err := <-closed:
		t.Fatalf("연결이 닫혔다: %v", err)
	case <-time.After(3 * policyWriteTimeout):
		t.Fatal("저장이 시한 없이 기다린다")
	}
	if relay.hit.Load() != 1 {
		t.Fatalf("COMMIT 을 %d번 가로챘다. 한 번이어야 재현이다", relay.hit.Load())
	}
	if got.Type != frameError || got.Code != "internal" || got.ClientMsgID != cid {
		t.Fatalf("응답이 사라진 저장의 답이 %+v 다. 그 글의 일시 오류(internal)여야 한다", got)
	}
	var rows int
	var seq int64
	if err := shared.Pool().QueryRow(ctx, "SELECT count(*), coalesce(max(seq), 0) FROM messages WHERE conversation_id = $1 AND client_msg_id = $2", conv.ID, cid).Scan(&rows, &seq); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("일시 오류를 받은 순간 행이 %d 개다. 커밋은 됐어야 한다(재현 조건)", rows)
	}

	if err := ws.Write(ctx, websocket.MessageText, send); err != nil {
		t.Fatal(err)
	}
	var ack ackFrame
	for done := false; !done; {
		select {
		case f := <-frames:
			if f.Type == frameError {
				t.Fatalf("다시 보낸 글이 또 실패했다: %+v", f)
			}
			if f.Type == frameAck && f.ClientMsgID == cid {
				ack, done = ackFrame{f.Seq}, true
			}
		case err := <-closed:
			t.Fatalf("연결이 닫혔다: %v", err)
		case <-time.After(policyWriteTimeout + 2*time.Second):
			t.Fatal("다시 보낸 글의 ack 가 오지 않는다")
		}
	}
	if ack.seq != seq {
		t.Errorf("다시 보낸 글의 ack 번호가 %d 다. 처음 저장한 번호 %d 여야 한다", ack.seq, seq)
	}
	if err := shared.Pool().QueryRow(ctx, "SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_msg_id = $2", conv.ID, cid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("그 글이 %d 번 저장됐다. 한 번이어야 한다", rows)
	}
	var appended []map[string]any
	for _, l := range logLines(logs, "message.appended") {
		if l["client_msg_id"] == cid {
			appended = append(appended, l)
		}
	}
	if len(appended) != 1 || appended[0]["created"] != false {
		t.Errorf("다시 보낸 저장은 created=false 한 줄이어야 한다(새 행을 만들지 않았다): %v", appended)
	}
}

// 구독(권한 확인·읽음 지점)도 같은 시한이다. 넘기면 일시 오류 프레임을 받고, 클라이언트가 다시 구독하면 새 연결로 ready 가 온다(DEF-72 와 짝).
func TestDBTimeout_구독이5초시한을넘기면일시오류프레임을받고다시구독하면ready가온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)

	relay := newHoleRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	t.Cleanup(relay.closeAll)
	logs := &syncBuffer{}
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	srv.connTick = time.Hour

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()
	relay.holeOld()
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	sent := time.Now()
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		took := time.Since(sent)
		if f.Type != frameError || f.Code != "internal" || f.ClientMsgID != "" {
			t.Fatalf("시한을 넘긴 구독의 답이 %+v 다. 글과 무관한 일시 오류(internal)여야 한다", f)
		}
		if took < policyReadTimeout-300*time.Millisecond || took > policyReadTimeout+2*time.Second {
			t.Errorf("구독이 %v 만에 포기했다. 시한은 %v 다", took, policyReadTimeout)
		}
	case err := <-closed:
		t.Fatalf("연결이 닫혔다: %v", err)
	case <-time.After(3 * policyReadTimeout):
		t.Fatal("구독이 시한 없이 기다린다")
	}
	if l := logLines(logs, "ws.db_timeout"); len(l) != 1 || l[0]["step"] != "authorize" {
		t.Errorf("ws.db_timeout 은 step=authorize 한 줄이어야 한다: %v", l)
	}
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.Type != frameReady {
			t.Fatalf("다시 구독한 답이 %+v 다. ready 여야 한다", f)
		}
	case err := <-closed:
		t.Fatalf("연결이 닫혔다: %v", err)
	case <-time.After(policyReadTimeout + 5*time.Second):
		t.Fatal("다시 구독해도 ready 가 오지 않는다")
	}
}

// 세션 재확인도 5초 시한이다. 넘기면 "확인 못 함" 이다 — 연결을 이어 가고 reason=timeout 으로 남긴다(DEF-02 규칙).
func TestSession_세션재확인이5초시한을넘기면확인못함으로보고연결을이어가며WARN의종류는timeout이다(t *testing.T) {
	logs := &syncBuffer{}
	srv := &Server{log: slog.New(slog.NewJSONHandler(logs, nil))}
	c := &conn{log: srv.log}
	d := &delivery{lastAuth: time.Now().Add(-time.Hour)}
	before := d.lastAuth
	start := time.Now()
	// 죽은 DB 연결처럼 시한까지 답하지 않는다
	keep := make(chan bool, 1)
	go func() {
		keep <- srv.recheckSession(context.Background(), c, d, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case ok := <-keep:
		if !ok {
			t.Fatal("확인 못 함인데 연결을 끊는다")
		}
	case <-time.After(3 * policyReadTimeout):
		t.Fatal("세션 재확인이 시한 없이 기다린다")
	}
	if took := time.Since(start); took < policyReadTimeout-100*time.Millisecond || took > policyReadTimeout+time.Second {
		t.Errorf("재확인이 %v 만에 포기했다. 시한은 %v 다", took, policyReadTimeout)
	}
	if !d.lastAuth.Equal(before) {
		t.Error("확인 못 함인데 확인 시각을 갱신했다")
	}
	if l := logLines(logs, "ws.revalidate_failed"); len(l) != 1 || l[0]["reason"] != "timeout" {
		t.Errorf("ws.revalidate_failed 는 reason=timeout 한 줄이어야 한다: %v", l)
	}
	if l := logLines(logs, "ws.db_timeout"); len(l) != 1 || l[0]["step"] != "revalidate" {
		t.Errorf("ws.db_timeout 은 step=revalidate 한 줄이어야 한다: %v", l)
	}
}

// 장애 조치 동안 같은 연결의 같은 단계가 이어서(또는 동시에) 시한을 넘기면 ws.db_timeout 은 첫 번만이다. 그 단계가 한 번 성공하면 다시 센다.
// 연결 수 × 프레임만큼 쌓이면 정작 봐야 할 줄이 묻힌다.
func TestDBTimeout_같은연결같은단계의연속시한초과는WARN을첫번만남기고성공하면다시센다(t *testing.T) {
	logs := &syncBuffer{}
	c := &conn{log: slog.New(slog.NewJSONHandler(logs, nil))}
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Go(func() { errs <- c.db(context.Background(), stepMarkRead, hang) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, errDBTimeout) {
			t.Errorf("시한을 넘긴 호출의 답이 %v 다. 시한 초과여야 한다", err)
		}
	}
	if n := len(logLines(logs, "ws.db_timeout")); n != 1 {
		t.Fatalf("세 번 넘겼는데 ws.db_timeout 이 %d 줄이다", n)
	}
	// 다른 단계는 따로 센다
	if err := c.db(context.Background(), stepTyping, hang); !errors.Is(err, errDBTimeout) {
		t.Fatal(err)
	}
	if n := len(logLines(logs, "ws.db_timeout")); n != 2 {
		t.Fatalf("다른 단계의 시한 초과가 남지 않았다(%d 줄)", n)
	}
	if err := c.db(context.Background(), stepMarkRead, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// 다른 단계의 성공은 이 단계를 다시 세지 않는다. 입력 중은 아직 이어서 넘기는 중이다(감사 14 G9)
	if err := c.db(context.Background(), stepTyping, hang); !errors.Is(err, errDBTimeout) {
		t.Fatal(err)
	}
	if n := len(logLines(logs, "ws.db_timeout")); n != 2 {
		t.Fatalf("읽음 표시가 성공했다고 입력 중의 연속 시한 초과를 새로 남겼다(%d 줄)", n)
	}
	if err := c.db(context.Background(), stepMarkRead, hang); !errors.Is(err, errDBTimeout) {
		t.Fatal(err)
	}
	if n := len(logLines(logs, "ws.db_timeout")); n != 3 {
		t.Errorf("성공한 뒤 다시 넘겼는데 새 줄이 없다(%d 줄)", n)
	}
	// 연결이 끝나서 실패한 것은 시한 초과가 아니다
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.db(ctx, stepSend, hang); errors.Is(err, errDBTimeout) {
		t.Error("연결이 닫히는 중의 실패를 시한 초과로 봤다")
	}
	if n := len(logLines(logs, "ws.db_timeout")); n != 3 {
		t.Errorf("연결이 닫히는 중의 실패를 ws.db_timeout 으로 남겼다(%d 줄)", n)
	}
}

// DEF-74: 전달용 조회가 실패하면 오류를 그대로 실었다. pgx 오류 문구에는 접속 정보(user·database)가 들어가고, 연결마다 깨울 때마다 쌓였다.
func TestDelivery_전달용조회가실패하면WARN에오류문구없이종류만싣고같은연결의연속실패는첫번만남긴다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)

	relay := newCutRelay(t)
	store, err := pgstore.OpenWithPool(ctx, relay.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	logs := &syncBuffer{}
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	srv.connTick = 50 * time.Millisecond

	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	relay.cut()
	time.Sleep(700 * time.Millisecond) // 틱마다 열 번 넘게 실패한다
	failed := logLines(logs, "ws.deliver_failed")
	if len(failed) != 1 {
		t.Fatalf("ws.deliver_failed 가 %d 줄이다. 연속 실패는 첫 번 한 줄이어야 한다:\n%s", len(failed), logs.String())
	}
	if l := failed[0]; l["level"] != "WARN" || l["reason"] != "error" || l["conversation_id"] != conv.ID || l["conn_id"] == nil {
		t.Errorf("내용이 틀렸다(수준·종류·대화·연결): %v", l)
	}
	for _, key := range []string{"err", "error"} {
		if _, ok := failed[0][key]; ok {
			t.Errorf("오류 문구가 실렸다: %v", failed[0])
		}
	}
	for _, leak := range []string{"user=", "database=", "127.0.0.1", "dial", "failed to"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("로그에 접속 정보 %q 가 실렸다:\n%s", leak, logs.String())
		}
	}

	// 돌아오면 다시 전달되고, 다시 끊기면 새 장애라 한 줄 더 남는다
	relay.restore()
	if _, err := shared.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
		SenderCustomer: custID, Visibility: core.Public, Body: "돌아온 뒤 쓴 글",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.Type != frameMessage {
			t.Fatalf("돌아온 뒤 첫 프레임이 %+v 다", f)
		}
	case err := <-closed:
		t.Fatalf("연결이 닫혔다: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("돌아온 뒤 전달되지 않는다")
	}
	relay.cut()
	time.Sleep(400 * time.Millisecond)
	relay.restore()
	if n := len(logLines(logs, "ws.deliver_failed")); n != 2 {
		t.Errorf("두 번째 장애 뒤 ws.deliver_failed 가 %d 줄이다. 2 줄이어야 한다", n)
	}
}

// DEF-73(감사 13 변이 N8·N9): 만료와 비활성 조건을 지우면 열린 연결이 계속 받는데도 테스트가 통과했다.
// 훅은 httpapi 의 것과 같다 — Subject 의 오류를 그대로 돌려준다. 세션 행은 지우지 않고 만료 시각·active 만 바꾼다.
func TestSession_만료된세션과비활성상담원의세션은재확인에서unauthorized와1000으로끊긴다(t *testing.T) {
	shared := testsupport.Store(t)
	svc := app.New(shared)
	custID, conv := testsupport.Conversation(t, shared)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cases := []struct {
		name  string
		actor func(t *testing.T) (app.Actor, string)
		end   func(t *testing.T, a app.Actor, token string)
	}{
		{"만료된 고객 세션", func(t *testing.T) (app.Actor, string) {
			token, err := svc.Commands.IssueCustomerSession(ctx, custID)
			if err != nil {
				t.Fatal(err)
			}
			return app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, token
		}, func(t *testing.T, a app.Actor, _ string) {
			testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE customer_id = $1", a.CustomerID)
		}},
		{"만료된 상담원 세션", agentSession(ctx, svc, shared), func(t *testing.T, a app.Actor, _ string) {
			testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE agent_id = $1", a.AgentID)
		}},
		{"비활성 상담원의 세션", agentSession(ctx, svc, shared), func(t *testing.T, a app.Actor, _ string) {
			testsupport.Exec(t, testsupport.URL(), "UPDATE agents SET active = false WHERE id = $1", a.AgentID)
			t.Cleanup(func() {
				testsupport.Exec(t, testsupport.URL(), "UPDATE agents SET active = true WHERE id = $1", a.AgentID)
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor, token := tc.actor(t)
			srv := NewServer(svc, NewHub(), 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
			srv.connTick, srv.sessionRecheck = 50*time.Millisecond, 0
			ws, frames, closed := dialSubscribed(t, ctx, srv, actor, conv.ID, func(ctx context.Context) error {
				_, err := svc.Queries.Subject(ctx, token)
				return err
			})
			defer ws.CloseNow()
			// 살아 있는 동안은 끊지 않는다
			select {
			case err := <-closed:
				t.Fatalf("살아 있는 세션인데 닫혔다: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			tc.end(t, actor, token)
			var last testFrame
			for {
				select {
				case f := <-frames:
					last = f
					continue
				case err := <-closed:
					if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
						t.Fatalf("close 코드가 %d 다(%v). 1000 이어야 한다", code, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("세션이 끝났는데 연결을 끊지 않는다")
				}
				break
			}
			if last.Type != frameError || last.Code != "unauthorized" {
				t.Errorf("닫기 전 마지막 프레임이 %+v 다. unauthorized 여야 한다", last)
			}
		})
	}
}

// 감사 13 변이 N2: 서버가 내려가며 1001 로 닫는 중인데 재확인이 세션 없음을 받으면 unauthorized 와 1000 이 1001 과 경합한다.
// 1000 을 받은 클라이언트는 백오프를 타고 "다시 로그인해라" 를 본다. 종료 중이면 닫기는 GoAway 의 1001 에 맡긴다.
// GoAway 가 표식을 켜고 닫기 프레임을 보내기 전의 순간을 만든다(표식만 켠다). 그 뒤 닫힘은 GoAway 몫이라 여기서는 1000 이 아닌 것만 본다.
func TestSession_종료중에는세션이없어도unauthorized와1000을내지않는다(t *testing.T) {
	store := testsupport.Store(t)
	custID, conv := testsupport.Conversation(t, store)
	srv := NewServer(app.New(store), NewHub(), 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.connTick, srv.sessionRecheck = 50*time.Millisecond, 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var gone atomic.Bool
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID,
		func(context.Context) error {
			if gone.Load() {
				return app.ErrNotFound
			}
			return nil
		})
	defer ws.CloseNow()
	// 종료가 시작됐다(GoAway 가 표식을 켠 순간). 닫기 프레임은 아직 안 나갔다
	srv.mu.Lock()
	for c := range srv.conns {
		c.goingAway.Store(true)
	}
	srv.mu.Unlock()
	gone.Store(true)
	for {
		select {
		case f := <-frames:
			if f.Type == frameError {
				t.Fatalf("종료 중인데 %+v 를 보냈다", f)
			}
			continue
		case err := <-closed:
			if code := websocket.CloseStatus(err); code == websocket.StatusNormalClosure {
				t.Fatalf("종료 중인데 1000 으로 닫았다(%v)", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("연결이 닫히지 않는다")
		}
		break
	}
}

// 감사 13 변이 N1: 연결이 닫히는 중(ctx 취소)에 재확인이 실패하면 그건 DB 장애가 아니다. 확인 못 함으로 보면 닫힐 때마다 WARN 이 쌓이고
// 끝나야 할 전달 루프가 이어진다.
func TestSession_연결을닫는중ctx가취소되면확인실패로보지않아WARN을남기지않고전달루프를끝낸다(t *testing.T) {
	logs := &syncBuffer{}
	srv := &Server{log: slog.New(slog.NewJSONHandler(logs, nil))}
	c := &conn{log: srv.log}
	d := &delivery{lastAuth: time.Now().Add(-time.Hour)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if srv.recheckSession(ctx, c, d, func(ctx context.Context) error { return ctx.Err() }) {
		t.Error("연결이 끝나는 중인데 이어 간다")
	}
	if logs.String() != "" {
		t.Errorf("연결이 끝나는 중의 실패를 로그에 남겼다: %s", logs.String())
	}
}

// ── 도구 ──────────────────────────────────────────────────

type testFrame struct {
	Type, Code, Message, Body string
	ClientMsgID               string `json:"client_msg_id"`
	TraceID                   string `json:"trace_id"`
	Seq                       int64
}

type ackFrame struct{ seq int64 }

// dialSubscribed 는 srv 에 붙어 conv 를 구독하고 ready 를 받은 뒤, 이어 오는 프레임과 닫힘을 채널로 준다.
func dialSubscribed(t *testing.T, ctx context.Context, srv *Server, actor app.Actor, convID string, revalidate func(context.Context) error) (*websocket.Conn, <-chan testFrame, <-chan error) {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handle(w, r, actor, revalidate)
	}))
	t.Cleanup(hs.Close)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "conversation_id": convID, "after_seq": 0})
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}
	frames := make(chan testFrame, 64)
	closed := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				closed <- err
				return
			}
			var f testFrame
			_ = json.Unmarshal(data, &f)
			frames <- f
		}
	}()
	select {
	case f := <-frames:
		if f.Type != frameReady {
			t.Fatalf("첫 프레임이 %+v 다", f)
		}
	case err := <-closed:
		t.Fatalf("구독 전에 닫혔다: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("ready 가 오지 않는다")
	}
	// 이미 있는 줄(있다면)을 받아 둔다
	for {
		select {
		case <-frames:
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	return ws, frames, closed
}

// agentSession 은 새 상담원과 그 세션을 만드는 함수를 준다.
func agentSession(ctx context.Context, svc *app.App, store *pgstore.Store) func(t *testing.T) (app.Actor, string) {
	return func(t *testing.T) (app.Actor, string) {
		a := testsupport.Agent(t, store)
		token, err := svc.Commands.IssueAgentSession(ctx, a.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return app.Actor{Kind: app.ActorAgent, AgentID: a.ID, WorkspaceID: a.WorkspaceID, Role: a.Role}, token
	}
}

func logLines(logs *syncBuffer, msg string) []map[string]any {
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// holeRelay 는 테스트 DB 앞의 TCP 중계기다. holeOld 하면 그때 열려 있던 연결은 끊지 않은 채 오가는 데이터를 버린다 — 장애 조치 뒤 옛 연결의
// 패킷이 거절되지 않고 사라지는 것과 같다. 그 뒤에 맺는 연결은 정상이다. 정리할 때 버리던 연결을 닫는다.
type holeRelay struct {
	url   string
	mu    sync.Mutex
	pairs []*holePair
}

type holePair struct {
	dead       atomic.Bool
	client, db net.Conn
}

func (p *holeRelay) holeOld() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.pairs {
		pr.dead.Store(true)
	}
}

func (p *holeRelay) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pr := range p.pairs {
		pr.client.Close()
		pr.db.Close()
	}
}

func newHoleRelay(t *testing.T) *holeRelay {
	t.Helper()
	target, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &holeRelay{}
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	upstream := target.Host
	pipe := func(pr *holePair, dst, src net.Conn) {
		defer dst.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 && !pr.dead.Load() {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			db, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			pr := &holePair{client: client, db: db}
			p.mu.Lock()
			p.pairs = append(p.pairs, pr)
			p.mu.Unlock()
			go pipe(pr, db, client)
			go pipe(pr, client, db)
		}
	}()
	target.Host = ln.Addr().String()
	p.url = target.String()
	return p
}

// commitRelay 는 armed 인 동안 처음 지나가는 COMMIT 을 DB 에 보내고, 그 연결의 응답부터 버린다 — 커밋은 됐는데 답이 사라진 경우다.
type commitRelay struct {
	url   string
	armed atomic.Bool
	hit   atomic.Int32
	mu    sync.Mutex
	conns []net.Conn
}

func (p *commitRelay) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
}

func newCommitRelay(t *testing.T) *commitRelay {
	t.Helper()
	target, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &commitRelay{}
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	upstream := target.Host
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			db, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, db)
			p.mu.Unlock()
			var mute atomic.Bool
			go func() { // 클라이언트 → DB
				defer db.Close()
				buf := make([]byte, 32<<10)
				for {
					n, err := client.Read(buf)
					if n > 0 {
						if p.armed.Load() && bytes.Contains(bytes.ToLower(buf[:n]), []byte("commit")) && p.armed.CompareAndSwap(true, false) {
							p.hit.Add(1)
							mute.Store(true) // 커밋은 DB 로 간다. 이 연결의 응답부터 버린다
						}
						if _, werr := db.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			go func() { // DB → 클라이언트
				defer client.Close()
				buf := make([]byte, 32<<10)
				for {
					n, err := db.Read(buf)
					if n > 0 && !mute.Load() {
						if _, werr := client.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	target.Host = ln.Addr().String()
	p.url = target.String()
	return p
}
