package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 토큰 보안 묶음(D46)의 정책 테스트. 정책 문장은 docs/design.md 정책 목록의 686~704 와 같다.

// signedMember 는 기본 워크스페이스의 회원 서명 비밀(하네스가 고정값을 싣는다)로 회원 토큰을 만든다.
func signedMember(t *testing.T, memberID string, exp time.Time) string {
	t.Helper()
	tok, err := app.SignMember([]byte("test-member-secret"), app.MemberClaims{MemberID: memberID, Name: "김회원", Exp: exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// sessionRow 는 세션 행의 수명 칸이다. 토큰 평문은 DB 에 없으니 해시로 찾는다. 절대 수명·유휴 한도가 없으면 nil 이다.
func (h *harness) sessionRow(token string) (expires *time.Time, lastUsed time.Time, idle *time.Duration, ok bool) {
	h.t.Helper()
	var idleFor *time.Duration
	err := h.store.Pool().QueryRow(context.Background(),
		"SELECT expires_at, last_used_at, idle_for FROM sessions WHERE token_hash = $1", auth.HashToken(token),
	).Scan(&expires, &lastUsed, &idleFor)
	if err != nil {
		return nil, time.Time{}, nil, false
	}
	return expires, lastUsed, idleFor, true
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) customerSessionsOf(customerID string) int {
	h.t.Helper()
	var n int
	if err := h.store.Pool().QueryRow(context.Background(), "SELECT count(*) FROM sessions WHERE customer_id = $1", customerID).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// ── 정책 686 공유 PC ─────────────────────────────────────────────────

// 감사 실험 1: 회원 A 가 호스트에서 로그아웃(위젯 logout 을 부르지 않음)한 뒤 같은 브라우저의 다음 사람이 위젯을 열자
// 저장된 회원 세션이 익명 입구에서 그대로 이어져 A 의 상담("서울시 비밀구 123")이 보였다.
func TestE2E_익명입구는회원세션토큰을이어주지않고새익명고객을연다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	memberTok := signedMember(t, "shared-pc-"+id.New()[28:], time.Now().Add(time.Hour))
	sess := h.post("/api/customer/member-session", "", map[string]any{"member_token": memberTok})
	held, _ := sess["token"].(string)
	memberID, _ := sess["customer"].(map[string]any)["id"].(string)
	conv := h.startConversation(held)
	h.send(conv["id"].(string), held, "회원A의 주소: 서울시 비밀구 123", false)

	next := h.post("/api/customer/session", "", map[string]any{"token": held, "display_name": "다음 사람"})
	nextTok, _ := next["token"].(string)
	nextID, _ := next["customer"].(map[string]any)["id"].(string)
	if nextTok == "" || nextTok == held {
		t.Fatalf("익명 입구가 회원 세션 토큰을 그대로 돌려줬다: %v", next)
	}
	if nextID == memberID {
		t.Fatalf("익명 입구가 회원 고객(%s)으로 이었다", memberID)
	}
	if next["conversation"] != nil {
		t.Errorf("새 익명 고객에게 대화가 따라왔다: %v", next["conversation"])
	}
	list := h.get("/api/customer/conversations", nextTok)
	if got := firstList(list, "conversations"); len(got) != 0 {
		t.Errorf("다음 사람에게 회원의 상담 %d개가 보인다", len(got))
	}
	// 회원 세션 자체는 죽이지 않는다 — 회원은 회원 입구로 계속 들어온다.
	if code := h.status(http.MethodGet, "/api/me", held, nil); code != http.StatusOK {
		t.Errorf("회원 세션이 죽었다: %d", code)
	}

	// 대조: 익명 고객의 세션은 전처럼 이어 준다.
	anon := h.post("/api/customer/session", "", map[string]any{"display_name": "익명"})
	anonTok, _ := anon["token"].(string)
	again := h.post("/api/customer/session", "", map[string]any{"token": anonTok})
	if again["token"] != anonTok {
		t.Errorf("익명 세션을 이어 주지 않았다: %v", again["token"])
	}
}

// ── 정책 688~690·704 회원 세션 수명 ───────────────────────────────────────────

func TestE2E_회원토큰으로연세션은세션수명과회원토큰만료중이른쪽에끝난다(t *testing.T) {
	h := newHarness(t, time.Second, false) // 하네스의 세션 수명 상한은 1시간이다
	cases := []struct {
		name string
		exp  time.Duration
		want time.Duration
	}{
		{"회원 토큰이 10분 남았으면 세션도 10분", 10 * time.Minute, 10 * time.Minute},
		{"회원 토큰이 3시간 남았으면 세션은 상한 1시간", 3 * time.Hour, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := signedMember(t, "life-"+id.New()[28:], time.Now().Add(tc.exp))
			sess := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
			expires, _, _, ok := h.sessionRow(sess["token"].(string))
			if !ok {
				t.Fatal("세션 행이 없다")
			}
			if expires == nil {
				t.Fatal("회원 세션에 절대 수명이 없다 — 회원 토큰 exp 에 묶이지 않았다")
			}
			if left := time.Until(*expires); left > tc.want+5*time.Second || left < tc.want-time.Minute {
				t.Errorf("남은 수명 %s, 기대 %s", left.Round(time.Second), tc.want)
			}
		})
	}
}

func TestE2E_회원서명비밀을새로발급하면회원고객의세션이끝나고익명고객과상담원의세션은산다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	member := h.memberSession(tn)
	anon, _ := h.customerSession()
	if code := h.status(http.MethodGet, "/api/me", member, nil); code != http.StatusOK {
		t.Fatalf("회원 세션이 처음부터 죽어 있다: %d", code)
	}
	h.post("/api/admin/workspace/rotate-member-secret", tn.token, nil)
	if code := h.status(http.MethodGet, "/api/me", member, nil); code != http.StatusUnauthorized {
		t.Errorf("비밀을 새로 받았는데 옛 비밀로 연 회원 세션이 산다: %d", code)
	}
	if code := h.status(http.MethodGet, "/api/me", anon, nil); code != http.StatusOK {
		t.Errorf("익명 고객 세션까지 죽었다: %d", code)
	}
	if code := h.status(http.MethodGet, "/api/me", tn.token, nil); code != http.StatusOK {
		t.Errorf("관리자 세션까지 죽었다: %d", code)
	}
}

func TestE2E_같은회원이identify를되풀이해도세션은쌓이지않는다_같은브라우저는이어쓰고상한은20개다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tok := signedMember(t, "repeat-"+id.New()[28:], time.Now().Add(time.Hour))
	first := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
	held, _ := first["token"].(string)
	customerID, _ := first["customer"].(map[string]any)["id"].(string)

	// 같은 브라우저(위젯이 들고 있는 그 회원의 세션)는 이어 쓴다 — 페이지마다 identify 해도 한 행이다.
	for range 5 {
		again := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok, "token": held})
		if again["token"] != held {
			t.Fatalf("들고 온 회원 세션을 이어 쓰지 않고 새로 만들었다")
		}
	}
	if n := h.customerSessionsOf(customerID); n != 1 {
		t.Errorf("같은 브라우저의 identify 5번에 세션 %d개", n)
	}
	// 남의 세션(다른 회원·익명)을 들고 오면 이어 쓰지 않는다.
	anon, _ := h.customerSession()
	other := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok, "token": anon})
	if other["token"] == anon {
		t.Error("익명 세션을 회원 세션으로 이어 썼다")
	}
	// 저장소가 막힌 브라우저처럼 매번 새로 받아도 상한 20개를 넘지 않는다. 오래된 것부터 지운다.
	for range 30 {
		h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
	}
	if n := h.customerSessionsOf(customerID); n != 20 {
		t.Errorf("회원 세션 %d개 — 상한은 20개다", n)
	}
	if code := h.status(http.MethodGet, "/api/me", held, nil); code != http.StatusUnauthorized {
		t.Errorf("상한을 넘겨 지워진 가장 오래된 세션이 산다: %d", code)
	}
}

