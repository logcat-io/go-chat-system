package httpapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 권한 체계(D29). 역할은 셋이고, 서버는 역할이 아니라 권한을 묻는다. 화면이 숨기는 건 편의고 막는 건 서버다.

// deniedButApplied 는 거절 응답 뒤에 실제로 바뀌었는지를 상태 코드에 싣는다. 바꾼 뒤 응답을 만들 때 권한을 다시 물어
// 403 이 나가는 길이 있다. 상태 코드만 보면 문지기가 빠져도 통과한다. 그런 경우를 500 으로 돌려 표가 잡게 한다.
func deniedButApplied(status int, body map[string]any, applied bool) (int, map[string]any) {
	if status == http.StatusForbidden && applied {
		return http.StatusInternalServerError, map[string]any{"leak": "403 으로 답했지만 바뀌었다", "body": body}
	}
	return status, body
}

func TestE2E_권한표의각행은되는역할만통과하고나머지는403(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("권한표 상점")
	// 배정·업무 기록의 "다른 상담원" 자리. 표를 도는 세 역할 누구와도 다르다.
	other := h.invited(tm.tn, "STAFF")

	type row struct {
		perm string
		name string
		ok   int // 권한이 있을 때의 상태
		// do 는 자기 픽스처를 새로 만든 뒤 token 으로 한 번 시도한다. 앞선 시도가 상태를 바꿔도 다음 시도가 흔들리지 않는다.
		do func(token string) (int, map[string]any)
	}
	newConv := func() (cust, convID string) { return h.widgetCustomer(tm.tn) }
	customerOf := func(convID string) string {
		c := h.get("/api/conversations/"+convID, tm.admin.token)
		v, _ := c["customer_id"].(string)
		return v
	}
	rows := []row{
		{"conversation.work", "받은함을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/agent/conversations", tok, nil)
		}},
		{"conversation.work", "이전 상담을 이어 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodGet, "/api/agent/conversations/"+conv+"/prior", tok, nil)
		}},
		{"conversation.assign", "다른 상담원에게 배정한다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodPost, "/api/agent/conversations/"+conv+"/assign", tok, map[string]any{"agent_id": other.id})
		}},
		{"conversation.assign", "남의 담당을 해제한다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			h.post("/api/agent/conversations/"+conv+"/assign", tm.admin.token, map[string]any{"agent_id": other.id})
			return h.call(http.MethodPost, "/api/agent/conversations/"+conv+"/assign", tok, map[string]any{"agent_id": ""})
		}},
		{"conversation.assign", "남이 맡은 상담을 나에게 가져온다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			h.post("/api/agent/conversations/"+conv+"/assign", tm.admin.token, map[string]any{"agent_id": other.id})
			return h.call(http.MethodPost, "/api/agent/conversations/"+conv+"/assign", tok, map[string]any{"agent_id": "me"})
		}},
		{"conversation.delete", "상담을 삭제한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodDelete, "/api/agent/conversations/"+conv, tok, map[string]any{"reason": "중복 문의"})
		}},
		{"conversation.delete", "삭제한 상담을 되살린다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			h.call(http.MethodDelete, "/api/agent/conversations/"+conv, tm.admin.token, map[string]any{"reason": "중복 문의"})
			return h.call(http.MethodPost, "/api/agent/conversations/"+conv+"/restore", tok, nil)
		}},
		{"message.moderate", "다른 상담원의 메시지를 지운다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			msg := h.send(conv, other.token, "잘못 보낸 안내", false)
			return h.call(http.MethodDelete, "/api/conversations/"+conv+"/messages/"+msg["id"].(string), tok, nil)
		}},
		{"message.revisions", "수정 전 원문 이력을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			cust, conv := newConv()
			msg := h.send(conv, cust, "원래 문장", false)
			return h.call(http.MethodGet, "/api/agent/conversations/"+conv+"/messages/"+msg["id"].(string)+"/revisions", tok, nil)
		}},
		{"search.all", "워크스페이스 전체를 검색한다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/agent/search?q=배송", tok, nil)
		}},
		{"customer.edit", "고객 프로필과 메모를 고친다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodPost, "/api/customers/"+customerOf(conv), tok, map[string]any{"display_name": "고친 이름", "note": "VIP"})
		}},
		{"customer.delete", "고객을 삭제(익명화)한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodDelete, "/api/customers/"+customerOf(conv), tok, nil)
		}},
		{"library.manage", "빠른 답변을 만든다", http.StatusCreated, func(tok string) (int, map[string]any) {
			return h.call(http.MethodPost, "/api/agent/snippets", tok, map[string]any{"shortcut": "s" + id.New()[30:], "title": "인사", "body": "안녕하세요"})
		}},
		{"library.manage", "빠른 답변을 지운다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			sn := h.post("/api/agent/snippets", tm.admin.token, map[string]any{"shortcut": "d" + id.New()[30:], "title": "지울 것", "body": "x"})
			return h.call(http.MethodDelete, "/api/agent/snippets/"+sn["id"].(string), tok, nil)
		}},
		{"library.manage", "빠른 답변을 고친다", http.StatusOK, func(tok string) (int, map[string]any) {
			sn := h.post("/api/agent/snippets", tm.admin.token, map[string]any{"shortcut": "u" + id.New()[30:], "title": "고칠 것", "body": "x"})
			status, body := h.call(http.MethodPut, "/api/agent/snippets/"+sn["id"].(string), tok, map[string]any{"shortcut": "u" + id.New()[30:], "title": "고친 제목", "body": "y"})
			applied := false
			for _, raw := range firstList(h.get("/api/agent/snippets", tm.admin.token), "snippets") {
				if s, _ := raw.(map[string]any); s["id"] == sn["id"] {
					applied = s["title"] == "고친 제목"
				}
			}
			return deniedButApplied(status, body, applied)
		}},
		{"library.manage", "없는 태그를 붙여 새 태그 정의를 만든다", http.StatusOK, func(tok string) (int, map[string]any) {
			_, conv := newConv()
			return h.call(http.MethodPost, "/api/agent/conversations/"+conv+"/tags", tok, map[string]any{"name": "새태그-" + id.New()[30:]})
		}},
		{"work.view_all", "다른 상담원의 업무 기록을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/agent/work?agent="+other.id, tok, nil)
		}},
		{"team.manage", "팀을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/team", tok, nil)
		}},
		{"team.manage", "초대한다", http.StatusCreated, func(tok string) (int, map[string]any) {
			return h.call(http.MethodPost, "/api/admin/invites", tok, map[string]any{"email": "inv-" + id.New()[28:] + "@example.com", "name": "후보", "role": "PART_TIME"})
		}},
		{"team.manage", "역할을 바꾼다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			target := h.invited(tm.tn, "PART_TIME")
			return h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/role", tok, map[string]any{"role": "STAFF"})
		}},
		{"team.manage", "비활성화한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			target := h.invited(tm.tn, "PART_TIME")
			return h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/active", tok, map[string]any{"active": false})
		}},
		{"team.manage", "다른 상담원의 이름을 바꾼다", http.StatusOK, func(tok string) (int, map[string]any) {
			target := h.invited(tm.tn, "PART_TIME")
			name := "바뀐 이름 " + id.New()[30:]
			status, body := h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/name", tok, map[string]any{"name": name})
			return deniedButApplied(status, body, h.get("/api/me", target.token)["name"] == name)
		}},
		{"team.manage", "초대를 취소한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			email := "rev-" + strings.ToLower(id.New()[28:]) + "@example.com"
			h.post("/api/admin/invites", tm.admin.token, map[string]any{"email": email, "name": "취소할 후보", "role": "PART_TIME"})
			return h.call(http.MethodDelete, "/api/admin/invites/"+email, tok, nil)
		}},
		{"team.manage", "비활성 상담원을 다시 활성화한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			target := h.invited(tm.tn, "PART_TIME")
			h.post("/api/admin/agents/"+target.id+"/active", tm.admin.token, map[string]any{"active": false})
			return h.call(http.MethodPost, "/api/admin/agents/"+target.id+"/active", tok, map[string]any{"active": true})
		}},
		// 권한이 있어도 승인 대기 계정의 역할은 바꾸지 못한다. 역할은 승인할 때 정한다 — 없는 계정처럼 404 다.
		{"team.manage", "승인 대기 계정의 역할은 바꿀 수 없다", http.StatusNotFound, func(tok string) (int, map[string]any) {
			pendingID := h.pendingSignup(tm.tn, tm.admin.token)
			return h.call(http.MethodPost, "/api/admin/agents/"+pendingID+"/role", tok, map[string]any{"role": "ADMIN"})
		}},
		{"team.manage", "가입 대기를 승인한다", http.StatusOK, func(tok string) (int, map[string]any) {
			pendingID := h.pendingSignup(tm.tn, tm.admin.token)
			return h.call(http.MethodPost, "/api/admin/agents/"+pendingID+"/approve", tok, map[string]any{"role": "PART_TIME"})
		}},
		{"team.manage", "가입 대기를 거절한다", http.StatusNoContent, func(tok string) (int, map[string]any) {
			pendingID := h.pendingSignup(tm.tn, tm.admin.token)
			return h.call(http.MethodPost, "/api/admin/agents/"+pendingID+"/reject", tok, nil)
		}},
		{"workspace.manage", "워크스페이스 키와 비밀을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/workspace", tok, nil)
		}},
		{"workspace.manage", "워크스페이스 이름과 허용 출처를 고친다", http.StatusOK, func(tok string) (int, map[string]any) {
			name := "권한표 상점 " + id.New()[30:]
			status, body := h.call(http.MethodPut, "/api/admin/workspace", tok, map[string]any{"name": name, "allowed_origins": []string{}})
			return deniedButApplied(status, body, h.get("/api/admin/workspace", tm.admin.token)["name"] == name)
		}},
		{"workspace.manage", "회원 서명 비밀을 새로 발급한다", http.StatusOK, func(tok string) (int, map[string]any) {
			before := h.get("/api/admin/workspace", tm.admin.token)["member_secret"]
			status, body := h.call(http.MethodPost, "/api/admin/workspace/rotate-member-secret", tok, nil)
			return deniedButApplied(status, body, h.get("/api/admin/workspace", tm.admin.token)["member_secret"] != before)
		}},
		{"workspace.manage", "운영 설정을 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/settings", tok, nil)
		}},
		{"workspace.manage", "운영 설정을 바꾼다", http.StatusOK, func(tok string) (int, map[string]any) {
			st := h.get("/api/admin/settings", tm.admin.token)
			return h.call(http.MethodPut, "/api/admin/settings", tok, st)
		}},
		{"workspace.manage", "위젯 FAQ 를 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/widget-faq", tok, nil)
		}},
		{"workspace.manage", "위젯 FAQ 를 바꾼다", http.StatusOK, func(tok string) (int, map[string]any) {
			q := "권한표 질문 " + id.New()[30:]
			status, body := h.call(http.MethodPut, "/api/admin/widget-faq", tok, map[string]any{"items": []map[string]any{{"question": q, "answer": "답"}}})
			got := firstList(h.get("/api/admin/widget-faq", tm.admin.token), "items")
			applied := len(got) == 1 && got[0].(map[string]any)["question"] == q
			return deniedButApplied(status, body, applied)
		}},
		{"workspace.manage", "감사 로그를 본다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/audit", tok, nil)
		}},
		{"workspace.manage", "내보낸다", http.StatusOK, func(tok string) (int, map[string]any) {
			return h.call(http.MethodGet, "/api/admin/export?format=json", tok, nil)
		}},
	}
	// 계약의 권한표 그대로다. 표가 바뀌면 여기와 permission.go 가 같이 바뀐다.
	granted := map[string][]string{
		"ADMIN":     {"conversation.work", "conversation.assign", "conversation.delete", "message.moderate", "message.revisions", "search.all", "customer.edit", "customer.delete", "library.manage", "work.view_all", "team.manage", "workspace.manage"},
		"STAFF":     {"conversation.work", "conversation.assign", "message.revisions", "search.all", "customer.edit", "library.manage", "work.view_all"},
		"PART_TIME": {"conversation.work"},
	}
	roles := []struct {
		name  string
		token string
	}{{"ADMIN", tm.admin.token}, {"STAFF", tm.staff.token}, {"PART_TIME", tm.partTime.token}}

	for _, r := range rows {
		for _, role := range roles {
			allowed := slices.Contains(granted[role.name], r.perm)
			t.Run(r.perm+"/"+r.name+"/"+role.name, func(t *testing.T) {
				status, body := r.do(role.token)
				if allowed {
					if status != r.ok {
						t.Errorf("%s 는 %s 권한이 있어 %d 여야 한다: %d %v", role.name, r.perm, r.ok, status, body)
					}
					return
				}
				if status != http.StatusForbidden {
					t.Errorf("%s 는 %s 권한이 없어 403 이어야 한다: %d %v", role.name, r.perm, status, body)
				}
			})
		}
	}

	t.Run("세션 정보의 permissions 는 역할의 권한표와 같다", func(t *testing.T) {
		for _, role := range roles {
			var got []string
			for _, p := range firstList(h.get("/api/me", role.token), "permissions") {
				got = append(got, p.(string))
			}
			if !slices.Equal(got, granted[role.name]) {
				t.Errorf("%s 의 permissions: %v, 표: %v", role.name, got, granted[role.name])
			}
		}
		cust, _ := newConv()
		if got := firstList(h.get("/api/me", cust), "permissions"); len(got) != 0 {
			t.Errorf("고객은 권한이 없다: %v", got)
		}
	})

	t.Run("권한이 없으면 403 forbidden 과 권한 문구로 답한다", func(t *testing.T) {
		status, body := h.call(http.MethodGet, "/api/admin/team", tm.partTime.token, nil)
		e, _ := body["error"].(map[string]any)
		if status != http.StatusForbidden || e["code"] != "forbidden" || e["message"] != "이 작업을 할 권한이 없습니다" {
			t.Errorf("403 forbidden \"이 작업을 할 권한이 없습니다\" 여야 한다: %d %v", status, body)
		}
	})
}

