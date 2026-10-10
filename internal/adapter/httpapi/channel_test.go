package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 내부 메모가 고객에게 새지 않는다는 보장은 코어가 소유한다.
// 전달 경로가 하나 늘 때마다 다시 구현해야 하는 보장이면 언젠가 샌다.
func TestE2E_내부메모(t *testing.T) {
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	custReady := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(custReady.LastSeq))

	agent := h.dial(ctx, agentToken)
	agentReady := agent.subscribe(ctx, convID, 0)
	agent.collectSeqs(ctx, int(agentReady.LastSeq))

	// 내부 메모 → 공개 회신 순서로 보낸다.
	agent.send(ctx, map[string]any{
		"type": "send", "conversation_id": convID, "internal": true,
		"client_msg_id": id.New(), "body": "이 고객 지난주에도 같은 문의함",
	})
	agent.send(ctx, map[string]any{
		"type": "send", "conversation_id": convID,
		"client_msg_id": id.New(), "body": "확인해서 안내드리겠습니다",
	})

	t.Run("상담원은 내부 메모와 공개 회신을 다 본다", func(t *testing.T) {
		var bodies []string
		for len(bodies) < 2 {
			f := agent.read(ctx)
			if f.Type == "message" {
				bodies = append(bodies, f.Body)
			}
		}
		if !strings.Contains(bodies[0], "지난주") || bodies[1] != "확인해서 안내드리겠습니다" {
			t.Errorf("상담원 화면이 다르다: %v", bodies)
		}
	})

	t.Run("고객에게는 내부 메모가 오지 않는다", func(t *testing.T) {
		got := cust.await(ctx, func(f frame) bool { return f.Type == "message" })
		if strings.Contains(got.Body, "지난주") {
			t.Fatalf("내부 메모가 고객에게 샜다: %q", got.Body)
		}
		if got.Body != "확인해서 안내드리겠습니다" {
			t.Errorf("공개 회신이 와야 한다: %q", got.Body)
		}
		if got.Visibility != "PUBLIC" {
			t.Errorf("공개 메시지여야 한다: %q", got.Visibility)
		}
	})

	t.Run("고객이 보는 번호에 빈칸이 없다", func(t *testing.T) {
		// 안내 문구 1 + 공개 회신 1 = 2. 내부 메모는 번호를 쓰지 않는다.
		msgs := h.get("/api/conversations/"+convID+"/messages?after_seq=0", custToken)
		list, _ := msgs["messages"].([]any)
		for i, raw := range list {
			m, _ := raw.(map[string]any)
			if seq, _ := m["seq"].(float64); int(seq) != i+1 {
				t.Fatalf("고객 번호가 1..N 연속이 아니다: %v", list)
			}
		}
		if len(list) != 2 {
			t.Errorf("공개 2건이어야 한다: %d건", len(list))
		}
	})

	t.Run("고객은 내부 메모를 쓸 수 없다", func(t *testing.T) {
		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID, "internal": true,
			"client_msg_id": id.New(), "body": "나도 내부 메모",
		})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "error" })
		if got.Code != "forbidden" {
			t.Errorf("forbidden 이어야 한다: %+v", got)
		}
	})

	t.Run("내부 메모는 대화를 다시 열지 않는다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(), "body": "닫힌 뒤 메모", "internal": true,
		})
		conv := h.get("/api/conversations/"+convID, agentToken)
		if conv["state"] != "CLOSED" {
			t.Errorf("내부 활동은 상태를 바꾸지 않는다: %v", conv["state"])
		}
	})
}

// 읽음 표시는 두 번호 공간을 오간다. 섞이면 엉뚱한 줄에 "읽음" 이 붙는다.
func TestE2E_내부메모와읽음(t *testing.T) {
	h := newHarness(t, time.Second, true)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	// 공개 1(안내) · 내부 1 · 공개 2 · 내부 2 · 공개 3
	for i, internal := range []bool{true, false, true, false} {
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(),
			"body":          fmt.Sprintf("메시지 %d", i),
			"internal":      internal,
		})
	}

	readSeq := func(t *testing.T, token, readerKind string) int64 {
		t.Helper()
		reads := h.get("/api/conversations/"+convID+"/reads", token)
		list, _ := reads["reads"].([]any)
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			if m["reader_kind"] == readerKind {
				if _, leaked := m["last_read_public_seq"]; leaked {
					t.Error("두 번호 공간을 같이 흘리면 숨긴 메시지 수가 드러난다")
				}
				v, _ := m["last_read_seq"].(float64)
				return int64(v)
			}
		}
		return -1
	}

	t.Run("읽음 지점은 보는 사람의 번호 공간으로 환산된다", func(t *testing.T) {
		// 고객 번호로는 3 까지다 (안내 + 공개 2건).
		h.post("/api/conversations/"+convID+"/read", custToken, map[string]any{"seq": 3})

		// 상담원 화면에서는 전체 번호 5(마지막 공개 메시지)에 붙어야 한다.
		if got := readSeq(t, agentToken, "CUSTOMER"); got != 5 {
			t.Errorf("상담원에게는 전체 번호 5 여야 한다: %d", got)
		}
		// 고객 화면에서는 고객 번호 3 이어야 한다.
		if got := readSeq(t, custToken, "CUSTOMER"); got != 3 {
			t.Errorf("고객에게는 고객 번호 3 이어야 한다: %d", got)
		}
	})
}