// 감사 20 A02: 회원 입구의 "같은 회원인가" 대조를 빼도 통과했다. 공유 PC 에서 회원 A 의 세션 토큰이 위젯 저장소에 남은 채 회원 B 가 로그인하면,
// 회원 입구가 A 의 세션을 이어 써 B 의 위젯이 A 의 상담을 본다. 회원 입구는 바로 그 회원의 세션만 잇는다.
func TestE2E_회원입구는다른회원의세션을이어쓰지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tokA := signedMember(t, "member-a-"+id.New()[28:], time.Now().Add(time.Hour))
	a := h.post("/api/customer/member-session", "", map[string]any{"member_token": tokA})
	heldA, _ := a["token"].(string)
	idA, _ := a["customer"].(map[string]any)["id"].(string)
	conv := h.startConversation(heldA)
	h.send(conv["id"].(string), heldA, "회원A의 주문 번호 1234", false)

	tokB := signedMember(t, "member-b-"+id.New()[28:], time.Now().Add(time.Hour))
	b := h.post("/api/customer/member-session", "", map[string]any{"member_token": tokB, "token": heldA})
	nextB, _ := b["token"].(string)
	idB, _ := b["customer"].(map[string]any)["id"].(string)
	if nextB == "" || nextB == heldA {
		t.Fatalf("회원 B 의 입구가 회원 A 의 세션 토큰을 이어 줬다: %v", b["token"])
	}
	if idB == "" || idB == idA {
		t.Fatalf("회원 B 가 회원 A(%s)로 들어왔다", idA)
	}
	me := h.get("/api/me", nextB)
	if me["customer_id"] == idA || me["id"] == idA {
		t.Errorf("B 의 세션 주체가 A 다: %v", me)
	}
	if got := firstList(h.get("/api/customer/conversations", nextB), "conversations"); len(got) != 0 {
		t.Errorf("회원 B 에게 회원 A 의 상담 %d개가 보인다", len(got))
	}
}

// ── 정책 691·692 보안 헤더 ───────────────────────────────────────────────

