package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 구독은 권한을 확인한 순간의 허가다. 그 뒤 권한이 사라지면(회원의 지난 상담, 가린 상담) 구독도 풀려야 한다.
// 안 풀리면 옛 탭이 그 대화의 수정·삭제·타이핑을 계속 받고, 전달 루프는 "없다" 를 영원히 다시 묻는다.

// silent 는 d 동안 받은 프레임 중 받은함 신호를 뺀 것을 돌려준다. 받은함 신호는 대화와 무관하게 상담원에게 간다.
// 시간이 다 되면 읽기가 연결을 닫으므로 이 연결로는 더 읽지 않는다.
func (c *client) silent(d time.Duration) []frame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var got []frame
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				c.t.Fatalf("읽기 실패: %v", err)
			}
			return got
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.t.Fatalf("프레임 해석 실패: %v", err)
		}
		if f.Type != "inbox_changed" {
			got = append(got, f)
		}
	}
}

func notFoundOn(convID string) func(frame) bool {
	return func(f frame) bool { return f.Type == "error" && f.Code == "not_found" && f.ConversationID == convID }
}

// failedLookups 는 그 대화의 전달용 조회 실패 경고(ws.deliver_failed) 수다. 사건 이름이 바뀌면(DEF-74) 여기도 바꿔야 한다 —
// 옛 문장 이름으로 세던 동안 이 검사는 언제나 0 을 세며 공회전했다(감사 14 변이 G12).
func failedLookups(t *testing.T, logs *logBuffer, convID string) int {
	t.Helper()
	n := 0
	for _, l := range logs.where(t, "conversation_id", convID) {
		if l.str("msg") == "ws.deliver_failed" {
			n++
		}
	}
	return n
}

func TestE2E_지난상담이된대화의구독은끊긴다(t *testing.T) {
	h, logs := newCapturedHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)
	tm := h.teamOf("구독 해제 상점")
	cust := h.memberSession(tm.tn)
	oldID, _ := h.startConversation(cust)["id"].(string)
	h.send(oldID, cust, "지난 상담 질문", false)
	reply, _ := h.send(oldID, tm.admin.token, "답변", false)["id"].(string)
	h.post("/api/agent/conversations/"+oldID+"/state", tm.admin.token, map[string]any{"state": "CLOSED"})

	custWS := h.dial(ctx, cust)
	if f := custWS.subscribe(ctx, oldID, 0); f.Type != "ready" {
		t.Fatalf("구독: %+v", f)
	}
	// 대조군: 상담원은 지난 상담을 계속 본다. 아래의 수정·타이핑이 실제로 일어났다는 증거다.
	staffWS := h.dial(ctx, tm.staff.token)
	if f := staffWS.subscribe(ctx, oldID, 0); f.Type != "ready" {
		t.Fatalf("상담원 구독: %+v", f)
	}
	adminWS := h.dial(ctx, tm.admin.token)

	newID, _ := h.startConversation(cust)["id"].(string)
	if newID == "" || newID == oldID {
		t.Fatalf("새 상담이 열려야 한다: %s", newID)
	}

	t.Run("새 상담을 열면 옛 연결은 not_found 를 한 번 받고, 그 뒤 지난 상담의 수정·타이핑을 받지 않는다", func(t *testing.T) {
		custWS.await(ctx, notFoundOn(oldID))

		if code := h.status(http.MethodPatch, "/api/conversations/"+oldID+"/messages/"+reply, tm.admin.token,
			map[string]any{"body": "고친 답변"}); code != http.StatusOK {
			t.Fatalf("상담원의 수정: %d", code)
		}
		staffWS.await(ctx, func(f frame) bool { return f.Type == "message" && f.Body == "고친 답변" })
		adminWS.send(ctx, map[string]any{"type": "typing", "conversation_id": oldID})
		staffWS.await(ctx, func(f frame) bool { return f.Type == "typing" })

		if got := custWS.silent(1500 * time.Millisecond); len(got) != 0 {
			t.Errorf("구독이 풀린 연결이 프레임을 받았다: %+v", got)
		}
	})

	t.Run("구독이 풀린 뒤 전달용 조회 실패 경고가 쌓이지 않는다", func(t *testing.T) {
		// 안전망 주기(1초)를 두 번 넘게 기다린다. 풀리지 않았다면 그 사이 몇 번이고 다시 묻는다.
		time.Sleep(2500 * time.Millisecond)
		if n := failedLookups(t, logs, oldID); n != 0 {
			t.Errorf("ws.deliver_failed 가 %d번 남았다", n)
		}
	})
}

func TestE2E_삭제한상담의구독은끊긴다(t *testing.T) {
	h, logs := newCapturedHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)
	tm := h.teamOf("삭제 구독 상점")
	cust, convID := h.widgetCustomer(tm.tn)
	h.send(convID, cust, "지울 상담", false)

	custWS := h.dial(ctx, cust)
	if f := custWS.subscribe(ctx, convID, 0); f.Type != "ready" {
		t.Fatalf("고객 구독: %+v", f)
	}
	staffWS := h.dial(ctx, tm.staff.token)
	if f := staffWS.subscribe(ctx, convID, 0); f.Type != "ready" {
		t.Fatalf("상담원 구독: %+v", f)
	}

	if code, out := h.call(http.MethodDelete, "/api/agent/conversations/"+convID, tm.admin.token, map[string]any{"reason": "중복 문의"}); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("삭제: %d %v", code, out)
	}

	t.Run("관리자가 상담을 지우면 구독 중이던 고객·상담원 모두 not_found 를 한 번 받는다", func(t *testing.T) {
		custWS.await(ctx, notFoundOn(convID))
		staffWS.await(ctx, notFoundOn(convID))
	})

	t.Run("되살린 뒤 새 메시지가 와도 풀린 구독에는 가지 않는다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/restore", tm.admin.token, nil)
		h.post("/api/conversations/"+convID+"/messages", tm.admin.token, map[string]any{"client_msg_id": id.New(), "body": "되살린 뒤 답변"})
		if got := custWS.silent(1500 * time.Millisecond); len(got) != 0 {
			t.Errorf("고객의 풀린 구독이 프레임을 받았다: %+v", got)
		}
		if got := staffWS.silent(500 * time.Millisecond); len(got) != 0 {
			t.Errorf("상담원의 풀린 구독이 프레임을 받았다: %+v", got)
		}
	})

	t.Run("구독이 풀린 뒤 전달용 조회 실패 경고가 쌓이지 않는다", func(t *testing.T) {
		if n := failedLookups(t, logs, convID); n != 0 {
			t.Errorf("ws.deliver_failed 가 %d번 남았다", n)
		}
	})
}