// 내부 메모 본문만 막는 걸로는 부족하다. 태그 이름과 담당자 실명도 운영 정보다.
func TestE2E_고객에게는운영정보가안간다(t *testing.T) {
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	h.post("/api/agent/conversations/"+convID+"/assign", agentToken, map[string]any{"agent_id": "me"})
	h.post("/api/agent/conversations/"+convID+"/tags", agentToken, map[string]any{"name": "악성민원"})
	h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
		"client_msg_id": id.New(), "body": "내부 전용 판단", "internal": true,
	})

	hidden := []string{"tags", "assignee_agent_id", "assignee_name", "last_public_seq", "updated_at"}

	t.Run("대화 조회에 운영 정보가 없다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, custToken)
		for _, key := range hidden {
			if _, ok := conv[key]; ok {
				t.Errorf("고객 응답에 %q 가 들어 있다: %v", key, conv[key])
			}
		}
		// 숨긴 메시지 수가 번호 차이로 드러나면 안 된다.
		// 이 대화에는 공개 1건(안내)과 내부 2건(담당 지정 기록·내부 메모)이 있다.
		custSeq, _ := conv["last_seq"].(float64)
		if int(custSeq) != 1 {
			t.Errorf("고객에게는 공개 번호 1 만 보여야 한다: %v", custSeq)
		}
		agentConv := h.get("/api/conversations/"+convID, agentToken)
		agentSeq, _ := agentConv["last_seq"].(float64)
		if int(agentSeq) <= int(custSeq) {
			t.Errorf("상담원은 내부 기록까지 세야 한다: 고객 %v / 상담원 %v", custSeq, agentSeq)
		}
	})

	t.Run("대화 목록에도 없다", func(t *testing.T) {
		out := h.get("/api/customer/conversations", custToken)
		list, _ := out["conversations"].([]any)
		for _, raw := range list {
			c, _ := raw.(map[string]any)
			for _, key := range hidden {
				if _, ok := c[key]; ok {
					t.Errorf("목록에 %q 가 들어 있다", key)
				}
			}
		}
	})

	t.Run("WebSocket 프레임에도 없다", func(t *testing.T) {
		cust := h.dial(ctx, custToken)
		ready := cust.subscribe(ctx, convID, 0)
		if ready.Conversation == nil {
			t.Fatal("ready 에 대화가 실려야 한다")
		}
		raw := map[string]any{}
		_ = json.Unmarshal(ready.Conversation, &raw)
		for _, key := range hidden {
			if _, ok := raw[key]; ok {
				t.Errorf("ready 프레임에 %q 가 들어 있다: %v", key, raw[key])
			}
		}
	})

	t.Run("고객 프로필에 운영 메모가 없다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, agentToken)
		customerID, _ := conv["customer_id"].(string)
		h.post("/api/customers/"+customerID, agentToken, map[string]any{
			"display_name": "김고객", "note": "소송 가능성 있음",
			"attributes": map[string]any{"risk": "high"},
		})
		me := h.get("/api/customers/"+customerID, custToken)
		for _, key := range []string{"note", "attributes", "conversations", "created_at"} {
			if _, ok := me[key]; ok {
				t.Errorf("고객 프로필에 %q 가 들어 있다: %v", key, me[key])
			}
		}
		if me["display_name"] != "김고객" {
			t.Errorf("이름은 보여야 한다: %v", me["display_name"])
		}
	})

	t.Run("상담원에게는 그대로 보인다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, agentToken)
		if list, _ := conv["tags"].([]any); len(list) != 1 {
			t.Errorf("상담원은 태그를 봐야 한다: %v", conv["tags"])
		}
		if conv["assignee_name"] == nil {
			t.Error("상담원은 담당자를 봐야 한다")
		}
	})
}