func TestE2E_모든응답은Referer를보내지않고내용추측을막으며API응답은저장하지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	agent := h.agentToken()
	cases := []struct {
		method, path, token string
		noStore             bool
	}{
		{http.MethodGet, "/api/me", agent, true},
		{http.MethodGet, "/api/me", "", true},                    // 401 도
		{http.MethodPost, "/api/ws-ticket", agent, true},         // 티켓
		{http.MethodGet, "/api/attachments/x/url", agent, true},  // 오류 응답
		{http.MethodPost, "/api/customer/session", "", true},     // 토큰 없는 입구
		{http.MethodGet, "/ws", "", true},                        // 업그레이드 전 거절
		{http.MethodGet, "/healthz", "", true},                   // 상태 확인
		{http.MethodGet, "/chat.js", "", false},                  // 정적 자산은 no-cache + ETag
		{http.MethodGet, "/desk", "", false},                     // 페이지
		{http.MethodGet, "/widget", "", false},                   // 페이지
		{http.MethodGet, "/", "", false},                         // 첫 화면
		{http.MethodPost, "/api/agent/logout-nowhere", "", true}, // 없는 API 도
		{http.MethodGet, "/api/agent/nowhere", agent, true},      // 없는 API 의 GET 404 도(감사 20 — 정적 서버가 Cache-Control 을 지웠다)
		{http.MethodPost, "/ws", "", true},                       // /ws 의 405 도
		{http.MethodGet, "/api", agent, true},                    // 슬래시 없는 /api 도(감사 21 — 정적 서버의 404 였다)
		{http.MethodPost, "/api", "", true},
	}
	for _, c := range cases {
		res := h.roundTrip(c.method, c.path, c.token, map[string]any{}, nil)
		res.Body.Close()
		hd := res.Header
		if hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, hd.Get("Referrer-Policy"))
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, hd.Get("X-Content-Type-Options"))
		}
		cc := hd.Get("Cache-Control")
		if c.noStore && cc != "no-store" {
			t.Errorf("%s %s: Cache-Control=%q, no-store 여야 한다", c.method, c.path, cc)
		}
		if !c.noStore && (cc != "no-cache" || hd.Get("ETag") == "") {
			t.Errorf("%s %s: 정적 자산은 no-cache + ETag 그대로다: %q %q", c.method, c.path, cc, hd.Get("ETag"))
		}
	}
	// 없는 API 의 GET 은 페이지가 아니라 JSON 404 다
	for _, path := range []string{"/api/agent/nowhere", "/api"} {
		if res := h.roundTrip(http.MethodGet, path, agent, nil, nil); res.StatusCode != http.StatusNotFound || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Errorf("없는 API GET %s: %d %q", path, res.StatusCode, res.Header.Get("Content-Type"))
		} else {
			res.Body.Close()
		}
	}
	// 붙은 WebSocket 의 업그레이드 응답(101)도 저장하지 않는다
	ctx := testCtx(t, 5*time.Second)
	ws, up, err := websocket.Dial(ctx, h.wsURL()+"?ticket="+h.wsTicket(agent), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	if up.Header.Get("Cache-Control") != "no-store" || up.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("WebSocket 업그레이드 응답: Cache-Control=%q Referrer-Policy=%q", up.Header.Get("Cache-Control"), up.Header.Get("Referrer-Policy"))
	}
	// 로그아웃(204)도
	res := h.roundTrip(http.MethodPost, "/api/logout", h.agentToken(), nil, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("로그아웃 %d Cache-Control=%q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
}

func TestE2E_HTML페이지는우리출처의스크립트만돌린다_데스크는어디에도끼워넣을수없다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	for _, path := range []string{"/", "/index.html", "/desk", "/desk.html", "/widget", "/widget.html?embed=1"} {
		res := h.raw(http.MethodGet, path, "")
		res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'self'", "object-src 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP 에 %q 가 없다: %q", path, want, csp)
			}
		}
		if strings.HasPrefix(path, "/desk") && !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: 데스크는 frame-ancestors 'none' 이어야 한다: %q", path, csp)
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: 인라인·eval 을 열었다: %q", path, csp)
		}
	}
}

// ── 정책 693 초대 링크 ───────────────────────────────────────────────

