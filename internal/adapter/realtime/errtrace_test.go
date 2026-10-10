package realtime

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// D47: 오류 프레임은 trace_id 를 싣고, 같은 trace_id 의 로그 한 줄이 남는다. 화면이 보여 준 문의 번호(앞 8자리)로 운영자가 그 줄을 찾는다.
// 전에는 구독 not_found·bad_frame 이 로그를 남기지 않아 번호가 있어도 찾을 곳이 없었다.

var traceHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestConn_오류프레임은trace_id를싣고같은trace_id로거절이나실패의로그한줄이남는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, st, logs := stallServer(shared)
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()
	isError := func(f testFrame) bool { return f.Type == frameError }

	byTrace := func(msg, trace string) map[string]any {
		t.Helper()
		var found []map[string]any
		for _, l := range logLines(logs, msg) {
			if l["trace_id"] == trace {
				found = append(found, l)
			}
		}
		if len(found) != 1 {
			t.Fatalf("trace_id=%s 인 %s 줄이 %d 개다. 한 줄이어야 한다:\n%s", trace, msg, len(found), logs.String())
		}
		return found[0]
	}

	t.Run("없는 대화를 구독하면 not_found 프레임의 trace_id 로 ws.rejected 를 찾는다", func(t *testing.T) {
		writeFrame(t, ctx, ws, map[string]any{"type": "subscribe", "conversation_id": id.New(), "after_seq": 0})
		f := awaitFrame(t, frames, closed, 5*time.Second, isError, "없는 대화 구독의 답")
		if f.Code != "not_found" || !traceHex.MatchString(f.TraceID) {
			t.Fatalf("프레임 %+v — not_found 와 32자리 trace_id 여야 한다", f)
		}
		l := byTrace("ws.rejected", f.TraceID)
		if l["code"] != "not_found" || l["frame"] != "subscribe" || l["level"] != "WARN" {
			t.Errorf("ws.rejected %v — code=not_found frame=subscribe WARN 이어야 한다", l)
		}
	})

	t.Run("모르는 type 의 프레임은 bad_frame 프레임의 trace_id 로 ws.rejected 를 찾는다", func(t *testing.T) {
		writeFrame(t, ctx, ws, map[string]any{"type": "bogus"})
		f := awaitFrame(t, frames, closed, 5*time.Second, isError, "모르는 프레임의 답")
		if f.Code != "bad_frame" || !traceHex.MatchString(f.TraceID) {
			t.Fatalf("프레임 %+v — bad_frame 과 32자리 trace_id 여야 한다", f)
		}
		// 모르는 type 은 클라이언트가 정한 글자라 그대로 싣지 않는다(D30, 감사 23). 길이·내용은 TestConn_클라이언트가정한글자는… 가 본다.
		if l := byTrace("ws.rejected", f.TraceID); l["code"] != "bad_frame" || l["frame"] != "unknown" {
			t.Errorf("ws.rejected %v — code=bad_frame frame=unknown 이어야 한다", l)
		}
	})

	t.Run("서버 잘못(internal)은 프레임의 trace_id 로 ws.failed ERROR 와 err_kind 를 찾는다", func(t *testing.T) {
		st.set("MarkRead", stallFail)
		writeFrame(t, ctx, ws, map[string]any{"type": "read", "conversation_id": conv.ID, "seq": 1})
		f := awaitFrame(t, frames, closed, 5*time.Second, isError, "실패한 읽음의 답")
		if f.Code != "internal" || !traceHex.MatchString(f.TraceID) {
			t.Fatalf("프레임 %+v — internal 과 32자리 trace_id 여야 한다", f)
		}
		if l := byTrace("ws.failed", f.TraceID); l["level"] != "ERROR" || l["err_kind"] != "57P01" || l["frame"] != "read" {
			t.Errorf("ws.failed %v — ERROR, err_kind=57P01, frame=read 여야 한다", l)
		}
	})
}

