package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 테넌트 경계. 설치형이라 설치 하나에 워크스페이스 하나이고 토큰 없는 입구는 전부 기본 워크스페이스로 간다(D35).
// 그래도 내부 workspace_id 와 격리 코드(I11)는 남아 있다. 두 번째 워크스페이스는 저장소 수준에서 만들어(newTenant) 격리를 계속 본다.

func TestE2E_테넌트경계(t *testing.T) {
	h := newHarness(t, time.Second, false)
	a := h.newTenant("A 상점")
	b := h.newTenant("B 상점")
	_, aConv := h.widgetCustomer(a)
	bCust, bConv := h.widgetCustomer(b)
	h.post("/api/conversations/"+bConv+"/messages", bCust, map[string]any{"client_msg_id": id.New(), "body": "B 의 비밀 주문번호 9999"})

	t.Run("다른 워크스페이스의 대화는 존재하지 않는 것으로 답한다", func(t *testing.T) {
		for _, path := range []string{"/api/conversations/" + bConv, "/api/conversations/" + bConv + "/messages", "/api/conversations/" + bConv + "/reads"} {
			if got := h.status(http.MethodGet, path, a.token, nil); got != http.StatusNotFound {
				t.Errorf("%s 는 404 여야 한다: %d", path, got)
			}
		}
		if got := h.status(http.MethodPost, "/api/conversations/"+bConv+"/messages", a.token,
			map[string]any{"client_msg_id": id.New(), "body": "침입"}); got != http.StatusNotFound {
			t.Errorf("남의 대화에 쓰기는 404 여야 한다: %d", got)
		}
		if got := h.status(http.MethodPost, "/api/agent/conversations/"+bConv+"/assign", a.token, map[string]any{"agent_id": ""}); got != http.StatusNotFound {
			t.Errorf("남의 대화 배정은 404 여야 한다: %d", got)
		}
	})

	t.Run("받은함·검색·건수는 자기 워크스페이스만 센다", func(t *testing.T) {
		inbox := h.get("/api/agent/conversations", a.token)
		for _, raw := range firstList(inbox, "conversations") {
			c, _ := raw.(map[string]any)
			if c["id"] == bConv {
				t.Error("A 의 받은함에 B 의 대화가 있다")
			}
			if c["id"] != aConv {
				t.Errorf("A 의 받은함에 모르는 대화가 있다: %v", c["id"])
			}
		}
		hits := h.get("/api/agent/search?q=9999", a.token)
		if n := len(firstList(hits, "hits")); n != 0 {
			t.Errorf("A 가 B 의 메시지를 검색했다: %d건", n)
		}
		counts := h.get("/api/agent/inbox/counts", a.token)
		if counts["all"] != float64(1) {
			t.Errorf("A 의 전체 건수는 1 이어야 한다: %v", counts["all"])
		}
	})

	t.Run("팀·태그·빠른 답변·설정·감사 로그는 워크스페이스마다 따로다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+bConv+"/tags", b.token, map[string]any{"name": "B전용"})
		h.post("/api/agent/snippets", b.token, map[string]any{"shortcut": "b", "title": "B", "body": "B 의 답변"})
		if n := len(firstList(h.get("/api/agent/tags", a.token), "tags")); n != 0 {
			t.Errorf("A 가 B 의 태그를 본다: %d", n)
		}
		if n := len(firstList(h.get("/api/agent/snippets", a.token), "snippets")); n != 0 {
			t.Errorf("A 가 B 의 빠른 답변을 본다: %d", n)
		}
		if n := len(firstList(h.get("/api/admin/team", a.token), "members")); n != 1 {
			t.Errorf("A 의 팀은 대표 한 명이어야 한다: %d", n)
		}
		st := h.get("/api/admin/settings", b.token)
		st["widget"].(map[string]any)["title"] = "B 의 제목"
		h.put("/api/admin/settings", b.token, st)
		if got := h.get("/api/admin/settings", a.token)["widget"].(map[string]any)["title"]; got == "B 의 제목" {
			t.Error("B 의 설정이 A 에 보인다")
		}
		for _, raw := range firstList(h.get("/api/admin/audit", a.token), "entries") {
			e, _ := raw.(map[string]any)
			if e["action"] == "settings.update" {
				t.Error("B 의 설정 변경이 A 의 감사 로그에 있다")
			}
		}
	})

	t.Run("다른 워크스페이스 상담원의 역할은 바꿀 수 없다", func(t *testing.T) {
		bMe := h.get("/api/me", b.token)
		bID, _ := bMe["agent_id"].(string)
		if got := h.status(http.MethodPost, "/api/admin/agents/"+bID+"/role", a.token, map[string]any{"role": "STAFF"}); got != http.StatusNotFound {
			t.Errorf("404 여야 한다: %d", got)
		}
		if got := h.status(http.MethodPost, "/api/agent/conversations/"+aConv+"/assign", a.token, map[string]any{"agent_id": bID}); got != http.StatusBadRequest {
			t.Errorf("남의 상담원에게 배정은 400 이어야 한다: %d", got)
		}
	})

	t.Run("다른 워크스페이스의 고객 토큰은 이어 쓰지 않는다", func(t *testing.T) {
		// 위젯 입구는 기본 워크스페이스로 간다. B 고객의 토큰을 들고 와도 거기서 이어 쓰지 않는다.
		resumed := h.post("/api/customer/session", "", map[string]any{"token": bCust})
		if got, _ := resumed["token"].(string); got == "" || got == bCust {
			t.Errorf("B 고객의 토큰이 기본 워크스페이스 위젯에서 그대로 살아났다: %v", resumed["token"])
		}
	})
}