func TestE2E_초대토큰은주소쿼리로오가지않는다_미리보기는POST본문이고관리자초대는24시간이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	admin := h.adminToken()
	invite := func(role string) (map[string]any, string) {
		out := h.post("/api/admin/invites", admin, map[string]any{"email": "inv-" + strings.ToLower(id.New()[28:]) + "@example.com", "name": "초대", "role": role})
		url, _ := out["accept_url"].(string)
		if !strings.Contains(url, "/desk#invite=") || strings.Contains(url, "?") {
			t.Fatalf("수락 링크의 토큰이 주소 조각에 있지 않다: %q", url)
		}
		return out["invite"].(map[string]any), url[strings.Index(url, "#invite=")+len("#invite="):]
	}
	staff, staffTok := invite("STAFF")
	adm, _ := invite("ADMIN")

	// GET 경로는 없다. 정적 파일 서버의 404 다(초대를 보이지 않는다).
	if code := h.status(http.MethodGet, "/api/agent/invites/preview?token="+staffTok, "", nil); code != http.StatusNotFound {
		t.Errorf("토큰을 쿼리로 받는 옛 미리보기가 %d 로 답한다 — 그 길은 없다(404)", code)
	}
	pre := h.post("/api/agent/invites/preview", "", map[string]any{"token": staffTok})
	if pre["role"] != "STAFF" {
		t.Errorf("POST 미리보기: %v", pre)
	}
	left := func(inv map[string]any) time.Duration {
		at, _ := time.Parse(time.RFC3339, inv["expires_at"].(string))
		return time.Until(at)
	}
	if l := left(adm); l > 24*time.Hour || l < 24*time.Hour-time.Minute {
		t.Errorf("관리자 초대 수명 %s — 24시간이어야 한다", l.Round(time.Minute))
	}
	if l := left(staff); l > 72*time.Hour || l < 72*time.Hour-time.Minute {
		t.Errorf("직원 초대 수명 %s — 72시간 그대로다", l.Round(time.Minute))
	}
}

// ── 정책 694·696 WebSocket 티켓 ──────────────────────────────────────────

func TestE2E_WebSocket티켓은30초일회용이고세션에묶인다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 20*time.Second)
	dialStatus := func(query string) int {
		ws, res, err := websocket.Dial(ctx, h.wsURL()+query, nil)
		if err == nil {
			_ = ws.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if res == nil {
			t.Fatalf("응답 없이 실패: %v", err)
		}
		return res.StatusCode
	}

	t.Run("티켓은 세션 토큰으로만 받는다", func(t *testing.T) {
		if code := h.status(http.MethodPost, "/api/ws-ticket", "", nil); code != http.StatusUnauthorized {
			t.Errorf("토큰 없이 티켓: %d", code)
		}
	})

	t.Run("수명은 30초다", func(t *testing.T) {
		out := h.post("/api/ws-ticket", h.agentToken(), nil)
		at, _ := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
		if l := time.Until(at); l > 30*time.Second || l < 25*time.Second {
			t.Errorf("티켓 수명 %s", l)
		}
	})

	t.Run("한 번 쓰면 끝이다", func(t *testing.T) {
		ticket := h.wsTicket(h.agentToken())
		if code := dialStatus("?ticket=" + ticket); code != http.StatusSwitchingProtocols {
			t.Fatalf("처음 쓰는 티켓: %d", code)
		}
		if code := dialStatus("?ticket=" + ticket); code != http.StatusUnauthorized {
			t.Errorf("이미 쓴 티켓으로 다시 붙었다: %d", code)
		}
	})

	t.Run("동시에 두 번 써도 하나만 붙는다", func(t *testing.T) {
		for range 5 {
			ticket := h.wsTicket(h.agentToken())
			var wg sync.WaitGroup
			codes := make([]int, 8)
			for i := range codes {
				wg.Go(func() { codes[i] = dialStatus("?ticket=" + ticket) })
			}
			wg.Wait()
			ok := 0
			for _, c := range codes {
				if c == http.StatusSwitchingProtocols {
					ok++
				}
			}
			if ok != 1 {
				t.Fatalf("같은 티켓으로 %d번 붙었다: %v", ok, codes)
			}
		}
	})

	t.Run("만료된 티켓은 쓸 수 없다", func(t *testing.T) {
		ticket := h.wsTicket(h.agentToken())
		h.exec("UPDATE ws_tickets SET expires_at = now() - interval '1 second' WHERE ticket_hash = $1", auth.HashToken(ticket))
		if code := dialStatus("?ticket=" + ticket); code != http.StatusUnauthorized {
			t.Errorf("만료된 티켓으로 붙었다: %d", code)
		}
	})

	t.Run("세션이 끝나면 그 세션의 티켓도 끝난다", func(t *testing.T) {
		session := h.agentToken()
		ticket := h.wsTicket(session)
		h.post("/api/logout", session, nil)
		if code := dialStatus("?ticket=" + ticket); code != http.StatusUnauthorized {
			t.Errorf("로그아웃한 세션의 티켓으로 붙었다: %d", code)
		}
		// 비활성 상담원(세션 행이 남아 있어도)의 티켓도 마찬가지다.
		agent := testsupport.Agent(t, h.store)
		login := h.post("/api/agent/login", "", map[string]any{"email": agent.Email, "password": "chat1234"})
		ticket = h.wsTicket(login["token"].(string))
		h.exec("UPDATE agents SET active = false WHERE id = $1", agent.ID)
		if code := dialStatus("?ticket=" + ticket); code != http.StatusUnauthorized {
			t.Errorf("비활성 상담원의 티켓으로 붙었다: %d", code)
		}
	})

	t.Run("없는 티켓은 401 이다", func(t *testing.T) {
		if code := dialStatus("?ticket=nope"); code != http.StatusUnauthorized {
			t.Errorf("없는 티켓: %d", code)
		}
	})
}

