package httpapi_test

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 상담 지표와 만족도 조사(D45). 테스트마다 자기 워크스페이스(teamOf)라 다른 테스트의 상담이 숫자에 섞이지 않는다.

// metricsWindow 는 지금을 가운데 둔 두 시간이다. 이 테스트가 만든 상담만 든다(워크스페이스가 따로다).
func (h *harness) metrics(token string) map[string]any {
	h.t.Helper()
	now := time.Now().UTC()
	return h.get("/api/agent/metrics?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Add(time.Hour).Format(time.RFC3339), token)
}

func (h *harness) closeConv(convID, token string) {
	h.t.Helper()
	if code, body := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", token, map[string]any{"state": "CLOSED"}); code != http.StatusOK {
		h.t.Fatalf("종료: %d %+v", code, body)
	}
}

func (h *harness) reopenConv(convID, token string) {
	h.t.Helper()
	if code, body := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", token, map[string]any{"state": "OPEN"}); code != http.StatusOK {
		h.t.Fatalf("다시 열기: %d %+v", code, body)
	}
}

// aiReply 는 AI 응대의 답 자리(Commands.AIReply)를 부른다. HTTP 입구는 없다 — 상담 흐름 개편의 AI 모듈이 서버 안에서 부른다.
func (h *harness) aiReply(convID, body string) {
	h.t.Helper()
	if _, err := h.svc.Commands.AIReply(context.Background(), convID, "ai-"+id.New(), body); err != nil {
		h.t.Fatalf("AI 응대: %v", err)
	}
}

func createdAt(t *testing.T, m map[string]any) time.Time {
	t.Helper()
	s, _ := m["created_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("created_at: %q", s)
	}
	return at
}

func num(m map[string]any, path ...string) float64 {
	var cur any = m
	for _, k := range path {
		mm, _ := cur.(map[string]any)
		cur = mm[k]
	}
	v, _ := cur.(float64)
	return v
}

func isNull(m map[string]any, path ...string) bool {
	var cur any = m
	for i, k := range path {
		mm, _ := cur.(map[string]any)
		v, ok := mm[k]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return v == nil
		}
		cur = v
	}
	return false
}

// near 는 응답 시간(ms)이 기대값과 50ms 안인지다. 기다리기 시작한 시각은 그 고객 글이 번호를 받는 순간이라 메시지 시각(같은 트랜잭션의
// INSERT)보다 조금 앞선다(-race 에서 1ms 넘게 본 적이 있다). 글 사이는 150ms 라 다른 글에서 시작했으면 이 폭을 넘는다.
func near(got float64, want time.Duration) bool {
	return math.Abs(got-float64(want.Microseconds())/1000) < 50
}

func responder(m map[string]any, kind, agentID string) map[string]any {
	for _, raw := range firstList(m, "responders") {
		r, _ := raw.(map[string]any)
		if r["kind"] == kind && (kind == "AI" || r["agent_id"] == agentID) {
			return r
		}
	}
	return nil
}

