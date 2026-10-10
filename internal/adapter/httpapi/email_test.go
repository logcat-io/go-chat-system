package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 이메일 상한은 글자가 아니라 바이트로 254 다. 계정을 만드는 입구(상담원 가입)가 정규화한 뒤 잰다.
// 회사 셀프 가입은 걷어냈다(D35). 상담원 가입은 형식이 맞으면 늘 202 라 받았는지는 202·400 으로 가른다.
func TestE2E_이메일은254바이트까지받고255바이트는400이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	home := tenant{wsID: core.DefaultWorkspaceID}
	const domain = "@example.com" // 12바이트
	local := strings.ToLower(strings.ReplaceAll(id.New(), "-", ""))

	for _, tc := range []struct {
		bytes int
		want  int
	}{{254, http.StatusAccepted}, {255, http.StatusBadRequest}} {
		email := local + strings.Repeat("a", tc.bytes-len(domain)-len(local)) + domain
		if len(email) != tc.bytes {
			t.Fatalf("만든 이메일이 %d바이트다", len(email))
		}
		if tc.want == http.StatusAccepted {
			if code := h.agentSignup(home, email, "알바", "join12345"); code != tc.want {
				t.Errorf("%d바이트: %d, %d 이어야 한다", tc.bytes, code, tc.want)
			}
			continue
		}
		code, body := h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"email": email, "name": "알바", "password": "join12345"})
		if code != tc.want {
			t.Errorf("%d바이트: %d %v, %d 이어야 한다", tc.bytes, code, body, tc.want)
		}
		if e, _ := body["error"].(map[string]any); e["message"] != "이메일 형식이 올바르지 않습니다" {
			t.Errorf("%d바이트: 이메일 거절이 아니다: %v", tc.bytes, body)
		}
	}
}

// 초대는 이메일을 소문자로 바꿔 저장하고, 로그인은 대소문자를 가리지 않는다. 저장소도 소문자로 한 번 더 맞춘다.
// 옛 정책(회사 셀프 가입에서 이름을 비우면 이메일 앞부분이 이름)은 가입 입구와 함께 걷어냈다(D35).
func TestE2E_대문자가섞인이메일로초대받아들어오면소문자로저장되고대문자로도로그인된다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	local := "Mixed.Case-" + strings.ToUpper(id.New()[28:])
	email := local + "@Example.COM"

	inv := h.post("/api/admin/invites", h.adminToken(), map[string]any{"email": email, "name": "대문자", "role": "STAFF"})
	url, _ := inv["accept_url"].(string)
	if !strings.Contains(url, "invite=") {
		t.Fatalf("초대를 만들지 못했다: %v", inv)
	}
	acc := h.post("/api/agent/invites/accept", "", map[string]any{"token": url[strings.Index(url, "invite=")+len("invite="):], "password": "member1234"})
	agent, _ := acc["agent"].(map[string]any)
	agentID, _ := agent["id"].(string)
	stored, err := h.store.AgentByID(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Email != strings.ToLower(email) {
		t.Errorf("저장된 이메일: %q", stored.Email)
	}
	if got := h.status(http.MethodPost, "/api/agent/login", "", map[string]any{"email": strings.ToUpper(email), "password": "member1234"}); got != http.StatusOK {
		t.Errorf("대문자로 로그인: %d", got)
	}
}