func TestE2E_옛WebSocket쿼리토큰은아직받고사건으로남기되토큰값은남기지않는다(t *testing.T) {
	h, logs := newLoggedHarness(t)
	ctx := testCtx(t, 10*time.Second)
	customer, convID := h.customerSession()
	c := h.dialToken(ctx, customer)
	if ready := c.subscribe(ctx, convID, 0); ready.Type != "ready" {
		t.Fatalf("옛 길로 붙은 연결이 구독하지 못했다: %+v", ready)
	}
	line := logs.await(t, func(l logLine) bool { return l.str("msg") == "ws.token_in_query" })
	if line.str("actor_kind") != "customer" || line.str("actor_id") == "" {
		t.Errorf("사건에 행위자 id 가 없다: %v", line)
	}
	if strings.Contains(logs.String(), customer) {
		t.Error("로그에 세션 토큰 값이 남았다")
	}
	// 티켓 길은 이 사건을 남기지 않는다.
	before := len(logs.where(t, "msg", "ws.token_in_query"))
	h.dial(ctx, customer)
	time.Sleep(200 * time.Millisecond)
	if after := len(logs.where(t, "msg", "ws.token_in_query")); after != before {
		t.Errorf("티켓으로 붙었는데 옛 길 사건이 남았다: %d → %d", before, after)
	}
}

// ── 정책 697 유휴 만료 ───────────────────────────────────────────────

// 운영 기본값(설정의 기본과 같은 리터럴): 상담원 12시간, 고객은 유휴 한도가 없다(D46 고침).
var defaultSessionPolicy = app.SessionPolicy{CustomerIdle: 0, AgentIdle: 12 * time.Hour}

func TestE2E_상담원세션은유휴한도동안쓰지않으면끝나고쓸때마다1분단위로사용시각이오른다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	h.svc.SetSessionPolicy(defaultSessionPolicy)
	t.Cleanup(func() { h.svc.SetSessionPolicy(app.SessionPolicy{}) })
	agent := h.agentToken()

	_, _, idle, _ := h.sessionRow(agent)
	if idle == nil || *idle != 12*time.Hour {
		t.Fatalf("상담원 세션의 유휴 한도: %v", idle)
	}

	t.Run("1분 안에는 다시 쓰지 않는다", func(t *testing.T) {
		h.exec("UPDATE sessions SET last_used_at = now() - interval '30 seconds' WHERE token_hash = $1", auth.HashToken(agent))
		_, before, _, _ := h.sessionRow(agent)
		h.get("/api/me", agent)
		if _, after, _, _ := h.sessionRow(agent); !after.Equal(before) {
			t.Errorf("30초 전에 쓴 세션의 사용 시각을 다시 썼다: %s → %s", before, after)
		}
	})

	t.Run("1분이 지났으면 쓸 때 사용 시각이 오른다", func(t *testing.T) {
		h.exec("UPDATE sessions SET last_used_at = now() - interval '11 hours' WHERE token_hash = $1", auth.HashToken(agent))
		h.get("/api/me", agent)
		if _, after, _, _ := h.sessionRow(agent); time.Since(after) > time.Minute {
			t.Errorf("쓴 세션의 사용 시각이 그대로다: %s 전", time.Since(after).Round(time.Second))
		}
	})

	t.Run("12시간을 넘기면 절대 수명이 남아도 끝나고 정리가 지운다", func(t *testing.T) {
		h.exec("UPDATE sessions SET last_used_at = now() - interval '11 hours 59 minutes' WHERE token_hash = $1", auth.HashToken(agent))
		if code := h.status(http.MethodGet, "/api/me", agent, nil); code != http.StatusOK {
			t.Errorf("12시간이 안 된 상담원 세션: %d", code)
		}
		h.exec("UPDATE sessions SET last_used_at = now() - interval '12 hours 1 minute' WHERE token_hash = $1", auth.HashToken(agent))
		if code := h.status(http.MethodGet, "/api/me", agent, nil); code != http.StatusUnauthorized {
			t.Errorf("12시간 넘게 안 쓴 상담원 세션: %d", code)
		}
		if _, err := h.store.DeleteExpiredSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, _, _, ok := h.sessionRow(agent); ok {
			t.Error("유휴가 지난 상담원 세션을 정리가 지우지 않았다")
		}
	})

	t.Run("고객 세션에는 기본으로 유휴 한도가 없다", func(t *testing.T) {
		customer, _ := h.customerSession()
		if _, _, idle, _ := h.sessionRow(customer); idle != nil {
			t.Errorf("고객 세션의 유휴 한도: %v", *idle)
		}
	})
}

// ── 정책 702·703 고객 세션은 상담이 이어지는 동안 끝나지 않는다(D46 고침) ─────────

// shiftSessions 는 그 세션들의 시각을 days 일 앞으로 돌린다 — 그만큼 시간이 흐른 것과 같다.
func (h *harness) shiftSessions(days int, tokens ...string) {
	h.t.Helper()
	for _, tok := range tokens {
		h.exec(`UPDATE sessions SET created_at = created_at - make_interval(days => $2),
		               last_used_at = last_used_at - make_interval(days => $2),
		               expires_at = expires_at - make_interval(days => $2)
		         WHERE token_hash = $1`, auth.HashToken(tok), days)
	}
}