func TestMetrics_응답은기다리던고객에게간상담원AI공개답이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("응답 지표 상점")
	admin := tm.tn.token
	// 접수 안내(BOT)를 켠다 — 고객의 첫 글에 곧바로 나간다. 응답이 아니어야 한다.
	settings := h.get("/api/admin/settings", admin)
	settings["auto_messages"] = map[string]any{"welcome": "환영", "first_reply": "접수됐습니다", "away": "접수됐습니다"}
	h.put("/api/admin/settings", admin, settings)

	cust, convID := h.widgetCustomer(tm.tn)

	t.Run("접수 안내·내부 메모는 응답이 아니고 차례는 답이 없던 고객 글 중 첫 글에서 시작한다", func(t *testing.T) {
		q1 := h.send(convID, cust, "주문이 안 왔어요", false)
		bot := false
		for _, m := range h.messagesOf(convID, admin) {
			if m["sender_kind"] == "BOT" && m["body"] == "접수됐습니다" {
				bot = true
			}
		}
		if !bot {
			t.Fatal("고객 글 바로 뒤에 접수 안내(BOT)가 저장돼야 한다 — 이 정책은 그 안내가 응답이 아닌지 본다")
		}

		time.Sleep(150 * time.Millisecond)
		h.send(convID, admin, "배송 조회 중", true) // 내부 메모
		time.Sleep(150 * time.Millisecond)
		h.send(convID, cust, "언제 오나요", false) // 같은 차례
		time.Sleep(150 * time.Millisecond)
		a1 := h.send(convID, tm.staff.token, "오늘 출발합니다", false)
		h.send(convID, tm.staff.token, "운송장 번호는 123 입니다", false) // 차례가 이미 닫혔다 — 응답이 아니다
		time.Sleep(150 * time.Millisecond)
		q3 := h.send(convID, cust, "고맙습니다", false)
		time.Sleep(150 * time.Millisecond)
		h.aiReply(convID, "도움이 되어 기쁩니다")
		ai := h.messagesOf(convID, admin)
		aiMsg := ai[len(ai)-1]
		if aiMsg["sender_kind"] != "AI" {
			t.Fatalf("마지막 줄은 AI 응대여야 한다: %+v", aiMsg)
		}

		m := h.metrics(admin)
		first := createdAt(t, a1).Sub(createdAt(t, q1))
		second := createdAt(t, aiMsg).Sub(createdAt(t, q3))
		if got := num(m, "response", "count"); got != 2 {
			t.Fatalf("응답은 두 차례여야 한다(접수 안내·내부 메모·이어진 답은 아니다): %v", got)
		}
		if !near(num(m, "first_response", "avg_ms"), first) || num(m, "first_response", "count") != 1 {
			t.Errorf("첫 응답 = 첫 고객 글 → 첫 상담원 공개 답 %v: %+v", first, m["first_response"])
		}
		if !near(num(m, "response", "avg_ms"), (first+second)/2) {
			t.Errorf("평균 응답 시간 = 차례들의 평균 %v: %+v", (first+second)/2, m["response"])
		}
		staff := responder(m, "AGENT", tm.staff.id)
		if staff == nil || num(staff, "response", "count") != 1 || !near(num(staff, "response", "avg_ms"), first) {
			t.Errorf("상담원별 응답: %+v", staff)
		}
		ai2 := responder(m, "AI", "")
		if ai2 == nil || num(ai2, "response", "count") != 1 || !near(num(ai2, "response", "p50_ms"), second) {
			t.Errorf("AI 응답: %+v", ai2)
		}
	})
}

