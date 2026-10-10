package httpapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 관리 화면 셋(D36): 상담원 이름, 빠른 답변 고치기, 위젯 FAQ.
// 모두 격리 DB 의 기본 워크스페이스에서 돈다. FAQ 의 "한 번도 저장하지 않음" 과 승인 대기 상한은 워크스페이스 전역 상태라
// 공유 DB 에서는 앞선 실행의 행에 흔들린다.

func defaultTenant(h *harness) tenant {
	return tenant{token: h.adminToken(), wsID: core.DefaultWorkspaceID}
}

func errorMessageOf(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

func TestE2E_상담원이름(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	logs, log := captureLogs(t)
	h := buildHarness(t, harnessConfig{store: store, catchup: time.Second, log: log})
	tn := defaultTenant(h)
	partTime := h.invited(tn, "PART_TIME")

	t.Run("상담원은 권한 없이 자기 이름을 바꾸고 앞뒤 공백을 떼며 빈 이름과 40자를 넘는 이름은 400 이다", func(t *testing.T) {
		status, body := h.call(http.MethodPost, "/api/agent/name", partTime.token, map[string]any{"name": "  김바뀜  "})
		if status != http.StatusOK || body["name"] != "김바뀜" || body["id"] != partTime.id {
			t.Fatalf("아르바이트도 자기 이름을 바꾼다(200, 공백 정리): %d %v", status, body)
		}
		if got := h.get("/api/me", partTime.token)["name"]; got != "김바뀜" {
			t.Errorf("세션 정보의 이름이 바뀌어야 한다: %v", got)
		}
		forty := strings.Repeat("가", 40)
		if status, body := h.call(http.MethodPost, "/api/agent/name", partTime.token, map[string]any{"name": forty}); status != http.StatusOK || body["name"] != forty {
			t.Errorf("40자는 받는다: %d %v", status, body)
		}
		for _, bad := range []string{"", "   ", strings.Repeat("가", 41)} {
			if status, body := h.call(http.MethodPost, "/api/agent/name", partTime.token, map[string]any{"name": bad}); status != http.StatusBadRequest || errorCodeOf(body) != "invalid" {
				t.Errorf("%q 는 400 invalid 여야 한다: %d %v", bad, status, body)
			}
		}
		if got := h.get("/api/me", partTime.token)["name"]; got != forty {
			t.Errorf("거절된 요청은 이름을 바꾸지 않는다: %v", got)
		}
		cust, _ := h.customerSession()
		if status := h.status(http.MethodPost, "/api/agent/name", cust, map[string]any{"name": "고객이 바꾼 이름"}); status != http.StatusForbidden {
			t.Errorf("고객은 상담원 이름 입구를 쓰지 못한다: %d", status)
		}
	})

	t.Run("고객이 상담원 전용 경로를 부르면 403 forbidden 이고 문구는 상담원만 할 수 있다 이다", func(t *testing.T) {
		// 남의 대화가 아니라 상담원이 아닌 것이다. "이 대화에 접근할 수 없다" 로 답하면 고칠 곳을 잘못 짚는다.
		cust, _ := h.customerSession()
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/api/agent/name", map[string]any{"name": "고객이 바꾼 이름"}},
			{http.MethodPost, "/api/agent/password", map[string]any{"current": "x", "next": "member12345"}},
			{http.MethodPost, "/api/admin/agents/" + partTime.id + "/name", map[string]any{"name": "고객"}},
			{http.MethodGet, "/api/admin/team", nil},
			{http.MethodGet, "/api/admin/widget-faq", nil},
			{http.MethodPut, "/api/admin/widget-faq", map[string]any{"items": []map[string]any{}}},
			{http.MethodGet, "/api/agent/snippets", nil},
			{http.MethodGet, "/api/agent/conversations", nil},
		}
		for _, c := range cases {
			status, body := h.call(c.method, c.path, cust, c.body)
			if status != http.StatusForbidden || errorCodeOf(body) != "forbidden" || errorMessageOf(body) != "상담원만 할 수 있습니다" {
				t.Errorf("%s %s: 403 forbidden \"상담원만 할 수 있습니다\" 여야 한다: %d %v", c.method, c.path, status, body)
			}
		}
	})

	t.Run("팀 관리 권한은 다른 상담원의 이름을 바꾸고 승인 대기 계정과 없는 상담원은 404 다", func(t *testing.T) {
		target := h.invited(tn, "STAFF")
		status, body := h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/name", tn.token, map[string]any{"name": " 이직원 "})
		if status != http.StatusOK || body["name"] != "이직원" {
			t.Fatalf("관리자가 다른 상담원의 이름을 바꾼다: %d %v", status, body)
		}
		var found bool
		for _, raw := range firstList(h.get("/api/admin/team", tn.token), "members") {
			m, _ := raw.(map[string]any)
			if m["id"] == target.id {
				found = m["name"] == "이직원"
			}
		}
		if !found {
			t.Error("팀 목록에 바뀐 이름이 보여야 한다")
		}
		if got := h.get("/api/me", target.token)["name"]; got != "이직원" {
			t.Errorf("바뀐 사람의 세션도 새 이름이다: %v", got)
		}
		pendingID := h.pendingSignup(tn, tn.token)
		if status, body := h.call(http.MethodPost, "/api/admin/agents/"+pendingID+"/name", tn.token, map[string]any{"name": "대기자"}); status != http.StatusNotFound {
			t.Errorf("승인 대기 계정은 404: %d %v", status, body)
		}
		if status, body := h.call(http.MethodPost, "/api/admin/agents/"+id.New()+"/name", tn.token, map[string]any{"name": "없는 사람"}); status != http.StatusNotFound {
			t.Errorf("없는 상담원은 404: %d %v", status, body)
		}
		if status, body := h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/name", tn.token, map[string]any{"name": ""}); status != http.StatusBadRequest {
			t.Errorf("관리자도 빈 이름은 400: %d %v", status, body)
		}
	})

	t.Run("이름을 바꾸면 감사 로그에 agent.name 이 바꾸기 전과 뒤의 이름과 함께 남고 운영 로그에는 이름이 없다", func(t *testing.T) {
		target := h.invited(tn, "STAFF") // 초대 이름은 "STAFF 상담원"
		newName := "감사추적" + id.New()[30:]
		h.post("/api/admin/agents/"+target.id+"/name", tn.token, map[string]any{"name": newName})
		var entry map[string]any
		for _, raw := range firstList(h.get("/api/admin/audit", tn.token), "entries") {
			e, _ := raw.(map[string]any)
			if e["action"] == "agent.name" && e["target"] == target.id {
				entry = e
				break
			}
		}
		if entry == nil {
			t.Fatal("감사 로그에 agent.name 이 없다")
		}
		detail, _ := entry["detail"].(map[string]any)
		if detail["from"] != "STAFF 상담원" || detail["to"] != newName {
			t.Errorf("detail 은 바꾸기 전과 뒤의 이름이다: %v", detail)
		}
		selfName := "본인감사" + id.New()[30:]
		h.post("/api/agent/name", partTime.token, map[string]any{"name": selfName})
		var self bool
		for _, raw := range firstList(h.get("/api/admin/audit", tn.token), "entries") {
			e, _ := raw.(map[string]any)
			d, _ := e["detail"].(map[string]any)
			if e["action"] == "agent.name" && e["target"] == partTime.id && d["to"] == selfName {
				self = true
			}
		}
		if !self {
			t.Error("자기 이름 바꾸기도 감사 로그에 남는다")
		}
		for _, name := range []string{newName, selfName} {
			if strings.Contains(logs.String(), name) {
				t.Errorf("운영 로그에 이름 %q 가 있다(D30)", name)
			}
		}
	})
}

