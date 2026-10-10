package httpapi_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func TestE2E_운영기능(t *testing.T) {
	h := newHarness(t, time.Second, true)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()
	me := h.get("/api/me", agentToken)
	agentID, _ := me["agent_id"].(string)

	h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
		"client_msg_id": id.New(), "body": "환불 받고 싶어요",
	})

	t.Run("담당자를 나로 지정하면 팔로워도 된다", func(t *testing.T) {
		conv := h.post("/api/agent/conversations/"+convID+"/assign", agentToken, map[string]any{"agent_id": "me"})
		if conv["assignee_agent_id"] != agentID {
			t.Fatalf("담당자가 안 잡혔다: %v", conv["assignee_agent_id"])
		}
		out := h.get("/api/agent/conversations/"+convID+"/followers", agentToken)
		list, _ := out["followers"].([]any)
		if len(list) != 1 {
			t.Errorf("담당자는 자동으로 팔로워가 된다: %+v", list)
		}
	})

	t.Run("담당자 변경은 내부 기록으로만 남는다", func(t *testing.T) {
		msgs := h.get("/api/conversations/"+convID+"/messages?after_seq=0", custToken)
		list, _ := msgs["messages"].([]any)
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			if body, _ := m["body"].(string); contains(body, "담당자") {
				t.Fatalf("담당 변경이 고객에게 보인다: %q", body)
			}
		}
	})

	t.Run("태그를 붙이고 뗀다", func(t *testing.T) {
		out := h.post("/api/agent/conversations/"+convID+"/tags", agentToken, map[string]any{"name": "환불"})
		tags, _ := out["tags"].([]any)
		if len(tags) != 1 {
			t.Fatalf("태그가 안 붙었다: %+v", out)
		}
		first, _ := tags[0].(map[string]any)
		tagID, _ := first["id"].(string)

		// 같은 이름을 다시 붙여도 하나다.
		out = h.post("/api/agent/conversations/"+convID+"/tags", agentToken, map[string]any{"name": "환불"})
		if tags, _ := out["tags"].([]any); len(tags) != 1 {
			t.Errorf("같은 태그가 두 번 붙었다: %+v", tags)
		}

		res := h.raw(http.MethodDelete, "/api/agent/conversations/"+convID+"/tags/"+tagID, agentToken)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("태그 제거가 %d", res.StatusCode)
		}
		conv := h.get("/api/conversations/"+convID, agentToken)
		if list, _ := conv["tags"].([]any); len(list) != 0 {
			t.Errorf("태그가 남았다: %+v", list)
		}
	})

	t.Run("받은함을 담당자와 태그로 거른다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/tags", agentToken, map[string]any{"name": "우선"})

		mine := h.get("/api/agent/conversations?assignee=me", agentToken)
		if list, _ := mine["conversations"].([]any); len(list) == 0 {
			t.Error("내가 맡은 대화가 보여야 한다")
		}
		none := h.get("/api/agent/conversations?assignee=none", agentToken)
		for _, raw := range firstList(none, "conversations") {
			m, _ := raw.(map[string]any)
			if m["id"] == convID {
				t.Error("담당자가 있는 대화가 미할당 목록에 나왔다")
			}
		}
		following := h.get("/api/agent/conversations?following=1", agentToken)
		if list, _ := following["conversations"].([]any); len(list) == 0 {
			t.Error("팔로우한 대화가 보여야 한다")
		}
	})

	t.Run("보류했다가 되돌린다", func(t *testing.T) {
		conv := h.post("/api/agent/conversations/"+convID+"/state", agentToken,
			map[string]any{"state": "SNOOZED", "snooze_minutes": 30})
		if conv["state"] != "SNOOZED" {
			t.Fatalf("SNOOZED 여야 한다: %v", conv["state"])
		}
		if conv["snoozed_until"] == nil {
			t.Error("보류 해제 시각이 있어야 한다")
		}
		conv = h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "OPEN"})
		if conv["state"] != "OPEN" || conv["snoozed_until"] != nil {
			t.Errorf("되돌리면 보류 시각이 지워져야 한다: %+v", conv)
		}
	})

	t.Run("보류는 미래 시각이 필요하다", func(t *testing.T) {
		res := h.postRaw("/api/agent/conversations/"+convID+"/state", agentToken,
			map[string]any{"state": "SNOOZED", "snooze_minutes": -5})
		defer res.Body.Close()
		// 음수는 기본값 60분으로 올라간다. 정책은 "과거 시각을 거절한다" 이므로 성공이 맞다.
		if res.StatusCode != http.StatusOK {
			t.Errorf("기본 보류 시간으로 받아야 한다: %d", res.StatusCode)
		}
	})

	t.Run("검색은 본문으로 찾는다", func(t *testing.T) {
		out := h.get("/api/agent/search?q=환불", agentToken)
		hits, _ := out["hits"].([]any)
		if len(hits) == 0 {
			t.Fatal("검색 결과가 없다")
		}
		first, _ := hits[0].(map[string]any)
		if first["conversation_id"] != convID {
			t.Errorf("다른 대화를 찾았다: %v", first)
		}
	})

	t.Run("고객은 검색도 받은함도 못 쓴다", func(t *testing.T) {
		for _, path := range []string{"/api/agent/search?q=환불", "/api/agent/conversations", "/api/agent/snippets"} {
			res := h.raw(http.MethodGet, path, custToken)
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s 는 403 이어야 한다: %d", path, res.StatusCode)
			}
		}
	})

	t.Run("빠른 답변을 만들고 지운다", func(t *testing.T) {
		created := h.post("/api/agent/snippets", agentToken, map[string]any{
			"shortcut": "환불안내", "title": "환불 절차", "body": "환불은 영업일 기준 3일 걸립니다.",
		})
		snippetID, _ := created["id"].(string)
		if snippetID == "" {
			t.Fatalf("생성 결과가 비었다: %+v", created)
		}
		dup := h.postRaw("/api/agent/snippets", agentToken, map[string]any{
			"shortcut": "환불안내", "title": "중복", "body": "중복",
		})
		dup.Body.Close()
		if dup.StatusCode != http.StatusConflict {
			t.Errorf("같은 단축어는 409 여야 한다: %d", dup.StatusCode)
		}
		res := h.raw(http.MethodDelete, "/api/agent/snippets/"+snippetID, agentToken)
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Errorf("204 여야 한다: %d", res.StatusCode)
		}
	})

	t.Run("고객 프로필을 상담원이 채운다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, agentToken)
		customerID, _ := conv["customer_id"].(string)
		out := h.post("/api/customers/"+customerID, agentToken, map[string]any{
			"display_name": "김고객", "email": "kim@example.com", "phone": "010-0000-0000",
			"note": "VIP. 재구매 많음", "attributes": map[string]any{"plan": "pro"},
		})
		if out["email"] != "kim@example.com" || out["note"] != "VIP. 재구매 많음" {
			t.Fatalf("프로필이 안 저장됐다: %+v", out)
		}
		attrs, _ := out["attributes"].(map[string]any)
		if attrs["plan"] != "pro" {
			t.Errorf("속성이 안 저장됐다: %+v", attrs)
		}
	})

	t.Run("고객은 자기 운영 메모를 못 바꾼다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, agentToken)
		customerID, _ := conv["customer_id"].(string)
		out := h.post("/api/customers/"+customerID, custToken, map[string]any{
			"display_name": "직접 고친 이름", "note": "내가 쓴 메모",
		})
		if out["display_name"] != "직접 고친 이름" {
			t.Errorf("이름은 바꿀 수 있어야 한다: %v", out["display_name"])
		}
		if _, leaked := out["note"]; leaked {
			t.Error("고객 응답에 운영 메모가 들어 있다")
		}
		// 메모가 그대로인지는 상담원 쪽에서 확인한다.
		seen := h.get("/api/customers/"+customerID, agentToken)
		if seen["note"] != "VIP. 재구매 많음" {
			t.Errorf("운영 메모는 그대로여야 한다: %v", seen["note"])
		}
	})

	t.Run("고객은 남의 프로필을 못 본다", func(t *testing.T) {
		otherToken, otherConv := h.customerSession()
		conv := h.get("/api/conversations/"+otherConv, otherToken)
		otherCustomer, _ := conv["customer_id"].(string)
		res := h.raw(http.MethodGet, "/api/customers/"+otherCustomer, custToken)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})
}