// D47(감사 23 N13): 구독 중인 대화가 지워져 서버가 구독을 풀 때 보내는 not_found 오류 프레임에도 trace_id 가 있고,
// 같은 trace_id 의 ws.unsubscribed 한 줄이 남는다. 화면은 이 번호를 문의 번호로 보여 준다.
func TestConn_구독이풀리는not_found프레임도trace_id를싣고같은trace_id로ws_unsubscribed가남는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, _, logs := stallServer(shared)
	listen(t, ctx, srv)
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	if err := shared.DeleteConversation(ctx, conv.ID, agent.ID, "지운 상담"); err != nil {
		t.Fatal(err)
	}
	f := awaitFrame(t, frames, closed, 5*time.Second, func(f testFrame) bool { return f.Type == frameError }, "지워진 대화의 구독 풀림")
	if f.Code != "not_found" || !traceHex.MatchString(f.TraceID) {
		t.Fatalf("프레임 %+v — not_found 와 32자리 trace_id 여야 한다", f)
	}
	var found []map[string]any
	for _, l := range logLines(logs, "ws.unsubscribed") {
		if l["trace_id"] == f.TraceID {
			found = append(found, l)
		}
	}
	if len(found) != 1 || found[0]["reason"] != "not_found" || found[0]["conversation_id"] != conv.ID {
		t.Fatalf("trace_id=%s 의 ws.unsubscribed 는 reason=not_found conversation_id 인 한 줄이어야 한다: %v\n%s", f.TraceID, found, logs.String())
	}
}

