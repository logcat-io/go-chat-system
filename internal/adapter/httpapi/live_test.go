package httpapi_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 읽음은 DB 에 남고, 타이핑은 안 남는다. 두 보장 수준을 끝단에서 확인한다.
func TestE2E_읽음표시(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	agent := h.dial(ctx, agentToken)
	ready := agent.subscribe(ctx, convID, 0)
	agent.collectSeqs(ctx, int(ready.LastSeq))

	if len(ready.Reads) != 0 {
		t.Errorf("아직 아무도 안 읽었으니 비어야 한다: %+v", ready.Reads)
	}

	// 상담원이 메시지를 보내고, 고객이 읽는다.
	agent.send(ctx, map[string]any{
		"type": "send", "conversation_id": convID,
		"client_msg_id": id.New(), "body": "답변 드렸습니다",
	})
	sent := agent.await(ctx, func(f frame) bool { return f.Type == "ack" })

	cust := h.dial(ctx, custToken)
	cust.subscribe(ctx, convID, 0)

	t.Run("고객이 읽으면 상담원이 읽음 프레임을 받는다", func(t *testing.T) {
		// 고객은 자기 번호 공간(공개 번호)으로 보고한다. 상담원 ack 의 번호는 전체 번호라 섞어 쓰지 않는다.
		conv := h.get("/api/conversations/"+convID, custToken)
		customerSeq, _ := conv["last_seq"].(float64)
		cust.send(ctx, map[string]any{"type": "read", "conversation_id": convID, "seq": int64(customerSeq)})
		got := agent.await(ctx, func(f frame) bool { return f.Type == "read" })
		if got.ReaderKind != "CUSTOMER" {
			t.Errorf("읽은 쪽이 고객이어야 한다: %+v", got)
		}
		// 상담원이 받는 읽음 지점은 전체 번호다. 메모가 없는 대화라 ack 번호와 같다.
		if got.Seq != sent.Seq {
			t.Errorf("읽음 지점이 %d 여야 한다: %d", sent.Seq, got.Seq)
		}
	})

	t.Run("내가 올린 읽음은 나에게 돌아오지 않는다", func(t *testing.T) {
		// 고객이 또 읽음을 올리고, 고객 쪽으로는 read 프레임이 오지 않아야 한다.
		cust.send(ctx, map[string]any{"type": "read", "conversation_id": convID, "seq": sent.Seq})
		cust.send(ctx, map[string]any{"type": "ping"})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "pong" || f.Type == "read" })
		if got.Type == "read" {
			t.Errorf("자기 읽음이 되돌아왔다: %+v", got)
		}
	})

	t.Run("재접속하면 ready 프레임이 읽음 지점을 실어 온다", func(t *testing.T) {
		again := h.dial(ctx, agentToken)
		r := again.subscribe(ctx, convID, 0)
		var custRead int64
		for _, rs := range r.Reads {
			if rs.ReaderKind == "CUSTOMER" {
				custRead = rs.LastReadSeq
			}
		}
		if custRead != sent.Seq {
			t.Errorf("고객 읽음 지점 %d 가 실려야 한다: %+v", sent.Seq, r.Reads)
		}
	})

	t.Run("읽음 지점은 REST 로도 확인된다", func(t *testing.T) {
		out := h.get("/api/conversations/"+convID+"/reads", agentToken)
		reads, ok := out["reads"].([]any)
		if !ok || len(reads) == 0 {
			t.Fatalf("읽음 지점이 있어야 한다: %+v", out)
		}
	})
}

