package httpapi_test

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 고객이 상담을 직접 종료한다(D48). 테스트마다 자기 워크스페이스(teamOf)라 다른 테스트의 상담이 지표에 섞이지 않는다.

func closePath(convID string) string { return "/api/customer/conversations/" + convID + "/close" }

// customerClose 는 고객이 위젯에서 상담 종료를 누른다. 대화(고객 표현)를 돌려준다.
func (h *harness) customerClose(convID, token string) map[string]any {
	h.t.Helper()
	code, body := h.call(http.MethodPost, closePath(convID), token, nil)
	if code != http.StatusOK {
		h.t.Fatalf("고객 종료: %d %+v", code, body)
	}
	conv, _ := body["conversation"].(map[string]any)
	return conv
}

func TestCustomerClose_고객은자기상담만닫고이미닫혔으면같은결과로200이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("고객 종료 권한 상점")
	cust, convID := h.widgetCustomer(tm.tn)
	other, _ := h.widgetCustomer(tm.tn)
	stranger := h.teamOf("다른 상점")
	outsider, _ := h.widgetCustomer(stranger.tn)

	if code, body := h.call(http.MethodPost, closePath(convID), other, nil); code != http.StatusForbidden {
		t.Errorf("남의 상담은 403: %d %+v", code, body)
	}
	if code, body := h.call(http.MethodPost, closePath(convID), outsider, nil); code != http.StatusNotFound {
		t.Errorf("다른 워크스페이스 고객에게는 없는 상담(404): %d %+v", code, body)
	}
	if code, body := h.call(http.MethodPost, closePath(convID), tm.admin.token, nil); code != http.StatusForbidden || !strings.Contains(body["error"].(map[string]any)["message"].(string), "고객만") {
		t.Errorf("상담원이 부르면 403 고객 전용: %d %+v", code, body)
	}
	if code, _ := h.call(http.MethodPost, closePath("not-an-id"), cust, nil); code != http.StatusBadRequest {
		t.Errorf("id 모양이 아니면 400: %d", code)
	}
	if st := h.get("/api/conversations/"+convID, tm.admin.token)["state"]; st != "OPEN" {
		t.Fatalf("거절된 요청이 상태를 바꿨다: %v", st)
	}

	first := h.customerClose(convID, cust)
	if first["state"] != "CLOSED" || first["closed_by"] != "CUSTOMER" {
		t.Errorf("닫으면 종료이고 닫은 쪽은 고객이다: %+v", first)
	}
	if _, leaked := first["assignee_agent_id"]; leaked {
		t.Errorf("고객 표현에는 운영 정보가 없다: %+v", first)
	}
	again := h.customerClose(convID, cust)
	if again["state"] != "CLOSED" || again["closed_by"] != "CUSTOMER" {
		t.Errorf("다시 눌러도 같은 결과로 200: %+v", again)
	}
	closedEvents := 0
	for _, raw := range firstList(h.get("/api/agent/work?conversation="+convID, tm.admin.token), "events") {
		if ev, _ := raw.(map[string]any); ev["kind"] == "closed" {
			closedEvents++
		}
	}
	if closedEvents != 1 {
		t.Errorf("두 번 눌러도 종료 기록은 한 번이다: %d", closedEvents)
	}

	t.Run("상담원이 먼저 닫은 상담을 고객이 닫아도 200 이고 닫은 쪽은 상담원 그대로다", func(t *testing.T) {
		c2, conv2 := h.widgetCustomer(tm.tn)
		h.closeConv(conv2, tm.staff.token)
		got := h.customerClose(conv2, c2)
		if got["state"] != "CLOSED" || got["closed_by"] != "AGENT" {
			t.Errorf("이미 닫힌 상담: %+v", got)
		}
	})
}