// D30(감사 23): 프레임의 type 과 client_msg_id 는 클라이언트가 정한 글자다. 전에는 그대로 실어 60KB type 의 프레임 500 개가 로그 30MB 가 됐고
// 이메일을 섞으면 그대로 남았다. 로그에는 아는 종류(모르면 unknown)와 모양이 맞는 client_msg_id(1~128 바이트, 영문·숫자·.·_·:·-)만 싣는다.
// 오류 프레임은 클라이언트가 자기 큐의 글을 찾도록 보낸 client_msg_id 를 그대로 돌려준다 — 로그와 다르다.
func TestConn_클라이언트가정한글자는로그에싣지않는다_모르는type은unknown이고모양이틀린client_msg_id는빠진다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	logs := &syncBuffer{}
	logger := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))
	// message.rejected 는 app 이 기본 로거로 남긴다
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	srv := NewServer(app.New(shared), NewHub(), 10*time.Millisecond, logger)
	srv.connTick = time.Hour
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()
	isError := func(f testFrame) bool { return f.Type == frameError }
	const pii = "hong@example.com"

	// 1) 60KB 의 모르는 type(이메일 섞음)
	writeFrame(t, ctx, ws, map[string]any{"type": pii + "-" + strings.Repeat("x", 60000)})
	bad := awaitFrame(t, frames, closed, 5*time.Second, isError, "모르는 type 의 답")
	// 2) 모양이 틀린 client_msg_id 둘 — 상한(128)을 넘는 20KB(오류 프레임이 되돌려주므로 테스트 클라이언트의 읽기 한도 32KB 아래), 상한 안이지만 이메일.
	// 둘 다 본문이 비어 거절된다
	longID := "CMID-" + pii + "-" + strings.Repeat("Y", 20000)
	writeFrame(t, ctx, ws, map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": longID, "body": ""})
	longF := awaitFrame(t, frames, closed, 5*time.Second, isError, "긴 client_msg_id 의 답")
	writeFrame(t, ctx, ws, map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": pii, "body": ""})
	mailF := awaitFrame(t, frames, closed, 5*time.Second, isError, "이메일 client_msg_id 의 답")
	// 3) 모양이 맞는 client_msg_id 는 싣는다 — 거르기가 다 지우지 않는다
	okID := id.New()
	writeFrame(t, ctx, ws, map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": okID, "body": ""})
	okF := awaitFrame(t, frames, closed, 5*time.Second, isError, "모양이 맞는 client_msg_id 의 답")
	// 4) 글자는 맞고 길이만 경계인 둘(감사 24 G3c) — 상한(128 바이트)은 싣고 한 바이트 넘으면 뺀다
	atMax, overMax := strings.Repeat("b", app.MaxClientMsgID), strings.Repeat("c", app.MaxClientMsgID+1)
	writeFrame(t, ctx, ws, map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": atMax, "body": ""})
	atMaxF := awaitFrame(t, frames, closed, 5*time.Second, isError, "상한 길이 client_msg_id 의 답")
	writeFrame(t, ctx, ws, map[string]any{"type": "send", "conversation_id": conv.ID, "client_msg_id": overMax, "body": ""})
	overMaxF := awaitFrame(t, frames, closed, 5*time.Second, isError, "상한을 한 바이트 넘은 client_msg_id 의 답")

	if mailF.ClientMsgID != pii || okF.ClientMsgID != okID {
		t.Errorf("오류 프레임은 보낸 client_msg_id 를 돌려줘야 한다: %q %q", mailF.ClientMsgID, okF.ClientMsgID)
	}
	byTrace := func(msg, trace string) map[string]any {
		t.Helper()
		for _, l := range logLines(logs, msg) {
			if l["trace_id"] == trace {
				return l
			}
		}
		t.Fatalf("trace_id=%s 인 %s 줄이 없다:\n%.2000s", trace, msg, logs.String())
		return nil
	}
	if l := byTrace("ws.rejected", bad.TraceID); l["frame"] != "unknown" || l["code"] != "bad_frame" {
		t.Errorf("모르는 type 의 ws.rejected %v — frame=unknown code=bad_frame 이어야 한다", l)
	}
	for _, f := range []testFrame{longF, mailF, overMaxF} {
		for _, msg := range []string{"ws.rejected", "message.rejected"} {
			if l := byTrace(msg, f.TraceID); l["client_msg_id"] != nil {
				t.Errorf("%s 에 모양이 틀린 client_msg_id 가 실렸다: %.200v", msg, l["client_msg_id"])
			}
		}
	}
	for _, msg := range []string{"ws.rejected", "message.rejected"} {
		if l := byTrace(msg, okF.TraceID); l["client_msg_id"] != okID || l["frame"] != nil && l["frame"] != "send" {
			t.Errorf("%s %v — 모양이 맞는 client_msg_id=%s 를 실어야 한다", msg, l, okID)
		}
		if l := byTrace(msg, atMaxF.TraceID); l["client_msg_id"] != atMax {
			t.Errorf("%s 에 상한 길이(%d 바이트)의 client_msg_id 가 없다", msg, app.MaxClientMsgID)
		}
	}
	out := logs.String()
	for _, needle := range []string{pii, "xxxxxxxxxx", "YYYYYYYYYY"} {
		if strings.Contains(out, needle) {
			t.Errorf("클라이언트가 정한 글자 %q 가 로그에 실렸다", needle)
		}
	}
	// 줄 하나의 길이 상한. 입력이 60KB 여도 한 줄은 2KB 를 넘지 않는다(이 테스트의 줄에는 스택이 없다)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > 2048 {
			t.Errorf("로그 한 줄이 %d 바이트다. 2048 을 넘으면 안 된다: %.200s…", len(line), line)
		}
	}
}

// D47: 고루틴 하나의 패닉은 recover 가 없으면 프로세스 전체를 끝낸다. 읽기 고루틴의 패닉은 그 연결만 1011 로 닫고
// panic.recovered(스택, panic 값의 타입만)를 남긴다. 같은 서버의 다른 연결은 그대로 프레임을 주고받는다.
func TestConn_읽기고루틴의패닉은그연결만1011로닫고스택과타입만남기며다른연결은산다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, _, logs := stallServer(shared)
	hook := func(f clientFrame) {
		if f.Type == "typing" && f.Body == "boom" {
			panic("비밀값-패닉원문")
		}
	}
	frameHook.Store(&hook)
	t.Cleanup(func() { frameHook.Store(nil) })
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	victim, _, victimClosed := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer victim.CloseNow()
	other, otherFrames, otherClosed := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer other.CloseNow()

	writeFrame(t, ctx, victim, map[string]any{"type": "typing", "conversation_id": conv.ID, "body": "boom"})
	select {
	case err := <-victimClosed:
		if code := websocket.CloseStatus(err); code != websocket.StatusInternalError {
			t.Fatalf("패닉 난 연결이 %d 로 닫혔다(%v). 1011 이어야 한다", code, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("패닉 난 연결이 닫히지 않는다")
	}

	l := logLines(logs, "panic.recovered")
	if len(l) != 1 || l[0]["where"] != "ws.read" || l[0]["panic_type"] != "string" || l[0]["level"] != "ERROR" {
		t.Fatalf("panic.recovered 는 where=ws.read panic_type=string 인 ERROR 한 줄이어야 한다: %v", l)
	}
	if stack, _ := l[0]["stack"].(string); !strings.Contains(stack, "handleFrame") {
		t.Errorf("스택에 패닉 자리(handleFrame)가 없다: %q", stack)
	}
	if strings.Contains(logs.String(), "비밀값-패닉원문") {
		t.Errorf("panic 값의 원문이 로그에 실렸다:\n%s", logs.String())
	}
	if closes := logLines(logs, "ws.closed"); len(closes) == 0 || closes[0]["close_code"] != float64(websocket.StatusInternalError) || closes[0]["closed_by"] != "server" {
		t.Errorf("ws.closed 는 close_code=1011 closed_by=server 여야 한다: %v", closes)
	}

	writeFrame(t, ctx, other, map[string]any{"type": "ping"})
	if f := awaitFrame(t, otherFrames, otherClosed, 5*time.Second, func(f testFrame) bool { return f.Type == framePong }, "다른 연결의 pong"); f.Type != framePong {
		t.Fatalf("다른 연결이 %+v 를 받았다", f)
	}
}

// 전달 고루틴(연결 틱의 세션 재확인 등)의 패닉도 같다. 그 연결만 1011 로 닫는다.
func TestConn_전달고루틴의패닉은그연결만1011로닫는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, _, logs := stallServer(shared)
	srv.connTick, srv.sessionRecheck = 50*time.Millisecond, 0
	var armed = make(chan struct{})
	ws, _, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID,
		func(context.Context) error {
			select {
			case <-armed:
				panic(context.DeadlineExceeded)
			default:
				return nil
			}
		})
	defer ws.CloseNow()
	close(armed)
	select {
	case err := <-closed:
		if code := websocket.CloseStatus(err); code != websocket.StatusInternalError {
			t.Fatalf("%d 로 닫혔다(%v). 1011 이어야 한다", code, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("패닉 난 연결이 닫히지 않는다")
	}
	if l := logLines(logs, "panic.recovered"); len(l) != 1 || l[0]["where"] != "ws.deliver" {
		t.Fatalf("panic.recovered 는 where=ws.deliver 한 줄이어야 한다: %v", l)
	}
}

// armFault 는 where 에서 한 번만 패닉을 일으키는 시험 길을 건다. 어느 연결이 맞을지는 정해지지 않는다(길은 연결을 모른다).
func armFault(t *testing.T, where string) {
	t.Helper()
	var fired atomic.Bool
	hook := func(w string) {
		if w == where && fired.CompareAndSwap(false, true) {
			panic("비밀값-패닉원문")
		}
	}
	faultHook.Store(&hook)
	t.Cleanup(func() { faultHook.Store(nil) })
}

// awaitClose 는 두 연결 중 먼저 닫히는 쪽의 번호와 닫기 코드다.
func awaitClose(t *testing.T, a, b <-chan error, within time.Duration) (idx int, code websocket.StatusCode) {
	t.Helper()
	select {
	case err := <-a:
		return 0, websocket.CloseStatus(err)
	case err := <-b:
		return 1, websocket.CloseStatus(err)
	case <-time.After(within):
		t.Fatal("패닉 난 연결이 닫히지 않는다")
	}
	return -1, -1
}

func recoveredOnce(t *testing.T, logs *syncBuffer, where string) {
	t.Helper()
	l := logLines(logs, "panic.recovered")
	if len(l) != 1 || l[0]["where"] != where || l[0]["panic_type"] != "string" || l[0]["level"] != "ERROR" {
		t.Fatalf("panic.recovered 는 where=%s panic_type=string 인 ERROR 한 줄이어야 한다: %v", where, l)
	}
	if strings.Contains(logs.String(), "비밀값-패닉원문") {
		t.Errorf("panic 값의 원문이 로그에 실렸다:\n%s", logs.String())
	}
}

func pongs(t *testing.T, ctx context.Context, ws *websocket.Conn, frames <-chan testFrame, closed <-chan error) {
	t.Helper()
	writeFrame(t, ctx, ws, map[string]any{"type": "ping"})
	awaitFrame(t, frames, closed, 5*time.Second, func(f testFrame) bool { return f.Type == framePong }, "살아남은 연결의 pong")
}

// ping 고루틴의 패닉도 같다. 그 연결만 1011 로 닫고 다른 연결은 산다.
func TestConn_ping고루틴의패닉은그연결만1011로닫고다른연결은산다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, _, logs := stallServer(shared)
	srv.pingEvery = 50 * time.Millisecond
	armFault(t, "ws.ping")
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	w0, f0, c0 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w0.CloseNow()
	w1, f1, c1 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w1.CloseNow()

	idx, code := awaitClose(t, c0, c1, 5*time.Second)
	if code != websocket.StatusInternalError {
		t.Fatalf("패닉 난 연결이 %d 로 닫혔다. 1011 이어야 한다", code)
	}
	recoveredOnce(t, logs, "ws.ping")
	if idx == 0 {
		pongs(t, ctx, w1, f1, c1)
	} else {
		pongs(t, ctx, w0, f0, c0)
	}
}

// GoAway 의 닫기 고루틴은 연결 밖에서 돈다. 거기서 난 패닉은 프로세스를 끝내지 않고, 그 연결만 1011 로 닫으며 나머지는 1001 로 닫힌다.
func TestServer_GoAway닫기고루틴의패닉은프로세스를살리고그연결만1011로닫는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	srv, _, logs := stallServer(shared)
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	w0, _, c0 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w0.CloseNow()
	w1, _, c1 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w1.CloseNow()
	armFault(t, "ws.goaway")

	if closed, left := srv.GoAway(ctx); closed != 2 || left != 0 {
		t.Fatalf("GoAway 가 closed=%d left=%d 다. 2, 0 이어야 한다", closed, left)
	}
	var codes []websocket.StatusCode
	for _, c := range []<-chan error{c0, c1} {
		select {
		case err := <-c:
			codes = append(codes, websocket.CloseStatus(err))
		case <-time.After(5 * time.Second):
			t.Fatal("연결이 닫히지 않는다")
		}
	}
	count := map[websocket.StatusCode]int{}
	for _, c := range codes {
		count[c]++
	}
	if count[websocket.StatusInternalError] != 1 || count[websocket.StatusGoingAway] != 1 {
		t.Fatalf("닫기 코드 %v — 패닉 난 연결은 1011, 나머지는 1001 이어야 한다", codes)
	}
	recoveredOnce(t, logs, "ws.goaway")
}