func TestE2E_아르바이트는응대만한다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("알바 상점")
	pt := tm.partTime

	cust, conv := h.widgetCustomer(tm.tn)
	h.send(conv, cust, "배송 문의", false)

	t.Run("자기에게 배정하고, 답하고, 메모하고, 종료한다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+conv+"/assign", pt.token, map[string]any{"agent_id": "me"})
		h.send(conv, pt.token, "확인해 드릴게요", false)
		h.send(conv, pt.token, "송장 조회 필요", true)
		h.post("/api/agent/conversations/"+conv+"/state", pt.token, map[string]any{"state": "CLOSED"})
		h.post("/api/agent/conversations/"+conv+"/state", pt.token, map[string]any{"state": "OPEN"})
		h.post("/api/agent/conversations/"+conv+"/assign", pt.token, map[string]any{"agent_id": ""})
	})

	t.Run("고객 프로필은 볼 수 있다", func(t *testing.T) {
		c := h.get("/api/conversations/"+conv, pt.token)
		h.get("/api/customers/"+c["customer_id"].(string), pt.token)
	})

	t.Run("이미 있는 태그는 붙인다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+conv+"/tags", tm.staff.token, map[string]any{"name": "배송"})
		_, other := h.widgetCustomer(tm.tn)
		h.post("/api/agent/conversations/"+other+"/tags", pt.token, map[string]any{"name": "배송"})
	})

	t.Run("아르바이트는 남의 업무 기록을 못 보고, 조회는 자기 것으로 고정된다", func(t *testing.T) {
		// 직원이 한 일과 아르바이트가 한 일이 둘 다 기록에 있다.
		h.post("/api/agent/conversations/"+conv+"/assign", tm.staff.token, map[string]any{"agent_id": "me"})
		for _, q := range []string{"", "?agent=all", "?agent=me"} {
			events := firstList(h.get("/api/agent/work"+q, pt.token), "events")
			if len(events) == 0 {
				t.Fatalf("%q: 자기 기록은 보여야 한다", q)
			}
			for _, raw := range events {
				e, _ := raw.(map[string]any)
				if e["agent_id"] != pt.id {
					t.Errorf("%q: 남의 기록이 보인다: %v", q, e)
				}
			}
		}
		if got, _ := h.call(http.MethodGet, "/api/agent/work?agent="+tm.staff.id, pt.token, nil); got != http.StatusForbidden {
			t.Errorf("남의 id 를 주면 403: %d", got)
		}
		rows := firstList(h.get("/api/agent/work/summary", pt.token), "agents")
		if len(rows) != 1 || rows[0].(map[string]any)["agent_id"] != pt.id {
			t.Errorf("집계도 자기 줄만: %v", rows)
		}
		if n := len(firstList(h.get("/api/agent/work/summary", tm.staff.token), "agents")); n < 3 {
			t.Errorf("직원은 팀 전체 집계를 본다: %d줄", n)
		}
	})
}