func TestE2E_여러대화(t *testing.T) {
	h := newHarness(t, time.Second, true)
	custToken, firstConv := h.customerSession()

	t.Run("새 문의를 열면 대화가 하나 더 생긴다", func(t *testing.T) {
		out := h.post("/api/customer/conversations", custToken, nil)
		conv, _ := out["conversation"].(map[string]any)
		secondID, _ := conv["id"].(string)
		if secondID == "" || secondID == firstConv {
			t.Fatalf("새 대화가 안 생겼다: %+v", out)
		}
		list := h.get("/api/customer/conversations", custToken)
		if convs, _ := list["conversations"].([]any); len(convs) != 2 {
			t.Errorf("대화 목록이 둘이어야 한다: %d개", len(convs))
		}
	})

	t.Run("각 대화의 번호는 따로 센다", func(t *testing.T) {
		list := h.get("/api/customer/conversations", custToken)
		convs, _ := list["conversations"].([]any)
		for _, raw := range convs {
			c, _ := raw.(map[string]any)
			if seq, _ := c["last_seq"].(float64); int(seq) != 1 {
				t.Errorf("새 대화는 안내 문구 1건으로 시작한다: %v", seq)
			}
		}
	})
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 상담원은 "지금 내가 봐야 할 것이 몇 건인가" 를 숫자로 볼 수 있어야 한다.
func TestE2E_배정과받은함건수(t *testing.T) {
	h := newHarness(t, time.Second, true)

	custToken, convID := h.customerSession()
	first := h.agentToken()
	second := h.agentToken()
	meFirst := h.get("/api/me", first)
	meSecond := h.get("/api/me", second)
	firstID, _ := meFirst["agent_id"].(string)
	secondID, _ := meSecond["agent_id"].(string)

	h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
		"client_msg_id": id.New(), "body": "확인 부탁드립니다",
	})

	counts := func(t *testing.T, token string) map[string]float64 {
		t.Helper()
		raw := h.get("/api/agent/inbox/counts", token)
		out := map[string]float64{}
		for k, v := range raw {
			if f, ok := v.(float64); ok {
				out[k] = f
			}
		}
		return out
	}

	t.Run("아무도 안 맡으면 미할당으로 센다", func(t *testing.T) {
		c := counts(t, first)
		if c["unassigned"] < 1 {
			t.Errorf("미할당이 1 이상이어야 한다: %+v", c)
		}
		if c["mine"] != 0 || c["todo"] != 0 {
			t.Errorf("아직 내 것이 아니다: %+v", c)
		}
	})

	t.Run("내게 배정되면 답할 차례로 잡힌다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", first, map[string]any{"agent_id": "me"})
		c := counts(t, first)
		if c["mine"] < 1 {
			t.Errorf("내 담당이 1 이상이어야 한다: %+v", c)
		}
		if c["todo"] < 1 {
			t.Errorf("안 읽었으니 답할 차례여야 한다: %+v", c)
		}
	})

	t.Run("읽기만 해서는 답할 차례에서 빠지지 않는다 — 고객은 여전히 기다린다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, first)
		lastSeq, _ := conv["last_seq"].(float64)
		h.post("/api/conversations/"+convID+"/read", first, map[string]any{"seq": int64(lastSeq)})
		if c := counts(t, first); c["todo"] != 1 {
			t.Errorf("읽어도 답을 안 했으면 1 이어야 한다: %+v", c)
		}
	})

	t.Run("내부 메모는 답이 아니다", func(t *testing.T) {
		h.post("/api/conversations/"+convID+"/messages", first, map[string]any{"client_msg_id": id.New(), "body": "메모", "internal": true})
		if c := counts(t, first); c["todo"] != 1 {
			t.Errorf("내부 메모 뒤에도 1 이어야 한다: %+v", c)
		}
	})

	t.Run("공개 답변을 보내면 빠지고, 고객이 다시 말하면 돌아온다", func(t *testing.T) {
		h.post("/api/conversations/"+convID+"/messages", first, map[string]any{"client_msg_id": id.New(), "body": "확인했습니다"})
		c := counts(t, first)
		if c["todo"] != 0 {
			t.Errorf("답했으면 0 이어야 한다: %+v", c)
		}
		if c["mine"] < 1 {
			t.Errorf("담당은 그대로다: %+v", c)
		}
		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{"client_msg_id": id.New(), "body": "하나 더요"})
		if c := counts(t, first); c["todo"] != 1 {
			t.Errorf("고객이 다시 말하면 1: %+v", c)
		}
		waiting := h.get("/api/agent/conversations?assignee=me&waiting=1", first)
		if n := len(firstList(waiting, "conversations")); n != 1 {
			t.Errorf("답할 차례 목록에도 보여야 한다: %d", n)
		}
		// 고객이 기다리는 채로 둔다. 다음 단계가 이관받은 쪽의 답할 차례를 본다.
	})

	t.Run("다른 상담원에게 이관하면 양쪽 숫자가 같이 움직인다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", first, map[string]any{"agent_id": secondID})
		conv := h.get("/api/conversations/"+convID, first)
		if conv["assignee_agent_id"] != secondID {
			t.Fatalf("이관이 안 됐다: %v", conv["assignee_agent_id"])
		}
		if c := counts(t, first); c["mine"] != 0 {
			t.Errorf("넘긴 쪽의 내 담당은 0 이어야 한다: %+v", c)
		}
		c := counts(t, second)
		if c["mine"] < 1 || c["todo"] < 1 {
			t.Errorf("받은 쪽이 답할 차례로 가져가야 한다: %+v", c)
		}
	})

	t.Run("해제하면 다시 미할당이 된다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", second, map[string]any{"agent_id": ""})
		conv := h.get("/api/conversations/"+convID, second)
		if conv["assignee_agent_id"] != nil && conv["assignee_agent_id"] != "" {
			t.Fatalf("해제가 안 됐다: %v", conv["assignee_agent_id"])
		}
		if c := counts(t, second); c["mine"] != 0 {
			t.Errorf("해제 뒤에는 0 이어야 한다: %+v", c)
		}
	})

	t.Run("답할 차례 목록은 답을 기다리는 내 담당만 준다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", first, map[string]any{"agent_id": "me"})
		h.post("/api/conversations/"+convID+"/messages", first, map[string]any{"client_msg_id": id.New(), "body": "답변했습니다"})
		out := h.get("/api/agent/conversations?assignee=me&waiting=1", first)
		list, _ := out["conversations"].([]any)
		found := false
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			if m["id"] == convID {
				found = true
			}
		}
		if found {
			t.Error("답한 대화는 답할 차례가 아니다")
		}

		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
			"client_msg_id": id.New(), "body": "아직 답이 없네요",
		})
		out = h.get("/api/agent/conversations?assignee=me&waiting=1", first)
		list, _ = out["conversations"].([]any)
		found = false
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			if m["id"] == convID {
				found = true
			}
		}
		if !found {
			t.Error("새 메시지가 오면 답할 차례로 올라와야 한다")
		}
		_ = firstID
	})

	t.Run("고객은 받은함 건수를 볼 수 없다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/agent/inbox/counts", custToken)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})
}