func TestCustomerClose_답없이고객이닫은상담은고객자체해결이고AI처리율의분자에든다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("고객 자체 해결 상점")
	admin := tm.tn.token

	// 잘못 눌렀다 — 아무 답 없이 고객이 닫았다
	c1, conv1 := h.widgetCustomer(tm.tn)
	h.customerClose(conv1, c1)
	// 문의를 남겼지만 답을 받기 전에 해결됐다 — 고객 글만 있다
	c2, conv2 := h.widgetCustomer(tm.tn)
	h.send(conv2, c2, "아 찾았습니다", false)
	h.send(conv2, admin, "메모: 고객이 스스로 찾음", true)
	h.customerClose(conv2, c2)
	// AI 가 답한 뒤 고객이 닫았다
	c3, conv3 := h.widgetCustomer(tm.tn)
	h.send(conv3, c3, "배송 언제 와요", false)
	h.aiReply(conv3, "내일 도착합니다")
	h.customerClose(conv3, c3)
	// 상담원이 답한 뒤 고객이 닫았다
	c4, conv4 := h.widgetCustomer(tm.tn)
	h.send(conv4, c4, "환불 되나요", false)
	h.send(conv4, tm.staff.token, "됩니다", false)
	h.customerClose(conv4, c4)
	// 상담원이 답 없이 닫았다 — 지금처럼 미응답이다
	c5, conv5 := h.widgetCustomer(tm.tn)
	h.send(conv5, c5, "문의", false)
	h.closeConv(conv5, admin)

	m := h.metrics(admin)
	closed, _ := m["closed"].(map[string]any)
	want := map[string]float64{"total": 5, "self": 2, "ai": 1, "agent": 1, "unanswered": 1, "by_customer": 4}
	for k, v := range want {
		if num(closed, k) != v {
			t.Errorf("closed.%s = %v, 기대 %v (%+v)", k, num(closed, k), v, closed)
		}
	}
	// 상담원 없이 끝난 비율 = (AI 1 + 고객 자체 해결 2) / (AI 1 + 자체 해결 2 + 상담원 1)
	if got := num(m, "ai_rate"); math.Abs(got-3.0/4) > 1e-9 {
		t.Errorf("AI 처리율의 분자에 고객 자체 해결이 든다 — 3/4: %v", got)
	}
	// 고객 자체 해결은 조사를 받지 않는다 — 응답률의 분모는 5 - 2 = 3 이다. 평가가 없으니 0/3 = 0.
	if isNull(m, "response_rate") || num(m, "response_rate") != 0 {
		t.Errorf("조사를 받은 종료 상담 3건, 평가 0: %v", m["response_rate"])
	}
	h.put("/api/conversations/"+conv3+"/rating", c3, map[string]any{"score": 5})
	if got := num(h.metrics(admin), "response_rate"); math.Abs(got-1.0/3) > 1e-9 {
		t.Errorf("응답률 = 평가 1 / 조사를 받은 종료 3: %v", got)
	}

	t.Run("상담원이 다시 열면 종료 수에서 빠지고 다시 닫으면 마지막 종료의 주체다", func(t *testing.T) {
		h.reopenConv(conv1, admin)
		h.send(conv1, tm.staff.token, "무엇을 도와드릴까요", false)
		h.closeConv(conv1, tm.staff.token)
		c, _ := h.metrics(admin)["closed"].(map[string]any)
		if num(c, "self") != 1 || num(c, "agent") != 2 || num(c, "by_customer") != 3 {
			t.Errorf("마지막 종료 기준: %+v", c)
		}
	})
}