// pendingSignup 은 그 워크스페이스에 가입 대기 계정 하나를 만들고 id 를 돌려준다.
func (h *harness) pendingSignup(tn tenant, adminToken string) string {
	h.t.Helper()
	email := "join-" + strings.ToLower(id.New()[28:]) + "@example.com"
	h.agentSignup(tn, email, "지원자", "join12345")
	for _, raw := range firstList(h.get("/api/admin/team", adminToken), "pending") {
		m, _ := raw.(map[string]any)
		if m["email"] == email {
			return m["id"].(string)
		}
	}
	h.t.Fatalf("가입 대기 목록에 %s 가 없다", email)
	return ""
}

// loginStatus 는 로그인 입구의 상태와 오류 코드다. 로그인은 기본 워크스페이스로만 간다(D35).
func (h *harness) loginStatus(email, password string) (int, string) {
	h.t.Helper()
	status, body := h.call(http.MethodPost, "/api/agent/login", "", map[string]any{"email": email, "password": password})
	return status, errorCodeOf(body)
}

func pendingEmails(h *harness, adminToken string) []string {
	var out []string
	for _, raw := range firstList(h.get("/api/admin/team", adminToken), "pending") {
		m, _ := raw.(map[string]any)
		out = append(out, m["email"].(string))
	}
	return out
}

// 가입 입구와 로그인 입구는 기본 워크스페이스로만 간다(D35). 대기 목록 전체를 단언하므로 격리 DB 에서 돈다.
func TestE2E_상담원가입(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	email := "joiner-" + strings.ToLower(id.New()[28:]) + "@example.com"

	t.Run("가입하면 202 이고 승인 대기 목록에 아르바이트로 올라온다", func(t *testing.T) {
		if got := h.agentSignup(tn, email, "새 알바", "join12345"); got != http.StatusAccepted {
			t.Fatalf("202 여야 한다: %d", got)
		}
		var found map[string]any
		for _, raw := range firstList(h.get("/api/admin/team", tn.token), "pending") {
			if m, _ := raw.(map[string]any); m["email"] == email {
				found = m
			}
		}
		if found == nil || found["role"] != "PART_TIME" || found["active"] != false {
			t.Fatalf("대기 목록에 비활성 PART_TIME 으로 있어야 한다: %v", found)
		}
		for _, raw := range firstList(h.get("/api/admin/team", tn.token), "members") {
			if m, _ := raw.(map[string]any); m["email"] == email {
				t.Error("승인 전에는 구성원 목록에 없다")
			}
		}
	})

	t.Run("승인 전에는 맞는 비밀번호면 403 pending_approval, 틀리면 401 이다", func(t *testing.T) {
		if got, code := h.loginStatus(email, "join12345"); got != http.StatusForbidden || code != "pending_approval" {
			t.Errorf("403 pending_approval 이어야 한다: %d %s", got, code)
		}
		if got, _ := h.loginStatus(email, "WRONGWRONG"); got != http.StatusUnauthorized {
			t.Errorf("틀린 비밀번호는 지금처럼 401: %d", got)
		}
	})

	t.Run("이미 있는 이메일로 가입해도 같은 202 이고 기존 계정은 바뀌지 않는다", func(t *testing.T) {
		ownerEmail := tn.ownerEmail
		if got := h.agentSignup(tn, ownerEmail, "가로채기", "hijack1234"); got != http.StatusAccepted {
			t.Errorf("있는 이메일도 202: %d", got)
		}
		if got := h.agentSignup(tn, email, "두번째 신청", "other12345"); got != http.StatusAccepted {
			t.Errorf("대기 중인 이메일도 202: %d", got)
		}
		if got, _ := h.loginStatus(ownerEmail, "owner1234"); got != http.StatusOK {
			t.Errorf("기존 비밀번호로 그대로 로그인돼야 한다: %d", got)
		}
		if got, _ := h.loginStatus(ownerEmail, "hijack1234"); got != http.StatusUnauthorized {
			t.Errorf("가입 요청의 비밀번호가 기존 계정에 먹으면 안 된다: %d", got)
		}
		if me := h.get("/api/me", tn.token); me["name"] != "대표" || me["role"] != "ADMIN" {
			t.Errorf("기존 계정의 이름·역할이 그대로여야 한다: %v", me)
		}
		if got, _ := h.loginStatus(email, "other12345"); got != http.StatusUnauthorized {
			t.Errorf("두 번째 신청의 비밀번호가 대기 계정을 덮으면 안 된다: %d", got)
		}
		if n := slices.Index(pendingEmails(h, tn.token), email); n < 0 || len(pendingEmails(h, tn.token)) != 1 {
			t.Errorf("대기는 처음 신청 하나뿐이어야 한다: %v", pendingEmails(h, tn.token))
		}
	})

	t.Run("형식이 틀린 가입은 400 이다", func(t *testing.T) {
		if got := h.agentSignup(tn, "not-an-email", "누구", "join12345"); got != http.StatusBadRequest {
			t.Errorf("이메일 형식: %d", got)
		}
		if got := h.agentSignup(tn, "short-"+id.New()[30:]+"@example.com", "누구", "short"); got != http.StatusBadRequest {
			t.Errorf("짧은 비밀번호: %d", got)
		}
	})

	var approvedID string
	t.Run("승인하면 고른 역할로 로그인되고 감사 로그에 남는다", func(t *testing.T) {
		approvedID = h.pendingIDOf(tn, email)
		h.post("/api/admin/agents/"+approvedID+"/approve", tn.token, map[string]any{"role": "STAFF"})
		out := h.post("/api/agent/login", "", map[string]any{"email": email, "password": "join12345"})
		tok, _ := out["token"].(string)
		if me := h.get("/api/me", tok); me["role"] != "STAFF" || me["agent_id"] != approvedID {
			t.Errorf("승인한 역할로 들어와야 한다: %v", me)
		}
		if !h.auditHas(tn.token, "agent.approve", approvedID) {
			t.Error("agent.approve 가 감사 로그에 없다")
		}
	})

	t.Run("승인된 계정은 다시 승인·거절할 수 없다", func(t *testing.T) {
		if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+approvedID+"/approve", tn.token, map[string]any{"role": "ADMIN"}); got != http.StatusNotFound {
			t.Errorf("다시 승인은 404: %d", got)
		}
		if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+approvedID+"/reject", tn.token, nil); got != http.StatusNotFound {
			t.Errorf("승인된 계정 거절은 404: %d", got)
		}
	})

	t.Run("거절하면 대기 계정이 사라지고 감사 로그에 남는다", func(t *testing.T) {
		rejectEmail := "reject-" + strings.ToLower(id.New()[28:]) + "@example.com"
		h.agentSignup(tn, rejectEmail, "거절될 사람", "join12345")
		rid := h.pendingIDOf(tn, rejectEmail)
		if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+rid+"/reject", tn.token, nil); got != http.StatusNoContent {
			t.Fatalf("거절: %d", got)
		}
		if slices.Contains(pendingEmails(h, tn.token), rejectEmail) {
			t.Error("거절한 계정이 대기 목록에 남았다")
		}
		if got, _ := h.loginStatus(rejectEmail, "join12345"); got != http.StatusUnauthorized {
			t.Errorf("거절된 사람은 401: %d", got)
		}
		if !h.auditHas(tn.token, "agent.reject", rid) {
			t.Error("agent.reject 가 감사 로그에 없다")
		}
	})

	t.Run("역할 변경은 감사 로그에 남는다", func(t *testing.T) {
		h.post("/api/admin/agents/"+approvedID+"/role", tn.token, map[string]any{"role": "PART_TIME"})
		if !h.auditHas(tn.token, "agent.role", approvedID) {
			t.Error("agent.role 이 감사 로그에 없다")
		}
	})

	t.Run("비활성화로는 대기 계정을 승인 없이 깨울 수 없다", func(t *testing.T) {
		wakeEmail := "wake-" + strings.ToLower(id.New()[28:]) + "@example.com"
		h.agentSignup(tn, wakeEmail, "깨우기", "join12345")
		wid := h.pendingIDOf(tn, wakeEmail)
		if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+wid+"/active", tn.token, map[string]any{"active": true}); got != http.StatusNotFound {
			t.Errorf("대기 계정 활성화는 404: %d", got)
		}
		if got, code := h.loginStatus(wakeEmail, "join12345"); got != http.StatusForbidden || code != "pending_approval" {
			t.Errorf("여전히 승인 대기여야 한다: %d %s", got, code)
		}
	})

	t.Run("마지막 관리자는 여전히 강등할 수 없다", func(t *testing.T) {
		adminID := h.get("/api/me", tn.token)["agent_id"].(string)
		for _, role := range []string{"STAFF", "PART_TIME"} {
			if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+adminID+"/role", tn.token, map[string]any{"role": role}); got != http.StatusBadRequest {
				t.Errorf("마지막 관리자를 %s 로 강등하면 400: %d", role, got)
			}
		}
		if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+approvedID+"/role", tn.token, map[string]any{"role": "AGENT"}); got != http.StatusBadRequest {
			t.Errorf("옛 역할 AGENT 는 이제 없는 값이라 400: %d", got)
		}
	})
}