// 교환·반품처럼 브랜드사 처리가 길어지면 고객은 몇 주~몇 달 뒤 같은 브라우저로 돌아온다(logcat 2026-10-08).
// 그때 위젯이 들고 있는 토큰이 죽어 있으면 새 익명 고객이 되어 지난 상담을 못 본다. 익명 고객 세션은 로그아웃과 고객 삭제로만 끝난다.
func TestE2E_익명고객세션은만료되지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	h.svc.SetSessionPolicy(defaultSessionPolicy)
	t.Cleanup(func() { h.svc.SetSessionPolicy(app.SessionPolicy{}) })
	ctx := context.Background()

	customer, convID := h.customerSession()
	h.send(convID, customer, "교환 접수했어요. 브랜드 답 기다릴게요", false)
	_, usedAt, _, _ := h.sessionRow(customer)

	t.Run("100일 뒤에도 같은 토큰으로 위젯이 이어지고 지난 상담이 보인다", func(t *testing.T) {
		h.shiftSessions(100, customer)
		if _, err := h.store.DeleteExpiredSessions(ctx); err != nil {
			t.Fatal(err)
		}
		again := h.post("/api/customer/session", "", map[string]any{"token": customer})
		if again["token"] != customer {
			t.Fatalf("100일 뒤 같은 브라우저인데 새 세션을 받았다: %v", again["token"])
		}
		list := firstList(h.get("/api/customer/conversations", customer), "conversations")
		if len(list) != 1 || list[0].(map[string]any)["id"] != convID {
			t.Fatalf("지난 상담이 보이지 않는다: %v", list)
		}
		msgs := h.messagesOf(convID, customer)
		if len(msgs) == 0 || msgs[len(msgs)-1]["body"] != "교환 접수했어요. 브랜드 답 기다릴게요" {
			t.Errorf("지난 상담의 글이 보이지 않는다: %v", msgs)
		}
		if code := h.status(http.MethodPost, "/api/ws-ticket", customer, nil); code != http.StatusOK {
			t.Errorf("100일 뒤 WebSocket 티켓: %d", code)
		}
	})

	// 감사 21: 고객 유휴 한도는 판정 시점의 설정이다. 지금 한도가 없어도 사용 시각을 적어야 나중에 켰을 때 마지막으로 쓴 때부터 잰다.
	t.Run("유휴 한도가 없어도 쓰면 사용 시각을 적는다", func(t *testing.T) {
		if _, after, _, _ := h.sessionRow(customer); time.Since(after) > time.Minute {
			t.Errorf("익명 고객 세션을 썼는데 사용 시각이 100일 전 그대로다: %s (처음 %s)", after, usedAt)
		}
	})

	t.Run("로그아웃하면 끝난다", func(t *testing.T) {
		other, _ := h.customerSession()
		h.post("/api/logout", other, nil)
		if code := h.status(http.MethodGet, "/api/me", other, nil); code != http.StatusUnauthorized {
			t.Errorf("로그아웃한 익명 고객 세션: %d", code)
		}
	})

	t.Run("고객을 지우면 끝난다", func(t *testing.T) {
		sess := h.post("/api/customer/session", "", map[string]any{"display_name": "지울 고객"})
		other, _ := sess["token"].(string)
		custID, _ := sess["customer"].(map[string]any)["id"].(string)
		if code := h.status(http.MethodDelete, "/api/customers/"+custID, h.adminToken(), nil); code >= 300 {
			t.Fatalf("고객 삭제: %d", code)
		}
		if code := h.status(http.MethodGet, "/api/me", other, nil); code != http.StatusUnauthorized {
			t.Errorf("지운 고객의 세션: %d", code)
		}
	})
}

