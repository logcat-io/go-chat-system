package httpapi_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 휘발성 이벤트(타이핑·읽음)를 누가 받는지 끝단에서 고정한다(D25).
// 고객이 낸 이벤트의 방향은 live_test.go 가 본다. 여기서는 상담원이 낸 이벤트를 본다.
//
// "받지 않는다" 는 시간으로 재지 않고 보초로 잰다. 휘발성 이벤트는 같은 알림 채널과 같은 허브를 거쳐
// 연결마다 순서대로 쌓이므로, 뒤에 낸 보초가 도착했는데 앞의 이벤트가 없었다면 걸러진 것이다.
func TestE2E_상담원휘발성이벤트의받는사람(t *testing.T) {
	h := newHarness(t, 2*time.Second, true)
	ctx := testCtx(t, 30*time.Second)

	custToken, convID := h.customerSession()
	tokenA := h.agentToken()
	tokenB := h.agentToken()

	cust := h.dial(ctx, custToken)
	ready := cust.subscribe(ctx, convID, 0)
	cust.collectSeqs(ctx, int(ready.LastSeq))

	// 상담원 A 는 탭 두 개, 상담원 B 는 하나다.
	a1 := h.dial(ctx, tokenA)
	agentReady := a1.subscribe(ctx, convID, 0)
	a1.collectSeqs(ctx, int(agentReady.LastSeq))
	a2 := h.dial(ctx, tokenA)
	a2.subscribe(ctx, convID, 0)
	a2.collectSeqs(ctx, int(agentReady.LastSeq))
	b := h.dial(ctx, tokenB)
	b.subscribe(ctx, convID, 0)
	b.collectSeqs(ctx, int(agentReady.LastSeq))

	// A 의 첫 탭이 읽고, 이어서 입력 중 신호를 낸다. 입력 중 신호가 보초다.
	a1.send(ctx, map[string]any{"type": "read", "conversation_id": convID, "seq": agentReady.LastSeq})
	a1.send(ctx, map[string]any{"type": "typing", "conversation_id": convID})

	var agentAID string
	t.Run("다른 상담원은 상담원의 읽음과 입력 중 신호를 받는다", func(t *testing.T) {
		read := b.await(ctx, func(f frame) bool { return f.Type == "read" })
		if read.ReaderKind != "AGENT" || read.ReaderID == "" {
			t.Fatalf("상담원 A 의 읽음이어야 한다: %+v", read)
		}
		if read.Seq != agentReady.LastSeq {
			t.Errorf("읽음 지점이 %d 여야 한다: %d", agentReady.LastSeq, read.Seq)
		}
		agentAID = read.ReaderID
		typing := b.await(ctx, func(f frame) bool { return f.Type == "typing" })
		if typing.SenderKind != "AGENT" {
			t.Errorf("보낸 쪽이 상담원이어야 한다: %+v", typing)
		}
	})

	t.Run("고객은 상담원의 읽음을 받지 않는다", func(t *testing.T) {
		reads := 0
		got := cust.await(ctx, func(f frame) bool {
			if f.Type == "read" {
				reads++
			}
			return f.Type == "typing"
		})
		if got.SenderKind != "AGENT" {
			t.Errorf("보초는 상담원의 입력 중 신호여야 한다: %+v", got)
		}
		if reads != 0 {
			t.Errorf("고객에게 상담원 읽음이 %d개 갔다", reads)
		}
	})

	// B 가 읽음을 낸다. A 의 두 탭에 이것이 보초다. B 는 이미 A 의 이벤트를 받았으므로
	// 허브는 A 의 이벤트를 A 의 탭들 앞에도 이미 놓았다(놓았다면).
	b.send(ctx, map[string]any{"type": "read", "conversation_id": convID, "seq": agentReady.LastSeq})
	sentinel := func(f frame) bool {
		return f.Type == "read" && f.ReaderKind == "AGENT" && f.ReaderID != agentAID
	}
	fromA := func(f frame) bool {
		return f.Type == "typing" || (f.Type == "read" && f.ReaderID == agentAID)
	}

	t.Run("자기가 낸 읽음과 입력 중 신호는 그 연결로 돌아오지 않는다", func(t *testing.T) {
		echoed := 0
		a1.await(ctx, func(f frame) bool {
			if fromA(f) {
				echoed++
			}
			return sentinel(f)
		})
		if echoed != 0 {
			t.Errorf("자기 이벤트가 %d개 돌아왔다", echoed)
		}
	})

	t.Run("같은 상담원의 다른 탭도 그 상담원이 낸 이벤트를 받지 않는다", func(t *testing.T) {
		// 지금 동작을 고정한다. 자기 이벤트를 연결이 아니라 행위자(종류와 id)로 가른다.
		echoed := 0
		a2.await(ctx, func(f frame) bool {
			if fromA(f) {
				echoed++
			}
			return sentinel(f)
		})
		if echoed != 0 {
			t.Errorf("같은 상담원의 다른 탭에 %d개가 갔다", echoed)
		}
	})
}

// convCounter 는 대화 행 조회(Conversation)를 센다. 운영 코드에 훅을 넣지 않고 저장소를 바깥에서 감싼다.
type convCounter struct {
	*pgstore.Store
	convID atomic.Value // string
	calls  atomic.Int64
}

func (c *convCounter) Conversation(ctx context.Context, convID string) (core.Conversation, error) {
	if want, _ := c.convID.Load().(string); want == convID {
		c.calls.Add(1)
	}
	return c.Store.Conversation(ctx, convID)
}

// 구독은 권한을 확인하며 대화를 한 번 읽는다. 읽음 지점을 실으려고 다시 읽지 않는다.
func TestE2E_구독은대화를한번만읽는다(t *testing.T) {
	counter := &convCounter{}
	h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { counter.Store = s; return counter })
	ctx := testCtx(t, 20*time.Second)

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()
	counter.convID.Store(convID)

	for _, tc := range []struct{ name, token string }{{"고객", custToken}, {"상담원", agentToken}} {
		t.Run(tc.name+" 구독 한 번에 대화 조회는 한 번이다", func(t *testing.T) {
			ws := h.dial(ctx, tc.token)
			counter.calls.Store(0)
			ready := ws.subscribe(ctx, convID, 0)
			if ready.Type != "ready" {
				t.Fatalf("ready 를 받아야 한다: %+v", ready)
			}
			if n := counter.calls.Load(); n != 1 {
				t.Errorf("대화 조회가 %d 번이다", n)
			}
		})
	}

	t.Run("REST 의 읽음 조회는 권한 확인으로 대화를 한 번 읽는다", func(t *testing.T) {
		counter.calls.Store(0)
		h.get("/api/conversations/"+convID+"/reads", custToken)
		if n := counter.calls.Load(); n != 1 {
			t.Errorf("대화 조회가 %d 번이다", n)
		}
	})
}