func (h *harness) pendingIDOf(tn tenant, email string) string {
	h.t.Helper()
	for _, raw := range firstList(h.get("/api/admin/team", tn.token), "pending") {
		m, _ := raw.(map[string]any)
		if m["email"] == email {
			return m["id"].(string)
		}
	}
	h.t.Fatalf("가입 대기 목록에 %s 가 없다", email)
	return ""
}

func (h *harness) auditHas(adminToken, action, target string) bool {
	h.t.Helper()
	for _, raw := range firstList(h.get("/api/admin/audit?limit=500", adminToken), "entries") {
		e, _ := raw.(map[string]any)
		if e["action"] == action && e["target"] == target {
			return true
		}
	}
	return false
}

func TestE2E_가입이꺼진워크스페이스는202이고아무것도만들지않는다(t *testing.T) {
	// 기본 워크스페이스의 가입 설정을 끈다. 공유 DB 의 다른 테스트가 흔들리지 않게 격리 DB 에서 돈다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	st := h.get("/api/admin/settings", tn.token)
	if st["agent_signup"] != true {
		t.Fatalf("기본은 열려 있다: %v", st["agent_signup"])
	}
	st["agent_signup"] = false
	h.put("/api/admin/settings", tn.token, st)

	email := "closed-" + strings.ToLower(id.New()[28:]) + "@example.com"
	if got := h.agentSignup(tn, email, "누구", "join12345"); got != http.StatusAccepted {
		t.Errorf("닫혀 있어도 202: %d", got)
	}
	if got := pendingEmails(h, tn.token); len(got) != 0 {
		t.Errorf("아무것도 만들면 안 된다: %v", got)
	}
	if got, _ := h.loginStatus(email, "join12345"); got != http.StatusUnauthorized {
		t.Errorf("만들지 않았으니 401: %d", got)
	}

	t.Run("agent_signup 을 빼고 저장해도 닫힌 채로 남는다", func(t *testing.T) {
		st := h.get("/api/admin/settings", tn.token)
		delete(st, "agent_signup")
		h.put("/api/admin/settings", tn.token, st)
		if got := h.get("/api/admin/settings", tn.token)["agent_signup"]; got != false {
			t.Errorf("모르는 화면이 저장해도 입구가 바뀌면 안 된다: %v", got)
		}
	})
}

func TestE2E_승인대기는워크스페이스당50건까지다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	// 49건은 SQL 로 채운다. 가입 입구로 49번 해시를 만들 이유가 없다. 50번째는 입구로 넣어 입구가 센다는 걸 같이 본다.
	testsupport.Exec(t, dbURL, `
		INSERT INTO agents (id, workspace_id, email, name, password_hash, role, active, approved_at)
		SELECT gen_random_uuid(), $1, 'bulk-' || g || '-' || $2 || '@example.com', '대기', 'x', 'PART_TIME', false, NULL
		  FROM generate_series(1, 49) g`, tn.wsID, strings.ToLower(id.New()[28:]))
	fiftieth := "fiftieth-" + strings.ToLower(id.New()[28:]) + "@example.com"
	if got := h.agentSignup(tn, fiftieth, "오십", "join12345"); got != http.StatusAccepted {
		t.Fatalf("50번째: %d", got)
	}
	over := "over-" + strings.ToLower(id.New()[28:]) + "@example.com"
	if got := h.agentSignup(tn, over, "오십일", "join12345"); got != http.StatusAccepted {
		t.Errorf("넘쳐도 같은 202: %d", got)
	}
	got := pendingEmails(h, tn.token)
	if len(got) != 50 || !slices.Contains(got, fiftieth) || slices.Contains(got, over) {
		t.Errorf("대기는 50건에서 멈춰야 한다: %d건, 50번째 있음=%v, 51번째 있음=%v",
			len(got), slices.Contains(got, fiftieth), slices.Contains(got, over))
	}
}