func TestCustomerClose_답없이고객이닫은상담은별점을받지않고답이있었으면받는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("고객 종료 만족도 상점")

	c1, conv1 := h.widgetCustomer(tm.tn)
	h.send(conv1, c1, "잘못 눌렀어요", false)
	h.customerClose(conv1, c1)
	path1 := "/api/conversations/" + conv1 + "/rating"
	if v := h.get(path1, c1); v["closed"] != true || v["can_rate"] != false {
		t.Errorf("답 없이 고객이 닫은 상담은 별점을 띄우지 않는다: %+v", v)
	}
	if code, body := h.call(http.MethodPut, path1, c1, map[string]any{"score": 5}); code != http.StatusConflict || errorCodeOf(body) != "no_reply" {
		t.Errorf("서버도 거절한다 — 409 no_reply: %d %+v", code, body)
	}
	if v := h.get(path1, tm.admin.token); v["handler"] != "SELF" {
		t.Errorf("상담원은 처리 주체(고객 자체 해결)를 본다: %+v", v)
	}

	for _, tc := range []struct {
		name  string
		reply func(convID string)
	}{
		{"AI 답", func(convID string) { h.aiReply(convID, "AI 답") }},
		{"상담원 답", func(convID string) { h.send(convID, tm.staff.token, "상담원 답", false) }},
	} {
		t.Run(tc.name+" 뒤 고객이 닫으면 별점을 받는다", func(t *testing.T) {
			c, conv := h.widgetCustomer(tm.tn)
			h.send(conv, c, "문의", false)
			tc.reply(conv)
			h.customerClose(conv, c)
			path := "/api/conversations/" + conv + "/rating"
			if v := h.get(path, c); v["can_rate"] != true {
				t.Errorf("답이 있었다 — 별점을 띄운다: %+v", v)
			}
			if code, body := h.call(http.MethodPut, path, c, map[string]any{"score": 4}); code != http.StatusOK {
				t.Errorf("평가를 받는다: %d %+v", code, body)
			}
		})
	}

	t.Run("상담원이 답 없이 닫은 상담은 지금처럼 별점을 받는다", func(t *testing.T) {
		c, conv := h.widgetCustomer(tm.tn)
		h.send(conv, c, "문의", false)
		h.closeConv(conv, tm.staff.token)
		if code, body := h.call(http.MethodPut, "/api/conversations/"+conv+"/rating", c, map[string]any{"score": 1}); code != http.StatusOK {
			t.Errorf("상담원이 닫은 상담의 규칙은 그대로다: %d %+v", code, body)
		}
	})
}

func TestCustomerClose_고객이닫은상담에고객이쓴글은REST와WebSocket모두closed_by_customer로거절되고다시열지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 30*time.Second)
	tm := h.teamOf("읽기 전용 상점")
	cust, convID := h.widgetCustomer(tm.tn)
	h.send(convID, cust, "문의", false)
	h.customerClose(convID, cust)

	code, body := h.call(http.MethodPost, "/api/conversations/"+convID+"/messages", cust, map[string]any{"client_msg_id": id.New(), "body": "닫은 뒤의 글"})
	if code != http.StatusConflict || errorCodeOf(body) != "closed_by_customer" {
		t.Errorf("REST 는 409 closed_by_customer: %d %+v", code, body)
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "새 문의하기") {
		t.Errorf("문구는 새 문의로 안내한다: %q", msg)
	}

	ws := h.dial(ctx, cust)
	if r := ws.subscribe(ctx, convID, 0); r.Type != "ready" {
		t.Fatalf("고객이 닫은 상담도 읽기는 된다: %+v", r)
	}
	cmid := id.New()
	ws.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": cmid, "body": "소켓으로 쓴 글"})
	f := ws.await(ctx, func(f frame) bool { return f.Type == "error" || f.Type == "ack" })
	if f.Type != "error" || f.Code != "closed_by_customer" || f.ClientMsgID != cmid {
		t.Errorf("WebSocket 은 그 글의 오류 프레임 closed_by_customer: %+v", f)
	}

	conv := h.get("/api/conversations/"+convID, tm.admin.token)
	if conv["state"] != "CLOSED" || conv["closed_by"] != "CUSTOMER" {
		t.Errorf("거절된 글은 다시 열지 않는다: %+v", conv)
	}
	for _, m := range h.messagesOf(convID, cust) {
		if b, _ := m["body"].(string); strings.Contains(b, "닫은 뒤") || strings.Contains(b, "소켓으로") {
			t.Errorf("거절된 글이 저장됐다: %+v", m)
		}
	}
}

func TestCustomerClose_상담원이다시열면고객입력이다시열리고상담원이닫은상담은지금처럼고객글이다시연다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("다시 열기 상점")

	cust, convID := h.widgetCustomer(tm.tn)
	h.customerClose(convID, cust)
	h.reopenConv(convID, tm.staff.token)
	if st := h.get("/api/conversations/"+convID, cust); st["state"] != "OPEN" || st["closed_by"] != nil {
		t.Errorf("다시 열린 상담에는 닫은 쪽이 없다: %+v", st)
	}
	if code, body := h.call(http.MethodPost, "/api/conversations/"+convID+"/messages", cust, map[string]any{"client_msg_id": id.New(), "body": "다시 열렸네요"}); code != http.StatusCreated {
		t.Errorf("상담원이 다시 열면 고객 입력이 다시 열린다: %d %+v", code, body)
	}

	c2, conv2 := h.widgetCustomer(tm.tn)
	h.closeConv(conv2, tm.staff.token)
	if code, body := h.call(http.MethodPost, "/api/conversations/"+conv2+"/messages", c2, map[string]any{"client_msg_id": id.New(), "body": "하나 더요"}); code != http.StatusCreated {
		t.Errorf("상담원이 닫은 상담에 고객이 쓰면 저장된다: %d %+v", code, body)
	}
	if st := h.get("/api/conversations/"+conv2, c2)["state"]; st != "OPEN" {
		t.Errorf("상담원이 닫은 상담은 고객 글이 다시 연다: %v", st)
	}
}