func TestMetrics_처리주체는상담원공개답이면상담원AI답만이면AI없으면미응답이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("처리 주체 상점")
	admin := tm.tn.token

	m0 := h.metrics(admin)
	if !isNull(m0, "ai_rate") || m0["ai_active"] != false || !isNull(m0, "response_rate") || !isNull(m0, "satisfaction", "average") {
		t.Errorf("데이터가 없으면 비율은 null 이다(0 이 아니다): %+v", m0)
	}

	// 상담원이 답했다
	c1, conv1 := h.widgetCustomer(tm.tn)
	h.send(conv1, c1, "문의 1", false)
	h.send(conv1, tm.staff.token, "답 1", false)
	h.closeConv(conv1, tm.staff.token)
	// AI 만 답했다
	c2, conv2 := h.widgetCustomer(tm.tn)
	h.send(conv2, c2, "문의 2", false)
	h.aiReply(conv2, "AI 답 2")
	h.closeConv(conv2, admin)
	// AI 가 먼저, 상담원이 나중 — 상담원 개입이다
	c3, conv3 := h.widgetCustomer(tm.tn)
	h.send(conv3, c3, "문의 3", false)
	h.aiReply(conv3, "AI 답 3")
	h.send(conv3, tm.staff.token, "상담원 답 3", false)
	h.closeConv(conv3, admin)
	// 사람·AI 답이 없다 — 내부 메모만
	c4, conv4 := h.widgetCustomer(tm.tn)
	h.send(conv4, c4, "문의 4", false)
	h.send(conv4, admin, "메모", true)
	h.closeConv(conv4, admin)
	// 열려 있는 상담은 세지 않는다
	c5, conv5 := h.widgetCustomer(tm.tn)
	h.send(conv5, c5, "문의 5", false)

	m := h.metrics(admin)
	closed, _ := m["closed"].(map[string]any)
	if num(closed, "total") != 4 || num(closed, "agent") != 2 || num(closed, "ai") != 1 || num(closed, "unanswered") != 1 {
		t.Errorf("종료 수와 처리 주체: %+v", closed)
	}
	if got := num(m, "ai_rate"); math.Abs(got-1.0/3) > 1e-9 {
		t.Errorf("AI 처리율 = AI / (AI + 상담원) = 1/3: %v", got)
	}
	if m["ai_active"] != true {
		t.Error("AI 응대가 있었다")
	}
	staff := responder(m, "AGENT", tm.staff.id)
	if staff == nil || num(staff, "handled") != 2 {
		t.Errorf("상담원별 처리 상담(마지막 답한 상담원): %+v", staff)
	}
	if ai := responder(m, "AI", ""); ai == nil || num(ai, "handled") != 1 {
		t.Errorf("AI 처리 상담: %+v", ai)
	}

	t.Run("다시 열었다 닫으면 마지막 종료의 처리 주체다", func(t *testing.T) {
		h.reopenConv(conv2, admin)
		mid := h.metrics(admin)
		if c, _ := mid["closed"].(map[string]any); num(c, "total") != 3 || num(c, "ai") != 0 {
			t.Errorf("다시 열린 상담은 종료 상담이 아니다: %+v", c)
		}
		h.send(conv2, tm.staff.token, "상담원이 이어 받았습니다", false)
		h.closeConv(conv2, admin)
		after := h.metrics(admin)
		c, _ := after["closed"].(map[string]any)
		if num(c, "total") != 4 || num(c, "agent") != 3 || num(c, "ai") != 0 {
			t.Errorf("마지막 종료 기준 — 한 번 세고 상담원 개입: %+v", c)
		}
		if !isNull(after, "ai_rate") && num(after, "ai_rate") != 0 {
			t.Errorf("AI 처리 0건: %v", after["ai_rate"])
		}
	})

	t.Run("가린 상담은 지표에 들지 않는다", func(t *testing.T) {
		before := h.metrics(admin)
		if code, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+conv1, admin, map[string]any{"reason": "테스트 정리"}); code != http.StatusNoContent {
			t.Fatalf("삭제: %d", code)
		}
		after := h.metrics(admin)
		b, _ := before["closed"].(map[string]any)
		a, _ := after["closed"].(map[string]any)
		if num(a, "total") != num(b, "total")-1 || num(after, "response", "count") != num(before, "response", "count")-1 {
			t.Errorf("가린 상담의 종료·응답이 빠져야 한다: %+v → %+v", before, after)
		}
	})
}

func TestMetrics_지표는남의업무기록을보는권한이본다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("지표 권한 상점")
	if code, _ := h.call(http.MethodGet, "/api/agent/metrics", tm.partTime.token, nil); code != http.StatusForbidden {
		t.Errorf("아르바이트(work.view_all 없음)는 403: %d", code)
	}
	if code, _ := h.call(http.MethodGet, "/api/agent/metrics", tm.staff.token, nil); code != http.StatusOK {
		t.Errorf("직원은 본다: %d", code)
	}
	cust, _ := h.widgetCustomer(tm.tn)
	if code, _ := h.call(http.MethodGet, "/api/agent/metrics", cust, nil); code != http.StatusForbidden {
		t.Errorf("고객은 403: %d", code)
	}
	for _, q := range []string{"from=어제", "from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z", "from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		if code, _ := h.call(http.MethodGet, "/api/agent/metrics?"+q, tm.staff.token, nil); code != http.StatusBadRequest {
			t.Errorf("잘못된 기간 %q 는 400: %d", q, code)
		}
	}
	// 기간 상한은 366일이다(감사 20 B05 — 700일로 늘려도 통과했다). 366일은 되고 1초라도 넘으면 400 이다.
	from := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		to   time.Time
		want int
	}{
		{from.Add(366 * 24 * time.Hour), http.StatusOK},
		{from.Add(366*24*time.Hour + time.Second), http.StatusBadRequest},
		{from.Add(367 * 24 * time.Hour), http.StatusBadRequest},
	} {
		q := "from=" + from.Format(time.RFC3339) + "&to=" + tc.to.Format(time.RFC3339)
		if code, _ := h.call(http.MethodGet, "/api/agent/metrics?"+q, tm.staff.token, nil); code != tc.want {
			t.Errorf("기간 %s: %d, 기대 %d", tc.to.Sub(from), code, tc.want)
		}
	}
}