// 감사 21: 고객 유휴 한도를 세션 행에 적었더니 CHAT_SESSION_IDLE_CUSTOMER 를 켜도 그 전에 연 세션(기본 0 으로 연 익명·회원 전부)에는 걸리지 않았다.
// 사고 때 "오래 버려진 고객 토큰을 한 번에 줄인다" 는 설정의 뜻이 서지 않는다. 판정은 그때의 설정으로 한다.
func TestE2E_고객유휴한도를켜면그전에연익명과회원세션모두에걸린다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	h.svc.SetSessionPolicy(defaultSessionPolicy)
	t.Cleanup(func() { h.svc.SetSessionPolicy(app.SessionPolicy{}) })
	ctx := context.Background()

	anon, _ := h.customerSession()
	mem := h.post("/api/customer/member-session", "", map[string]any{"member_token": signedMember(t, "idle-"+id.New()[28:], time.Now().Add(24*time.Hour))})
	member, _ := mem["token"].(string)
	fresh, _ := h.customerSession()
	agent := h.agentToken()
	// 저장 방식(행의 idle_for)은 보지 않는다. 정책은 관찰 결과다 — 설정이 0 이면 200, 켜면 그 전에 연 세션도 401(감사 22).
	idleFor := func(tok string, ago string) {
		h.exec("UPDATE sessions SET last_used_at = now() - $2::interval WHERE token_hash = $1", auth.HashToken(tok), ago)
	}
	idleFor(anon, "3 minutes")
	idleFor(member, "3 minutes")
	idleFor(fresh, "1 minute")
	idleFor(agent, "3 minutes")

	t.Run("설정이 0 이면 오래 안 쓴 고객 세션도 산다", func(t *testing.T) {
		for _, tok := range []string{anon, member} {
			if code := h.status(http.MethodGet, "/api/me", tok, nil); code != http.StatusOK {
				t.Errorf("한도가 없는데 끝났다: %d", code)
			}
		}
		idleFor(anon, "3 minutes")
		idleFor(member, "3 minutes")
	})

	h.svc.SetSessionPolicy(app.SessionPolicy{CustomerIdle: 2 * time.Minute, AgentIdle: 12 * time.Hour})

	t.Run("켜면 그 전에 연 익명·회원 세션이 그 한도로 끝난다", func(t *testing.T) {
		for name, tok := range map[string]string{"익명": anon, "회원": member} {
			if code := h.status(http.MethodGet, "/api/me", tok, nil); code != http.StatusUnauthorized {
				t.Errorf("%s: 2분 한도에 3분 안 쓴 세션이 %d", name, code)
			}
			if code := h.status(http.MethodPost, "/api/ws-ticket", tok, nil); code != http.StatusUnauthorized {
				t.Errorf("%s: 끝난 세션에 WebSocket 티켓을 줬다: %d", name, code)
			}
		}
		again := h.post("/api/customer/session", "", map[string]any{"token": anon})
		if again["token"] == anon {
			t.Error("유휴 한도가 지난 익명 세션을 익명 입구가 이어 줬다")
		}
	})

	t.Run("한도 안에서 쓴 고객 세션과 상담원 세션은 산다", func(t *testing.T) {
		if code := h.status(http.MethodGet, "/api/me", fresh, nil); code != http.StatusOK {
			t.Errorf("1분 전에 쓴 고객 세션: %d", code)
		}
		if code := h.status(http.MethodGet, "/api/me", agent, nil); code != http.StatusOK {
			t.Errorf("상담원 세션은 상담원 한도(12시간)다: %d", code)
		}
	})

	t.Run("정리가 같은 판정으로 지운다", func(t *testing.T) {
		if _, err := h.store.DeleteExpiredSessions(ctx); err != nil {
			t.Fatal(err)
		}
		for name, tok := range map[string]string{"익명": anon, "회원": member} {
			if _, _, _, ok := h.sessionRow(tok); ok {
				t.Errorf("%s: 유휴 한도가 지난 고객 세션을 정리가 남겼다", name)
			}
		}
		for name, tok := range map[string]string{"한도 안의 고객": fresh, "상담원": agent} {
			if _, _, _, ok := h.sessionRow(tok); !ok {
				t.Errorf("%s 세션을 정리가 지웠다", name)
			}
		}
	})
}

// 회원 세션은 회원 토큰 exp 에 묶이고 비밀 재발급 때 지워진다(보안 경계). 그래도 상담은 회원 번호에 묶여 있어,
// 세션이 끝난 뒤 호스트가 새 회원 토큰으로 다시 identify 하면 같은 고객으로 들어와 지난 상담이 이어진다.
func TestE2E_회원은세션이끝나도다시identify하면지난상담이이어진다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	h.svc.SetSessionPolicy(defaultSessionPolicy)
	t.Cleanup(func() { h.svc.SetSessionPolicy(app.SessionPolicy{}) })
	ctx := context.Background()
	memberID := "returning-" + id.New()[28:]

	first := h.post("/api/customer/member-session", "", map[string]any{"member_token": signedMember(t, memberID, time.Now().Add(10*time.Minute))})
	held, _ := first["token"].(string)
	customerID, _ := first["customer"].(map[string]any)["id"].(string)
	conv := h.startConversation(held)
	convID, _ := conv["id"].(string)
	h.send(convID, held, "반품 요청합니다", false)

	comeBack := func(t *testing.T, secret []byte, oldToken string) string {
		t.Helper()
		tok, err := app.SignMember(secret, app.MemberClaims{MemberID: memberID, Name: "김회원", Exp: time.Now().Add(time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		again := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok, "token": oldToken})
		next, _ := again["token"].(string)
		if next == "" || next == oldToken {
			t.Fatalf("끝난 세션을 다시 돌려줬다: %v", again["token"])
		}
		if got, _ := again["customer"].(map[string]any)["id"].(string); got != customerID {
			t.Fatalf("다른 고객(%s)으로 들어왔다, 원래 %s", got, customerID)
		}
		list := firstList(h.get("/api/customer/conversations", next), "conversations")
		if len(list) != 1 || list[0].(map[string]any)["id"] != convID {
			t.Fatalf("지난 상담이 이어지지 않았다: %v", list)
		}
		msgs := h.messagesOf(convID, next)
		if len(msgs) == 0 || msgs[len(msgs)-1]["body"] != "반품 요청합니다" {
			t.Errorf("지난 상담의 글이 보이지 않는다: %v", msgs)
		}
		return next
	}

	t.Run("회원 토큰 exp 가 지나 세션이 끝나도", func(t *testing.T) {
		h.shiftSessions(1, held)
		if _, err := h.store.DeleteExpiredSessions(ctx); err != nil {
			t.Fatal(err)
		}
		if code := h.status(http.MethodGet, "/api/me", held, nil); code != http.StatusUnauthorized {
			t.Fatalf("회원 토큰 exp 가 지났는데 세션이 산다: %d", code)
		}
		held = comeBack(t, []byte("test-member-secret"), held)
	})

	t.Run("회원 서명 비밀을 새로 발급해 세션이 끝나도", func(t *testing.T) {
		tn := h.homeTenant()
		h.post("/api/admin/workspace/rotate-member-secret", tn.token, nil)
		if code := h.status(http.MethodGet, "/api/me", held, nil); code != http.StatusUnauthorized {
			t.Fatalf("비밀을 새로 받았는데 회원 세션이 산다: %d", code)
		}
		secret, _ := h.get("/api/admin/workspace", tn.token)["member_secret"].(string)
		comeBack(t, []byte(secret), held)
	})
}