// 설치형 단일 워크스페이스(D35). 회사 셀프 가입과 위젯 키를 걷어냈다. 토큰 없는 입구는 전부 기본 워크스페이스로 간다.
func TestE2E_단일워크스페이스입구(t *testing.T) {
	h := newHarness(t, time.Second, false)
	customerWorkspace := func(t *testing.T, out map[string]any) string {
		t.Helper()
		cust, _ := out["customer"].(map[string]any)
		custID, _ := cust["id"].(string)
		var ws string
		if err := h.store.Pool().QueryRow(context.Background(), "SELECT workspace_id FROM customers WHERE id = $1", custID).Scan(&ws); err != nil {
			t.Fatalf("고객을 찾지 못했다: %v %v", err, out)
		}
		return ws
	}
	pageStatus := func(path string) int {
		t.Helper()
		res := h.raw(http.MethodGet, path, "")
		res.Body.Close()
		return res.StatusCode
	}

	t.Run("위젯은 키 없이 기본 워크스페이스로 세션을 연다", func(t *testing.T) {
		status, out := h.call(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "키 없음"})
		if status != http.StatusOK {
			t.Fatalf("키 없는 세션 생성은 200: %d %v", status, out)
		}
		if ws := customerWorkspace(t, out); ws != core.DefaultWorkspaceID {
			t.Errorf("고객이 기본 워크스페이스에 있어야 한다: %s", ws)
		}
		if got := h.status(http.MethodGet, "/api/widget/settings", "", nil); got != http.StatusOK {
			t.Errorf("키 없는 위젯 설정은 200: %d", got)
		}
		if got := pageStatus("/widget?embed=1"); got != http.StatusOK {
			t.Errorf("키 없는 /widget 은 200: %d", got)
		}
	})

	t.Run("옛 설치 코드가 data-key 를 보내도 같은 워크스페이스로 간다", func(t *testing.T) {
		// 옛 embed.js 는 iframe 주소에 ?key= 를, 옛 위젯은 본문에 widget_key 를 싣는다. 어떤 값이든 읽지 않는다.
		for _, key := range []string{"wk_옛설치코드의키", "wk_" + strings.Repeat("0", 36)} {
			status, out := h.call(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "옛 위젯", "widget_key": key})
			if status != http.StatusOK {
				t.Fatalf("옛 키를 실은 세션 생성은 200: %d %v", status, out)
			}
			if ws := customerWorkspace(t, out); ws != core.DefaultWorkspaceID {
				t.Errorf("옛 키를 실어도 기본 워크스페이스다: %s", ws)
			}
			if got := h.status(http.MethodGet, "/api/widget/settings?key="+url.QueryEscape(key), "", nil); got != http.StatusOK {
				t.Errorf("옛 키를 붙인 위젯 설정은 200: %d", got)
			}
			if got := pageStatus("/widget?embed=1&key=" + url.QueryEscape(key)); got != http.StatusOK {
				t.Errorf("옛 키를 붙인 /widget 은 200: %d", got)
			}
		}
		secret, _ := h.get("/api/admin/workspace", h.adminToken())["member_secret"].(string)
		tok, _ := app.SignMember([]byte(secret), app.MemberClaims{MemberID: "m-" + id.New()[28:], Name: "옛 회원", Exp: time.Now().Add(time.Hour).Unix()})
		status, out := h.call(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": tok, "widget_key": "wk_옛설치코드의키"})
		if status != http.StatusOK {
			t.Fatalf("옛 키를 실은 회원 세션은 200: %d %v", status, out)
		}
		if ws := customerWorkspace(t, out); ws != core.DefaultWorkspaceID {
			t.Errorf("옛 키를 실은 회원도 기본 워크스페이스다: %s", ws)
		}
	})

	t.Run("옛 데스크가 workspace 를 실어 보내도 로그인은 200, 상담원 가입은 202 이고 다른 워크스페이스의 슬러그여도 기본 워크스페이스로 간다", func(t *testing.T) {
		// 옛 데스크의 로그인·가입 화면은 본문에 워크스페이스 슬러그를 싣는다. 본문 해석은 모르는 필드를 400 으로 거절하므로
		// 필드는 받되 읽지 않는다(D35). 실제로 있는 다른 워크스페이스의 슬러그를 실어도 그쪽으로 가지 않는다.
		ctx := context.Background()
		suffix := strings.ToLower(strings.ReplaceAll(id.New(), "-", "")[20:])
		otherSlug := "old-" + suffix
		otherOwner := "other-owner-" + suffix + "@example.com"
		if _, _, err := h.store.CreateWorkspaceWithAdmin(ctx, otherSlug, "다른 상점", otherOwner, "대표", ownerPassword); err != nil {
			t.Fatal(err)
		}
		homeEmail := "home-" + suffix + "@example.com"
		home, err := h.store.CreateAgent(ctx, core.DefaultWorkspaceID, homeEmail, "기본 상담원", ownerPassword)
		if err != nil {
			t.Fatal(err)
		}
		for _, slug := range []string{otherSlug, "없는-슬러그", ""} {
			status, out := h.call(http.MethodPost, "/api/agent/login", "", map[string]any{"workspace": slug, "email": homeEmail, "password": ownerPassword})
			agent, _ := out["agent"].(map[string]any)
			if status != http.StatusOK || agent["id"] != home.ID || agent["workspace_id"] != core.DefaultWorkspaceID {
				t.Errorf("workspace=%q 를 실은 로그인은 200 이고 기본 워크스페이스의 상담원이다: %d %v", slug, status, out)
			}
		}
		// 다른 워크스페이스의 관리자가 자기 슬러그를 실어도 그 워크스페이스로 로그인되지 않는다 — 로그인은 기본 워크스페이스만 본다.
		if status := h.status(http.MethodPost, "/api/agent/login", "", map[string]any{"workspace": otherSlug, "email": otherOwner, "password": ownerPassword}); status != http.StatusUnauthorized {
			t.Errorf("다른 워크스페이스의 계정은 슬러그를 실어도 401: %d", status)
		}

		signupEmail := "old-signup-" + suffix + "@example.com"
		t.Cleanup(func() {
			_, _ = h.store.Pool().Exec(context.Background(), "DELETE FROM agents WHERE lower(email) = lower($1) AND approved_at IS NULL", signupEmail)
		})
		status, out := h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"workspace": otherSlug, "email": signupEmail, "name": "옛 화면 신청자", "password": ownerPassword})
		if status != http.StatusAccepted {
			t.Fatalf("workspace 를 실은 상담원 가입은 202: %d %v", status, out)
		}
		var ws string
		if err := h.store.Pool().QueryRow(ctx, "SELECT workspace_id FROM agents WHERE lower(email) = lower($1)", signupEmail).Scan(&ws); err != nil {
			t.Fatalf("신청이 저장되지 않았다: %v", err)
		}
		if ws != core.DefaultWorkspaceID {
			t.Errorf("다른 워크스페이스의 슬러그를 실어도 신청은 기본 워크스페이스로 간다: %s", ws)
		}
	})

	t.Run("회사 가입 경로는 없다 — POST /api/signup 은 405 이고 워크스페이스가 생기지 않는다", func(t *testing.T) {
		// 이 주소를 받는 핸들러가 없다. 남은 것은 정적 파일의 GET 뿐이라 서버 다중화기가 405 로 답한다.
		count := func() int {
			var n int
			if err := h.store.Pool().QueryRow(context.Background(), "SELECT count(*) FROM workspaces").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		before := count()
		res := h.postRaw("/api/signup", "", map[string]any{"workspace": "가입 시도 " + id.New()[28:],
			"email": "signup-" + strings.ToLower(id.New()[28:]) + "@example.com", "name": "대표", "password": ownerPassword})
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("405 여야 한다: %d", res.StatusCode)
		}
		if after := count(); after != before {
			t.Errorf("워크스페이스가 생겼다: %d → %d", before, after)
		}
	})
}