func TestRating_만족도는그상담의고객만종료뒤에낸다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("만족도 상점")
	admin := tm.tn.token
	cust, convID := h.widgetCustomer(tm.tn)
	other, otherConv := h.widgetCustomer(tm.tn)
	h.send(convID, cust, "문의", false)
	h.send(convID, tm.staff.token, "답", false)
	path := "/api/conversations/" + convID + "/rating"

	t.Run("진행 중인 상담에는 낼 수 없다", func(t *testing.T) {
		if v := h.get(path, cust); v["closed"] != false || v["can_rate"] != false || v["rating"] != nil {
			t.Errorf("진행 중: %+v", v)
		}
		if code, body := h.call(http.MethodPut, path, cust, map[string]any{"score": 5}); code != http.StatusConflict || errorCodeOf(body) != "not_closed" {
			t.Errorf("진행 중에 내면 409 not_closed: %d %+v", code, body)
		}
	})

	h.closeConv(convID, tm.staff.token)
	h.closeConv(otherConv, admin)

	t.Run("남의 상담에는 낼 수 없고 상담원도 낼 수 없다", func(t *testing.T) {
		if code, _ := h.call(http.MethodPut, path, other, map[string]any{"score": 1}); code != http.StatusForbidden {
			t.Errorf("다른 고객이 남의 상담 id 로 내면 403: %d", code)
		}
		if code, _ := h.call(http.MethodGet, path, other, nil); code != http.StatusForbidden {
			t.Errorf("다른 고객은 남의 평가를 못 본다: %d", code)
		}
		if code, _ := h.call(http.MethodPut, path, admin, map[string]any{"score": 5}); code != http.StatusForbidden {
			t.Errorf("상담원은 낼 수 없다: %d", code)
		}
		foreign := h.teamOf("다른 상점")
		stranger, _ := h.widgetCustomer(foreign.tn)
		if code, _ := h.call(http.MethodPut, path, stranger, map[string]any{"score": 1}); code != http.StatusNotFound {
			t.Errorf("다른 워크스페이스의 고객에게는 없는 상담이다(I11): %d", code)
		}
		if v := h.get(path, admin); v["rating"] != nil {
			t.Errorf("거절된 평가가 남았다: %+v", v)
		}
	})

	t.Run("별점은 1~5, 한 줄 의견은 200자까지다", func(t *testing.T) {
		for _, body := range []map[string]any{{"score": 0}, {"score": 6}, {"score": 3, "comment": strings.Repeat("가", 201)}} {
			if code, _ := h.call(http.MethodPut, path, cust, body); code != http.StatusBadRequest {
				t.Errorf("%v 는 400: %d", body["score"], code)
			}
		}
		long := strings.Repeat("가", 200)
		if code, _ := h.call(http.MethodPut, path, cust, map[string]any{"score": 3, "comment": long}); code != http.StatusOK {
			t.Errorf("200자는 된다: %d", code)
		}
	})

	t.Run("종료 뒤 24시간 안에는 고치고 마지막 값이 남는다", func(t *testing.T) {
		out := h.put(path, cust, map[string]any{"score": 4, "comment": "  빨리\n답해 주셨어요  "})
		r, _ := out["rating"].(map[string]any)
		if num(r, "score") != 4 || r["comment"] != "빨리 답해 주셨어요" || out["current"] != true || out["can_rate"] != true {
			t.Errorf("고친 값: %+v", out)
		}
		if _, leaked := r["handler"]; leaked {
			t.Errorf("고객 응답에 처리 주체가 실리면 안 된다: %+v", r)
		}
		agentView := h.get(path, admin)
		ar, _ := agentView["rating"].(map[string]any)
		if ar["handler"] != "AGENT" || ar["agent_id"] != tm.staff.id || num(ar, "score") != 4 {
			t.Errorf("상담원은 평가와 처리 주체·담당 상담원을 본다: %+v", agentView)
		}
	})

	t.Run("24시간이 지나면 고칠 수 없다", func(t *testing.T) {
		testsupport.Exec(t, testsupport.URL(),
			"UPDATE conversation_closures SET closed_at = closed_at - interval '24 hours 1 second' WHERE conversation_id = $1", convID)
		if v := h.get(path, cust); v["can_rate"] != false {
			t.Errorf("지난 뒤에는 낼 수 없다고 보여야 한다: %+v", v)
		}
		if code, body := h.call(http.MethodPut, path, cust, map[string]any{"score": 1}); code != http.StatusConflict || errorCodeOf(body) != "rating_closed" {
			t.Errorf("24시간 뒤 수정은 409 rating_closed: %d %+v", code, body)
		}
		r, _ := h.get(path, admin)["rating"].(map[string]any)
		if num(r, "score") != 4 {
			t.Errorf("거절된 수정이 값을 바꿨다: %+v", r)
		}
	})

	t.Run("다시 열었다 닫으면 다시 받고 마지막 값이 남는다", func(t *testing.T) {
		h.reopenConv(convID, admin)
		if code, _ := h.call(http.MethodPut, path, cust, map[string]any{"score": 2}); code != http.StatusConflict {
			t.Errorf("다시 열린 상담은 조사를 받지 않는다: %d", code)
		}
		h.closeConv(convID, admin)
		v := h.get(path, cust)
		if v["can_rate"] != true || v["current"] != false {
			t.Errorf("새 종료 — 앞 평가는 지난 종료의 것이고 다시 낼 수 있다: %+v", v)
		}
		out := h.put(path, cust, map[string]any{"score": 2, "comment": "다시 열려서 불편했어요"})
		if r, _ := out["rating"].(map[string]any); num(r, "score") != 2 || out["current"] != true {
			t.Errorf("마지막 값: %+v", out)
		}
		m := h.metrics(admin)
		if num(m, "satisfaction", "responses") != 1 || num(m, "satisfaction", "average") != 2 {
			t.Errorf("상담당 한 번 — 마지막 값만 센다: %+v", m["satisfaction"])
		}
	})
}