// 보류 자동 해제(1분 주기)는 삭제한 상담을 건드리지 않는다. 같은 주기에 삭제하지 않은 보류 상담은 열린다.
// 실제 경로로 재현한다: 상담원이 보류하고, 관리자가 삭제하고, 보류 시각이 지난 뒤 운영 조립이 거는 것과 같은 해제 주기가 돈다.
func TestE2E_보류자동해제는삭제한상담을다시열지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("보류 삭제 상점")
	_, deleted := h.namedCustomer(tm.tn, "지운고객")
	_, live := h.namedCustomer(tm.tn, "산고객")
	for _, convID := range []string{deleted, live} {
		if code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", tm.admin.token,
			map[string]any{"state": "SNOOZED", "snooze_minutes": 30}); code != http.StatusOK || out["state"] != "SNOOZED" {
			t.Fatalf("보류: %d %v", code, out)
		}
	}
	if code, out := h.call(http.MethodDelete, "/api/agent/conversations/"+deleted, tm.admin.token, map[string]any{"reason": "보류 중 삭제"}); code >= 300 {
		t.Fatalf("삭제: %d %v", code, out)
	}
	// 30분을 기다리는 대신 보류 시각을 지난 시각으로 옮긴다.
	for _, convID := range []string{deleted, live} {
		testsupport.Exec(t, testsupport.URL(), `UPDATE conversations SET snoozed_until = now() - interval '1 minute' WHERE id = $1`, convID)
	}

	app.NewMaintenance(h.svc.Commands, h.store, nil).WakeSnoozed(context.Background())

	if code, out := h.call(http.MethodGet, "/api/conversations/"+live, tm.admin.token, nil); code != http.StatusOK || out["state"] != "OPEN" {
		t.Fatalf("삭제하지 않은 보류 상담은 같은 주기에 열려야 한다: %d %v", code, out["state"])
	}
	code, out := h.call(http.MethodGet, "/api/agent/conversations/"+deleted+"/deleted", tm.admin.token, nil)
	if code != http.StatusOK {
		t.Fatalf("삭제한 상담 보기: %d %v", code, out)
	}
	conv, _ := out["conversation"].(map[string]any)
	if conv["state"] != "SNOOZED" {
		t.Errorf("삭제한 상담이 보류 해제로 다시 열렸다: %v", conv["state"])
	}
	if ids := h.inboxIDs(tm.admin.token, "state=OPEN"); slices.Contains(ids, deleted) {
		t.Errorf("삭제한 상담이 받은함에 나왔다: %v", ids)
	}
}

// 시각은 어느 응답이든 한 형식(RFC 3339)이다. 화면이 필드마다 다른 해석기를 두지 않게 한다.
func TestE2E_시각형식(t *testing.T) {
	h := newHarness(t, time.Second, false)
	custToken, convID := h.customerSession()
	agentToken := h.agentToken()
	u := id.New()
	word := "시각형식" + u[len(u)-12:]
	msg := h.send(convID, custToken, word+" 문의", false)
	rfc3339 := func(t *testing.T, where string, v any) {
		t.Helper()
		raw, _ := v.(string)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || time.Since(at) > time.Hour || time.Until(at) > time.Minute {
			t.Errorf("%s 의 created_at 이 RFC 3339 의 지금 시각이 아니다: %q", where, raw)
		}
	}
	rfc3339(t, "메시지", msg["created_at"])

	t.Run("고객 프로필의 created_at 은 다른 응답과 같은 RFC 3339 다", func(t *testing.T) {
		customerID, _ := h.get("/api/conversations/"+convID, agentToken)["customer_id"].(string)
		rfc3339(t, "고객 프로필", h.get("/api/customers/"+customerID, agentToken)["created_at"])
	})

	t.Run("검색 결과의 created_at 은 다른 응답과 같은 RFC 3339 다", func(t *testing.T) {
		hits := firstList(h.get("/api/agent/search?q="+word, agentToken), "hits")
		if len(hits) != 1 {
			t.Fatalf("검색 결과가 하나여야 한다: %v", hits)
		}
		hit, _ := hits[0].(map[string]any)
		rfc3339(t, "검색 결과", hit["created_at"])
	})
}

