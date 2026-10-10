package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 잘못된 입력은 400 이어야 한다. 500 은 "서버가 깨졌다" 는 뜻이고,
// 그게 섞이면 운영자가 진짜 장애를 로그에서 못 찾는다.
func TestE2E_잘못된입력은400이다(t *testing.T) {
	h := newHarness(t, time.Second, false)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	t.Run("형식이 아닌 식별자", func(t *testing.T) {
		cases := []struct{ method, path string }{
			{http.MethodGet, "/api/conversations/not-a-uuid"},
			{http.MethodGet, "/api/customers/not-a-uuid"},
			{http.MethodGet, "/api/attachments/not-a-uuid"},
			{http.MethodDelete, "/api/agent/snippets/not-a-uuid"},
			{http.MethodDelete, "/api/agent/conversations/" + convID + "/tags/not-a-uuid"},
			{http.MethodGet, "/api/agent/conversations?assignee=not-a-uuid"},
			{http.MethodGet, "/api/agent/conversations?tag_id=not-a-uuid"},
		}
		for _, tc := range cases {
			t.Run(tc.path, func(t *testing.T) {
				res := h.raw(tc.method, tc.path, agentToken)
				res.Body.Close()
				if res.StatusCode != http.StatusBadRequest {
					t.Errorf("400 이어야 한다: %d", res.StatusCode)
				}
			})
		}
	})

	t.Run("없는 상담원에게 배정하면 400 이다", func(t *testing.T) {
		res := h.postRaw("/api/agent/conversations/"+convID+"/assign", agentToken,
			map[string]any{"agent_id": id.New()})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("빈 파일은 400 이다", func(t *testing.T) {
		res := h.uploadRaw(convID, custToken, "empty.txt", nil)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("정해지지 않은 상태 값은 400 이다", func(t *testing.T) {
		res := h.postRaw("/api/agent/conversations/"+convID+"/state", agentToken,
			map[string]any{"state": "ARCHIVED"})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})
}

// 쿼리 토큰은 WebSocket 에서만 받는다.
// 모든 엔드포인트에서 받으면 주소 하나를 복사하는 것으로 세션이 넘어간다.
func TestE2E_쿼리토큰은웹소켓에서만(t *testing.T) {
	h := newHarness(t, time.Second, false)
	custToken, convID := h.customerSession()

	t.Run("REST 는 쿼리 토큰을 받지 않는다", func(t *testing.T) {
		for _, path := range []string{
			"/api/conversations/" + convID,
			"/api/conversations/" + convID + "/messages",
			"/api/me",
		} {
			res := h.raw(http.MethodGet, path+"?token="+custToken, "")
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s 는 401 이어야 한다: %d", path, res.StatusCode)
			}
		}
	})

	t.Run("헤더로 주면 통한다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/conversations/"+convID, custToken)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", res.StatusCode)
		}
	})
}

// 보내지 않은 업로드가 쌓이면 디스크가 요청 한도보다 먼저 바닥난다.
func TestE2E_업로드쿼터(t *testing.T) {
	h := newHarness(t, time.Second, false)
	custToken, convID := h.customerSession()

	t.Run("상한까지는 올라간다", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			res := h.uploadRaw(convID, custToken, "file.txt", []byte(strings.Repeat("a", 32)))
			res.Body.Close()
			if res.StatusCode != http.StatusCreated {
				t.Fatalf("%d번째 업로드가 %d", i+1, res.StatusCode)
			}
		}
	})

	t.Run("상한을 넘으면 거절한다", func(t *testing.T) {
		res := h.uploadRaw(convID, custToken, "one-more.txt", []byte("more"))
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests {
			t.Errorf("429 여야 한다: %d", res.StatusCode)
		}
		// 지우는 API 는 없다. 할 수 있는 일(보내기)과 저절로 지워지는 때(6시간)를 말한다(감사 23).
		const want = "보내지 않은 첨부는 10개까지 둘 수 있습니다. 올린 파일을 먼저 보내 주세요. 보내지 않은 첨부는 올린 지 6시간이 지나면 저절로 지워집니다"
		if !strings.Contains(string(raw), want) {
			t.Errorf("문구가 %s 다. %q 여야 한다", raw, want)
		}
	})

	t.Run("보내고 나면 다시 올릴 수 있다", func(t *testing.T) {
		out := h.get("/api/conversations/"+convID+"/messages?after_seq=0", custToken)
		_ = out
		// 쌓인 업로드 하나를 실제로 보내 쿼터를 비운다.
		pending := h.pendingAttachment(convID)
		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
			"client_msg_id": id.New(), "body": "첨부 보냅니다", "attachment_id": pending,
		})
		res := h.uploadRaw(convID, custToken, "again.txt", []byte("again"))
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Errorf("201 이어야 한다: %d", res.StatusCode)
		}
	})
}

// 잘못된 프레임은 클라이언트 잘못이다. 서버 장애처럼 "잠시 뒤 다시 시도해라" 라고 알리면 고칠 곳을 잘못 짚는다.
func TestE2E_잘못된프레임의오류문구(t *testing.T) {
	h := newHarness(t, time.Second, false)
	custToken, _ := h.customerSession()
	ctx := testCtx(t, 5*time.Second)
	c := h.dial(ctx, custToken)

	t.Run("알 수 없는 type 의 프레임은 bad_frame 이고 message 가 원인을 말한다", func(t *testing.T) {
		c.send(ctx, map[string]any{"type": "bogus"})
		f := c.await(ctx, func(f frame) bool { return f.Type == "error" })
		if f.Code != "bad_frame" {
			t.Errorf("code 가 bad_frame 이어야 한다: %q", f.Code)
		}
		if f.Message != "알 수 없는 종류의 요청입니다. 페이지를 새로 고쳐 주세요" {
			t.Errorf("원인을 말해야 한다: %q", f.Message)
		}
	})
}