func TestE2E_타이핑(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	agent := h.dial(ctx, agentToken)
	ready := agent.subscribe(ctx, convID, 0)
	agent.collectSeqs(ctx, int(ready.LastSeq))

	cust := h.dial(ctx, custToken)
	cust.subscribe(ctx, convID, 0)

	t.Run("고객이 입력하면 상담원이 입력 중 신호를 받는다", func(t *testing.T) {
		cust.send(ctx, map[string]any{"type": "typing", "conversation_id": convID})
		got := agent.await(ctx, func(f frame) bool { return f.Type == "typing" })
		if got.SenderKind != "CUSTOMER" {
			t.Errorf("보낸 쪽이 고객이어야 한다: %+v", got)
		}
		if got.SenderName == "" {
			t.Error("표시할 이름이 비었다")
		}
	})

	t.Run("자기 입력 신호는 자기에게 오지 않는다", func(t *testing.T) {
		cust.send(ctx, map[string]any{"type": "ping"})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "pong" || f.Type == "typing" })
		if got.Type == "typing" {
			t.Error("자기 타이핑이 되돌아왔다")
		}
	})

	t.Run("연달아 보내도 서버가 간격으로 걸러낸다", func(t *testing.T) {
		// 1.5초 간격 제한이라 즉시 20번 보내면 하나만 통과해야 한다.
		for i := 0; i < 20; i++ {
			agent.send(ctx, map[string]any{"type": "typing", "conversation_id": convID})
		}
		// 통과한 신호가 도착할 시간을 준 뒤 실제 메시지를 보초로 세운다.
		// 보초 메시지를 받는 순간까지 센 타이핑 수가 통과한 개수다.
		time.Sleep(600 * time.Millisecond)
		agent.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "보초",
		})

		typings := 0
		cust.await(ctx, func(f frame) bool {
			if f.Type == "typing" {
				typings++
			}
			return f.Type == "message" && f.Body == "보초"
		})
		if typings != 1 {
			t.Errorf("간격 제한이 안 걸렸다: %d개 통과 (1개여야 한다)", typings)
		}
	})

	t.Run("타이핑은 메시지 로그를 건드리지 않는다", func(t *testing.T) {
		seqs, err := h.store.MessageSeqs(ctx, convID)
		if err != nil {
			t.Fatal(err)
		}
		// 위 서브테스트가 넣은 보초 메시지 한 건만 늘어야 한다. 타이핑 21번은 흔적이 없다.
		if int64(len(seqs)) != ready.LastSeq+1 {
			t.Errorf("메시지 수가 %d 여야 한다: %d", ready.LastSeq+1, len(seqs))
		}
	})
}

// 연결이 끊기면 구독자가 정리돼야 한다. 안 그러면 허브에 죽은 구독자가 쌓인다.
func TestE2E_연결정리(t *testing.T) {
	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(ready.LastSeq))

	if h.hub.Size() != 1 {
		t.Fatalf("구독자가 1 이어야 한다: %d", h.hub.Size())
	}

	t.Run("클라이언트가 끊으면 허브에서 사라진다", func(t *testing.T) {
		if err := cust.ws.Close(4000, "bye"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if h.hub.Size() == 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("구독자가 남았다: %d", h.hub.Size())
	})
}

// 종료·재개는 메시지가 아니라서 seq 로는 보이지 않는다. 별도 프레임으로 알려야
// "닫힌 줄 알았는데 열려 있는" 화면이 안 생긴다.
func TestE2E_대화상태전파(t *testing.T) {
	h := newHarness(t, 500*time.Millisecond, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(ready.LastSeq))
	if ready.State != "OPEN" {
		t.Fatalf("처음에는 OPEN 이어야 한다: %s", ready.State)
	}

	t.Run("상담원이 닫으면 고객 화면에 상태가 전파된다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "conversation" })
		if got.State != "CLOSED" {
			t.Errorf("CLOSED 여야 한다: %+v", got)
		}
	})

	t.Run("고객이 다시 쓰면 상담원 화면에 재개가 전파된다", func(t *testing.T) {
		agent := h.dial(ctx, agentToken)
		r := agent.subscribe(ctx, convID, 0)
		agent.collectSeqs(ctx, int(r.LastSeq))
		if r.State != "CLOSED" {
			t.Fatalf("구독 시점에 CLOSED 여야 한다: %s", r.State)
		}

		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "하나만 더 여쭤봐도 될까요",
		})
		got := agent.await(ctx, func(f frame) bool { return f.Type == "conversation" })
		if got.State != "OPEN" {
			t.Errorf("다시 열려야 한다: %+v", got)
		}
	})

	t.Run("메시지마다 대화 프레임을 따라 보내지 않는다", func(t *testing.T) {
		// 보초 메시지가 올 때까지 대화 프레임이 몇 개 오는지 센다.
		states := 0
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(), "body": "보초",
		})
		cust.await(ctx, func(f frame) bool {
			if f.Type == "conversation" {
				states++
			}
			return f.Type == "message" && f.Body == "보초"
		})
		// 앞 서브테스트의 재개 알림이 아직 안 왔을 수 있어 0 또는 1 이다.
		// 메시지를 보낸 것만으로 따라붙으면 2 이상이 된다.
		if states > 1 {
			t.Errorf("대화 프레임이 %d개 왔다. 상태·담당자·태그가 바뀔 때만 보내야 한다", states)
		}
	})
}

