package httpapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/id"
)

func TestE2E_고객과상담원왕복(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	if ready.Type != "ready" {
		t.Fatalf("ready 를 받아야 한다: %+v", ready)
	}
	if ready.LastSeq != 1 {
		t.Errorf("안내 문구 한 건이 있어야 한다: last_seq %d", ready.LastSeq)
	}
	// 안내 문구가 먼저 내려온다. 사람이 아니라 자동 메시지다.
	welcome := cust.await(ctx, func(f frame) bool { return f.Type == "message" })
	if welcome.SenderKind != "BOT" {
		t.Errorf("첫 메시지는 BOT 이어야 한다: %+v", welcome)
	}

	agent := h.dial(ctx, agentToken)
	agent.subscribe(ctx, convID, 0)

	t.Run("고객이 보낸 메시지를 상담원이 실시간으로 받는다", func(t *testing.T) {
		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "배송이 아직 안 왔다",
		})
		got := agent.await(ctx, func(f frame) bool { return f.Type == "message" && f.SenderKind == "CUSTOMER" })
		if got.Body != "배송이 아직 안 왔다" {
			t.Errorf("본문이 다르다: %q", got.Body)
		}
		if got.Seq != 2 {
			t.Errorf("seq 는 2 여야 한다: %d", got.Seq)
		}
	})

	t.Run("상담원 회신을 고객이 받는다", func(t *testing.T) {
		agent.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID,
			"client_msg_id": id.New(), "body": "확인하고 바로 알려드린다",
		})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "message" && f.SenderKind == "AGENT" })
		if got.Body != "확인하고 바로 알려드린다" {
			t.Errorf("본문이 다르다: %q", got.Body)
		}
	})

	t.Run("ack 는 DB 커밋 뒤에 오고 seq 를 담는다", func(t *testing.T) {
		cmid := id.New()
		cust.send(ctx, map[string]any{
			"type": "send", "conversation_id": convID, "client_msg_id": cmid, "body": "ack 확인",
		})
		ack := cust.await(ctx, func(f frame) bool { return f.Type == "ack" && f.ClientMsgID == cmid })
		if ack.Seq == 0 {
			t.Fatal("ack 에 seq 가 없다")
		}
		// ack 를 받은 순간 DB 에 이미 있어야 한다.
		msgs, err := h.store.ListMessages(context.Background(), convID, ack.Seq-1, 1, false)
		if err != nil || len(msgs) != 1 || msgs[0].ClientMsgID != cmid {
			t.Fatalf("ack 시점에 DB 에 없다: %v / %+v", err, msgs)
		}
	})
}

func TestE2E_전달완전성(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 40*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	cust.subscribe(ctx, convID, 0)

	const burst = 80
	t.Run("빠르게 쏟아지는 메시지의 seq 가 하나도 빠지지 않는다", func(t *testing.T) {
		agent := h.dial(ctx, agentToken)
		agent.subscribe(ctx, convID, 0)
		go func() {
			for i := 0; i < burst; i++ {
				agent.send(ctx, map[string]any{
					"type": "send", "conversation_id": convID,
					"client_msg_id": id.New(), "body": fmt.Sprintf("연속 %d", i),
				})
			}
		}()

		// 안내 문구 1건 + burst 건
		got := cust.collectSeqs(ctx, burst+1)
		for i, seq := range got {
			if seq != int64(i+1) {
				t.Fatalf("전달된 seq 가 1..%d 연속이 아니다: 인덱스 %d 에서 %d", burst+1, i, seq)
			}
		}
	})
}

func TestE2E_알림없이도전달된다(t *testing.T) {
	// LISTEN/NOTIFY 리스너를 띄우지 않는다. 안전망(ChangeWatcher)만으로 도착해야 한다.
	h := newHarness(t, 200*time.Millisecond, false)
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	cust.subscribe(ctx, convID, 0)
	cust.await(ctx, func(f frame) bool { return f.Type == "message" }) // 안내 문구

	t.Run("NOTIFY 가 전부 사라져도 안전망이 메시지를 전달한다", func(t *testing.T) {
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(), "body": "알림 없이 도착해야 한다",
		})
		got := cust.await(ctx, func(f frame) bool { return f.Type == "message" })
		if got.Body != "알림 없이 도착해야 한다" {
			t.Errorf("본문이 다르다: %q", got.Body)
		}
	})
}