// 내부 메모에 붙은 파일은 고객이 받으면 안 된다. 대화 권한만 보면 통과해 버린다.
func TestE2E_내부메모첨부(t *testing.T) {
	h := newHarness(t, time.Second, true)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	out := h.upload(convID, agentToken, "내부증빙.txt", []byte("환불 거절 사유 내부 문서"))
	att, _ := out["attachment"].(map[string]any)
	attID, _ := att["id"].(string)

	t.Run("전송 전 업로드는 올린 사람 외에는 못 받는다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/attachments/"+attID, custToken)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
		"client_msg_id": id.New(), "body": "증빙 첨부", "internal": true, "attachment_id": attID,
	})

	t.Run("내부 메모에 붙은 파일을 고객이 못 받는다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/attachments/"+attID, custToken)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			body, _ := io.ReadAll(res.Body)
			t.Errorf("403 이어야 한다: %d (%s)", res.StatusCode, body)
		}
	})

	t.Run("상담원은 받는다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/attachments/"+attID, agentToken)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("공개 메시지에 붙은 파일은 고객도 받는다", func(t *testing.T) {
		pub := h.upload(convID, agentToken, "안내문.txt", []byte("배송 안내"))
		pubAtt, _ := pub["attachment"].(map[string]any)
		pubID, _ := pubAtt["id"].(string)
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(), "body": "안내 드립니다", "attachment_id": pubID,
		})
		res := h.raw(http.MethodGet, "/api/attachments/"+pubID, custToken)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("200 이어야 한다: %d", res.StatusCode)
		}
	})
}

func TestE2E_첨부(t *testing.T) {
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(ready.LastSeq))

	var attachmentID string

	t.Run("이미지를 올리면 크기를 읽어 둔다", func(t *testing.T) {
		out := h.upload(convID, custToken, "스크린샷.png", pngBytes(40, 25))
		att, _ := out["attachment"].(map[string]any)
		attachmentID, _ = att["id"].(string)
		if attachmentID == "" {
			t.Fatalf("첨부 id 가 없다: %+v", out)
		}
		if att["content_type"] != "image/png" {
			t.Errorf("종류를 내용으로 판정해야 한다: %v", att["content_type"])
		}
		if w, _ := att["width"].(float64); int(w) != 40 {
			t.Errorf("가로 40 이어야 한다: %v", att["width"])
		}
	})

	t.Run("첨부를 붙여 보내면 메시지에 실려 온다", func(t *testing.T) {
		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "화면 캡처입니다", "attachment_id": attachmentID,
		})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "message" && f.Body == "화면 캡처입니다" })
		if got.Attachment == nil || got.Attachment.Filename != "스크린샷.png" {
			t.Fatalf("첨부가 안 실렸다: %+v", got.Attachment)
		}
	})

	t.Run("같은 첨부를 두 번 보낼 수 없다", func(t *testing.T) {
		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "또 보내기", "attachment_id": attachmentID,
		})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "error" })
		if got.Code != "invalid" {
			t.Errorf("invalid 여야 한다: %+v", got)
		}
	})

	t.Run("상담원도 같은 첨부를 내려받는다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/attachments/"+attachmentID, agentToken)
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("200 이어야 한다: %d", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("image/png 여야 한다: %s", ct)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("브라우저가 종류를 추측하지 않게 막아야 한다")
		}
		body, _ := io.ReadAll(res.Body)
		if len(body) == 0 {
			t.Error("내용이 비었다")
		}
	})

	t.Run("남의 대화 첨부는 내려받을 수 없다", func(t *testing.T) {
		otherToken, _ := h.customerSession()
		res := h.raw(http.MethodGet, "/api/attachments/"+attachmentID, otherToken)
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("남의 대화에는 올릴 수 없다", func(t *testing.T) {
		otherToken, _ := h.customerSession()
		res := h.uploadRaw(convID, otherToken, "몰래.png", pngBytes(5, 5))
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("한도를 넘는 파일은 거절한다", func(t *testing.T) {
		big := bytes.Repeat([]byte("A"), 11<<20)
		res := h.uploadRaw(convID, custToken, "큰파일.txt", big)
		defer res.Body.Close()
		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("413 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("크기 한도를 넘는 업로드 신청은 413 이고 code 가 too_large 다", func(t *testing.T) {
		// 화면이 code 로 분기할 때 다른 invalid(이름 없음·빈 파일)와 갈라져야 한다.
		status, body := h.call(http.MethodPost, "/api/conversations/"+convID+"/attachments", custToken, map[string]any{
			"filename": "큰파일.txt", "content_type": "text/plain", "byte_size": 11 << 20,
		})
		if status != http.StatusRequestEntityTooLarge || errorCodeOf(body) != "too_large" {
			t.Errorf("413 too_large 여야 한다: %d %v", status, body)
		}
	})
}