func TestRating_만족도집계는평균CSAT응답수응답률이고주체와상담원별로나뉜다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("만족도 집계 상점")
	admin := tm.tn.token
	closeWith := func(reply func(convID string), score int) {
		cust, convID := h.widgetCustomer(tm.tn)
		h.send(convID, cust, "문의", false)
		reply(convID)
		h.closeConv(convID, admin)
		if score > 0 {
			h.put("/api/conversations/"+convID+"/rating", cust, map[string]any{"score": score})
		}
	}
	staffReply := func(convID string) { h.send(convID, tm.staff.token, "상담원 답", false) }
	aiReply := func(convID string) { h.aiReply(convID, "AI 답") }
	none := func(string) {}

	closeWith(staffReply, 5)
	closeWith(staffReply, 3)
	closeWith(staffReply, 0) // 조사를 받았지만 내지 않았다
	closeWith(aiReply, 4)
	closeWith(none, 1)

	m := h.metrics(admin)
	s, _ := m["satisfaction"].(map[string]any)
	if num(s, "responses") != 4 || math.Abs(num(s, "average")-13.0/4) > 1e-9 || math.Abs(num(s, "csat")-0.5) > 1e-9 {
		t.Errorf("응답 4, 평균 3.25, CSAT(4·5점) 2/4: %+v", s)
	}
	if math.Abs(num(m, "response_rate")-4.0/5) > 1e-9 {
		t.Errorf("응답률 = 낸 수 / 종료 상담 수 = 4/5: %v", m["response_rate"])
	}
	staff := responder(m, "AGENT", tm.staff.id)
	ss, _ := staff["satisfaction"].(map[string]any)
	if num(ss, "responses") != 2 || num(ss, "average") != 4 || num(ss, "csat") != 0.5 {
		t.Errorf("상담원별 만족도: %+v", staff)
	}
	ai := responder(m, "AI", "")
	as, _ := ai["satisfaction"].(map[string]any)
	if num(as, "responses") != 1 || num(as, "average") != 4 || num(as, "csat") != 1 {
		t.Errorf("AI 만족도: %+v", ai)
	}
}