func TestE2E_허용오리진과회원서명비밀(t *testing.T) {
	// 기본 워크스페이스의 허용 오리진과 회원 서명 비밀을 바꾼다. 공유 DB 의 다른 테스트가 흔들리지 않게 격리 DB 에서 돈다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()

	// 정책 문장을 바꿨다(D46): 전에는 "CSP 를 걸지 않는다" 였다. 지금은 모든 페이지에 script-src 'self' 를 걸고,
	// 허용 목록이 비어 있을 때 빠지는 것은 끼워 넣을 곳을 묶는 frame-ancestors 다.
	t.Run("허용 목록이 비어 있으면 어디서든 뜨고 frame-ancestors 를 걸지 않는다", func(t *testing.T) {
		if got := h.status(http.MethodPost, "/api/customer/session", "", map[string]any{"host_origin": "https://anywhere.example"}); got != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", got)
		}
		res := h.raw(http.MethodGet, "/widget", "")
		res.Body.Close()
		if csp := res.Header.Get("Content-Security-Policy"); strings.Contains(csp, "frame-ancestors") {
			t.Errorf("허용 목록이 비어 있으면 frame-ancestors 를 걸지 않는다: %q", csp)
		}
	})

	t.Run("허용 오리진 밖 사이트는 여전히 거절된다", func(t *testing.T) {
		h.put("/api/admin/workspace", tn.token, map[string]any{"name": "오리진 상점", "allowed_origins": []string{"https://shop.example.com"}})
		ok := h.status(http.MethodPost, "/api/customer/session", "", map[string]any{"host_origin": "https://shop.example.com"})
		bad := h.status(http.MethodPost, "/api/customer/session", "", map[string]any{"host_origin": "https://evil.example"})
		if ok != http.StatusOK || bad != http.StatusForbidden {
			t.Errorf("허용 %d / 거절 %d", ok, bad)
		}
		// 옛 설치 코드의 키를 실어도 오리진 판정은 그대로다.
		if got := h.status(http.MethodPost, "/api/customer/session", "", map[string]any{"widget_key": "wk_옛키", "host_origin": "https://evil.example"}); got != http.StatusForbidden {
			t.Errorf("옛 키를 실은 다른 사이트도 403: %d", got)
		}
		// 호스트 오리진을 안 보내는 것도 거절한다. 목록이 없을 때만 어디서든 된다.
		if got := h.status(http.MethodPost, "/api/customer/session", "", map[string]any{}); got != http.StatusForbidden {
			t.Errorf("호스트 오리진 없이 세션 생성은 403: %d", got)
		}
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": "x.y", "host_origin": "https://evil.example"}); got != http.StatusForbidden {
			t.Errorf("회원 세션도 403: %d", got)
		}
		// 진짜 문은 브라우저가 강제하는 CSP 다. 경로·파라미터·Referer 가 무엇이든 헤더가 붙는다.
		for _, path := range []string{"/widget?embed=1", "/widget", "/widget.html", "/widget.html?embed=1", "/widget?embed=1&key=wk_old"} {
			req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
			req.Header.Set("Referer", "https://evil.example/page")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			csp := res.Header.Get("Content-Security-Policy")
			if res.StatusCode != http.StatusOK || !strings.Contains(csp, "frame-ancestors") || !strings.Contains(csp, "https://shop.example.com") {
				t.Errorf("%s: status=%d csp=%q", path, res.StatusCode, csp)
			}
		}
	})

	t.Run("허용 목록이 있어도 공개 위젯 설정은 오리진 없이 읽힌다", func(t *testing.T) {
		// 색·제목·운영 여부는 비밀이 아니다. 세션 생성만 오리진을 본다.
		if got := h.status(http.MethodGet, "/api/widget/settings", "", nil); got != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", got)
		}
	})

	t.Run("오리진 모양이 틀리면 400", func(t *testing.T) {
		if got := h.status(http.MethodPut, "/api/admin/workspace", tn.token, map[string]any{"name": "x", "allowed_origins": []string{"shop.example.com/path"}}); got != http.StatusBadRequest {
			t.Errorf("%d", got)
		}
	})

	t.Run("회원 서명 비밀은 워크스페이스마다 다르고, 새로 발급하면 옛 토큰이 즉시 죽는다", func(t *testing.T) {
		sign := func(secret string) string {
			tok, _ := app.SignMember([]byte(secret), app.MemberClaims{MemberID: "m-1", Name: "회원", Exp: time.Now().Add(time.Hour).Unix()})
			return tok
		}
		secret, _ := h.get("/api/admin/workspace", tn.token)["member_secret"].(string)
		tok := sign(secret)
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": tok, "host_origin": "https://shop.example.com"}); got != http.StatusOK {
			t.Fatalf("자기 비밀로 서명한 토큰은 통한다: %d", got)
		}
		other := h.newTenant("남의 상점")
		otherSecret, _ := h.get("/api/admin/workspace", other.token)["member_secret"].(string)
		if otherSecret == "" || otherSecret == secret {
			t.Fatalf("워크스페이스마다 비밀이 달라야 한다: %q", otherSecret)
		}
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": sign(otherSecret), "host_origin": "https://shop.example.com"}); got != http.StatusBadRequest {
			t.Errorf("다른 워크스페이스의 비밀로 서명한 토큰은 안 맞아야 한다: %d", got)
		}
		h.post("/api/admin/workspace/rotate-member-secret", tn.token, nil)
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": tok, "host_origin": "https://shop.example.com"}); got != http.StatusBadRequest {
			t.Errorf("돌린 뒤 옛 토큰은 거절: %d", got)
		}
	})
}