// 상태·배정은 행을 쓴 뒤 별도 트랜잭션으로 시스템 줄을 남긴다. 그래서 마지막 메시지 시각이 변경 시각보다 늦다.
// 안전망이 "마지막 변경이 메시지인가" 로 메타를 가르면 이 변경을 메시지로 보고 대화 프레임을 안 보낸다 — 연결 틱(60초)까지 늦는다.
func TestE2E_알림을놓쳐도상담원이바꾼상태와담당자는안전망몇주기안에데스크와고객에게간다(t *testing.T) {
	const catchup = 200 * time.Millisecond
	h := newHarness(t, catchup, false)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()
	desk := h.dial(ctx, agentToken)
	desk.subscribe(ctx, convID, 0)
	cust := h.dial(ctx, custToken)
	cust.subscribe(ctx, convID, 0)

	// 안전망 열 주기 안에 와야 한다. 연결 틱(60초)에 기대면 넘는다. 시한을 넘기면 read 가 실패로 끝낸다.
	within := func(name string, c *client, pred func(frame) bool) {
		t.Helper()
		t.Logf("%s 기다림", name)
		wctx, cancel := context.WithTimeout(ctx, 10*catchup)
		defer cancel()
		c.await(wctx, pred)
	}
	state := func(s string) func(frame) bool {
		return func(f frame) bool { return f.Type == "conversation" && f.State == s }
	}

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
	within("닫기 → 데스크", desk, state("CLOSED"))
	within("닫기 → 고객", cust, state("CLOSED"))

	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "OPEN"})
	within("다시 열기 → 데스크", desk, state("OPEN"))
	within("다시 열기 → 고객", cust, state("OPEN"))

	h.post("/api/agent/conversations/"+convID+"/assign", agentToken, map[string]any{"agent_id": "me"})
	within("배정 → 데스크", desk, func(f frame) bool {
		return f.Type == "conversation" && strings.Contains(string(f.Conversation), `"assignee_agent_id":"`) &&
			!strings.Contains(string(f.Conversation), `"assignee_agent_id":""`)
	})

	// 닫힌 상담에 고객이 쓰면 그 글이 상담을 다시 연다. 상태와 메시지가 한 문장에서 바뀐다.
	h.post("/api/agent/conversations/"+convID+"/state", agentToken, map[string]any{"state": "CLOSED"})
	within("다시 닫기 → 고객", cust, state("CLOSED"))
	h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{"client_msg_id": id.New(), "body": "다시 문의"})
	within("고객 글로 다시 열림 → 데스크", desk, state("OPEN"))
	within("고객 글로 다시 열림 → 고객", cust, state("OPEN"))
}

func TestE2E_재접속캐치업(t *testing.T) {
	h := newHarness(t, 500*time.Millisecond, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	seen := ready.LastSeq
	cust.collectSeqs(ctx, int(seen))

	// 고객이 창을 닫는다.
	_ = cust.ws.Close(websocket.StatusNormalClosure, "")

	const offline = 5
	for i := 0; i < offline; i++ {
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{
			"client_msg_id": id.New(), "body": fmt.Sprintf("오프라인 중 %d", i),
		})
	}

	t.Run("끊긴 사이의 메시지를 재접속하면 전부 받는다", func(t *testing.T) {
		again := h.dial(ctx, custToken)
		again.subscribe(ctx, convID, seen)
		got := again.collectSeqs(ctx, offline)
		for i, seq := range got {
			want := seen + int64(i) + 1
			if seq != want {
				t.Fatalf("캐치업 seq 가 틀렸다: %d 번째가 %d (기대 %d)", i, seq, want)
			}
		}
	})
}

func TestE2E_재전송은한번만저장된다(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(ready.LastSeq))

	t.Run("같은 client_msg_id 를 다섯 번 보내도 메시지는 하나다", func(t *testing.T) {
		cmid := id.New()
		for i := 0; i < 5; i++ {
			cust.send(ctx, map[string]any{
				"type": "send", "conversation_id": convID, "client_msg_id": cmid, "body": "재전송",
			})
		}
		// ack 다섯 개가 모두 같은 seq 를 담는다.
		var seqs []int64
		for len(seqs) < 5 {
			f := cust.read(ctx)
			if f.Type == "ack" && f.ClientMsgID == cmid {
				seqs = append(seqs, f.Seq)
			}
		}
		for _, s := range seqs {
			if s != seqs[0] {
				t.Fatalf("ack seq 가 갈렸다: %v", seqs)
			}
		}

		all, err := h.store.MessageSeqs(context.Background(), convID)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(all)) != ready.LastSeq+1 {
			t.Errorf("메시지가 중복 저장됐다: %d건", len(all))
		}
	})
}

func TestE2E_권한(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 20*time.Second)

	tokenA, _ := h.customerSession()
	_, convB := h.customerSession()

	t.Run("고객은 남의 대화를 구독할 수 없다", func(t *testing.T) {
		cust := h.dial(ctx, tokenA)
		got := cust.subscribe(ctx, convB, 0)
		if got.Type != "error" || got.Code != "forbidden" {
			t.Fatalf("forbidden 이어야 한다: %+v", got)
		}
	})

	t.Run("토큰 없이 WebSocket 에 붙을 수 없다", func(t *testing.T) {
		url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws"
		ws, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			_ = ws.CloseNow()
			t.Fatal("거절해야 한다")
		}
	})
}