// 메시지가 대화를 다시 열면 그건 메타 변경이다.
// 메타로 안 알리면 상태 전파가 안전망으로 떠넘겨져서, 그동안 상담원 화면은
// "완료" 로 보이는 대화에 메시지가 꽂히는 걸 본다.
//
// 안전망 주기를 길게 둬서 "안전망이 메워 준 것" 과 "제때 알린 것" 을 구분한다.
func TestE2E_재개는즉시전파된다(t *testing.T) {
	const slowSafetyNet = 30 * time.Second
	h := newHarness(t, slowSafetyNet, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	agent := h.dial(ctx, agentToken)
	ready := agent.subscribe(ctx, convID, 0)
	agent.collectSeqs(ctx, int(ready.LastSeq))

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
	closed := agent.await(ctx, func(f frame) bool { return f.Type == "conversation" })
	if closed.State != "CLOSED" {
		t.Fatalf("닫힘이 전파돼야 한다: %+v", closed)
	}

	t.Run("고객 메시지가 대화를 열면 상태가 메시지와 같이 전파된다", func(t *testing.T) {
		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
			"client_msg_id": id.New(), "body": "하나만 더 여쭤볼게요",
		})

		// 메시지와 대화 프레임이 둘 다 와야 한다. 순서는 보장하지 않는다.
		var sawMessage, sawOpen bool
		for !sawMessage || !sawOpen {
			f := agent.read(ctx)
			switch {
			case f.Type == "message" && f.Body == "하나만 더 여쭤볼게요":
				sawMessage = true
			case f.Type == "conversation" && f.State == "OPEN":
				sawOpen = true
			}
		}
	})
}

// 메시지만으로는 대화 프레임이 따라붙지 않아야 한다.
// 붙으면 메시지 한 건마다 상담원 쪽 조회가 늘어난다.
func TestE2E_평범한메시지는대화프레임을부르지않는다(t *testing.T) {
	h := newHarness(t, 30*time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	agent := h.dial(ctx, agentToken)
	ready := agent.subscribe(ctx, convID, 0)
	agent.collectSeqs(ctx, int(ready.LastSeq))

	for i := 0; i < 3; i++ {
		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
			"client_msg_id": id.New(), "body": "문의 " + strconv.Itoa(i),
		})
	}
	// 보초를 세우고 그때까지 온 대화 프레임을 센다.
	h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{
		"client_msg_id": id.New(), "body": "보초",
	})

	frames := 0
	agent.await(ctx, func(f frame) bool {
		if f.Type == "conversation" {
			frames++
		}
		return f.Type == "message" && f.Body == "보초"
	})
	if frames != 0 {
		t.Errorf("메시지마다 대화 프레임이 따라붙었다: %d개", frames)
	}
}

// 상태·담당 변경은 커밋 뒤 알림(NOTIFY)으로 리스너를 거쳐 1초 안에 데스크와 고객 구독에 간다.
// 안전망 주기를 30초로 둬서 "안전망이 메워 준 것" 과 "제때 알린 것" 을 가른다 — 감사 10 에서 커밋 뒤 알림을 빼도
// 안전망이 메워 테스트가 통과했고 지연만 5초로 늘었다.
func TestE2E_상태와담당변경은알림으로1초안에데스크와고객에게간다(t *testing.T) {
	const slowSafetyNet = 30 * time.Second
	h := newHarness(t, slowSafetyNet, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()
	desk := h.dial(ctx, agentToken)
	r := desk.subscribe(ctx, convID, 0)
	desk.collectSeqs(ctx, int(r.LastSeq))
	cust := h.dial(ctx, custToken)
	r = cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(r.LastSeq))

	within := func(name string, c *client, pred func(frame) bool) {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, data, err := c.ws.Read(wctx)
		for ; err == nil; _, data, err = c.ws.Read(wctx) {
			var f frame
			if jerr := json.Unmarshal(data, &f); jerr == nil && pred(f) {
				return
			}
		}
		t.Fatalf("%s: 1초 안에 오지 않았다: %v", name, err)
	}
	state := func(s string) func(frame) bool {
		return func(f frame) bool { return f.Type == "conversation" && f.State == s }
	}
	assigned := func(f frame) bool {
		return f.Type == "conversation" && strings.Contains(string(f.Conversation), `"assignee_agent_id":"`) &&
			!strings.Contains(string(f.Conversation), `"assignee_agent_id":""`)
	}

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
	within("닫기 → 데스크", desk, state("CLOSED"))
	within("닫기 → 고객", cust, state("CLOSED"))

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "OPEN"})
	within("다시 열기 → 데스크", desk, state("OPEN"))
	within("다시 열기 → 고객", cust, state("OPEN"))

	h.post("/api/agent/conversations/"+convID+"/assign", agentToken, map[string]any{"agent_id": "me", "expected_assignee": ""})
	within("배정 → 데스크", desk, assigned)
	within("배정 → 고객", cust, func(f frame) bool { return f.Type == "conversation" })
}
