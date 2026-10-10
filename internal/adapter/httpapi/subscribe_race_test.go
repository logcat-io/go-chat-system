package httpapi_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// afterAuthorize 는 구독 처리의 첫 권한 확인이 대화를 읽은 직후에 한 번 끼어든다. 돌려주는 것은 바뀌기 전의 행이다.
// 운영 코드에 훅을 넣지 않고 저장소를 바깥에서 감싼다.
type afterAuthorize struct {
	*pgstore.Store
	convID atomic.Value // string. 비어 있으면 끼어들지 않는다
	change atomic.Value // func()
	fired  atomic.Bool
}

func (a *afterAuthorize) Conversation(ctx context.Context, convID string) (core.Conversation, error) {
	conv, err := a.Store.Conversation(ctx, convID)
	if want, _ := a.convID.Load().(string); want != "" && want == convID && a.fired.CompareAndSwap(false, true) {
		if f, _ := a.change.Load().(func()); f != nil {
			f()
		}
	}
	return conv, err
}

// 구독은 대화를 한 번만 읽는다(TestE2E_구독은대화를한번만읽는다). 그래서 권한 확인과 ready 사이에 권한이 사라지면
// ready 는 권한 확인 시점의 대화와 읽음 지점을 싣고 나가고, 전달 루프의 첫 조회가 not_found 로 구독을 푼다.
// 메시지 프레임은 하나도 나가지 않는다. 지금 동작이다(docs/refactoring.md 의 D 결과).
func TestE2E_구독처리중권한확인직후상담이삭제되거나지난상담이되면ready뒤에not_found로끊기고메시지는가지않는다(t *testing.T) {
	wrapped := &afterAuthorize{}
	h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { wrapped.Store = s; return wrapped })
	agentToken := h.agentToken()

	deleteConv := func(t *testing.T, convID string) {
		if err := h.store.DeleteConversation(context.Background(), convID, "", "구독 경합"); err != nil {
			t.Errorf("삭제 실패: %v", err)
		}
	}
	pastConv := func(t *testing.T, convID string) {
		testsupport.Exec(t, testsupport.URL(), `UPDATE conversations SET past_for_customer = true WHERE id = $1`, convID)
	}

	for _, tc := range []struct {
		name   string
		agent  bool
		change func(*testing.T, string)
	}{
		{"삭제·고객", false, deleteConv},
		{"삭제·상담원", true, deleteConv},
		{"지난 상담·고객", false, pastConv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t, 20*time.Second)
			custToken, convID := h.customerSession()
			h.send(convID, custToken, "고객 문의", false)
			h.send(convID, agentToken, "상담원 답변", false)
			token := custToken
			if tc.agent {
				token = agentToken
			}
			ws := h.dial(ctx, token)

			wrapped.fired.Store(false)
			wrapped.change.Store(func() { tc.change(t, convID) })
			wrapped.convID.Store(convID)
			t.Cleanup(func() { wrapped.convID.Store("") })
			ws.send(ctx, map[string]any{"type": "subscribe", "conversation_id": convID, "after_seq": 0})
			frames := ws.silent(1500 * time.Millisecond)
			wrapped.convID.Store("")

			if !wrapped.fired.Load() {
				t.Fatal("권한 확인 직후에 끼어들지 못했다")
			}
			var types []string
			for _, f := range frames {
				types = append(types, f.Type)
				if f.Type == "message" {
					t.Errorf("메시지 프레임이 나갔다: seq=%d", f.Seq)
				}
			}
			if len(frames) != 2 || frames[0].Type != "ready" || frames[1].Type != "error" ||
				frames[1].Code != "not_found" || frames[1].ConversationID != convID {
				t.Fatalf("ready 뒤에 not_found 하나여야 한다: %v %+v", types, frames)
			}
			if len(frames[0].Conversation) == 0 {
				t.Errorf("ready 가 권한 확인 시점의 대화를 싣지 않았다: %+v", frames[0])
			}
		})
	}
}