// 플랜은 없다(D21). 설치형이라 상담원 수도, 한 달의 새 문의 수도 막지 않는다. 막는 것은 남용 방어(입구 한도)뿐이다.
func TestE2E_플랜없음(t *testing.T) {
	h := newHarness(t, time.Second, false)

	t.Run("상담원을 몇 명이든 초대·수락·승인·재활성화할 수 있다", func(t *testing.T) {
		tn := h.newTenant("큰 팀 상점")
		activeCount := func() int {
			var n int
			if err := h.store.Pool().QueryRow(context.Background(),
				"SELECT count(*) FROM agents WHERE workspace_id = $1 AND active", tn.wsID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		accept := func(token string) string {
			status, out := h.call(http.MethodPost, "/api/agent/invites/accept", "", map[string]any{"token": token, "password": "welcome123"})
			if status != http.StatusCreated {
				t.Fatalf("수락은 201 이어야 한다: %d %v", status, out)
			}
			ag, _ := out["agent"].(map[string]any)
			idv, _ := ag["id"].(string)
			return idv
		}
		// 관리자 1 + 수락 19 = 20명. 옛 free(3)·pro(15) 한도를 넘는다.
		var first string
		for i := 0; i < 19; i++ {
			agentID := accept(h.inviteToken(tn, "big-"+strings.ToLower(id.New()[28:])+"@x.com", "STAFF"))
			if i == 0 {
				first = agentID
			}
		}
		if got := activeCount(); got != 20 {
			t.Fatalf("활성 상담원이 20명이어야 한다: %d", got)
		}
		// 20명인 상태에서도 초대를 만든다. 수락은 아래에서 빈자리 없이 한다.
		spare := h.inviteToken(tn, "spare-"+strings.ToLower(id.New()[28:])+"@x.com", "PART_TIME")
		email := "join-" + strings.ToLower(id.New()[28:]) + "@x.com"
		if got := h.agentSignup(tn, email, "스물한째", "join12345"); got != http.StatusAccepted {
			t.Fatalf("가입 신청 202: %d", got)
		}
		if got, body := h.call(http.MethodPost, "/api/admin/agents/"+h.pendingIDOf(tn, email)+"/approve", tn.token, map[string]any{"role": "STAFF"}); got != http.StatusOK {
			t.Fatalf("21번째 승인은 200 이어야 한다: %d %v", got, body)
		}
		if got := h.status(http.MethodPost, "/api/admin/agents/"+first+"/active", tn.token, map[string]any{"active": false}); got >= 300 {
			t.Fatalf("비활성화: %d", got)
		}
		accept(spare)
		if got, body := h.call(http.MethodPost, "/api/admin/agents/"+first+"/active", tn.token, map[string]any{"active": true}); got >= 300 {
			t.Fatalf("22번째로 되살리는 재활성화가 막혔다: %d %v", got, body)
		}
		if got := activeCount(); got != 22 {
			t.Errorf("활성 상담원이 22명이어야 한다: %d", got)
		}
	})

	t.Run("한 달에 대화를 몇 개든 새로 열 수 있다", func(t *testing.T) {
		tn := h.newTenant("붐비는 문의 상점")
		custToken, _ := h.widgetCustomer(tn) // 1건째
		// 200건을 API 로 만들지 않는다. 이번 달 대화 199건을 SQL 로 깔아 200건을 채우고 201번째를 요청한다.
		testsupport.Exec(t, testsupport.URL(), `
			INSERT INTO conversations (id, workspace_id, customer_id)
			SELECT gen_random_uuid(), c.workspace_id, c.id FROM customers c, generate_series(1, 199)
			 WHERE c.workspace_id = $1`, tn.wsID)
		if got, body := h.call(http.MethodPost, "/api/customer/conversations", custToken, nil); got != http.StatusCreated {
			t.Fatalf("이번 달 201번째 새 문의도 201 이어야 한다: %d %v", got, body)
		}
		var n int
		if err := h.store.Pool().QueryRow(context.Background(), `
			SELECT count(*) FROM conversations
			 WHERE workspace_id = $1 AND created_at >= date_trunc('month', now())`, tn.wsID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 201 {
			t.Errorf("이번 달 대화가 201개여야 한다: %d", n)
		}
	})
}

// 연결 수·모델 이름은 운영 정보다. 고객 토큰으로는 안 보인다.
func TestE2E_고객은운영정보를못본다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("운영정보 상점")
	cust, _ := h.widgetCustomer(tn)
	me := h.get("/api/me", cust)
	if _, ok := me["connections"]; ok {
		t.Errorf("고객 응답에는 연결 수 키 자체가 없어야 한다: %+v", me)
	}
	if _, ok := me["draft_model"]; ok {
		t.Errorf("고객에게 모델 이름을 주면 안 된다: %+v", me)
	}
	// 상담원 응답은 운영 정보를 담을 수 있다. 연결이 0 이면 키가 생략되므로 소켓을 하나 열어 둔 상태로 본다.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := h.dial(ctx, tn.token)
	defer c.ws.Close(websocket.StatusNormalClosure, "")
	agent := h.get("/api/me", tn.token)
	if n, _ := agent["connections"].(float64); n < 1 {
		t.Errorf("상담원에게는 연결 수가 있다: %+v", agent)
	}
}

// 남의 client_msg_id 로 보내면 재전송이 아니라 거절이다. 본문도 새지 않는다.
func TestE2E_남의client_msg_id는400(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("멱등 상점")
	cust, convID := h.widgetCustomer(tn)
	cmid := id.New()
	h.post("/api/conversations/"+convID+"/messages", tn.token, map[string]any{"client_msg_id": cmid, "body": "내부: 카드번호 1234", "internal": true})
	res := h.postRaw("/api/conversations/"+convID+"/messages", cust, map[string]any{"client_msg_id": cmid, "body": "x"})
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || strings.Contains(string(raw), "카드번호") {
		t.Errorf("400 이고 본문이 새면 안 된다: %d %s", res.StatusCode, raw)
	}
}

func TestE2E_감사로그와내보내기와고객삭제(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("기록 상점")
	cust, convID := h.widgetCustomer(tn)
	h.post("/api/conversations/"+convID+"/messages", cust, map[string]any{"client_msg_id": id.New(), "body": "제 전화는 010-1234-5678"})
	h.post("/api/conversations/"+convID+"/messages", tn.token, map[string]any{"client_msg_id": id.New(), "body": "내부: VIP", "internal": true})

	t.Run("관리자 행동이 감사 로그에 남는다", func(t *testing.T) {
		h.put("/api/admin/workspace", tn.token, map[string]any{"name": "기록 상점2", "allowed_origins": []string{}})
		actions := map[string]bool{}
		for _, raw := range firstList(h.get("/api/admin/audit", tn.token), "entries") {
			e, _ := raw.(map[string]any)
			actions[e["action"].(string)] = true
		}
		for _, want := range []string{"workspace.update"} {
			if !actions[want] {
				t.Errorf("%s 가 감사 로그에 없다: %v", want, actions)
			}
		}
	})

	t.Run("내보내기는 자기 워크스페이스의 대화만, 내부 메모까지 담는다", func(t *testing.T) {
		other := h.newTenant("남의 상점")
		oc, oconv := h.widgetCustomer(other)
		h.post("/api/conversations/"+oconv+"/messages", oc, map[string]any{"client_msg_id": id.New(), "body": "남의 비밀"})
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/admin/export?format=csv", nil)
		req.Header.Set("Authorization", "Bearer "+tn.token)
		res, _ := http.DefaultClient.Do(req)
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body := string(raw)
		if !strings.Contains(body, "내부: VIP") || !strings.Contains(body, "010-1234-5678") {
			t.Errorf("자기 대화와 내부 메모가 있어야 한다:\n%s", body)
		}
		if strings.Contains(body, "남의 비밀") {
			t.Error("남의 워크스페이스 메시지가 섞였다")
		}
		h.post("/api/conversations/"+convID+"/messages", cust, map[string]any{"client_msg_id": id.New(), "body": `=HYPERLINK("http://evil","click")`})
		res, _ = http.DefaultClient.Do(req)
		raw, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(raw), `'=HYPERLINK`) {
			t.Error("수식으로 시작하는 본문은 따옴표를 앞에 붙여 무력화한다")
		}
		if got := h.status(http.MethodGet, "/api/admin/export", h.agentToken(), nil); got != http.StatusForbidden {
			t.Errorf("일반 상담원은 내보내지 못한다: %d", got)
		}
	})

	t.Run("고객을 삭제하면 개인정보는 사라지고 대화 번호는 그대로다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, tn.token)
		customerID, _ := conv["customer_id"].(string)
		before := conv["last_seq"]
		if got := h.status(http.MethodDelete, "/api/customers/"+customerID, tn.token, nil); got != http.StatusNoContent {
			t.Fatalf("삭제: %d", got)
		}
		c := h.get("/api/customers/"+customerID, tn.token)
		if c["display_name"] != "삭제된 고객" || c["email"] != nil {
			t.Errorf("익명화돼야 한다: %+v", c)
		}
		after := h.get("/api/conversations/"+convID, tn.token)
		if after["last_seq"] != before {
			t.Errorf("번호가 바뀌면 안 된다: %v → %v", before, after["last_seq"])
		}
		msgs := firstList(h.get("/api/conversations/"+convID+"/messages", tn.token), "messages")
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			if m["sender_kind"] == "CUSTOMER" && strings.Contains(m["body"].(string), "010-1234") {
				t.Error("고객 메시지 본문이 남아 있다")
			}
		}
		if got := h.status(http.MethodGet, "/api/me", cust, nil); got != http.StatusUnauthorized {
			t.Errorf("삭제된 고객의 세션은 죽어야 한다: %d", got)
		}
		seqs, _ := h.store.MessageSeqs(context.Background(), convID)
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("번호에 빈칸: %v", seqs)
			}
		}
	})
}