// ── 답할 차례 목록·상태 변경 원자성·가져가기 경합 (D39) ─────────────────────

// 종료한 상담은 답할 차례 목록에 없고, 목록 건수와 배지 수(todo)가 같다. 둘은 저장소의 한 조건을 쓴다.
// 실사용에서 종료한 상담이 목록에만 쌓여 배지(0)와 목록(수십 건)이 어긋났다. 100건 넘게 종료한 뒤에도 같아야 한다.
func TestE2E_종료한상담은답할차례목록에없고목록건수와배지수가같다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("답할 차례 상점")
	const closedN, openN = 105, 3

	var wg sync.WaitGroup
	jobs := make(chan bool)
	var failed atomic.Int32
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for closeIt := range jobs {
				ctx := context.Background()
				custID, _, err := h.svc.Commands.StartCustomerSession(ctx, tm.tn.wsID, "", "기다리는고객")
				if err != nil {
					failed.Add(1)
					continue
				}
				cust, err := h.svc.Commands.IssueCustomerSession(ctx, custID)
				if err != nil {
					failed.Add(1)
					continue
				}
				code, started := h.call(http.MethodPost, "/api/customer/conversations", cust, nil)
				conv, _ := started["conversation"].(map[string]any)
				convID, _ := conv["id"].(string)
				if code != http.StatusOK && code != http.StatusCreated || convID == "" {
					failed.Add(1)
					continue
				}
				steps := []struct {
					path, token string
					body        map[string]any
				}{
					{"/api/conversations/" + convID + "/messages", cust, map[string]any{"client_msg_id": id.New(), "body": "답을 기다립니다"}},
					{"/api/agent/conversations/" + convID + "/assign", tm.staff.token, map[string]any{"agent_id": "me"}},
				}
				if closeIt {
					steps = append(steps, struct {
						path, token string
						body        map[string]any
					}{"/api/agent/conversations/" + convID + "/state", tm.staff.token, map[string]any{"state": "CLOSED"}})
				}
				for _, s := range steps {
					if code, out := h.call(http.MethodPost, s.path, s.token, s.body); code >= 300 {
						t.Errorf("%s: %d %v", s.path, code, out)
						failed.Add(1)
					}
				}
			}
		}()
	}
	for i := 0; i < closedN+openN; i++ {
		jobs <- i < closedN
	}
	close(jobs)
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("준비 중 %d 건 실패", failed.Load())
	}

	list := firstList(h.get("/api/agent/conversations?assignee=me&waiting=1&limit=200", tm.staff.token), "conversations")
	for _, raw := range list {
		if m, _ := raw.(map[string]any); m["state"] == "CLOSED" {
			t.Fatalf("종료한 상담이 답할 차례 목록에 있다: %v", m["id"])
		}
	}
	counts := h.get("/api/agent/inbox/counts", tm.staff.token)
	todo, _ := counts["todo"].(float64)
	if len(list) != openN || int(todo) != openN {
		t.Fatalf("답할 차례 목록 %d 건과 배지 %v 가 둘 다 %d 여야 한다", len(list), counts["todo"], openN)
	}
}

