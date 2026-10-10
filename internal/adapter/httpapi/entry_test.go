package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 입구의 규칙. 위젯 오리진, 이어 쓰는 고객 토큰, 로그인 이메일 정규화.

func TestE2E_위젯입구의오리진판정(t *testing.T) {
	// 기본 워크스페이스의 허용 오리진을 바꾼다. 공유 DB 의 다른 테스트가 흔들리지 않게 격리 DB 에서 돈다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	tn := h.homeTenant()
	h.put("/api/admin/workspace", tn.token, map[string]any{"name": "입구 상점", "allowed_origins": []string{"https://shop.example.com"}})
	session := func(origin string) int {
		return h.status(http.MethodPost, "/api/customer/session", "", map[string]any{"host_origin": origin})
	}

	t.Run("허용 목록이 있어도 우리 자신의 오리진(독립 위젯 페이지)은 된다", func(t *testing.T) {
		// 하네스의 PublicURL 은 http://test.local 이다.
		if got := session("http://test.local"); got != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", got)
		}
		if got := session("  http://test.local  "); got != http.StatusOK {
			t.Errorf("앞뒤 공백은 떼고 본다: %d", got)
		}
	})

	t.Run("허용 목록의 오리진은 대소문자와 끝 슬래시를 가리지 않는다", func(t *testing.T) {
		if got := session("HTTPS://SHOP.EXAMPLE.COM/"); got != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", got)
		}
	})

	t.Run("회원 세션도 같은 오리진 판정을 지난다", func(t *testing.T) {
		secret, _ := h.get("/api/admin/workspace", tn.token)["member_secret"].(string)
		tok, _ := app.SignMember([]byte(secret), app.MemberClaims{MemberID: "m-" + id.New()[28:], Name: "회원", Exp: time.Now().Add(time.Hour).Unix()})
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": tok, "host_origin": "http://test.local"}); got != http.StatusOK {
			t.Errorf("우리 오리진은 200: %d", got)
		}
		// 오리진 판정이 회원 토큰 검사보다 먼저다. 틀린 토큰이어도 403 이다.
		if got := h.status(http.MethodPost, "/api/customer/member-session", "", map[string]any{"member_token": "x.y", "host_origin": "https://evil.example"}); got != http.StatusForbidden {
			t.Errorf("허용되지 않은 오리진은 403: %d", got)
		}
	})
}

func TestE2E_고객토큰이어쓰기(t *testing.T) {
	h := newHarness(t, time.Second, false)
	first := h.post("/api/customer/session", "", map[string]any{"display_name": "고객"})
	tok, _ := first["token"].(string)

	t.Run("같은 워크스페이스의 고객 토큰은 그대로 이어 쓴다", func(t *testing.T) {
		again := h.post("/api/customer/session", "", map[string]any{"token": "  " + tok + "  "})
		if again["token"] != tok {
			t.Errorf("같은 토큰이어야 한다: %v", again["token"])
		}
	})

	t.Run("상담원 토큰은 고객 토큰으로 이어 쓰지 않는다", func(t *testing.T) {
		agentToken := h.agentToken()
		again := h.post("/api/customer/session", "", map[string]any{"token": agentToken})
		if got, _ := again["token"].(string); got == "" || got == agentToken {
			t.Errorf("새 고객 토큰이어야 한다: %v", again["token"])
		}
	})

	t.Run("만료·위조 토큰은 조용히 새로 발급한다", func(t *testing.T) {
		again := h.post("/api/customer/session", "", map[string]any{"token": "forged"})
		if got, _ := again["token"].(string); got == "" || got == "forged" {
			t.Errorf("새 고객 토큰이어야 한다: %v", again["token"])
		}
	})
}

func TestE2E_로그인이메일정규화(t *testing.T) {
	h := newHarness(t, time.Second, false)
	email := "Case-" + id.New()[28:] + "@Example.com"
	if _, err := h.store.CreateAgent(context.Background(), core.DefaultWorkspaceID, strings.ToLower(email), "대소문자", ownerPassword); err != nil {
		t.Fatal(err)
	}

	t.Run("앞뒤 공백과 대소문자를 가리지 않고 로그인한다", func(t *testing.T) {
		for _, in := range []string{strings.ToLower(email), strings.ToUpper(email), " " + email + "\t"} {
			if got := h.status(http.MethodPost, "/api/agent/login", "", map[string]any{"email": in, "password": ownerPassword}); got != http.StatusOK {
				t.Errorf("%q: %d", in, got)
			}
		}
	})

	t.Run("형식이 틀린 이메일의 로그인은 400 이 아니라 401 이다", func(t *testing.T) {
		if got := h.status(http.MethodPost, "/api/agent/login", "", map[string]any{"email": "no-at-sign", "password": ownerPassword}); got != http.StatusUnauthorized {
			t.Errorf("401 이어야 한다: %d", got)
		}
	})
}

func TestLogging_관리자설정조회거절(t *testing.T) {
	h, logs := newCapturedHarness(t, 2*time.Second, false)
	tm := h.teamOf("설정 거절 상점")

	t.Run("설정 조회 권한 거절은 403 forbidden 과 auth.denied 한 줄이다", func(t *testing.T) {
		res, body := h.request(http.MethodGet, "/api/admin/settings", tm.staff.token, nil, nil)
		e, _ := body["error"].(map[string]any)
		if res.StatusCode != http.StatusForbidden || e["code"] != "forbidden" || e["message"] != "이 작업을 할 권한이 없습니다" {
			t.Fatalf("403 forbidden 이어야 한다: %d %v", res.StatusCode, body)
		}
		denied := linesOf(t, logs, res, "auth.denied")
		if len(denied) != 1 {
			t.Fatalf("auth.denied 가 %d줄: %v", len(denied), denied)
		}
		if l := denied[0]; l.str("permission") != "workspace.manage" || l.str("level") != "WARN" || l.str("actor_id") != tm.staff.id {
			t.Errorf("auth.denied 내용이 틀렸다: %v", l)
		}
	})
}

// 사내 NAT 뒤에서는 상담원과 테스트 고객이 같은 IP 로 들어온다. 한 입구를 다 써도 다른 입구는 막히지 않아야 한다.
func TestE2E_입구한도는입구마다따로센다(t *testing.T) {
	h := buildHarness(t, harnessConfig{catchup: time.Second, entry: 3})
	agent := testsupport.Agent(t, h.store)
	session := func() *http.Response {
		return h.postRaw("/api/customer/session", "", map[string]any{"display_name": "한도"})
	}
	for i := 0; i < 3; i++ {
		res := session()
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%d번째 세션 생성이 %d", i+1, res.StatusCode)
		}
	}

	t.Run("같은 IP 가 고객 세션 생성 한도를 다 쓰면 그 입구는 429 와 Retry-After 다", func(t *testing.T) {
		res := session()
		res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("429 여야 한다: %d", res.StatusCode)
		}
		if res.Header.Get("Retry-After") == "" {
			t.Error("Retry-After 가 있어야 한다")
		}
	})

	t.Run("같은 IP 가 고객 세션 생성 한도를 다 써도 상담원 로그인은 된다", func(t *testing.T) {
		code := h.status(http.MethodPost, "/api/agent/login", "", map[string]any{"email": agent.Email, "password": "chat1234"})
		if code != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", code)
		}
	})
}