// ── 정책 698 로그아웃·비활성 때 열린 연결 ─────────────────────────────

// 감사 실험 2·3: 상담원이 로그아웃하거나 비활성이 된 뒤에도 그 세션으로 열린 WebSocket 이 다음 세션 재확인(최대 60초)까지 열려 있었다.
// 재확인 주기는 운영 값(60초) 그대로다. 5초 안에 닫히면 알림 길이 닫은 것이다.
func TestE2E_로그아웃하거나비활성이되면그세션의열린WebSocket이곧바로닫힌다(t *testing.T) {
	h := newHarness(t, time.Second, true)
	admin := h.adminToken()
	_, convID := h.customerSession()

	awaitClosed := func(t *testing.T, c *client) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gotUnauthorized := false
		for {
			_, data, err := c.ws.Read(ctx)
			if err != nil {
				if ctx.Err() != nil {
					t.Fatal("5초 안에 닫히지 않았다 — 다음 재확인(60초)까지 열려 있다")
				}
				if code := websocket.CloseStatus(err); code != websocket.StatusNormalClosure {
					t.Errorf("닫는 코드 %d, 1000 이어야 한다", code)
				}
				if !gotUnauthorized {
					t.Error("unauthorized 를 알리지 않고 닫았다")
				}
				return
			}
			if strings.Contains(string(data), `"unauthorized"`) {
				gotUnauthorized = true
			}
		}
	}

	t.Run("로그아웃", func(t *testing.T) {
		agent := h.agentToken()
		c := h.dial(testCtx(t, 10*time.Second), agent)
		c.subscribe(testCtx(t, 5*time.Second), convID, 0)
		other := h.agentToken() // 다른 세션(같은 계정이 아닐 수도 있다)의 연결은 그대로다
		o := h.dial(testCtx(t, 10*time.Second), other)
		o.subscribe(testCtx(t, 5*time.Second), convID, 0)
		h.post("/api/logout", agent, nil)
		awaitClosed(t, c)
		// 살아 있는 세션의 연결은 그대로 받는다.
		h.send(convID, other, "남은 연결에 온다", false)
		ctx := testCtx(t, 5*time.Second)
		o.await(ctx, func(f frame) bool { return f.Type == "message" && f.Body == "남은 연결에 온다" })
	})

	t.Run("비활성", func(t *testing.T) {
		agent := testsupport.Agent(t, h.store)
		login := h.post("/api/agent/login", "", map[string]any{"email": agent.Email, "password": "chat1234"})
		c := h.dial(testCtx(t, 10*time.Second), login["token"].(string))
		c.subscribe(testCtx(t, 5*time.Second), convID, 0)
		h.post("/api/admin/agents/"+agent.ID+"/active", admin, map[string]any{"active": false})
		awaitClosed(t, c)
	})
}

// ── 정책 698 세션 종료 알림 ───────────────────────────────────────────────

func TestE2E_세션이끝났다는알림은다른세션의연결을끊지않는다(t *testing.T) {
	// 알림은 "다시 물어라" 일 뿐이다. 판정은 DB 다 — 알림이 섞여도 살아 있는 세션의 연결은 산다.
	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 10*time.Second)
	agent := h.agentToken()
	_, convID := h.customerSession()
	c := h.dial(ctx, agent)
	c.subscribe(ctx, convID, 0)
	sub, err := h.svc.Queries.Subject(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	h.hub.SessionsEnded([]core.SessionRef{sub.Session}) // 세션은 살아 있는데 알림만 왔다
	time.Sleep(300 * time.Millisecond)
	h.send(convID, agent, "아직 열려 있다", false)
	if f := c.await(ctx, func(f frame) bool {
		return f.Type == "error" || (f.Type == "message" && f.Body == "아직 열려 있다")
	}); f.Type != "message" {
		t.Errorf("살아 있는 세션의 연결이 끊겼다: %+v", f)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("시간 초과")
	}
}