// eventKinds 는 그 대화의 업무 기록 종류를 남긴 순서대로 준다.
func (h *harness) eventKinds(convID string) []string {
	h.t.Helper()
	rows, err := h.store.Pool().Query(context.Background(),
		"SELECT kind FROM conversation_events WHERE conversation_id = $1 ORDER BY id", convID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// systemNotes 는 그 대화의 시스템 줄 가운데 needle 이 든 것의 수다.
func (h *harness) systemNotes(convID, needle string) int {
	h.t.Helper()
	var n int
	if err := h.store.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM messages WHERE conversation_id = $1 AND sender_kind = 'SYSTEM' AND body LIKE '%' || $2 || '%'",
		convID, needle).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) stateOf(convID string) string {
	h.t.Helper()
	var s string
	if err := h.store.Pool().QueryRow(context.Background(), "SELECT state FROM conversations WHERE id = $1", convID).Scan(&s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

// 상태 변경·시스템 줄·업무 기록은 한 트랜잭션이다. 업무 기록이 실패하면 종료도 시스템 줄도 남지 않는다.
// 실사용에서 종료는 커밋됐는데 "종료했습니다" 줄과 closed 기록이 빠진 상담이 나왔다.
func TestE2E_업무기록이실패하면상태변경도되돌린다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("원자성 상점")
	cust, convID := h.namedCustomer(tm.tn, "원자성고객")
	h.send(convID, cust, "문의합니다", false)

	fn := "test_fail_event_" + strings.ReplaceAll(id.New(), "-", "")
	testsupport.Exec(t, testsupport.URL(), `CREATE FUNCTION `+fn+`() RETURNS trigger AS $$
		BEGIN
		  IF NEW.conversation_id = '`+convID+`'::uuid THEN RAISE EXCEPTION 'test: 업무 기록 실패'; END IF;
		  RETURN NEW;
		END $$ LANGUAGE plpgsql`)
	testsupport.Exec(t, testsupport.URL(), `CREATE TRIGGER `+fn+` BEFORE INSERT ON conversation_events FOR EACH ROW EXECUTE FUNCTION `+fn+`()`)
	t.Cleanup(func() {
		testsupport.Exec(t, testsupport.URL(), `DROP TRIGGER IF EXISTS `+fn+` ON conversation_events`)
		testsupport.Exec(t, testsupport.URL(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})

	code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", tm.staff.token, map[string]any{"state": "CLOSED"})
	if code < 500 {
		t.Errorf("기록이 실패했으면 성공으로 답하지 않는다: %d %v", code, out)
	}
	if s := h.stateOf(convID); s != "OPEN" {
		t.Errorf("상태가 되돌려져야 한다: %s", s)
	}
	if n := h.systemNotes(convID, "종료했습니다"); n != 0 {
		t.Errorf("시스템 줄도 되돌려져야 한다: %d 줄", n)
	}

	t.Run("담당 지정도 같다", func(t *testing.T) {
		code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", tm.staff.token,
			map[string]any{"agent_id": "me", "expected_assignee": ""})
		if code < 500 {
			t.Errorf("기록이 실패했으면 성공으로 답하지 않는다: %d %v", code, out)
		}
		var assignee *string
		if err := h.store.Pool().QueryRow(context.Background(),
			"SELECT assignee_agent_id::text FROM conversations WHERE id = $1", convID).Scan(&assignee); err != nil {
			t.Fatal(err)
		}
		if assignee != nil {
			t.Errorf("담당자가 되돌려져야 한다: %v", *assignee)
		}
		if n := h.systemNotes(convID, "지정했습니다"); n != 0 {
			t.Errorf("시스템 줄도 되돌려져야 한다: %d 줄", n)
		}
	})
}

// commitCutter 는 테스트 DB 앞의 TCP 중계기다. armed 동안 처음 지나가는 commit 의 응답을 클라이언트에 넘긴 직후
// 모든 연결을 끊고 새 연결도 받지 않는다. "커밋은 됐는데 그 뒤의 DB 읽기가 실패한다" 를 만든다.
type commitCutter struct {
	url   string
	armed atomic.Bool
	down  atomic.Bool
	mu    sync.Mutex
	conns []net.Conn
}

func (p *commitCutter) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}

func (p *commitCutter) cut() {
	p.down.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

func newCommitCutter(t *testing.T) *commitCutter {
	t.Helper()
	target, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &commitCutter{}
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
			p.track(client)
			p.track(server)
			var cutAfterReply atomic.Bool
			go func() {
				chunk := make([]byte, 64<<10)
				for {
					n, err := server.Read(chunk)
					if n > 0 {
						if _, werr := client.Write(chunk[:n]); werr != nil {
							return
						}
						if cutAfterReply.Load() {
							p.cut()
							return
						}
					}
					if err != nil {
						client.Close()
						return
					}
				}
			}()
			go func() {
				chunk := make([]byte, 64<<10)
				for {
					n, err := client.Read(chunk)
					if n > 0 {
						if p.armed.Load() && bytes.Contains(bytes.ToLower(chunk[:n]), []byte("commit")) {
							p.armed.Store(false)
							cutAfterReply.Store(true)
						}
						if _, werr := server.Write(chunk[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						server.Close()
						return
					}
				}
			}()
		}
	}()
	q := target.Query()
	q.Set("default_query_exec_mode", "simple_protocol")
	target.Host = ln.Addr().String()
	target.RawQuery = q.Encode()
	p.url = target.String()
	return p
}

// 커밋된 종료는 그 뒤에 DB 가 끊겨도 성공으로 답한다. 응답 본문은 트랜잭션 안에서 이미 아는 값으로 만든다.
// 실사용에서 커밋 뒤의 다시 읽기가 실패해 500 이 나갔고, 화면은 종료가 안 된 줄 알았다.
func TestE2E_상태변경이커밋된뒤DB가끊겨도성공으로답한다(t *testing.T) {
	testsupport.Store(t) // 스키마 준비
	cutter := newCommitCutter(t)
	store, err := pgstore.Open(context.Background(), cutter.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	h := newHarnessOn(t, store, time.Hour, false)
	tm := h.teamOf("끊긴 커밋 상점")
	cust, convID := h.namedCustomer(tm.tn, "끊긴커밋고객")
	h.send(convID, cust, "문의합니다", false)

	cutter.armed.Store(true)
	code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", tm.staff.token, map[string]any{"state": "CLOSED"})
	if !cutter.down.Load() {
		t.Fatalf("중계기가 커밋 뒤에 끊지 않았다 — 시험이 성립하지 않는다: %d %v", code, out)
	}
	if code != http.StatusOK || out["state"] != "CLOSED" || out["id"] != convID {
		t.Fatalf("커밋된 종료는 200 과 종료된 대화로 답해야 한다: %d %v", code, out)
	}

	direct := testsupport.Store(t)
	conv, err := direct.Conversation(context.Background(), convID)
	if err != nil || conv.State != "CLOSED" {
		t.Fatalf("종료가 커밋돼 있어야 한다: %v %v", conv.State, err)
	}
	if last, _ := out["last_seq"].(float64); int64(last) != conv.LastSeq {
		t.Errorf("응답의 last_seq %v 가 커밋된 값 %d 와 다르다", out["last_seq"], conv.LastSeq)
	}
	var notes, closed int
	if err := direct.Pool().QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM messages WHERE conversation_id = $1 AND sender_kind = 'SYSTEM' AND body LIKE '%종료했습니다%'),
		       (SELECT count(*) FROM conversation_events WHERE conversation_id = $1 AND kind = 'closed')`, convID).Scan(&notes, &closed); err != nil {
		t.Fatal(err)
	}
	if notes != 1 || closed != 1 {
		t.Errorf("시스템 줄 1, closed 기록 1 이어야 한다: %d %d", notes, closed)
	}
}

// 두 상담원이 동시에 "나에게" 를 누르면 한 사람만 가져가고 다른 사람은 409 를 받는다. 조용히 덮어쓰지 않는다.
// 실사용에서 경합 38건이 뒤에 누른 사람에게 조용히 넘어갔다. 가져가기는 미배정일 때만 되고 DB 가 그 조건을 지킨다.
func TestE2E_동시에가져가면한명만성공하고나머지는409다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("가져가기 경합 상점")
	const pairs = 20
	convs := make([]string, pairs)
	for i := range convs {
		cust, convID := h.namedCustomer(tm.tn, "경합고객")
		h.send(convID, cust, "누가 받나요", false)
		convs[i] = convID
	}
	takers := []member{tm.admin, tm.staff}

	type result struct {
		code int
		out  map[string]any
	}
	for _, convID := range convs {
		results := make([]result, len(takers))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, m := range takers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", m.token,
					map[string]any{"agent_id": "me", "expected_assignee": ""})
				results[i] = result{code, out}
			}()
		}
		close(start)
		wg.Wait()

		winner := ""
		for i, r := range results {
			switch r.code {
			case http.StatusOK:
				if winner != "" {
					t.Fatalf("%s: 두 사람이 다 성공했다", convID)
				}
				winner = takers[i].id
				if r.out["assignee_agent_id"] != winner {
					t.Errorf("성공 응답의 담당자가 가져간 사람이 아니다: %v", r.out["assignee_agent_id"])
				}
			case http.StatusConflict:
				e, _ := r.out["error"].(map[string]any)
				if e["code"] != "conflict" || e["message"] != "다른 상담원이 먼저 가져갔습니다" {
					t.Errorf("409 의 본문이 틀렸다: %v", r.out)
				}
			default:
				t.Fatalf("%s: 200 아니면 409 여야 한다: %d %v", convID, r.code, r.out)
			}
		}
		if winner == "" {
			t.Fatalf("%s: 아무도 못 가져갔다 %+v", convID, results)
		}
		var assignee string
		if err := h.store.Pool().QueryRow(context.Background(),
			"SELECT assignee_agent_id::text FROM conversations WHERE id = $1", convID).Scan(&assignee); err != nil {
			t.Fatal(err)
		}
		if assignee != winner {
			t.Errorf("%s: 담당자 %s 가 성공한 사람 %s 가 아니다", convID, assignee, winner)
		}
		if kinds := h.eventKinds(convID); slices.Contains(kinds, "transferred") || count(kinds, "assigned") != 1 {
			t.Errorf("%s: 가져가기는 assigned 한 건이다: %v", convID, kinds)
		}
		if n := h.systemNotes(convID, "지정했습니다"); n != 1 {
			t.Errorf("%s: 담당 지정 시스템 줄은 한 줄이다: %d", convID, n)
		}
	}
}

func count(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// 이관은 화면이 보던 담당자(expected_assignee)가 그 사이 바뀌었으면 409 다. 맞으면 이관하고 transferred 로 남긴다.
func TestE2E_이관은화면이본담당자가그사이바뀌었으면409다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("이관 상점")
	cust, convID := h.namedCustomer(tm.tn, "이관고객")
	h.send(convID, cust, "이관해 주세요", false)
	assign := func(m member, body map[string]any) (int, map[string]any) {
		return h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", m.token, body)
	}

	if code, out := assign(tm.staff, map[string]any{"agent_id": "me", "expected_assignee": ""}); code != http.StatusOK {
		t.Fatalf("가져가기: %d %v", code, out)
	}
	// 관리자의 화면은 직원이 맡은 것을 본다. 그 사이 직원이 아르바이트에게 넘긴다.
	if code, out := assign(tm.staff, map[string]any{"agent_id": tm.partTime.id, "expected_assignee": tm.staff.id}); code != http.StatusOK {
		t.Fatalf("직원의 이관: %d %v", code, out)
	}
	code, out := assign(tm.admin, map[string]any{"agent_id": "me", "expected_assignee": tm.staff.id})
	if code != http.StatusConflict || errorCodeOf(out) != "conflict" {
		t.Fatalf("보던 담당자가 바뀌었으면 409 conflict 다: %d %v", code, out)
	}
	if e, _ := out["error"].(map[string]any); e["message"] != "그 사이 담당자가 바뀌었습니다. 다시 확인해 주세요" {
		t.Errorf("문구: %v", out)
	}
	if got := h.get("/api/conversations/"+convID, tm.admin.token)["assignee_agent_id"]; got != tm.partTime.id {
		t.Fatalf("담당자는 아르바이트 그대로다: %v", got)
	}

	code, out = assign(tm.admin, map[string]any{"agent_id": "me", "expected_assignee": tm.partTime.id})
	if code != http.StatusOK || out["assignee_agent_id"] != tm.admin.id {
		t.Fatalf("보던 담당자가 맞으면 이관한다: %d %v", code, out)
	}
	if kinds := h.eventKinds(convID); !slices.Equal(kinds[len(kinds)-3:], []string{"assigned", "transferred", "transferred"}) {
		t.Errorf("가져가기는 assigned, 이관은 transferred 다: %v", kinds)
	}

	t.Run("expected_assignee 가 없으면 지금처럼 바로 바꾼다", func(t *testing.T) {
		if code, out := assign(tm.admin, map[string]any{"agent_id": tm.staff.id}); code != http.StatusOK || out["assignee_agent_id"] != tm.staff.id {
			t.Fatalf("%d %v", code, out)
		}
	})
	t.Run("expected_assignee 형식이 틀리면 400 이다", func(t *testing.T) {
		if code, out := assign(tm.admin, map[string]any{"agent_id": "me", "expected_assignee": "abc"}); code != http.StatusBadRequest {
			t.Fatalf("%d %v", code, out)
		}
	})
}

// takeBetween 은 권한 판정과 배정 쓰기 사이에 다른 상담원이 먼저 가져간 상황을 만든다. 첫 배정 쓰기 직전에 한 번 끼어든다.
type takeBetween struct {
	*pgstore.Store
	once   sync.Once
	convID string
	by     string
}

func (w *takeBetween) AssignConversation(ctx context.Context, convID, agentID string, expected *string, record core.ChangeRecorder) (core.Conversation, error) {
	if convID == w.convID {
		w.once.Do(func() { _, _ = w.Store.AssignConversation(ctx, convID, w.by, nil, nil) })
	}
	return w.Store.AssignConversation(ctx, convID, agentID, expected, record)
}

// 배정 권한이 없는 상담원은 expected_assignee 를 보내지 않아도 권한을 판정한 담당자를 조건으로 건다.
// 판정과 쓰기 사이에 남이 가져갔으면 덮어쓰지 않고 409 다 — 덮어쓰면 권한 없이 남의 상담을 가져온 것이다.
func TestE2E_응대권한만있는상담원은판정뒤남이가져간상담을덮어쓰지않는다(t *testing.T) {
	wrapped := &takeBetween{}
	h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { wrapped.Store = s; return wrapped })
	tm := h.teamOf("판정 뒤 경합 상점")
	cust, convID := h.namedCustomer(tm.tn, "판정고객")
	h.send(convID, cust, "누가 받나요", false)
	wrapped.convID, wrapped.by = convID, tm.staff.id

	code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", tm.partTime.token, map[string]any{"agent_id": "me"})
	if code != http.StatusConflict || errorCodeOf(out) != "conflict" {
		t.Fatalf("409 conflict 여야 한다: %d %v", code, out)
	}
	if got := h.get("/api/conversations/"+convID, tm.staff.token)["assignee_agent_id"]; got != tm.staff.id {
		t.Fatalf("먼저 가져간 직원이 담당자여야 한다: %v", got)
	}
}

// ── 같은 값이면 쓰지 않는다·담당자 목록의 조건·커밋 뒤 알림 (D39 보완, 감사 10) ─────────────────

// 잠근 행의 담당자가 이미 요청한 사람이면 expected_assignee 가 있든 없든, 화면이 늦게 봤든 바꾸지도 기록하지도 않고 200 이다.
// 해제를 요청했는데 이미 미배정인 것도 같다. 감사 10 에서 같은 사람이 "나에게" 를 네 번 누르자 assigned 가 세 건 남았다.
func TestE2E_같은담당자로다시배정하면바꾸지도기록하지도않고200이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("같은 담당자 상점")
	cust, convID := h.namedCustomer(tm.tn, "같은담당고객")
	h.send(convID, cust, "누가 받나요", false)
	assign := func(m member, body map[string]any) (int, map[string]any) {
		return h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", m.token, body)
	}
	updatedAt := func() time.Time {
		var at time.Time
		if err := h.store.Pool().QueryRow(context.Background(),
			"SELECT updated_at FROM conversations WHERE id = $1", convID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	if code, out := assign(tm.staff, map[string]any{"agent_id": "me", "expected_assignee": ""}); code != http.StatusOK {
		t.Fatalf("가져가기: %d %v", code, out)
	}
	since := updatedAt()
	again := []map[string]any{
		{"agent_id": "me"}, // expected 없음
		{"agent_id": "me", "expected_assignee": tm.staff.id},           // 지금 담당자를 봤다
		{"agent_id": "me", "expected_assignee": ""},                    // 미배정을 본 늦은 화면
		{"agent_id": tm.staff.id, "expected_assignee": tm.partTime.id}, // 엉뚱한 담당자를 본 화면
	}
	for _, body := range again {
		code, out := assign(tm.staff, body)
		if code != http.StatusOK || out["assignee_agent_id"] != tm.staff.id || out["id"] != convID {
			t.Fatalf("이미 담당자면 200 과 지금 대화다: %v → %d %v", body, code, out)
		}
	}
	if code, out := assign(tm.admin, map[string]any{"agent_id": tm.staff.id}); code != http.StatusOK || out["assignee_agent_id"] != tm.staff.id {
		t.Fatalf("관리자가 지금 담당자에게 다시 배정해도 200 이다: %d %v", code, out)
	}
	if kinds := h.eventKinds(convID); count(kinds, "assigned") != 1 || slices.Contains(kinds, "transferred") {
		t.Errorf("다시 눌러도 assigned 는 한 건이다: %v", kinds)
	}
	if n := h.systemNotes(convID, "지정했습니다"); n != 1 {
		t.Errorf("지정 시스템 줄은 한 줄이다: %d", n)
	}
	if !updatedAt().Equal(since) {
		t.Errorf("같은 담당자면 행을 쓰지 않는다: updated_at 이 바뀌었다")
	}

	t.Run("이미 미배정인데 해제를 요청해도 쓰지 않는다", func(t *testing.T) {
		// 미배정이면 응답에 assignee_agent_id 가 없다(omitempty).
		if code, out := assign(tm.admin, map[string]any{"agent_id": "", "expected_assignee": tm.staff.id}); code != http.StatusOK || out["id"] != convID || out["assignee_agent_id"] != nil {
			t.Fatalf("해제: %d %v", code, out)
		}
		since := updatedAt()
		for _, body := range []map[string]any{{"agent_id": ""}, {"agent_id": "", "expected_assignee": tm.staff.id}} {
			if code, out := assign(tm.admin, body); code != http.StatusOK || out["id"] != convID || out["assignee_agent_id"] != nil {
				t.Fatalf("이미 미배정이면 200 이다: %v → %d %v", body, code, out)
			}
		}
		if kinds := h.eventKinds(convID); count(kinds, "unassigned") != 1 {
			t.Errorf("unassigned 는 한 건이다: %v", kinds)
		}
		if n := h.systemNotes(convID, "해제했습니다"); n != 1 {
			t.Errorf("해제 시스템 줄은 한 줄이다: %d", n)
		}
		if !updatedAt().Equal(since) {
			t.Errorf("이미 미배정이면 행을 쓰지 않는다")
		}
	})

	t.Run("응대 권한만 있는 상담원이 두 번 눌러도 한 번이다", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if code, out := assign(tm.partTime, map[string]any{"agent_id": "me"}); code != http.StatusOK || out["assignee_agent_id"] != tm.partTime.id {
				t.Fatalf("%d 번째: %d %v", i+1, code, out)
			}
		}
		if kinds := h.eventKinds(convID); count(kinds, "assigned") != 2 {
			t.Errorf("assigned 는 직원 한 건, 아르바이트 한 건이다: %v", kinds)
		}
	})
}

// 잠근 행이 이미 요청한 상태면 아무것도 쓰지 않고 200 이다. 두 상담원이 동시에 "완료" 를 눌러도 closed 기록과 종료 줄은 한 번이다.
// 감사 10 에서 동시 완료가 둘 다 200 이고 closed 기록이 두 건 남았다.
func TestE2E_동시에두명이완료를눌러도종료기록은한건이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("동시 완료 상점")
	const pairs = 20
	closers := []member{tm.admin, tm.staff}
	for i := 0; i < pairs; i++ {
		cust, convID := h.namedCustomer(tm.tn, "동시완료고객")
		h.send(convID, cust, "끝났어요", false)
		codes := make([]int, len(closers))
		states := make([]any, len(closers))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j, m := range closers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", m.token, map[string]any{"state": "CLOSED"})
				codes[j], states[j] = code, out["state"]
			}()
		}
		close(start)
		wg.Wait()
		for j := range closers {
			if codes[j] != http.StatusOK || states[j] != "CLOSED" {
				t.Fatalf("%s: 둘 다 200 과 종료된 대화다: %v %v", convID, codes, states)
			}
		}
		if kinds := h.eventKinds(convID); count(kinds, "closed") != 1 {
			t.Fatalf("%s: closed 기록은 한 건이다: %v", convID, kinds)
		}
		if n := h.systemNotes(convID, "종료했습니다"); n != 1 {
			t.Fatalf("%s: 종료 시스템 줄은 한 줄이다: %d", convID, n)
		}
	}

	t.Run("이미 열린 상담을 다시 열어도 기록하지 않고 보류 시각을 바꾸는 보류는 기록한다", func(t *testing.T) {
		cust, convID := h.namedCustomer(tm.tn, "보류고객")
		h.send(convID, cust, "기다릴게요", false)
		state := func(body map[string]any) map[string]any {
			code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/state", tm.staff.token, body)
			if code != http.StatusOK {
				t.Fatalf("%v → %d %v", body, code, out)
			}
			return out
		}
		state(map[string]any{"state": "OPEN"})
		if kinds := h.eventKinds(convID); slices.Contains(kinds, "reopened") {
			t.Errorf("열린 상담을 다시 열면 기록하지 않는다: %v", kinds)
		}
		first := state(map[string]any{"state": "SNOOZED", "snooze_minutes": 30})
		second := state(map[string]any{"state": "SNOOZED", "snooze_minutes": 90})
		if first["snoozed_until"] == second["snoozed_until"] {
			t.Errorf("보류 시각이 바뀌어야 한다: %v %v", first["snoozed_until"], second["snoozed_until"])
		}
		if kinds := h.eventKinds(convID); count(kinds, "snoozed") != 2 {
			t.Errorf("보류 시각을 바꾼 보류도 기록한다: %v", kinds)
		}
	})
}

// 상태 필터 없이 부른 내 담당(assignee=me)·미할당(assignee=none) 목록은 배지(mine·unassigned)와 같은 조건이라 종료한 상담이 없다.
// 상태를 명시하면(state=CLOSED) 그 상태를 따른다. 감사 10 에서 배지 3 과 목록 113·43 이 어긋났다(대부분 종료).
func TestE2E_상태필터없는내담당과미할당목록은종료를빼고배지수와같다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("담당 목록 상점")
	const closedN, openN = 100, 3

	type job struct{ mine, closeIt bool }
	var wg sync.WaitGroup
	jobs := make(chan job)
	var failed atomic.Int32
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				ctx := context.Background()
				custID, _, err := h.svc.Commands.StartCustomerSession(ctx, tm.tn.wsID, "", "담당목록고객")
				if err != nil {
					failed.Add(1)
					continue
				}
				cust, err := h.svc.Commands.IssueCustomerSession(ctx, custID)
				if err != nil {
					failed.Add(1)
					continue
				}
				code, started := h.call(http.MethodPost, "/api/customer/conversations", cust, nil)
				conv, _ := started["conversation"].(map[string]any)
				convID, _ := conv["id"].(string)
				if code >= 300 || convID == "" {
					failed.Add(1)
					continue
				}
				var steps [][2]any
				if j.mine {
					steps = append(steps, [2]any{"/assign", map[string]any{"agent_id": "me"}})
				}
				if j.closeIt {
					steps = append(steps, [2]any{"/state", map[string]any{"state": "CLOSED"}})
				}
				for _, s := range steps {
					if code, out := h.call(http.MethodPost, "/api/agent/conversations/"+convID+s[0].(string), tm.staff.token, s[1]); code >= 300 {
						t.Errorf("%s: %d %v", s[0], code, out)
						failed.Add(1)
					}
				}
			}
		}()
	}
	for _, mine := range []bool{true, false} {
		for i := 0; i < closedN+openN; i++ {
			jobs <- job{mine: mine, closeIt: i < closedN}
		}
	}
	close(jobs)
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("준비 중 %d 건 실패", failed.Load())
	}

	counts := h.get("/api/agent/inbox/counts", tm.staff.token)
	for _, c := range []struct{ query, badge string }{{"assignee=me", "mine"}, {"assignee=none", "unassigned"}} {
		list := firstList(h.get("/api/agent/conversations?"+c.query+"&limit=500", tm.staff.token), "conversations")
		for _, raw := range list {
			if m, _ := raw.(map[string]any); m["state"] == "CLOSED" {
				t.Fatalf("%s: 상태 필터가 없으면 종료한 상담이 없다: %v", c.query, m["id"])
			}
		}
		badge, _ := counts[c.badge].(float64)
		if len(list) != openN || int(badge) != openN {
			t.Errorf("%s 목록 %d 건과 배지 %s=%v 가 둘 다 %d 여야 한다", c.query, len(list), c.badge, counts[c.badge], openN)
		}
		closed := firstList(h.get("/api/agent/conversations?"+c.query+"&state=CLOSED&limit=500", tm.staff.token), "conversations")
		if len(closed) != closedN {
			t.Errorf("%s&state=CLOSED 는 종료한 상담 %d 건이어야 한다: %d", c.query, closedN, len(closed))
		}
	}
}

// expected_assignee 의 "me" 는 요청한 상담원 자신이다. 내가 맡은 것을 넘길 때 화면이 나를 봤다고 "me" 로 보내면 되고,
// 그 사이 담당자가 바뀌었으면 409 다.
func TestE2E_보던담당자를me로보내면자기자신으로본다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("me 상점")
	cust, convID := h.namedCustomer(tm.tn, "me고객")
	h.send(convID, cust, "넘겨 주세요", false)
	assign := func(m member, body map[string]any) (int, map[string]any) {
		return h.call(http.MethodPost, "/api/agent/conversations/"+convID+"/assign", m.token, body)
	}
	if code, out := assign(tm.admin, map[string]any{"agent_id": "me", "expected_assignee": ""}); code != http.StatusOK {
		t.Fatalf("가져가기: %d %v", code, out)
	}
	if code, out := assign(tm.admin, map[string]any{"agent_id": tm.staff.id, "expected_assignee": "me"}); code != http.StatusOK || out["assignee_agent_id"] != tm.staff.id {
		t.Fatalf("내가 맡은 것을 me 로 보고 넘기면 바꾼다: %d %v", code, out)
	}
	code, out := assign(tm.admin, map[string]any{"agent_id": tm.partTime.id, "expected_assignee": "me"})
	if code != http.StatusConflict || errorCodeOf(out) != "conflict" {
		t.Fatalf("이제 내가 담당자가 아니면 409 conflict 다: %d %v", code, out)
	}
	if got := h.get("/api/conversations/"+convID, tm.admin.token)["assignee_agent_id"]; got != tm.staff.id {
		t.Fatalf("담당자는 직원 그대로다: %v", got)
	}
}
