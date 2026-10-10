package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/logcat-io/go-chat-system/internal/adapter/ollama"
	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 상담 추적 로깅 (docs/proposals/2026-10-03-권한과-로깅.md 3절).
// 로그는 운영자가 쓰는 화면이다. 서버를 통째로 띄우고 실제 요청을 흘린 뒤, 운영자가 할 조회(키 하나로 거르기)를 그대로 해 본다.

func msgs(lines []logLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.str("msg"))
	}
	return out
}

func TestLogging_상담하나를키하나로따라간다(t *testing.T) {
	h, logs := newLoggedHarness(t)
	ctx := testCtx(t, 20*time.Second)

	// 찾으면 안 되는 값들. 전부 이 테스트에서만 쓰는 고유한 문자열이다.
	suffix := strings.ReplaceAll(id.New(), "-", "")[20:]
	customerName := "로그고객" + suffix
	customerBody := "고객본문-" + suffix
	agentBody := "상담원본문-" + suffix
	wsBody := "웹소켓본문-" + suffix
	editedBody := "고친본문-" + suffix
	agentEmail := "log-agent-" + suffix + "@test.local"
	agentName := "로그상담원" + suffix
	password := "pw-" + suffix
	filename := "secret-file-" + suffix + ".png"

	agent, err := h.store.CreateAgent(context.Background(), core.DefaultWorkspaceID, agentEmail, agentName, password)
	if err != nil {
		t.Fatal(err)
	}

	// ── 고객: 위젯을 열고(세션) 문의를 시작해 메시지를 보낸다 ──
	_, session := h.request(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": customerName}, nil)
	customerToken, _ := session["token"].(string)
	customer, _ := session["customer"].(map[string]any)
	customerID, _ := customer["id"].(string)
	if customerToken == "" || customerID == "" {
		t.Fatalf("세션 응답이 비었다: %v", session)
	}
	_, started := h.request(http.MethodPost, "/api/customer/conversations", customerToken, nil, nil)
	conv, _ := started["conversation"].(map[string]any)
	convID, _ := conv["id"].(string)
	if convID == "" {
		t.Fatalf("대화를 열지 못했다: %v", started)
	}
	sent := h.send(convID, customerToken, customerBody, false)
	customerMsgID, _ := sent["id"].(string)
	h.send(convID, customerToken, customerBody, false) // 같은 본문 한 번 더 — 본문이 어디에도 안 남는지 보려고

	// ── 상담원: 로그인하고 답하고, 고객은 첨부를 올리고 자기 메시지를 고치고, 상담원이 종료한다 ──
	_, login := h.request(http.MethodPost, "/api/agent/login", "", map[string]any{"email": agentEmail, "password": password}, nil)
	agentToken, _ := login["token"].(string)
	if agentToken == "" {
		t.Fatalf("로그인 실패: %v", login)
	}
	h.request(http.MethodPost, "/api/agent/login", "", map[string]any{"email": agentEmail, "password": password + "x"}, nil)
	h.send(convID, agentToken, agentBody, false)
	h.upload(convID, customerToken, filename, pngBytes(4, 4))
	if res, _ := h.request(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+customerMsgID, customerToken,
		map[string]any{"body": editedBody}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("수정이 %d", res.StatusCode)
	}
	h.request(http.MethodPost, "/api/conversations/"+convID+"/read", agentToken, map[string]any{"seq": 1}, nil)
	h.post("/api/agent/conversations/"+convID+"/assign", agentToken, map[string]any{"agent_id": "me"})

	// WebSocket 으로도 한 통 보낸다. 프레임 경로도 같은 키로 거를 수 있어야 한다.
	c := h.dial(ctx, customerToken)
	if f := c.subscribe(ctx, convID, 0); f.Type != "ready" {
		t.Fatalf("구독 실패: %+v", f)
	}
	wsClientMsgID := id.New()
	c.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": wsClientMsgID, "body": wsBody})
	c.await(ctx, func(f frame) bool { return f.Type == "ack" })
	_ = c.ws.Close(websocket.StatusNormalClosure, "")

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
	logs.await(t, func(l logLine) bool { return l.str("msg") == "ws.closed" && l.str("customer_id") == customerID })

	t.Run("conversation_id 로 거르면 문의부터 종료까지 순서대로 나온다", func(t *testing.T) {
		lines := logs.where(t, "conversation_id", convID)
		want := []func(logLine) bool{
			func(l logLine) bool {
				return l.str("msg") == "conversation.opened" && l.str("customer_id") == customerID
			},
			func(l logLine) bool { return l.str("msg") == "message.appended" && l.str("sender_kind") == "CUSTOMER" },
			func(l logLine) bool { return l.str("msg") == "message.appended" && l.str("sender_kind") == "AGENT" },
			func(l logLine) bool { return l.str("msg") == "conversation.state_changed" && l.str("to") == "CLOSED" },
		}
		next := 0
		for _, l := range lines {
			if next < len(want) && want[next](l) {
				next++
			}
		}
		if next != len(want) {
			t.Fatalf("%d번째 사건까지만 순서대로 나왔다: %v", next, msgs(lines))
		}
		for _, event := range []string{"message.revised", "upload.started", "upload.completed", "read.marked", "conversation.assigned", "ws.subscribed"} {
			if !hasMsg(lines, event) {
				t.Errorf("%s 가 conversation_id 로 걸리지 않는다: %v", event, msgs(lines))
			}
		}
	})

	t.Run("대화가 생기기 전의 세션은 customer_id 로 이어진다", func(t *testing.T) {
		lines := logs.where(t, "customer_id", customerID)
		if !hasMsg(lines, "session.created") || !hasMsg(lines, "conversation.opened") {
			t.Fatalf("세션과 첫 문의가 customer_id 로 이어지지 않는다: %v", msgs(lines))
		}
		for _, l := range lines {
			if l.str("msg") == "session.created" && l["member"] != false {
				t.Errorf("익명 고객인데 member 가 %v", l["member"])
			}
		}
	})

	t.Run("인증된 요청의 로그에는 workspace_id·actor_kind·actor_id 가 붙는다", func(t *testing.T) {
		for _, l := range logs.where(t, "conversation_id", convID) {
			if l.str("msg") != "message.appended" {
				continue
			}
			wantKind, wantID := "customer", customerID
			if l.str("sender_kind") == "AGENT" {
				wantKind, wantID = "agent", agent.ID
			}
			if l.str("workspace_id") != core.DefaultWorkspaceID || l.str("actor_kind") != wantKind || l.str("actor_id") != wantID {
				t.Errorf("행위자 키가 틀렸다: %v", l)
			}
			if l.str("trace_id") == "" {
				t.Errorf("trace_id 가 없다: %v", l)
			}
		}
	})

	t.Run("로그인 성공과 실패가 남고 실패는 사유 코드만 싣는다", func(t *testing.T) {
		ok := logs.where(t, "actor_id", agent.ID)
		if !hasMsg(ok, "agent.login") {
			t.Errorf("agent.login 이 없다")
		}
		failed := false
		for _, l := range logs.lines(t) {
			if l.str("msg") == "agent.login_failed" && l.str("reason") == "invalid_credentials" {
				failed = true
			}
		}
		if !failed {
			t.Error("agent.login_failed 가 없다")
		}
	})

	t.Run("WebSocket 연결·구독·종료가 conn_id 로 이어진다", func(t *testing.T) {
		connected := logs.await(t, func(l logLine) bool { return l.str("msg") == "ws.connected" && l.str("customer_id") == customerID })
		connID := connected.str("conn_id")
		if connID == "" {
			t.Fatalf("conn_id 가 없다: %v", connected)
		}
		lines := logs.where(t, "conn_id", connID)
		for _, event := range []string{"ws.connected", "ws.subscribed", "message.appended", "ws.closed"} {
			if !hasMsg(lines, event) {
				t.Errorf("%s 가 conn_id 로 걸리지 않는다: %v", event, msgs(lines))
			}
		}
		var traces []string
		for _, l := range lines {
			switch l.str("msg") {
			case "ws.closed":
				if l["close_code"] != float64(websocket.StatusNormalClosure) || l.str("closed_by") != "client" {
					t.Errorf("종료 사유가 틀렸다: %v", l)
				}
				if sent, _ := l["frames_sent"].(float64); sent < 2 { // ready + ack 이상
					t.Errorf("보낸 프레임 수가 %v", l["frames_sent"])
				}
			case "ws.subscribed", "message.appended":
				if l.str("conversation_id") != convID {
					t.Errorf("프레임 로그에 conversation_id 가 없다: %v", l)
				}
				// 저장 줄은 브라우저 큐의 글을 가리키는 client_msg_id 를 싣는다 — 고객이 보낸 글 하나를 클라이언트 기록과 서버 저장으로 잇는다(감사 24 G24).
				if l.str("msg") == "message.appended" && l.str("client_msg_id") != wsClientMsgID {
					t.Errorf("message.appended 의 client_msg_id=%q — 보낸 %s 여야 한다", l.str("client_msg_id"), wsClientMsgID)
				}
				traces = append(traces, l.str("trace_id"))
			}
		}
		// 프레임마다 새 trace 다. 구독과 전송은 다른 trace_id 를 갖는다.
		if len(traces) == 2 && (traces[0] == "" || traces[0] == traces[1]) {
			t.Errorf("프레임마다 trace_id 가 새로 나오지 않는다: %v", traces)
		}
	})

	t.Run("로그 어디에도 본문·이름·이메일·토큰·비밀번호·파일 이름이 나오지 않는다", func(t *testing.T) {
		all := logs.String()
		for what, secret := range map[string]string{
			"고객 본문": customerBody, "상담원 본문": agentBody, "WebSocket 본문": wsBody, "고친 본문": editedBody,
			"고객 이름": customerName, "상담원 이름": agentName, "상담원 이메일": agentEmail,
			"고객 토큰": customerToken, "상담원 토큰": agentToken, "비밀번호": password, "파일 이름": filename,
		} {
			if strings.Contains(all, secret) {
				t.Errorf("%s 가 로그에 나왔다", what)
			}
		}
		// 서명된 주소의 서명·만료 값도 싣지 않는다. 로컬 저장소의 주소는 쿼리에 서명이 있다 — 경로 패턴만 남아야 한다.
		if strings.Contains(all, "sig=") || strings.Contains(all, "expires=") {
			t.Error("서명된 주소가 로그에 나왔다")
		}
	})
}

func TestLogging_요청마다trace가있다(t *testing.T) {
	h, logs := newLoggedHarness(t)
	token, convID := h.customerSession()

	t.Run("응답 헤더의 X-Trace-Id 로 거르면 그 요청의 http.request 가 나온다", func(t *testing.T) {
		res, _ := h.request(http.MethodGet, "/api/conversations/"+convID+"/messages", token, nil, nil)
		traceID := res.Header.Get("X-Trace-Id")
		if len(traceID) != 32 {
			t.Fatalf("X-Trace-Id 가 W3C 형식이 아니다: %q", traceID)
		}
		if tp := res.Header.Get("traceparent"); !strings.HasPrefix(tp, "00-"+traceID+"-") {
			t.Errorf("traceparent 가 같은 trace 를 가리키지 않는다: %q", tp)
		}
		lines := logs.where(t, "trace_id", traceID)
		var req logLine
		for _, l := range lines {
			if l.str("msg") == "http.request" {
				req = l
			}
		}
		if req == nil {
			t.Fatalf("http.request 가 없다: %v", msgs(lines))
		}
		if req.str("method") != "GET" || req.str("http_route") != "GET /api/conversations/{id}/messages" ||
			req["status"] != float64(200) || req["bytes"] == float64(0) || req["duration_ms"] == nil {
			t.Errorf("http.request 내용이 틀렸다: %v", req)
		}
		// 경로의 대화 id 와 인증 주체가 요청 한 줄에도 붙는다.
		if req.str("conversation_id") != convID || req.str("actor_kind") != "customer" || req.str("workspace_id") == "" {
			t.Errorf("http.request 에 ctx 키가 없다: %v", req)
		}
	})

	t.Run("들어온 traceparent 는 이어받지 않고 요청마다 새 trace 를 만든다", func(t *testing.T) {
		// 공개 입구의 클라이언트가 trace id 를 정하면 문의 번호(D47)를 남의 요청과 같게 만들 수 있다(D33, 감사 24).
		const upstream = "4bf92f3577b34da6a3ce929d0e0e4736"
		res, _ := h.request(http.MethodGet, "/api/conversations/"+convID, token, nil,
			map[string]string{"traceparent": "00-" + upstream + "-00f067aa0ba902b7-01"})
		got := res.Header.Get("X-Trace-Id")
		if len(got) != 32 || got == upstream {
			t.Fatalf("X-Trace-Id 는 들어온 값이 아닌 새 32자리여야 한다: %q", got)
		}
		if tp := res.Header.Get("traceparent"); !strings.HasPrefix(tp, "00-"+got+"-") {
			t.Errorf("응답 traceparent 가 새 trace 를 가리키지 않는다: %q", tp)
		}
		if lines := logs.where(t, "trace_id", upstream); len(lines) != 0 {
			t.Errorf("들어온 trace_id 로 남은 로그가 %d 줄이다: %v", len(lines), msgs(lines))
		}
		if !hasMsg(logs.where(t, "trace_id", got), "http.request") {
			t.Error("새 trace_id 로 거른 로그에 http.request 가 없다")
		}
		// 같은 traceparent 를 두 번 보내도 두 요청은 다른 trace 다.
		again, _ := h.request(http.MethodGet, "/api/conversations/"+convID, token, nil,
			map[string]string{"traceparent": "00-" + upstream + "-00f067aa0ba902b7-01"})
		if again.Header.Get("X-Trace-Id") == got {
			t.Error("같은 traceparent 의 두 요청이 같은 trace 다")
		}
	})

	t.Run("권한 거절은 auth.denied 로 남고 누가 거절됐는지 붙는다", func(t *testing.T) {
		res, _ := h.request(http.MethodPost, "/api/agent/conversations/"+convID+"/state", token, map[string]any{"state": "CLOSED"}, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("고객이 상태를 바꿨다: %d", res.StatusCode)
		}
		lines := logs.where(t, "trace_id", res.Header.Get("X-Trace-Id"))
		var denied logLine
		for _, l := range lines {
			if l.str("msg") == "auth.denied" {
				denied = l
			}
		}
		if denied == nil || denied.str("level") != "WARN" || denied.str("actor_kind") != "customer" || denied.str("conversation_id") != convID {
			t.Errorf("auth.denied 가 없거나 틀렸다: %v", lines)
		}
	})

	t.Run("저장 거절은 message.rejected 로 남고 사유 코드만 싣는다", func(t *testing.T) {
		res, _ := h.request(http.MethodPost, "/api/conversations/"+convID+"/messages", token,
			map[string]any{"client_msg_id": id.New(), "body": "   "}, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("빈 본문이 %d", res.StatusCode)
		}
		var rejected logLine
		for _, l := range logs.where(t, "trace_id", res.Header.Get("X-Trace-Id")) {
			if l.str("msg") == "message.rejected" {
				rejected = l
			}
		}
		if rejected == nil || rejected.str("code") != "invalid" || rejected.str("level") != "WARN" || rejected.str("conversation_id") != convID {
			t.Errorf("message.rejected 가 없거나 틀렸다: %v", rejected)
		}
	})

	t.Run("안전망이 대화를 다시 깨우면 delivery.catchup 이 건수와 함께 debug 로 남는다", func(t *testing.T) {
		// 안전망 주기(2초)가 한 번 돌 때까지 기다린다. 이 테스트에서 이미 대화를 바꿨으니 건수가 1 이상이다.
		l := logs.await(t, func(l logLine) bool { return l.str("msg") == "delivery.catchup" })
		if n, _ := l["conversations"].(float64); n < 1 || l.str("trace_id") == "" || l.str("level") != "DEBUG" {
			t.Errorf("delivery.catchup 내용이 틀렸다: %v", l)
		}
	})

	t.Run("상태 확인 요청은 debug 로 낮춘다", func(t *testing.T) {
		res, _ := h.request(http.MethodGet, "/healthz", "", nil, nil)
		lines := logs.where(t, "trace_id", res.Header.Get("X-Trace-Id"))
		if len(lines) != 1 || lines[0].str("level") != "DEBUG" {
			t.Errorf("healthz 로그가 debug 한 줄이 아니다: %v", lines)
		}
	})

	t.Run("로깅 미들웨어를 거쳐도 WebSocket 업그레이드가 된다", func(t *testing.T) {
		ctx := testCtx(t, 10*time.Second)
		c := h.dial(ctx, token)
		if f := c.subscribe(ctx, convID, 0); f.Type != "ready" {
			t.Fatalf("업그레이드 뒤 구독이 안 된다: %+v", f)
		}
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
		up := logs.await(t, func(l logLine) bool {
			return l.str("msg") == "http.request" && l.str("http_route") == "GET /ws"
		})
		if up["status"] != float64(http.StatusSwitchingProtocols) {
			t.Errorf("업그레이드 상태가 101 이 아니다: %v", up)
		}
	})
}

func hasMsg(lines []logLine, msg string) bool {
	for _, l := range lines {
		if l.str("msg") == msg {
			return true
		}
	}
	return false
}

func TestLogging_거절과상담삭제(t *testing.T) {
	h, logs := newCapturedHarness(t, 2*time.Second, false)
	tm := h.teamOf("로그 거절 상점")
	cust, convID := h.widgetCustomer(tm.tn)

	t.Run("권한 거절은 요청 하나에 auth.denied 한 줄이고 모자란 권한을 싣는다", func(t *testing.T) {
		res, _ := h.request(http.MethodDelete, "/api/agent/conversations/"+convID, tm.partTime.token, map[string]any{"reason": "권한 없음"}, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("아르바이트가 상담을 지웠다: %d", res.StatusCode)
		}
		denied := linesOf(t, logs, res, "auth.denied")
		if len(denied) != 1 {
			t.Fatalf("auth.denied 가 %d줄: %v", len(denied), denied)
		}
		if l := denied[0]; l.str("permission") != "conversation.delete" || l.str("level") != "WARN" || l.str("actor_id") != tm.partTime.id {
			t.Errorf("auth.denied 내용이 틀렸다: %v", l)
		}
	})

	t.Run("권한이 아닌 거절(남의 대화)은 전달 계층이 code 와 함께 한 줄 남긴다", func(t *testing.T) {
		other, _ := h.widgetCustomer(tm.tn)
		res, _ := h.request(http.MethodGet, "/api/conversations/"+convID, other, nil, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("남의 대화: %d", res.StatusCode)
		}
		denied := linesOf(t, logs, res, "auth.denied")
		if len(denied) != 1 || denied[0].str("code") != "forbidden" || denied[0].str("permission") != "" {
			t.Errorf("auth.denied: %v", denied)
		}
	})

	t.Run("WebSocket 의 권한 거절도 auth.denied 로 남는다", func(t *testing.T) {
		ctx := testCtx(t, 10*time.Second)
		other, _ := h.widgetCustomer(tm.tn)
		c := h.dial(ctx, other)
		if f := c.subscribe(ctx, convID, 0); f.Type != "error" || f.Code != "forbidden" {
			t.Fatalf("남의 대화를 구독했다: %+v", f)
		}
		l := logs.await(t, func(l logLine) bool {
			return l.str("msg") == "auth.denied" && l.str("conn_id") != "" && l.str("conversation_id") == convID
		})
		if l.str("level") != "WARN" || l.str("code") != "forbidden" || l.str("actor_kind") != "customer" {
			t.Errorf("WebSocket auth.denied 내용이 틀렸다: %v", l)
		}
	})

	t.Run("첨부 거절은 upload.rejected 로 남고 단계와 사유 코드를 싣는다", func(t *testing.T) {
		res, _ := h.request(http.MethodPost, "/api/conversations/"+convID+"/attachments", cust,
			map[string]any{"filename": "empty.txt", "content_type": "text/plain", "byte_size": 0}, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("빈 파일 신청: %d", res.StatusCode)
		}
		rejected := linesOf(t, logs, res, "upload.rejected")
		if len(rejected) != 1 || rejected[0].str("step") != "start" || rejected[0].str("code") != "invalid" ||
			rejected[0].str("level") != "WARN" || rejected[0].str("conversation_id") != convID {
			t.Errorf("upload.rejected(start): %v", rejected)
		}
		started := h.startUpload(convID, cust, "never.txt", "text/plain", 10)
		res, _ = h.request(http.MethodPost, "/api/attachments/"+started.Attachment.ID+"/complete", cust, nil, nil)
		rejected = linesOf(t, logs, res, "upload.rejected")
		if len(rejected) != 1 || rejected[0].str("step") != "complete" || rejected[0].str("attachment_id") != started.Attachment.ID {
			t.Errorf("upload.rejected(complete): %v", rejected)
		}
		// 완료 주소에는 대화 id 가 없다. 첨부 행에서 알게 된 대화 id 와 고정 사유(file_missing)를 싣는다(D47).
		if len(rejected) == 1 && (rejected[0].str("conversation_id") != convID || rejected[0].str("reason") != "file_missing") {
			t.Errorf("upload.rejected(complete) 는 conversation_id 와 reason=file_missing 을 싣는다: %v", rejected)
		}
		// 같은 요청의 http.request 한 줄만으로도 응답 code 와 사유를 가른다.
		if req := linesOf(t, logs, res, "http.request"); len(req) != 1 || req[0].str("code") != "invalid" || req[0].str("reason") != "file_missing" {
			t.Errorf("http.request 는 code=invalid reason=file_missing 을 싣는다: %v", req)
		}
	})

	t.Run("승인 대기 계정의 로그인은 agent.login_failed reason=pending_approval 이다", func(t *testing.T) {
		email := "pending-" + strings.ToLower(id.New()[28:]) + "@example.com"
		// 로그인 입구는 기본 워크스페이스로만 간다(D35). 대기 계정도 거기 만든다.
		h.agentSignup(tenant{wsID: core.DefaultWorkspaceID}, email, "대기자", "join12345")
		res, _ := h.request(http.MethodPost, "/api/agent/login", "",
			map[string]any{"email": email, "password": "join12345"}, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("승인 대기 로그인: %d", res.StatusCode)
		}
		failed := linesOf(t, logs, res, "agent.login_failed")
		if len(failed) != 1 || failed[0].str("reason") != "pending_approval" || failed[0].str("level") != "WARN" {
			t.Errorf("agent.login_failed: %v", failed)
		}
		if strings.Contains(logs.String(), email) {
			t.Error("이메일이 로그에 나왔다")
		}
	})

	t.Run("상담 삭제·되살리기는 conversation.deleted·restored 로 남고 사유는 싣지 않는다", func(t *testing.T) {
		const reason = "로그에-나오면-안-되는-사유"
		res, _ := h.request(http.MethodDelete, "/api/agent/conversations/"+convID, tm.admin.token, map[string]any{"reason": reason}, nil)
		if res.StatusCode >= 300 {
			t.Fatalf("삭제: %d", res.StatusCode)
		}
		if got := linesOf(t, logs, res, "conversation.deleted"); len(got) != 1 || got[0].str("conversation_id") != convID || got[0].str("actor_id") != tm.admin.id {
			t.Errorf("conversation.deleted: %v", got)
		}
		res, _ = h.request(http.MethodPost, "/api/agent/conversations/"+convID+"/restore", tm.admin.token, nil, nil)
		if got := linesOf(t, logs, res, "conversation.restored"); len(got) != 1 || got[0].str("conversation_id") != convID {
			t.Errorf("conversation.restored: %v", got)
		}
		if strings.Contains(logs.String(), reason) {
			t.Error("삭제 사유가 로그에 나왔다")
		}
	})
}

// D30(감사 23): 클라이언트가 정한 글자(메서드, 경로의 id, 신청한 파일 종류)는 로그에 그대로 싣지 않는다.
// 모르는 메서드는 _OTHER, id 모양이 아닌 경로 id 는 키째 빠지고, 신청한 종류는 큰 갈래(image/*)만 싣는다.
func TestLogging_클라이언트가정한글자는로그에싣지않는다_모르는메서드는_OTHER이고id모양이아닌경로id는빠진다(t *testing.T) {
	o := observe(t) // 라우팅 전 스팬 이름도 본다(감사 24 G8). 하네스보다 먼저 설치한다
	h, logs := newLoggedHarness(t)
	token, convID := h.customerSession()
	const pii = "hong@example.com"

	// 1) 모르는 메서드(길다)
	weird := "HONGEXAMPLECOM" + strings.Repeat("M", 3000)
	res, _ := h.request(weird, "/api/conversations/"+convID, token, nil, nil)
	methodTrace := res.Header.Get("X-Trace-Id")
	// 2) id 모양이 아닌 메시지 id 로 고치기 — message.rejected
	res, _ = h.request(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+url.PathEscape(pii), token, map[string]any{"body": "고침"}, nil)
	editTrace := res.Header.Get("X-Trace-Id")
	// 3) id 모양이 아닌 첨부 id 로 완료 — upload.rejected step=complete
	res, _ = h.request(http.MethodPost, "/api/attachments/"+url.PathEscape(pii)+"/complete", token, nil, nil)
	completeTrace := res.Header.Get("X-Trace-Id")
	// 4) 신청한 종류에 이메일을 섞은 업로드 시작 — upload.started 는 큰 갈래만
	res = h.postRaw("/api/conversations/"+convID+"/attachments", token, map[string]any{
		"filename": "a.png", "content_type": "image/" + pii, "byte_size": 10,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		t.Fatalf("업로드 시작이 %d", res.StatusCode)
	}
	startTrace := res.Header.Get("X-Trace-Id")

	one := func(trace, msg string) logLine {
		t.Helper()
		for _, l := range logs.where(t, "trace_id", trace) {
			if l.str("msg") == msg {
				return l
			}
		}
		t.Fatalf("trace_id=%s 의 %s 가 없다", trace, msg)
		return nil
	}
	if l := one(methodTrace, "http.request"); l.str("method") != "_OTHER" {
		t.Errorf("모르는 메서드의 http.request method=%.40q — _OTHER 여야 한다", l.str("method"))
	}
	// 라우팅 전(405)이라 스팬 이름은 라우트 패턴이 아니라 메서드다. 모르는 메서드는 _OTHER 다 — 원문이면 이름이 3KB 이고 이름의 종류가 끝없이 는다.
	methodSpan := o.awaitSpan(t, func(s tracetest.SpanStub) bool {
		return s.SpanContext.TraceID().String() == methodTrace && !s.Parent.IsValid()
	})
	if methodSpan.Name != "_OTHER" {
		t.Errorf("모르는 메서드의 스팬 이름 %.40q — _OTHER 여야 한다", methodSpan.Name)
	}
	if l := one(editTrace, "message.rejected"); l["ref_message_id"] != nil || l.str("kind") != "EDIT" {
		t.Errorf("id 모양이 아닌 ref_message_id 가 실렸다: %v", l)
	}
	if l := one(completeTrace, "upload.rejected"); l["attachment_id"] != nil || l.str("step") != "complete" {
		t.Errorf("id 모양이 아닌 attachment_id 가 실렸다: %v", l)
	}
	if l := one(startTrace, "upload.started"); l.str("content_type") != "image/*" {
		t.Errorf("upload.started content_type=%q — 신청한 종류의 큰 갈래 image/* 여야 한다", l.str("content_type"))
	}
	out := logs.String()
	for _, needle := range []string{pii, "hong%40example.com", "MMMMMMMMMM"} {
		if strings.Contains(out, needle) {
			t.Errorf("클라이언트가 정한 글자 %q 가 로그에 실렸다", needle)
		}
	}
	// 모양이 맞는 id 는 그대로 싣는다 — 거르기가 다 지우지 않는다
	other := id.New()
	res, _ = h.request(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+other, token, map[string]any{"body": "고침"}, nil)
	if l := one(res.Header.Get("X-Trace-Id"), "message.rejected"); l.str("ref_message_id") != other {
		t.Errorf("id 모양의 ref_message_id 를 싣지 않았다: %v", l)
	}
}

// D47(감사 23): http.request 한 줄만으로 무엇이 왜 실패했는지 거른다. 401(토큰 없음·틀린 토큰·틀린 비밀번호), 본문 해석 실패 400,
// 초안 모델 실패 502 에도 고정 사유를 싣는다. 502 의 사유는 모델 쪽 원인(ollama_http_503)이다.
func TestLogging_http_request는401과400과502에도고정사유를싣는다(t *testing.T) {
	h, logs := newLoggedHarness(t)
	_, convID := h.customerSession()
	agentToken, adminToken := h.agentToken(), h.adminToken()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(model.Close)
	h.svc.Queries.SetDraftProvider(ollama.New(model.URL, "m"))

	reasonOf := func(res *http.Response) logLine {
		t.Helper()
		trace := res.Header.Get("X-Trace-Id")
		for _, l := range logs.where(t, "trace_id", trace) {
			if l.str("msg") == "http.request" {
				return l
			}
		}
		t.Fatalf("trace_id=%s 의 http.request 가 없다", trace)
		return nil
	}
	cases := []struct {
		name         string
		do           func() *http.Response
		status       int
		code, reason string
	}{
		{"토큰 없음", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/api/conversations/"+convID, "", nil, nil)
			return r
		}, http.StatusUnauthorized, "unauthorized", "no_token"},
		{"틀린 토큰", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/api/conversations/"+convID, "not-a-token", nil, nil)
			return r
		}, http.StatusUnauthorized, "unauthorized", "bad_session"},
		{"틀린 비밀번호", func() *http.Response {
			r, _ := h.request(http.MethodPost, "/api/agent/login", "", map[string]any{"email": "agent@example.com", "password": "틀림"}, nil)
			return r
		}, http.StatusUnauthorized, "unauthorized", "invalid_credentials"},
		{"본문 해석 실패", func() *http.Response {
			r, _ := h.request(http.MethodPost, "/api/agent/login", "", json.RawMessage(`{"email":1}`), nil)
			return r
		}, http.StatusBadRequest, "invalid", "bad_body"},
		{"초안 모델 503", func() *http.Response {
			r := h.postRaw("/api/agent/conversations/"+convID+"/draft", agentToken, nil)
			r.Body.Close()
			return r
		}, http.StatusBadGateway, "draft_failed", "ollama_http_503"},
		// 감사 24: 아래 다섯은 사유를 적는 줄이 있었지만 그 줄을 빼도 통과했다(변이 G12·G14~G17).
		{"없거나 쓴 티켓", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/ws?ticket=not-a-ticket", "", nil, nil)
			return r
		}, http.StatusUnauthorized, "unauthorized", "bad_ticket"},
		{"업무 기록의 틀린 상담원 거르개", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/api/agent/work?agent=hong", agentToken, nil, nil)
			return r
		}, http.StatusBadRequest, "invalid", "bad_agent_filter"},
		{"틀린 시각", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/api/agent/work/summary?from=어제", agentToken, nil, nil)
			return r
		}, http.StatusBadRequest, "invalid", "bad_time"},
		{"내보내기의 틀린 형식", func() *http.Response {
			r, _ := h.request(http.MethodGet, "/api/admin/export?format=xml", adminToken, nil, nil)
			return r
		}, http.StatusBadRequest, "invalid", "bad_format"},
		{"FAQ 의 빈 본문", func() *http.Response {
			r, _ := h.request(http.MethodPut, "/api/admin/widget-faq", adminToken, map[string]any{}, nil)
			return r
		}, http.StatusBadRequest, "invalid", "items_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := c.do()
			l := reasonOf(res)
			if res.StatusCode != c.status || l.str("code") != c.code || l.str("reason") != c.reason {
				t.Errorf("상태 %d, http.request code=%q reason=%q — %d·%s·%s 여야 한다", res.StatusCode, l.str("code"), l.str("reason"), c.status, c.code, c.reason)
			}
		})
	}

	// 인증을 지난 뒤 세션이 사라진 티켓 발급(그 사이 로그아웃)은 bad_session 이다(감사 24 G13). 사이에 끼어들 저장소로 띄운다.
	t.Run("티켓을 받는 사이 사라진 세션", func(t *testing.T) {
		log := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		lost := buildHarness(t, harnessConfig{catchup: time.Second, log: log, wrap: func(s *pgstore.Store) app.Store {
			return ticketLost{s}
		}})
		cust, _ := lost.customerSession()
		res, _ := lost.request(http.MethodPost, "/api/ws-ticket", cust, nil, nil)
		if l := reasonOf(res); res.StatusCode != http.StatusUnauthorized || l.str("code") != "unauthorized" || l.str("reason") != "bad_session" {
			t.Errorf("상태 %d, http.request code=%q reason=%q — 401·unauthorized·bad_session 이어야 한다", res.StatusCode, l.str("code"), l.str("reason"))
		}
	})
}