func TestE2E_빠른답변고치기(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := defaultTenant(h)
	create := func(shortcut string) string {
		sn := h.post("/api/agent/snippets", tn.token, map[string]any{"shortcut": shortcut, "title": "제목 " + shortcut, "body": "본문 " + shortcut})
		return sn["id"].(string)
	}
	snippetByID := func(snippetID string) map[string]any {
		for _, raw := range firstList(h.get("/api/agent/snippets", tn.token), "snippets") {
			s, _ := raw.(map[string]any)
			if s["id"] == snippetID {
				return s
			}
		}
		return nil
	}

	t.Run("빠른 답변을 고치면 단축어·제목·본문이 바뀌고 만들 때와 같은 규칙으로 검증하며 없는 빠른 답변은 404 다", func(t *testing.T) {
		snID := create("before")
		status, body := h.call(http.MethodPut, "/api/agent/snippets/"+snID, tn.token, map[string]any{"shortcut": " /after ", "title": " 새 제목 ", "body": " 새 본문 "})
		if status != http.StatusOK || body["id"] != snID || body["shortcut"] != "after" || body["title"] != "새 제목" || body["body"] != "새 본문" {
			t.Fatalf("200 과 다듬은 세 칸이어야 한다: %d %v", status, body)
		}
		if got := snippetByID(snID); got["shortcut"] != "after" || got["title"] != "새 제목" || got["body"] != "새 본문" {
			t.Errorf("목록에도 바뀐 값이 보인다: %v", got)
		}
		// 만들기와 같은 문구, 같은 순서다.
		cases := []struct {
			in   map[string]any
			want string
		}{
			{map[string]any{"shortcut": "", "title": "", "body": ""}, "단축어가 비어 있습니다"},
			{map[string]any{"shortcut": "a", "title": " ", "body": ""}, "제목이 비어 있습니다"},
			{map[string]any{"shortcut": "a", "title": "t", "body": "  "}, "본문이 비어 있습니다"},
			{map[string]any{"shortcut": strings.Repeat("s", 41), "title": "t", "body": "b"}, "단축어은 40자까지입니다"},
			{map[string]any{"shortcut": "a", "title": strings.Repeat("가", 81), "body": "b"}, "제목은 80자까지입니다"},
			{map[string]any{"shortcut": "a", "title": "t", "body": strings.Repeat("가", 4001)}, "본문은 4000자까지입니다"},
		}
		for _, c := range cases {
			status, body := h.call(http.MethodPut, "/api/agent/snippets/"+snID, tn.token, c.in)
			if status != http.StatusBadRequest || errorMessageOf(body) != c.want {
				t.Errorf("%v: 400 %q 여야 한다: %d %v", c.in, c.want, status, body)
			}
			created, cbody := h.call(http.MethodPost, "/api/agent/snippets", tn.token, c.in)
			if created != http.StatusBadRequest || errorMessageOf(cbody) != c.want {
				t.Errorf("만들기도 같은 규칙이어야 한다 %v: %d %v", c.in, created, cbody)
			}
		}
		if got := snippetByID(snID); got["shortcut"] != "after" {
			t.Errorf("거절된 고치기는 아무것도 바꾸지 않는다: %v", got)
		}
		if status := h.status(http.MethodPut, "/api/agent/snippets/"+id.New(), tn.token, map[string]any{"shortcut": "x", "title": "t", "body": "b"}); status != http.StatusNotFound {
			t.Errorf("없는 빠른 답변은 404: %d", status)
		}
		if status := h.status(http.MethodPut, "/api/agent/snippets/not-a-uuid", tn.token, map[string]any{"shortcut": "x", "title": "t", "body": "b"}); status != http.StatusBadRequest {
			t.Errorf("id 형식이 틀리면 400: %d", status)
		}
	})

	t.Run("다른 빠른 답변의 단축어로 고치면 대소문자가 달라도 409 conflict 이고 아무것도 바뀌지 않는다", func(t *testing.T) {
		create("taken")
		snID := create("mine")
		status, body := h.call(http.MethodPut, "/api/agent/snippets/"+snID, tn.token, map[string]any{"shortcut": "TAKEN", "title": "바뀔 제목", "body": "바뀔 본문"})
		if status != http.StatusConflict || errorCodeOf(body) != "conflict" {
			t.Fatalf("409 conflict 여야 한다: %d %v", status, body)
		}
		if got := snippetByID(snID); got["shortcut"] != "mine" || got["title"] != "제목 mine" || got["body"] != "본문 mine" {
			t.Errorf("겹친 고치기는 아무것도 바꾸지 않는다: %v", got)
		}
		// 자기 단축어를 그대로 두거나 대소문자만 바꾸는 건 겹침이 아니다.
		if status, body := h.call(http.MethodPut, "/api/agent/snippets/"+snID, tn.token, map[string]any{"shortcut": "MINE", "title": "t", "body": "b"}); status != http.StatusOK {
			t.Errorf("자기 단축어의 대소문자만 바꾸면 200: %d %v", status, body)
		}
	})
}