func TestRating_고객을지우면의견은지우고점수는남긴다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("만족도 삭제 상점")
	admin := tm.tn.token
	cust, convID := h.widgetCustomer(tm.tn)
	h.send(convID, cust, "문의", false)
	h.send(convID, admin, "답", false)
	h.closeConv(convID, admin)
	h.put("/api/conversations/"+convID+"/rating", cust, map[string]any{"score": 5, "comment": "김철수 010-1234-5678 친절해요"})
	custID, _ := h.get("/api/conversations/"+convID, admin)["customer_id"].(string)
	if code := h.status(http.MethodDelete, "/api/customers/"+custID, admin, nil); code != http.StatusNoContent {
		t.Fatalf("고객 삭제: %d", code)
	}
	r, _ := h.get("/api/conversations/"+convID+"/rating", admin)["rating"].(map[string]any)
	if r["comment"] != "" || num(r, "score") != 5 {
		t.Errorf("의견은 지우고 점수는 남긴다: %+v", r)
	}
}

// 감사 20(logcat 결정): 다시 열었다 닫은 상담의 지난 평가가 지금 종료의 응답 수·응답률과 주체별 줄에 섞였다. 지난 평가는 앞 종료의
// 처리 주체·담당 상담원을 옮겨 적은 것이라, 섞이면 그 상담원이 받지 않은 점수가 그 상담원 줄에 든다. 지표는 그 종료에 낸 평가만 센다.
func TestRating_지표는그종료에낸평가만센다_다시열고닫은뒤새평가가없으면그종료는평가없음이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("지난 평가 상점")
	admin := tm.tn.token
	cust, convID := h.widgetCustomer(tm.tn)
	path := "/api/conversations/" + convID + "/rating"
	h.send(convID, cust, "문의", false)
	h.closeConv(convID, admin) // 답 없이 닫았다 — 미응답 종료
	h.put(path, cust, map[string]any{"score": 1, "comment": "답이 없었어요"})
	if m := h.metrics(admin); num(m, "satisfaction", "responses") != 1 {
		t.Fatalf("첫 종료의 평가: %+v", m["satisfaction"])
	}

	h.reopenConv(convID, admin)
	h.send(convID, tm.staff.token, "늦었지만 답합니다", false)
	h.closeConv(convID, admin) // 이번 종료는 상담원 처리다. 고객은 아직 평가하지 않았다

	m := h.metrics(admin)
	if num(m, "satisfaction", "responses") != 0 || !isNull(m, "satisfaction", "average") {
		t.Errorf("지난 종료의 평가를 지금 종료의 응답으로 셌다: %+v", m["satisfaction"])
	}
	if num(m, "response_rate") != 0 {
		t.Errorf("응답률 = 그 종료에 낸 평가 / 종료 상담 = 0/1: %v", m["response_rate"])
	}
	if ss, _ := responder(m, "AGENT", tm.staff.id)["satisfaction"].(map[string]any); num(ss, "responses") != 0 {
		t.Errorf("상담원이 받지 않은 지난 평가(1점)가 그 상담원 줄에 들었다: %+v", ss)
	}
	// 데스크는 지난 평가를 그대로 보인다(지난 종료의 평가 표시)
	if v := h.get(path, admin); v["current"] != false || v["rating"] == nil {
		t.Errorf("상담원 화면의 지난 평가: %+v", v)
	}

	h.put(path, cust, map[string]any{"score": 4})
	m = h.metrics(admin)
	if num(m, "satisfaction", "responses") != 1 || num(m, "satisfaction", "average") != 4 || num(m, "response_rate") != 1 {
		t.Errorf("이번 종료에 낸 평가는 센다: %+v rate=%v", m["satisfaction"], m["response_rate"])
	}
	if ss, _ := responder(m, "AGENT", tm.staff.id)["satisfaction"].(map[string]any); num(ss, "responses") != 1 || num(ss, "average") != 4 {
		t.Errorf("상담원 줄: %+v", ss)
	}
}