// ticketLost 는 티켓을 발급하려는 순간 세션이 사라진 저장소다(인증과 발급 사이에 로그아웃).
type ticketLost struct{ *pgstore.Store }

func (ticketLost) IssueWSTicket(context.Context, core.SessionRef, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, core.ErrNotFound
}

// workspaceDown 은 워크스페이스 읽기만 실패하는 저장소다(DB 가 잠깐 닿지 않는 것과 같은 SQLSTATE).
type workspaceDown struct {
	*pgstore.Store
	down atomic.Bool
}

type sqlStateErr struct{}

func (sqlStateErr) Error() string    { return "failed to connect to user=chat database=secretdb" }
func (sqlStateErr) SQLState() string { return "57P01" }

func (s *workspaceDown) Workspace(ctx context.Context, id string) (core.Workspace, error) {
	if s.down.Load() {
		return core.Workspace{}, sqlStateErr{}
	}
	return s.Store.Workspace(ctx, id)
}

// D47(감사 23 N10): /widget 이 워크스페이스를 못 읽으면 고객에게 503 과 다시 시도할 문구를 주고, 원인 줄 widget.unavailable(WARN, err_kind)을
// 남긴다. http.request 의 reason 도 같은 종류다. 원문(접속 정보)은 어디에도 싣지 않는다.
func TestLogging_위젯페이지가워크스페이스를못읽으면503과widget_unavailable에종류만남긴다(t *testing.T) {
	logs, log := captureLogs(t)
	var wrapped *workspaceDown
	h := buildHarness(t, harnessConfig{catchup: time.Second, log: log, wrap: func(s *pgstore.Store) app.Store {
		wrapped = &workspaceDown{Store: s}
		return wrapped
	}})
	wrapped.down.Store(true)
	res, err := http.Get(h.srv.URL + "/widget")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "상담 창을 열지 못했습니다. 잠시 후 다시 시도해 주세요") {
		t.Fatalf("응답 %d %q — 503 과 다시 시도 문구여야 한다", res.StatusCode, body)
	}
	trace := res.Header.Get("X-Trace-Id")
	var unavailable, request logLine
	for _, l := range logs.where(t, "trace_id", trace) {
		switch l.str("msg") {
		case "widget.unavailable":
			unavailable = l
		case "http.request":
			request = l
		}
	}
	if unavailable == nil || unavailable.str("level") != "WARN" || unavailable.str("err_kind") != "57P01" {
		t.Errorf("widget.unavailable 은 WARN err_kind=57P01 이어야 한다: %v", unavailable)
	}
	if request == nil || request.str("reason") != "57P01" || request.str("level") != "WARN" {
		t.Errorf("http.request 는 503 WARN reason=57P01 이어야 한다: %v", request)
	}
	if strings.Contains(logs.String(), "secretdb") {
		t.Error("오류 원문(접속 정보)이 로그에 실렸다")
	}
}