func TestE2E_다른워크스페이스관리자는남의대기계정을승인거절못한다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	a, b := h.newTenant("가 가입"), h.newTenant("나 가입")
	email := "cross-" + strings.ToLower(id.New()[28:]) + "@example.com"
	h.agentSignup(b, email, "B 지원자", "join12345")
	pid := h.pendingIDOf(b, email)

	if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+pid+"/approve", a.token, map[string]any{"role": "ADMIN"}); got != http.StatusNotFound {
		t.Errorf("남의 대기 계정 승인은 404: %d", got)
	}
	if got, _ := h.call(http.MethodPost, "/api/admin/agents/"+pid+"/reject", a.token, nil); got != http.StatusNotFound {
		t.Errorf("남의 대기 계정 거절은 404: %d", got)
	}
	if !slices.Contains(pendingEmails(h, b.token), email) {
		t.Error("B 의 대기 계정이 그대로 있어야 한다")
	}
	if got := pendingEmails(h, a.token); slices.Contains(got, email) {
		t.Error("A 의 대기 목록에 B 의 지원자가 보인다")
	}
}

// 가입 입구는 이메일 소유를 확인하지 않는다. 남이 먼저 그 이메일로 신청해 두어도 진짜 주인은 초대로 들어온다.
func TestE2E_직접가입과초대가겹친다(t *testing.T) {
	// 로그인 입구로 확인하므로 기본 워크스페이스에서 돈다. 팀 목록을 단언하므로 격리 DB 다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	email := "owner-" + strings.ToLower(id.New()[28:]) + "@example.com"
	if got := h.agentSignup(tn, email, "가로챈 사람", "squat12345"); got != http.StatusAccepted {
		t.Fatalf("가입 202: %d", got)
	}
	h.pendingIDOf(tn, email)

	var inviteToken string
	t.Run("대기 중인 이메일로 초대할 수 있다", func(t *testing.T) {
		status, out := h.call(http.MethodPost, "/api/admin/invites", tn.token, map[string]any{"email": email, "name": "진짜 주인", "role": "STAFF"})
		if status != http.StatusCreated {
			t.Fatalf("대기 중인 이메일도 초대 201: %d %v", status, out)
		}
		url, _ := out["accept_url"].(string)
		inviteToken = url[strings.Index(url, "invite=")+len("invite="):]
	})

	t.Run("제3자가 같은 이메일로 먼저 가입해도 초대 수락이 되고 그 계정의 비밀번호는 수락한 사람의 것이다", func(t *testing.T) {
		if inviteToken == "" {
			t.Fatal("초대를 만들지 못했다")
		}
		status, acc := h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": inviteToken, "password": "owner12345"})
		if status >= 300 {
			t.Fatalf("수락: %d %v", status, acc)
		}
		if got, _ := h.loginStatus(email, "owner12345"); got != http.StatusOK {
			t.Errorf("수락한 사람의 비밀번호로 로그인돼야 한다: %d", got)
		}
		if got, _ := h.loginStatus(email, "squat12345"); got != http.StatusUnauthorized {
			t.Errorf("먼저 가입한 사람의 비밀번호는 먹으면 안 된다: %d", got)
		}
		if slices.Contains(pendingEmails(h, tn.token), email) {
			t.Error("대기 행이 남았다")
		}
		n := 0
		for _, raw := range firstList(h.get("/api/admin/team", tn.token), "members") {
			if m, _ := raw.(map[string]any); m["email"] == email {
				n++
				if m["role"] != "STAFF" || m["active"] != true {
					t.Errorf("초대의 역할로 활성이어야 한다: %v", m)
				}
			}
		}
		if n != 1 {
			t.Errorf("구성원 목록에 그 이메일이 한 번 있어야 한다: %d", n)
		}
	})

	t.Run("승인된 계정이 있는 이메일은 여전히 초대할 수 없다", func(t *testing.T) {
		if status, _ := h.call(http.MethodPost, "/api/admin/invites", tn.token, map[string]any{"email": email, "name": "또", "role": "STAFF"}); status != http.StatusBadRequest {
			t.Errorf("이미 팀에 있는 이메일은 400: %d", status)
		}
	})
}

// auditDetail 은 그 행동·대상의 가장 최근 감사 로그의 detail 이다. 없으면 nil.
func (h *harness) auditDetail(adminToken, action, target string) map[string]any {
	h.t.Helper()
	for _, raw := range firstList(h.get("/api/admin/audit?limit=500", adminToken), "entries") {
		e, _ := raw.(map[string]any)
		if e["action"] == action && e["target"] == target {
			d, _ := e["detail"].(map[string]any)
			if d == nil {
				d = map[string]any{}
			}
			return d
		}
	}
	return nil
}

// inviteToken 은 초대를 만들고 수락 링크의 토큰을 꺼낸다.
func (h *harness) inviteToken(tn tenant, email, role string) string {
	h.t.Helper()
	status, out := h.call(http.MethodPost, "/api/admin/invites", tn.token, map[string]any{"email": email, "name": "초대 후보", "role": role})
	url, _ := out["accept_url"].(string)
	if status != http.StatusCreated || !strings.Contains(url, "invite=") {
		h.t.Fatalf("초대: %d %v", status, out)
	}
	return url[strings.Index(url, "invite=")+len("invite="):]
}

// 초대 수락이 같은 이메일의 승인 대기 행을 대체했으면 감사 로그가 그 사실을 남긴다. 평범한 수락에는 그 필드가 없다.
func TestE2E_초대수락의감사로그는승인대기를대체했을때만replaced_pending을남긴다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("대체 기록 상점")

	t.Run("대기 행을 대체한 초대 수락은 감사 로그에 replaced_pending 이 true 다", func(t *testing.T) {
		email := "squatted-" + strings.ToLower(id.New()[28:]) + "@example.com"
		if got := h.agentSignup(tn, email, "먼저 신청한 사람", "squat12345"); got != http.StatusAccepted {
			t.Fatalf("가입 202: %d", got)
		}
		h.pendingIDOf(tn, email)
		token := h.inviteToken(tn, email, "STAFF")
		if status, acc := h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": token, "password": "owner12345"}); status >= 300 {
			t.Fatalf("수락: %d %v", status, acc)
		}
		d := h.auditDetail(tn.token, "invite.accept", email)
		if d == nil {
			t.Fatal("invite.accept 가 감사 로그에 없다")
		}
		if d["replaced_pending"] != true {
			t.Errorf("replaced_pending 이 true 여야 한다: %v", d)
		}
	})

	t.Run("평범한 초대 수락에는 replaced_pending 이 없다", func(t *testing.T) {
		m := h.invited(tn, "PART_TIME")
		d := h.auditDetail(tn.token, "invite.accept", m.email)
		if d == nil {
			t.Fatal("invite.accept 가 감사 로그에 없다")
		}
		if _, ok := d["replaced_pending"]; ok {
			t.Errorf("대체하지 않은 수락에 replaced_pending 이 있다: %v", d)
		}
	})

	t.Run("다른 워크스페이스에 같은 이메일의 승인 대기 행이 있어도 초대 수락은 그 행을 대체하지 않는다", func(t *testing.T) {
		mine, other := h.newTenant("수락 상점"), h.newTenant("옆 상점")
		email := "both-" + strings.ToLower(id.New()[28:]) + "@example.com"
		if got := h.agentSignup(other, email, "옆 상점 지원자", "other12345"); got != http.StatusAccepted {
			t.Fatalf("옆 상점 가입 202: %d", got)
		}
		pendingID := h.pendingIDOf(other, email)
		token := h.inviteToken(mine, email, "STAFF")
		if status, acc := h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": token, "password": "owner12345"}); status >= 300 {
			t.Fatalf("수락: %d %v", status, acc)
		}
		if got := h.pendingIDOf(other, email); got != pendingID {
			t.Errorf("옆 상점의 승인 대기 행이 바뀌었다: %s → %s", pendingID, got)
		}
		d := h.auditDetail(mine.token, "invite.accept", email)
		if d == nil {
			t.Fatal("invite.accept 가 감사 로그에 없다")
		}
		if _, ok := d["replaced_pending"]; ok {
			t.Errorf("다른 워크스페이스의 대기 행을 대체했다고 남겼다: %v", d)
		}
	})
}