func TestE2E_위젯FAQ(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := defaultTenant(h)
	faqOf := func(m map[string]any, key string) []map[string]any {
		var out []map[string]any
		for _, raw := range firstList(m, key) {
			it, _ := raw.(map[string]any)
			out = append(out, it)
		}
		return out
	}
	items := func(n int, prefix string) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"question": fmt.Sprintf("%s 질문 %d", prefix, i+1), "answer": fmt.Sprintf("%s 답 %d", prefix, i+1)}
		}
		return out
	}

	t.Run("위젯 FAQ 를 한 번도 저장하지 않았으면 위젯 설정과 관리 화면은 기본 FAQ 세 항목을 준다", func(t *testing.T) {
		want := []string{"배송은 얼마나 걸리나요?", "교환·반품은 어떻게 하나요?", "주문 내역은 어디서 보나요?"}
		wantAnswer := "결제 확인 후 영업일 기준 2~3일 안에 받아보실 수 있습니다."
		for _, got := range [][]map[string]any{faqOf(h.get("/api/widget/settings", ""), "faq"), faqOf(h.get("/api/admin/widget-faq", tn.token), "items")} {
			if len(got) != 3 {
				t.Fatalf("기본 FAQ 는 세 항목이다: %v", got)
			}
			for i, q := range want {
				if got[i]["question"] != q {
					t.Errorf("%d번째 질문: %v, 기대 %q", i+1, got[i]["question"], q)
				}
			}
			if got[0]["answer"] != wantAnswer {
				t.Errorf("첫 답: %v", got[0]["answer"])
			}
		}
		if h.get("/api/admin/widget-faq", tn.token)["is_default"] != true {
			t.Error("관리 화면은 기본값을 보여 주는 중임을 안다(is_default)")
		}
	})

	t.Run("저장한 위젯 FAQ 는 입력 순서 그대로 위젯 설정에 실리고 빈 목록을 저장하면 FAQ 가 없다", func(t *testing.T) {
		saved := h.put("/api/admin/widget-faq", tn.token, map[string]any{"items": []map[string]any{
			{"question": "  셋째로 적은 것  ", "answer": "  C  "}, {"question": "첫째", "answer": "A"}, {"question": "둘째", "answer": "B"},
		}})
		if saved["is_default"] != false {
			t.Errorf("저장하면 기본값이 아니다: %v", saved)
		}
		got := faqOf(h.get("/api/widget/settings", ""), "faq")
		wantQ := []string{"셋째로 적은 것", "첫째", "둘째"}
		wantA := []string{"C", "A", "B"}
		if len(got) != 3 {
			t.Fatalf("세 항목이어야 한다: %v", got)
		}
		for i := range wantQ {
			if got[i]["question"] != wantQ[i] || got[i]["answer"] != wantA[i] {
				t.Errorf("%d번째: %v, 기대 %s/%s", i+1, got[i], wantQ[i], wantA[i])
			}
		}
		h.put("/api/admin/widget-faq", tn.token, map[string]any{"items": []map[string]any{}})
		pub := h.get("/api/widget/settings", "")
		list, ok := pub["faq"].([]any)
		if !ok || len(list) != 0 {
			t.Errorf("빈 목록을 저장하면 faq 는 빈 배열이다(기본값으로 돌아가지 않는다): %v", pub["faq"])
		}
		if admin := h.get("/api/admin/widget-faq", tn.token); admin["is_default"] != false || len(firstList(admin, "items")) != 0 {
			t.Errorf("관리 화면도 빈 목록이다: %v", admin)
		}
	})

	t.Run("위젯 FAQ 는 10개까지이고 질문은 1~80자, 답은 1~500자이며 앞뒤 공백을 떼고 센다", func(t *testing.T) {
		if status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": items(10, "열")}); status != http.StatusOK {
			t.Fatalf("10개는 받는다: %d %v", status, body)
		}
		if status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": items(11, "열하나")}); status != http.StatusBadRequest || errorMessageOf(body) != "자주 묻는 질문은 10개까지입니다" {
			t.Errorf("11개는 400: %d %v", status, body)
		}
		edge := []map[string]any{{"question": " " + strings.Repeat("질", 80) + " ", "answer": " " + strings.Repeat("답", 500) + " "}}
		if status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": edge}); status != http.StatusOK {
			t.Fatalf("질문 80자·답 500자는 공백을 떼고 받는다: %d %v", status, body)
		}
		got := faqOf(h.get("/api/admin/widget-faq", tn.token), "items")
		if len(got) != 1 || got[0]["question"] != strings.Repeat("질", 80) || got[0]["answer"] != strings.Repeat("답", 500) {
			t.Errorf("앞뒤 공백을 떼고 저장한다: %v", got)
		}
		cases := []struct {
			item map[string]any
			want string
		}{
			{map[string]any{"question": strings.Repeat("질", 81), "answer": "a"}, "1번째 질문은 80자까지입니다"},
			{map[string]any{"question": "q", "answer": strings.Repeat("답", 501)}, "1번째 답은 500자까지입니다"},
			{map[string]any{"question": "   ", "answer": "a"}, "1번째 질문이 비어 있습니다"},
			{map[string]any{"question": "q", "answer": "\n\t "}, "1번째 답이 비어 있습니다"},
		}
		for _, c := range cases {
			status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": []map[string]any{c.item}})
			if status != http.StatusBadRequest || errorCodeOf(body) != "invalid" || errorMessageOf(body) != c.want {
				t.Errorf("%v: 400 invalid %q 여야 한다: %d %v", c.item, c.want, status, body)
			}
		}
	})

	t.Run("위젯 FAQ 가 여러 곳 틀리면 입력 순서로 첫 오류를 말하고 아무것도 저장하지 않는다", func(t *testing.T) {
		h.put("/api/admin/widget-faq", tn.token, map[string]any{"items": items(2, "그대로")})
		bad := []map[string]any{
			{"question": "멀쩡", "answer": "멀쩡"},
			{"question": "둘째", "answer": ""},
			{"question": "", "answer": strings.Repeat("답", 501)},
		}
		for range 3 {
			status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": bad})
			if status != http.StatusBadRequest || errorMessageOf(body) != "2번째 답이 비어 있습니다" {
				t.Fatalf("첫 오류(2번째 답)를 말해야 한다: %d %v", status, body)
			}
		}
		// 한 항목 안에서는 질문을 답보다 먼저 본다.
		if _, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": []map[string]any{{"question": "", "answer": ""}}}); errorMessageOf(body) != "1번째 질문이 비어 있습니다" {
			t.Errorf("질문이 먼저다: %v", body)
		}
		// 개수 초과는 항목보다 먼저 본다.
		over := append(items(10, "넘침"), map[string]any{"question": "", "answer": ""})
		over[0] = map[string]any{"question": "", "answer": ""}
		if _, body := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, map[string]any{"items": over}); errorMessageOf(body) != "자주 묻는 질문은 10개까지입니다" {
			t.Errorf("개수 초과가 첫 오류다: %v", body)
		}
		got := faqOf(h.get("/api/widget/settings", ""), "faq")
		if len(got) != 2 || got[0]["question"] != "그대로 질문 1" {
			t.Errorf("거절된 저장은 아무것도 바꾸지 않는다: %v", got)
		}
	})

	t.Run("위젯 FAQ 저장에 items 가 없거나 null 이면 400 invalid 이고 목록을 지우지 않는다 — 빈 배열만 FAQ 없음이다", func(t *testing.T) {
		h.put("/api/admin/widget-faq", tn.token, map[string]any{"items": items(2, "남는다")})
		for _, body := range []map[string]any{{}, {"items": nil}} {
			status, out := h.call(http.MethodPut, "/api/admin/widget-faq", tn.token, body)
			if status != http.StatusBadRequest || errorCodeOf(out) != "invalid" || errorMessageOf(out) != "items 가 필요합니다. FAQ 를 두지 않으려면 빈 배열을 보내 주세요" {
				t.Errorf("%v: 400 invalid 여야 한다: %d %v", body, status, out)
			}
		}
		got := faqOf(h.get("/api/widget/settings", ""), "faq")
		if len(got) != 2 || got[0]["question"] != "남는다 질문 1" {
			t.Errorf("items 가 없는 저장은 목록을 바꾸지 않는다: %v", got)
		}
	})

	t.Run("위젯 FAQ 를 저장하면 감사 로그에 widget_faq.update 가 항목 수와 함께 남는다", func(t *testing.T) {
		h.put("/api/admin/widget-faq", tn.token, map[string]any{"items": items(4, "감사")})
		entries := firstList(h.get("/api/admin/audit", tn.token), "entries")
		if len(entries) == 0 {
			t.Fatal("감사 로그가 비었다")
		}
		latest, _ := entries[0].(map[string]any)
		detail, _ := latest["detail"].(map[string]any)
		if latest["action"] != "widget_faq.update" || detail["items"] != float64(4) {
			t.Errorf("가장 최근 감사 로그는 widget_faq.update, items=4 여야 한다: %v", latest)
		}
	})
}