// 받은함 신호 타이머(time.AfterFunc)도 연결 밖에서 돈다. 패닉은 프로세스를 끝내지 않고 그 연결만 1011 로 닫으며 다른 연결은 산다.
func TestConn_받은함신호타이머의패닉은그연결만1011로닫고다른연결은산다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, _, logs := stallServer(shared)
	actor := app.Actor{Kind: app.ActorAgent, AgentID: agent.ID, WorkspaceID: agent.WorkspaceID, Role: agent.Role}
	w0, f0, c0 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w0.CloseNow()
	w1, f1, c1 := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
	defer w1.CloseNow()
	armFault(t, "ws.inbox_signal")

	// 첫 신호는 바로 나가고, 묶는 간격 안의 두 번째 신호가 타이머를 건다.
	srv.hub.Resync()
	time.Sleep(200 * time.Millisecond)
	srv.hub.Resync()

	idx, code := awaitClose(t, c0, c1, inboxCoalesce+5*time.Second)
	if code != websocket.StatusInternalError {
		t.Fatalf("패닉 난 연결이 %d 로 닫혔다. 1011 이어야 한다", code)
	}
	recoveredOnce(t, logs, "ws.inbox_signal")
	if idx == 0 {
		pongs(t, ctx, w1, f1, c1)
	} else {
		pongs(t, ctx, w0, f0, c0)
	}
}