// bcrypt 는 72바이트까지만 받는다. 검증이 그보다 길게 받으면 해시가 오류를 내 500 으로 끝난다.
// 한글은 한 글자가 3바이트라 24자에서 닿는다. 로그인은 긴 비밀번호를 틀린 비밀번호로 본다.
func TestE2E_비밀번호는72바이트까지이고넘으면400이며로그인은401이다(t *testing.T) {
	// 로그인 입구로 확인하므로 기본 워크스페이스에서 돈다. 관리자 비밀번호를 바꾸므로 격리 DB 다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	ownerEmail := tn.ownerEmail
	long := strings.Repeat("가", 30)  // 90바이트
	exact := strings.Repeat("나", 24) // 72바이트
	invalidCode := func(t *testing.T, what string, status int, body map[string]any) {
		t.Helper()
		if status != http.StatusBadRequest || errorCodeOf(body) != "invalid" {
			t.Errorf("%s: 400 invalid 여야 한다: %d %v", what, status, body)
		}
	}

	t.Run("한글 30자 비밀번호는 상담원 가입·초대 수락·비밀번호 변경에서 모두 400 이다", func(t *testing.T) {
		status, body := h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"email": "long-" + strings.ToLower(id.New()[28:]) + "@example.com", "name": "알바", "password": long})
		invalidCode(t, "상담원 가입", status, body)
		token := h.inviteToken(tn, "long-"+strings.ToLower(id.New()[28:])+"@example.com", "STAFF")
		status, body = h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": token, "password": long})
		invalidCode(t, "초대 수락", status, body)
		status, body = h.call(http.MethodPost, "/api/agent/password", tn.token, map[string]any{"current": "owner1234", "next": long})
		invalidCode(t, "비밀번호 변경", status, body)
		if got, _ := h.loginStatus(ownerEmail, "owner1234"); got != http.StatusOK {
			t.Errorf("거절한 변경은 비밀번호를 바꾸지 않는다: %d", got)
		}
	})

	t.Run("72바이트를 넘는 비밀번호로 로그인하면 500 이 아니라 401 이다", func(t *testing.T) {
		if got, _ := h.loginStatus(ownerEmail, long); got != http.StatusUnauthorized {
			t.Errorf("있는 계정: 401 이어야 한다: %d", got)
		}
		if got, _ := h.loginStatus("nobody-"+strings.ToLower(id.New()[28:])+"@example.com", long); got != http.StatusUnauthorized {
			t.Errorf("없는 계정: 401 이어야 한다: %d", got)
		}
	})

	t.Run("정확히 72바이트(한글 24자)는 받고, 그 뒤에 글자를 붙인 비밀번호로는 로그인되지 않는다", func(t *testing.T) {
		email := "exact-" + strings.ToLower(id.New()[28:]) + "@example.com"
		token := h.inviteToken(tn, email, "STAFF")
		status, acc := h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": token, "password": exact})
		if status >= 300 {
			t.Fatalf("72바이트 비밀번호로 수락: %d %v", status, acc)
		}
		if got, _ := h.loginStatus(email, exact); got != http.StatusOK {
			t.Errorf("72바이트 비밀번호로 로그인: %d", got)
		}
		// bcrypt 는 72바이트 뒤를 잘라 비교한다. 길이를 따로 보지 않으면 이것도 맞는 비밀번호가 된다.
		if got, _ := h.loginStatus(email, exact+"가"); got != http.StatusUnauthorized {
			t.Errorf("72바이트 뒤에 글자를 붙이면 401: %d", got)
		}
		if got := h.agentSignup(tn, "exact-"+strings.ToLower(id.New()[28:])+"@example.com", "알바", exact); got != http.StatusAccepted {
			t.Errorf("72바이트 비밀번호로 상담원 가입: %d", got)
		}
		if status, body := h.call(http.MethodPost, "/api/agent/password", tn.token, map[string]any{"current": "owner1234", "next": exact}); status != http.StatusNoContent {
			t.Errorf("72바이트 비밀번호로 변경: %d %v", status, body)
		}
		if got, _ := h.loginStatus(ownerEmail, exact); got != http.StatusOK {
			t.Errorf("바꾼 72바이트 비밀번호로 로그인: %d", got)
		}
	})

	t.Run("비밀번호 하한은 바이트가 아니라 글자로 8자다 — 한글 7글자는 400, 8글자는 받는다", func(t *testing.T) {
		signup := func(password string) (int, map[string]any) {
			return h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"email": "min-" + strings.ToLower(id.New()[28:]) + "@example.com", "name": "알바", "password": password})
		}
		status, body := signup(strings.Repeat("가", 7)) // 21바이트
		invalidCode(t, "한글 7글자 상담원 가입", status, body)
		if status, body := signup(strings.Repeat("가", 8)); status >= 300 {
			t.Errorf("한글 8글자 상담원 가입: %d %v", status, body)
		}
	})

	t.Run("정확히 72바이트는 받고 73바이트는 영문이든 멀티바이트가 경계에 걸치든 400 이다", func(t *testing.T) {
		signup := func(password string) (int, map[string]any) {
			return h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"email": "max-" + strings.ToLower(id.New()[28:]) + "@example.com", "name": "알바", "password": password})
		}
		if status, body := signup(strings.Repeat("a", 72)); status >= 300 {
			t.Errorf("영문 72자 상담원 가입: %d %v", status, body)
		}
		status, body := signup(strings.Repeat("a", 73))
		invalidCode(t, "영문 73자 상담원 가입", status, body)
		status, body = signup("a" + strings.Repeat("가", 24)) // 마지막 글자가 72바이트 경계에 걸친다
		invalidCode(t, "멀티바이트가 경계에 걸친 73바이트 상담원 가입", status, body)
	})
}

// ── 상담 삭제 ──────────────────────────────────────────────────────