// D47(감사 24): 입력 거절(400 invalid)은 http.request 의 reason 에 필드 수준의 고정 사유를 싣는다. 전에는 reason 없이 남는 invalid 가 15종을 넘어
// 로그 한 줄로는 어느 필드가 틀렸는지 몰랐다. 사유는 코드이고 사용자 문구·입력값은 싣지 않는다. 모든 invalid 호출의 코드가 리터럴인지는
// internal/archtest 의 TestInput_입력거절의사유코드는소문자리터럴이다 가 본다.
func TestLogging_입력거절은http_request의reason에어느필드가틀렸는지고정사유를싣는다(t *testing.T) {
	h, logs := newLoggedHarness(t)
	cust, convID := h.customerSession()
	admin, agent := h.adminToken(), h.agentToken()
	const pii = "hong-at-example"

	cases := []struct {
		name, method, path, token string
		body                      any
		reason                    string
	}{
		{"초대 이메일 모양", http.MethodPost, "/api/admin/invites", admin, map[string]any{"email": pii, "name": "홍", "role": "STAFF"}, "email_format"},
		{"초대 이름 빈칸", http.MethodPost, "/api/admin/invites", admin, map[string]any{"email": "a-" + id.New()[:8] + "@example.com", "name": " ", "role": "STAFF"}, "name_required"},
		{"초대 역할", http.MethodPost, "/api/admin/invites", admin, map[string]any{"email": "b-" + id.New()[:8] + "@example.com", "name": "홍", "role": pii}, "role_invalid"},
		{"본문 길이", http.MethodPost, "/api/conversations/" + convID + "/messages", cust, map[string]any{"client_msg_id": id.New(), "body": strings.Repeat("가", 4001)}, "body_too_long"},
		{"빈 본문", http.MethodPost, "/api/conversations/" + convID + "/messages", cust, map[string]any{"client_msg_id": id.New(), "body": " "}, "body_required"},
		{"client_msg_id 길이", http.MethodPost, "/api/conversations/" + convID + "/messages", cust, map[string]any{"client_msg_id": strings.Repeat("c", 129), "body": "안녕"}, "client_msg_id_length"},
		{"상태 값", http.MethodPost, "/api/agent/conversations/" + convID + "/state", agent, map[string]any{"state": pii}, "state_invalid"},
		{"만족도 점수", http.MethodPut, "/api/conversations/" + convID + "/rating", cust, map[string]any{"score": 9}, "score_range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := h.request(c.method, c.path, c.token, c.body, nil)
			if res.StatusCode != http.StatusBadRequest || errorCodeOf(body) != "invalid" {
				t.Fatalf("상태 %d %v — 400 invalid 여야 한다", res.StatusCode, body)
			}
			var line logLine
			for _, l := range logs.where(t, "trace_id", res.Header.Get("X-Trace-Id")) {
				if l.str("msg") == "http.request" {
					line = l
				}
			}
			if line.str("code") != "invalid" || line.str("reason") != c.reason {
				t.Errorf("http.request code=%q reason=%q — invalid·%s 여야 한다", line.str("code"), line.str("reason"), c.reason)
			}
		})
	}
	if strings.Contains(logs.String(), pii) {
		t.Error("거절된 입력값이 로그에 실렸다")
	}
}