func TestCustomerClose_고객종료는상담원만보는시스템줄과고객이닫은업무기록을남기고받은함에서빠진다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("고객 종료 기록 상점")
	admin := tm.tn.token
	cust, convID := h.widgetCustomer(tm.tn)
	h.send(convID, cust, "문의합니다", false)

	inList := func(query string) bool {
		return slices.Contains(listIDs(firstList(h.get("/api/agent/conversations?"+query, admin), "conversations")), convID)
	}
	if !inList("state=OPEN") || !inList("waiting=1") {
		t.Fatal("닫기 전에는 받은함에 있다")
	}
	h.customerClose(convID, cust)
	if inList("state=OPEN") || inList("waiting=1") || inList("assignee=none") {
		t.Error("고객이 닫은 상담은 받은함(진행중·답할 차례·미배정)에서 빠진다")
	}
	if !inList("state=CLOSED") {
		t.Error("완료 목록에는 있다")
	}

	note := func(token string) bool {
		for _, m := range h.messagesOf(convID, token) {
			if m["sender_kind"] == "SYSTEM" && m["body"] == "고객이 상담을 종료했습니다." {
				return m["visibility"] == "PRIVATE"
			}
		}
		return false
	}
	if !note(admin) {
		t.Error("상담원은 시스템 줄 \"고객이 상담을 종료했습니다.\" 을 본다(내부 줄)")
	}
	if note(cust) {
		t.Error("시스템 줄은 상담원만 본다(D25) — 고객은 상태로 안다")
	}

	var closed map[string]any
	for _, raw := range firstList(h.get("/api/agent/work?conversation="+convID, admin), "events") {
		if ev, _ := raw.(map[string]any); ev["kind"] == "closed" {
			closed = ev
		}
	}
	detail, _ := closed["detail"].(map[string]any)
	if closed == nil || closed["agent_id"] != nil || detail["by"] != "customer" {
		t.Errorf("업무 기록: 고객이 닫았다(상담원 없음, by=customer): %+v", closed)
	}
	for _, raw := range firstList(h.get("/api/agent/work/summary", admin), "agents") {
		if a, _ := raw.(map[string]any); num(a, "closed") != 0 {
			t.Errorf("상담원별 종료 수는 상담원의 종료만 센다: %+v", a)
		}
	}
}

func TestCustomerClose_고객종료는데스크와그고객의다른탭에대화프레임으로간다(t *testing.T) {
	h := newHarness(t, 30*time.Second, true)
	ctx := testCtx(t, 30*time.Second)
	cust, convID := h.customerSession()
	agent := h.agentToken()
	desk := h.dial(ctx, agent)
	desk.collectSeqs(ctx, int(desk.subscribe(ctx, convID, 0).LastSeq))
	tab := h.dial(ctx, cust)
	tab.collectSeqs(ctx, int(tab.subscribe(ctx, convID, 0).LastSeq))

	h.customerClose(convID, cust)
	closedByCustomer := func(f frame) bool {
		return f.Type == "conversation" && f.State == "CLOSED" && strings.Contains(string(f.Conversation), `"closed_by":"CUSTOMER"`)
	}
	for name, c := range map[string]*client{"데스크": desk, "고객의 다른 탭": tab} {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		f := c.await(wctx, func(f frame) bool { return f.Type == "conversation" })
		cancel()
		if !closedByCustomer(f) {
			t.Errorf("%s: 대화 프레임에 종료와 닫은 쪽이 실린다: %+v %s", name, f, f.Conversation)
		}
	}
}