// D30·api.md 9절(감사 23 N6): 규칙을 어긴 프레임(바이너리, JSON 아님)은 오류 프레임 없이 1000 으로 닫고, 그 까닭을 ws.read_closed INFO
// 한 줄(err_kind=not_text_frame·bad_json)로 남긴다. 상대가 그냥 닫은 것은 흔해서 INFO 로 남기지 않는다(DEBUG).
func TestConn_규칙을어긴프레임은1000으로닫고ws_read_closed를INFO로남기며그냥닫힌연결은남기지않는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	for _, tc := range []struct {
		name string
		send func(ws *websocket.Conn) error
		kind string // 빈 값이면 ws.read_closed INFO 가 없어야 한다
	}{
		{"바이너리 프레임", func(ws *websocket.Conn) error { return ws.Write(ctx, websocket.MessageBinary, []byte{1, 2}) }, "not_text_frame"},
		{"JSON 아님", func(ws *websocket.Conn) error { return ws.Write(ctx, websocket.MessageText, []byte("{nope")) }, "bad_json"},
		{"클라이언트가 그냥 닫음", func(ws *websocket.Conn) error { return ws.Close(websocket.StatusNormalClosure, "") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, logs := stallServer(shared)
			ws, _, closed := dialSubscribed(t, ctx, srv, actor, conv.ID, nil)
			defer ws.CloseNow()
			if err := tc.send(ws); err != nil {
				t.Fatal(err)
			}
			if tc.kind != "" {
				select {
				case err := <-closed:
					if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
						t.Fatalf("닫기 코드 %d — 1000 이어야 한다", code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("규칙을 어긴 프레임 뒤에 연결이 닫히지 않는다")
				}
			}
			waitLog(t, logs, "ws.closed", 5*time.Second)
			lines := logLines(logs, "ws.read_closed")
			switch {
			case tc.kind == "" && len(lines) != 0:
				t.Errorf("그냥 닫힌 연결이 ws.read_closed 를 INFO 로 남겼다: %v", lines)
			case tc.kind != "" && (len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["err_kind"] != tc.kind):
				t.Errorf("ws.read_closed 는 err_kind=%s INFO 한 줄이어야 한다: %v", tc.kind, lines)
			}
		})
	}
}