func TestE2E_상담삭제(t *testing.T) {
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)
	tm := h.teamOf("삭제 상점")
	admin := tm.admin.token

	// 회원 한 명의 두 상담: 첫 상담(닫힘)과 지금 상담. 지울 것은 첫 상담이다 — 이전 상담 이어 보기에서 빠져야 한다.
	cust := h.memberSession(tm.tn)
	first := h.startConversation(cust)["id"].(string)
	h.send(first, cust, "지울상담 비밀문의 7777", false)
	h.send(first, admin, "지울상담 내부메모", true)
	h.post("/api/agent/conversations/"+first+"/state", admin, map[string]any{"state": "CLOSED"})
	second := h.startConversation(cust)["id"].(string)
	h.send(second, cust, "두번째 문의", false)

	countsBefore := h.get("/api/agent/inbox/counts", admin)["all"].(float64)

	t.Run("사유가 없거나 200자를 넘으면 400 이다", func(t *testing.T) {
		for _, reason := range []string{"", "   ", strings.Repeat("가", 201)} {
			if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, admin, map[string]any{"reason": reason}); got != http.StatusBadRequest {
				t.Errorf("사유 %d자: 400 이어야 한다: %d", len([]rune(reason)), got)
			}
		}
	})

	t.Run("관리자가 아니면 지울 수 없다", func(t *testing.T) {
		for _, tok := range []string{tm.staff.token, tm.partTime.token} {
			if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, tok, map[string]any{"reason": "정리"}); got != http.StatusForbidden {
				t.Errorf("403 이어야 한다: %d", got)
			}
		}
		if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, cust, map[string]any{"reason": "정리"}); got != http.StatusForbidden {
			t.Errorf("고객도 403: %d", got)
		}
	})

	t.Run("다른 워크스페이스 관리자가 지우려 하면 404 다", func(t *testing.T) {
		other := h.newTenant("남의 삭제 상점")
		if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, other.token, map[string]any{"reason": "침입"}); got != http.StatusNotFound {
			t.Errorf("404 여야 한다: %d", got)
		}
	})

	if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, admin, map[string]any{"reason": "개인정보가 담긴 중복 문의"}); got != http.StatusNoContent {
		t.Fatalf("삭제: %d", got)
	}

	t.Run("삭제한 상담은 받은함 목록과 건수에서 빠진다", func(t *testing.T) {
		for _, q := range []string{"", "?state=CLOSED", "?q=7777"} {
			if ids := listIDs(firstList(h.get("/api/agent/conversations"+q, admin), "conversations")); slices.Contains(ids, first) {
				t.Errorf("받은함%s 에 지운 상담이 있다", q)
			}
		}
		if got := h.get("/api/agent/inbox/counts", admin)["all"].(float64); got != countsBefore-1 {
			t.Errorf("전체 건수가 하나 줄어야 한다: %v → %v", countsBefore, got)
		}
	})

	t.Run("삭제한 상담은 검색에서 빠진다", func(t *testing.T) {
		if hits := firstList(h.get("/api/agent/search?q=7777", admin), "hits"); len(hits) != 0 {
			t.Errorf("검색에 나온다: %v", hits)
		}
	})

	t.Run("삭제한 상담은 이전 상담 이어 보기에서 빠진다", func(t *testing.T) {
		if got := h.bodyOf("/api/agent/conversations/"+second+"/prior?limit=50", admin); strings.Contains(got, "지울상담") {
			t.Errorf("이어 보기에 지운 상담이 나온다: %s", got)
		}
		cursor := "/api/agent/conversations/" + second + "/prior?limit=50&before_conversation=" + first + "&before_seq=999"
		if got := h.bodyOf(cursor, admin); strings.Contains(got, "지울상담") {
			t.Errorf("커서로 지운 상담을 가리켜도 나오면 안 된다: %s", got)
		}
	})

	t.Run("id 로 열면 상담원도 고객도 404 다", func(t *testing.T) {
		for _, tok := range []string{admin, tm.staff.token, cust} {
			for _, path := range []string{"/api/conversations/" + first, "/api/conversations/" + first + "/messages", "/api/conversations/" + first + "/reads"} {
				if got, _ := h.call(http.MethodGet, path, tok, nil); got != http.StatusNotFound {
					t.Errorf("%s: 404 여야 한다: %d", path, got)
				}
			}
		}
		if got, _ := h.call(http.MethodPost, "/api/conversations/"+first+"/messages", admin, map[string]any{"client_msg_id": id.New(), "body": "x"}); got != http.StatusNotFound {
			t.Errorf("지운 상담에 쓰기도 404: %d", got)
		}
		if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, admin, map[string]any{"reason": "또"}); got != http.StatusNotFound {
			t.Errorf("두 번 지우면 404: %d", got)
		}
	})

	t.Run("WebSocket 으로 구독할 수 없다", func(t *testing.T) {
		c := h.dial(ctx, admin)
		if f := c.subscribe(ctx, first, 0); f.Type != "error" || f.Code != "not_found" {
			t.Errorf("not_found 오류여야 한다: %+v", f)
		}
	})

	t.Run("삭제는 감사 로그에 사유와 함께, 업무 기록에는 사유 없이 남는다", func(t *testing.T) {
		found := false
		for _, raw := range firstList(h.get("/api/admin/audit?limit=500", admin), "entries") {
			e, _ := raw.(map[string]any)
			if e["action"] == "conversation.delete" && e["target"] == first {
				detail, _ := e["detail"].(map[string]any)
				found = detail["reason"] == "개인정보가 담긴 중복 문의"
			}
		}
		if !found {
			t.Error("conversation.delete 가 사유와 함께 감사 로그에 없다")
		}
		deleted := false
		// 기록 자체는 전체 목록에 남는다. 대화 id 로 좁혀 보는 것은 아래에서 따로 본다.
		for _, raw := range firstList(h.get("/api/agent/work?limit=500", admin), "events") {
			e, _ := raw.(map[string]any)
			if e["kind"] == "deleted" && e["conversation_id"] == first && e["agent_id"] == tm.admin.id {
				deleted = true
				if detail, _ := e["detail"].(map[string]any); detail["reason"] != nil {
					t.Errorf("업무 기록에 삭제 사유가 실렸다: %v", detail)
				}
			}
		}
		if !deleted {
			t.Error("업무 기록에 deleted 가 없다")
		}
		if strings.Contains(h.bodyOf("/api/agent/work?limit=500", admin), "개인정보가 담긴 중복 문의") {
			t.Error("업무 기록 응답 어디에도 삭제 사유가 나오면 안 된다")
		}
	})

	t.Run("삭제한 상담의 업무 기록은 삭제 권한이 없으면 목록에도 집계에도 없다", func(t *testing.T) {
		eventsOf := func(tok string) int {
			n := 0
			for _, raw := range firstList(h.get("/api/agent/work?limit=500", tok), "events") {
				if e, _ := raw.(map[string]any); e["conversation_id"] == first {
					n++
				}
			}
			return n
		}
		if n := eventsOf(admin); n == 0 {
			t.Error("삭제 권한이 있으면 지운 상담의 기록을 본다")
		}
		if n := eventsOf(tm.staff.token); n != 0 {
			t.Errorf("직원에게 지운 상담의 기록이 %d건 보인다", n)
		}
		// 지운 상담의 종료는 관리자가 했다. 관리자 줄의 종료 수가 보는 사람에 따라 하나 다르다.
		closedOf := func(tok string) float64 {
			for _, raw := range firstList(h.get("/api/agent/work/summary", tok), "agents") {
				if a, _ := raw.(map[string]any); a["agent_id"] == tm.admin.id {
					v, _ := a["closed"].(float64)
					return v
				}
			}
			t.Fatal("집계에 관리자 줄이 없다")
			return 0
		}
		if byAdmin, byStaff := closedOf(admin), closedOf(tm.staff.token); byAdmin-byStaff != 1 {
			t.Errorf("직원의 집계는 지운 상담의 종료를 세지 않는다: 관리자가 본 %v, 직원이 본 %v", byAdmin, byStaff)
		}
	})

	t.Run("삭제 권한이 있으면 업무 기록을 삭제한 상담으로 좁혀 보고, 없으면 404 다", func(t *testing.T) {
		status, body := h.call(http.MethodGet, "/api/agent/work?conversation="+first, admin, nil)
		if status != http.StatusOK {
			t.Fatalf("삭제 권한이 있으면 200: %d %v", status, body)
		}
		events := firstList(body, "events")
		if len(events) == 0 {
			t.Error("삭제한 상담으로 좁힌 기록이 비었다")
		}
		for _, raw := range events {
			if e, _ := raw.(map[string]any); e["conversation_id"] != first {
				t.Errorf("다른 상담의 기록이 섞였다: %v", e)
			}
		}
		if status, body := h.call(http.MethodGet, "/api/agent/work?conversation="+first, tm.staff.token, nil); status != http.StatusNotFound {
			t.Errorf("삭제 권한이 없으면 404: %d %v", status, body)
		}
		other := h.newTenant("남의 상점")
		if status, body := h.call(http.MethodGet, "/api/agent/work?conversation="+first, other.token, nil); status != http.StatusNotFound {
			t.Errorf("다른 워크스페이스 관리자는 삭제 권한이 있어도 404: %d %v", status, body)
		}
	})

	t.Run("내보내기에는 deleted_at 과 함께 남는다", func(t *testing.T) {
		raw := h.bodyOf("/api/admin/export?format=json", admin)
		var found bool
		for _, line := range strings.Split(raw, "\n") {
			if strings.Contains(line, `"id":"`+first+`"`) {
				found = strings.Contains(line, `"deleted_at":"`) && strings.Contains(line, "7777")
			}
		}
		if !found {
			t.Errorf("내보내기에 지운 상담이 deleted_at·메시지와 함께 있어야 한다")
		}
		csv := h.bodyOf("/api/admin/export?format=csv", admin)
		if !strings.Contains(strings.SplitN(csv, "\n", 2)[0], "deleted_at") || !strings.Contains(csv, "7777") {
			t.Error("CSV 에도 deleted_at 열과 지운 상담의 메시지가 있어야 한다")
		}
	})

	t.Run("되살리면 모든 경로에 돌아오고 감사 로그에 남는다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+first+"/restore", admin, nil)
		if ids := listIDs(firstList(h.get("/api/agent/conversations", admin), "conversations")); !slices.Contains(ids, first) {
			t.Error("받은함에 돌아와야 한다")
		}
		if hits := firstList(h.get("/api/agent/search?q=7777", admin), "hits"); len(hits) == 0 {
			t.Error("검색에 돌아와야 한다")
		}
		if got := h.bodyOf("/api/agent/conversations/"+second+"/prior?limit=50", admin); !strings.Contains(got, "지울상담") {
			t.Error("이어 보기에 돌아와야 한다")
		}
		h.get("/api/conversations/"+first, admin)
		if !h.auditHas(admin, "conversation.restore", first) {
			t.Error("conversation.restore 가 감사 로그에 없다")
		}
		if got, _ := h.call(http.MethodPost, "/api/agent/conversations/"+first+"/restore", admin, nil); got != http.StatusNotFound {
			t.Errorf("지우지 않은 상담을 되살리면 404: %d", got)
		}
	})

	t.Run("삭제한 상담은 고객의 목록에서 빠진다", func(t *testing.T) {
		before := listIDs(firstList(h.get("/api/customer/conversations", cust), "conversations"))
		if !slices.Contains(before, second) {
			t.Fatalf("지금 상담은 고객 목록에 있어야 한다: %v", before)
		}
		h.call(http.MethodDelete, "/api/agent/conversations/"+second, admin, map[string]any{"reason": "테스트"})
		if after := listIDs(firstList(h.get("/api/customer/conversations", cust), "conversations")); slices.Contains(after, second) {
			t.Errorf("지운 상담이 고객 목록에 있다: %v", after)
		}
	})

	t.Run("열린 상담을 지우면 그 회원이 다시 문의할 때 새 대화가 열린다", func(t *testing.T) {
		// 바로 앞에서 열린 상담(second)을 지웠다.
		third := h.startConversation(cust)["id"].(string)
		if third == second || third == first {
			t.Fatalf("새 대화여야 한다: %s", third)
		}
		// 지운 상담을 되살려도 회원의 "지금 상담" 은 새 대화 그대로다.
		h.post("/api/agent/conversations/"+second+"/restore", admin, nil)
		if again := h.startConversation(cust)["id"].(string); again != third {
			t.Errorf("되살린 옛 상담이 회원의 지금 상담 자리를 차지하면 안 된다: %s, want %s", again, third)
		}
		if got, _ := h.call(http.MethodGet, "/api/conversations/"+second, cust, nil); got != http.StatusNotFound {
			t.Errorf("되살린 옛 상담은 회원에게 지난 상담이라 404: %d", got)
		}
	})

	t.Run("회원에게 지난 상담이 된 상담을 되살리면 종료 상태로 돌아온다", func(t *testing.T) {
		// second 는 열린 채로 지워졌고, 그 사이 third 가 열려 지난 상담이 됐다. 바로 앞에서 되살렸다.
		if st := h.get("/api/conversations/"+second, admin)["state"]; st != "CLOSED" {
			t.Errorf("답할 수 없는 열린 상담이 남으면 안 된다: %v", st)
		}
		if ids := listIDs(firstList(h.get("/api/agent/conversations?state=OPEN", admin), "conversations")); slices.Contains(ids, second) {
			t.Error("되살린 지난 상담이 진행중 받은함에 있다")
		}
	})

	t.Run("지난 상담이 아니면 삭제 전 상태 그대로 되살린다", func(t *testing.T) {
		_, anon := h.widgetCustomer(tm.tn)
		h.call(http.MethodDelete, "/api/agent/conversations/"+anon, admin, map[string]any{"reason": "테스트"})
		restored := h.post("/api/agent/conversations/"+anon+"/restore", admin, nil)
		if restored["state"] != "OPEN" {
			t.Errorf("열린 채로 지운 상담은 열린 채로 돌아온다: %v", restored["state"])
		}
	})
}