// DEF-148: 기간의 끝(to)을 주지 않은 업무 집계와 상담 지표는 위쪽 끝을 앱 시계가 아니라 DB 시계로 잡는다. 세는 기록의 시각은 DB 가 적는다 —
// DB 시계가 앱보다 앞서면(Docker Desktop) 앱 시계로 잡은 끝보다 방금 남긴 종료 기록이 늦어 창 밖이 됐다(관리자·직원의 차가 0, 한 번 실패).
// 격리 DB 에서 DB 시계만 10분 앞세운다: 검색 경로 맨 앞에 clock_timestamp()·now() 를 덮는 스키마를 두고, 업무 기록의 기본값도 그 시계로 바꾼다.
func TestE2E_DB시계가앱시계보다앞서도기간의끝을주지않은집계와지표는방금남긴종료를센다(t *testing.T) {
	_, dbURL := testsupport.Isolated(t)
	db, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(db.Path, "/")
	testsupport.Exec(t, dbURL, `CREATE SCHEMA skew`)
	testsupport.Exec(t, dbURL, `CREATE FUNCTION skew.clock_timestamp() RETURNS timestamptz LANGUAGE sql AS 'SELECT pg_catalog.clock_timestamp() + interval ''10 minutes'''`)
	testsupport.Exec(t, dbURL, `CREATE FUNCTION skew.now() RETURNS timestamptz LANGUAGE sql AS 'SELECT pg_catalog.now() + interval ''10 minutes'''`)
	testsupport.Exec(t, dbURL, `ALTER TABLE conversation_events ALTER COLUMN created_at SET DEFAULT skew.clock_timestamp()`)
	testsupport.Exec(t, dbURL, `ALTER DATABASE "`+name+`" SET search_path = skew, pg_catalog, public`)
	ctx := context.Background()
	store, err := pgstore.Open(ctx, dbURL) // 새 연결이라 위 검색 경로를 쓴다
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if dbNow, err := store.Now(ctx); err != nil || time.Until(dbNow) < 9*time.Minute {
		t.Fatalf("DB 시계를 앞세우지 못했다: %v (%v)", dbNow, err)
	}

	h := newHarnessOn(t, store, time.Second, false)
	admin := h.adminToken()
	_, convID := h.customerSession()
	h.closeConv(convID, admin)

	var closed float64
	for _, raw := range firstList(h.get("/api/agent/work/summary", admin), "agents") {
		row, _ := raw.(map[string]any)
		closed += num(row, "closed")
	}
	if closed != 1 {
		t.Errorf("to 를 주지 않은 업무 집계의 종료가 %v 건이다. 방금 닫은 1건이어야 한다", closed)
	}
	m := h.get("/api/agent/metrics", admin)
	if c, _ := m["closed"].(map[string]any); num(c, "total") != 1 {
		t.Errorf("to 를 주지 않은 지표의 종료가 %v 건이다. 방금 닫은 1건이어야 한다: %v", num(c, "total"), m["closed"])
	}
	// to 를 준 조회는 준 값 그대로다 — 앱 시계의 지금을 끝으로 주면 DB 시계로 10분 뒤의 기록은 세지 않는다.
	to := url.QueryEscape(time.Now().UTC().Format(time.RFC3339))
	closed = 0
	for _, raw := range firstList(h.get("/api/agent/work/summary?to="+to, admin), "agents") {
		row, _ := raw.(map[string]any)
		closed += num(row, "closed")
	}
	if closed != 0 {
		t.Errorf("to 를 준 업무 집계가 그 뒤의 종료를 셌다: %v", closed)
	}
}
