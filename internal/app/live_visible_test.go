package app_test

import (
	"testing"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

func TestLiveVisible(t *testing.T) {
	agentA := app.Actor{Kind: app.ActorAgent, AgentID: "a1"}
	agentB := app.Actor{Kind: app.ActorAgent, AgentID: "a2"}
	customer := app.Actor{Kind: app.ActorCustomer, CustomerID: "c1"}
	ev := func(kind string, actor app.ActorKind, actorID string) core.LiveEvent {
		return core.LiveEvent{Kind: kind, ConversationID: "conv", ActorKind: string(actor), ActorID: actorID}
	}

	cases := []struct {
		name   string
		viewer app.Actor
		ev     core.LiveEvent
		want   bool
	}{
		{"상담원 읽음은 고객에게 보이지 않는다", customer, ev(core.LiveRead, app.ActorAgent, "a1"), false},
		{"상담원 입력 중 신호는 고객에게 보인다", customer, ev(core.LiveTyping, app.ActorAgent, "a1"), true},
		{"고객 읽음은 상담원에게 보인다", agentA, ev(core.LiveRead, app.ActorCustomer, "c1"), true},
		{"상담원 읽음은 다른 상담원에게 보인다", agentB, ev(core.LiveRead, app.ActorAgent, "a1"), true},
		{"자기 읽음은 자기에게 오지 않는다", agentA, ev(core.LiveRead, app.ActorAgent, "a1"), false},
		{"자기 입력 중 신호는 자기에게 오지 않는다", customer, ev(core.LiveTyping, app.ActorCustomer, "c1"), false},
		// 종류가 다르면 id 가 같아도 남이다.
		{"id 가 같아도 종류가 다르면 남이다", app.Actor{Kind: app.ActorCustomer, CustomerID: "x"}, ev(core.LiveTyping, app.ActorAgent, "x"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := app.LiveVisible(tc.viewer, tc.ev); got != tc.want {
				t.Errorf("LiveVisible = %v, 기대 %v", got, tc.want)
			}
		})
	}

	t.Run("판정은 할당하지 않는다", func(t *testing.T) {
		// 전달 루프가 이벤트마다 연결마다 부른다. 전송 경로에 할당을 더하지 않는다.
		read := ev(core.LiveRead, app.ActorAgent, "a1")
		typing := ev(core.LiveTyping, app.ActorCustomer, "c1")
		allocs := testing.AllocsPerRun(1000, func() {
			_ = app.LiveVisible(customer, read)
			_ = app.LiveVisible(agentB, read)
			_ = app.LiveVisible(customer, typing)
			_ = app.LiveVisible(agentA, typing)
		})
		if allocs != 0 {
			t.Errorf("할당이 %v 번 있다", allocs)
		}
	})
}