// "N번째 상담" 은 이어 보기와 같은 수를 센다. 지운 상담은 이어 보기에 없으니 세지 않는다.
func TestE2E_몇번째상담인지는삭제하지않은상담만센다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("몇번째 상점")
	admin := tm.admin.token
	cust := h.memberSession(tm.tn)
	first := h.startConversation(cust)["id"].(string)
	h.post("/api/agent/conversations/"+first+"/state", admin, map[string]any{"state": "CLOSED"})
	if got, _ := h.call(http.MethodDelete, "/api/agent/conversations/"+first, admin, map[string]any{"reason": "중복"}); got != http.StatusNoContent {
		t.Fatalf("삭제: %d", got)
	}
	second := h.startConversation(cust)["id"].(string)
	if got := h.bodyOf("/api/conversations/"+second+"/messages?after_seq=0", admin); strings.Contains(got, "번째 상담") {
		t.Errorf("지운 상담만 앞에 있으면 첫 상담이다: %s", got)
	}
	h.post("/api/agent/conversations/"+second+"/state", admin, map[string]any{"state": "CLOSED"})
	third := h.startConversation(cust)["id"].(string)
	if got := h.bodyOf("/api/conversations/"+third+"/messages?after_seq=0", admin); !strings.Contains(got, "2번째 상담") {
		t.Errorf("지운 상담을 빼면 2번째다: %s", got)
	}
}
