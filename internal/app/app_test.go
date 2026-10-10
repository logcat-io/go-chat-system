package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func TestSend_입력정책(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}

	cases := []struct {
		name        string
		clientMsgID string
		body        string
	}{
		{"빈 본문은 거절한다", id.New(), ""},
		{"공백만 있는 본문은 거절한다", id.New(), "   \n\t "},
		{"길이를 넘는 본문은 거절한다", id.New(), strings.Repeat("가", app.MaxBodyRunes+1)},
		{"client_msg_id 가 비면 거절한다", "", "안녕"},
		{"예약된 client_msg_id 는 거절한다", "system-welcome", "안녕"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Commands.Send(ctx, actor, app.SendParams{ConversationID: conv.ID, ClientMsgID: tc.clientMsgID, Body: tc.body})
			var inv app.InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("InvalidError 여야 한다: %v", err)
			}
		})
	}

	t.Run("경계 길이 본문은 받는다", func(t *testing.T) {
		res, err := svc.Commands.Send(ctx, actor, app.SendParams{ConversationID: conv.ID, ClientMsgID: id.New(), Body: strings.Repeat("나", app.MaxBodyRunes)})
		if err != nil {
			t.Fatalf("받아야 한다: %v", err)
		}
		if res.Message.Seq == 0 {
			t.Error("seq 가 붙어야 한다")
		}
	})

	t.Run("앞뒤 공백은 떼고 저장한다", func(t *testing.T) {
		res, err := svc.Commands.Send(ctx, actor, app.SendParams{ConversationID: conv.ID, ClientMsgID: id.New(), Body: "  문의합니다  "})
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Body != "문의합니다" {
			t.Errorf("공백이 남았다: %q", res.Message.Body)
		}
	})
}

func TestAuthorize_채널가시성(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()

	mine, myConv := testsupport.Conversation(t, store)
	_, otherConv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)

	t.Run("고객은 자기 대화를 본다", func(t *testing.T) {
		if _, err := svc.Queries.Authorize(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, myConv.ID); err != nil {
			t.Errorf("자기 대화는 허용해야 한다: %v", err)
		}
	})

	t.Run("고객은 남의 대화를 보지 못한다", func(t *testing.T) {
		_, err := svc.Queries.Authorize(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, otherConv.ID)
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("고객은 남의 대화에 쓰지 못한다", func(t *testing.T) {
		_, err := svc.Commands.Send(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, app.SendParams{ConversationID: otherConv.ID, ClientMsgID: id.New(), Body: "끼어들기"})
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("고객은 남의 대화 이력을 읽지 못한다", func(t *testing.T) {
		_, err := svc.Queries.History(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, otherConv.ID, 0, 10)
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("상담원은 모든 대화를 본다", func(t *testing.T) {
		a := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: agent.ID, Role: agent.Role}
		for _, convID := range []string{myConv.ID, otherConv.ID} {
			if _, err := svc.Queries.Authorize(ctx, a, convID); err != nil {
				t.Errorf("상담원은 허용해야 한다: %v", err)
			}
		}
	})

	t.Run("고객은 대화 상태를 바꾸지 못한다", func(t *testing.T) {
		_, err := svc.Commands.SetState(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, myConv.ID, "CLOSED", nil)
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("고객은 받은목록을 보지 못한다", func(t *testing.T) {
		_, err := svc.Queries.Inbox(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: mine}, core.InboxFilter{Limit: 10})
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("없는 대화는 형식이 맞아도 not_found 다", func(t *testing.T) {
		_, err := svc.Queries.Authorize(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: agent.ID}, id.New())
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})
}

func TestStartCustomerSession(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()

	t.Run("위젯을 여는 것만으로는 대화가 생기지 않는다", func(t *testing.T) {
		customerID, conv, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "홍길동")
		if err != nil {
			t.Fatal(err)
		}
		if customerID == "" {
			t.Fatal("고객은 만들어져야 한다")
		}
		if conv != nil {
			t.Errorf("대화는 아직 없어야 한다: %+v", conv)
		}
	})

	t.Run("문의를 시작하면 대화가 열리고 안내 문구가 남는다", func(t *testing.T) {
		customerID, _, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "문의시작")
		if err != nil {
			t.Fatal(err)
		}
		actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		conv, err := svc.Commands.StartConversation(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		if conv.LastPublicSeq != 1 {
			t.Errorf("안내 문구 한 건이 있어야 한다: %d", conv.LastPublicSeq)
		}
		msgs, err := svc.Queries.History(ctx, actor, conv.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || msgs[0].Body != app.WelcomeBody {
			t.Errorf("안내 문구가 아니다: %+v", msgs)
		}
	})

	t.Run("다시 들어오면 열려 있는 대화를 이어 쓴다", func(t *testing.T) {
		customerID, _, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "재방문")
		if err != nil {
			t.Fatal(err)
		}
		actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		first, err := svc.Commands.StartConversation(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		_, again, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		if again == nil || again.ID != first.ID {
			t.Errorf("열린 대화를 이어 써야 한다: %+v", again)
		}
	})

	t.Run("문의를 또 시작하면 대화가 하나 더 생긴다", func(t *testing.T) {
		customerID, _, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "두번문의")
		if err != nil {
			t.Fatal(err)
		}
		actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		first, err := svc.Commands.StartConversation(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		second, err := svc.Commands.StartConversation(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID == second.ID {
			t.Fatal("새 대화가 생겨야 한다")
		}
		convs, err := svc.Queries.CustomerConversations(ctx, actor, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(convs) != 2 {
			t.Errorf("대화가 둘이어야 한다: %d개", len(convs))
		}
	})
}

func TestSetState_재오픈(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	agentActor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: agent.ID, Role: agent.Role}

	customerID, _, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "종료확인")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := svc.Commands.StartConversation(ctx, app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("상담원이 대화를 닫으면 상담원 타임라인에만 줄이 남는다", func(t *testing.T) {
		closed, err := svc.Commands.SetState(ctx, agentActor, conv.ID, "CLOSED", nil)
		if err != nil {
			t.Fatal(err)
		}
		if closed.State != "CLOSED" {
			t.Errorf("CLOSED 여야 한다: %s", closed.State)
		}
		msgs, err := svc.Queries.History(ctx, agentActor, conv.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		last := msgs[len(msgs)-1]
		if last.SenderKind != "SYSTEM" || !strings.Contains(last.Body, "종료") {
			t.Errorf("상담원 타임라인에 종료 줄이 남아야 한다: %+v", last)
		}
		// 시스템 알림은 고객에게 보이지 않는다. 고객은 상태를 대화 프레임으로 받는다.
		custActor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		custMsgs, err := svc.Queries.History(ctx, custActor, conv.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range custMsgs {
			if m.SenderKind == "SYSTEM" {
				t.Errorf("고객에게 시스템 줄이 보인다: %+v", m)
			}
		}
	})

	t.Run("닫힌 대화에 고객이 보내면 대화가 다시 열린다", func(t *testing.T) {
		custActor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		before, err := svc.Queries.Authorize(ctx, custActor, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Commands.Send(ctx, custActor, app.SendParams{
			ConversationID: conv.ID, ClientMsgID: id.New(), Body: "추가 문의가 있다",
		})
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := svc.Queries.Authorize(ctx, agentActor, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reopened.State != "OPEN" {
			t.Errorf("다시 열려야 한다: %s", reopened.State)
		}
		if want := before.LastPublicSeq + 1; res.Message.Seq != want {
			t.Errorf("고객 번호가 이어져야 한다: %d (기대 %d)", res.Message.Seq, want)
		}
	})

	t.Run("state 값이 정해진 둘이 아니면 거절한다", func(t *testing.T) {
		_, err := svc.Commands.SetState(ctx, agentActor, conv.ID, "ARCHIVED", nil)
		var inv app.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("InvalidError 여야 한다: %v", err)
		}
	})
}

func TestMarkRead_정책(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	agentActor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: agent.ID, Name: agent.Name, Role: agent.Role}

	customerID, _, err := svc.Commands.StartCustomerSession(ctx, core.DefaultWorkspaceID, "", "읽음확인")
	if err != nil {
		t.Fatal(err)
	}
	custActor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID, Name: "읽음확인"}
	conv, err := svc.Commands.StartConversation(ctx, custActor)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.Commands.Send(ctx, custActor, app.SendParams{ConversationID: conv.ID, ClientMsgID: id.New(), Body: "문의"}); err != nil {
			t.Fatal(err)
		}
	}
	// 안내 문구 1 + 문의 3 = last_seq 4

	t.Run("있는 메시지보다 앞선 보고는 last_seq 로 깎인다", func(t *testing.T) {
		latest, err := svc.Queries.Authorize(ctx, agentActor, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Commands.MarkRead(ctx, agentActor, conv.ID, 9999)
		if err != nil {
			t.Fatal(err)
		}
		if got != latest.LastSeq {
			t.Errorf("대화의 마지막 번호 %d 로 깎여야 한다: %d", latest.LastSeq, got)
		}
	})

	t.Run("음수 보고는 거절한다", func(t *testing.T) {
		_, err := svc.Commands.MarkRead(ctx, agentActor, conv.ID, -1)
		var inv app.InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("InvalidError 여야 한다: %v", err)
		}
	})

	t.Run("되돌아가는 보고는 반영되지 않는다", func(t *testing.T) {
		latest, err := svc.Queries.Authorize(ctx, agentActor, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Commands.MarkRead(ctx, agentActor, conv.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != latest.LastSeq {
			t.Errorf("읽음 지점이 되돌아갔다: %d (현재 %d)", got, latest.LastSeq)
		}
	})

	t.Run("고객은 남의 대화 읽음을 보고할 수 없다", func(t *testing.T) {
		_, otherConv := testsupport.Conversation(t, store)
		_, err := svc.Commands.MarkRead(ctx, custActor, otherConv.ID, 1)
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})

	t.Run("읽음 지점 목록에 양쪽이 보인다", func(t *testing.T) {
		if _, err := svc.Commands.MarkRead(ctx, custActor, conv.ID, 2); err != nil {
			t.Fatal(err)
		}
		states, err := svc.Queries.ReadStates(ctx, custActor, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 2 {
			t.Fatalf("둘이어야 한다: %+v", states)
		}
	})
}

func TestTyping_정책(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(store)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)

	t.Run("자기 대화에는 입력 중 신호를 보낼 수 있다", func(t *testing.T) {
		actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID, Name: "타이핑"}
		if err := svc.Commands.Typing(ctx, actor, conv.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("남의 대화에는 보낼 수 없다", func(t *testing.T) {
		_, otherConv := testsupport.Conversation(t, store)
		actor := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: customerID}
		if err := svc.Commands.Typing(ctx, actor, otherConv.ID); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("ErrForbidden 이어야 한다: %v", err)
		}
	})
}

func TestActorKey(t *testing.T) {
	t.Run("주체 종류와 식별자를 합친다", func(t *testing.T) {
		a := app.ActorKey(app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: "a1"})
		c := app.ActorKey(app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorCustomer, CustomerID: "c1"})
		if a == c {
			t.Error("상담원과 고객 키가 겹치면 자기 이벤트를 걸러낼 수 없다")
		}
		if a != "AGENT:a1" || c != "CUSTOMER:c1" {
			t.Errorf("예상과 다르다: %q, %q", a, c)
		}
	})
}

// 입력이 여럿 틀리면 정해진 순서에서 첫 번째 것을 알린다. 고정된 필드는 선언 순서, 사용자가 키를 정하는 map(운영시간 요일)은 키 오름차순이다.
// map 순회 순서를 따르면 같은 요청에 문구가 실행마다 달라진다. 한 번에 50번 불러 매번 같은지 본다.
func TestValidation_입력이여럿틀리면정해진순서의첫오류를매번같은문구로알린다(t *testing.T) {
	svc := app.New(nil) // 검증은 저장소에 닿기 전에 끝난다
	ctx := context.Background()
	admin := app.Actor{WorkspaceID: core.DefaultWorkspaceID, Kind: app.ActorAgent, AgentID: id.New(), Role: core.RoleAdmin}
	long := strings.Repeat("가", 1001)

	cases := []struct {
		name, want string
		call       func() error
	}{
		{"받은함 필터는 assignee 다음 tag_id", "assignee 형식이 올바르지 않습니다", func() error {
			_, err := svc.Queries.Inbox(ctx, admin, core.InboxFilter{AssigneeID: "x", TagID: "y"})
			return err
		}},
		{"빠른 답변은 단축어, 제목, 본문", "단축어가 비어 있습니다", func() error {
			_, err := svc.Commands.CreateSnippet(ctx, admin, " ", " ", " ")
			return err
		}},
		{"빠른 답변은 단축어가 맞으면 제목", "제목이 비어 있습니다", func() error {
			_, err := svc.Commands.CreateSnippet(ctx, admin, "인사", " ", " ")
			return err
		}},
		{"자동 메시지는 환영, 접수, 부재중, 초안 지시", "환영 메시지는 1000자까지입니다", func() error {
			_, err := svc.Commands.UpdateSettings(ctx, admin, app.Settings{
				Messages: app.AutoMessages{Welcome: long, FirstReply: long, Away: long, DraftStyle: long},
				Widget:   app.WidgetSettings{Color: "#000000", Position: "right"},
			})
			return err
		}},
		{"자동 메시지는 환영이 맞으면 접수", "접수 안내는 1000자까지입니다", func() error {
			_, err := svc.Commands.UpdateSettings(ctx, admin, app.Settings{
				Messages: app.AutoMessages{FirstReply: long, Away: long, DraftStyle: long},
				Widget:   app.WidgetSettings{Color: "#000000", Position: "right"},
			})
			return err
		}},
		{"운영시간 요일은 키 오름차순", "요일이 올바르지 않습니다: abc", func() error {
			_, err := svc.Commands.UpdateSettings(ctx, admin, app.Settings{
				Hours: app.BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]app.HoursRange{
					"zzz": nil, "mon": {{From: "10:00", To: "09:00"}}, "abc": nil, "sun": {{From: "x", To: "y"}},
				}},
				Widget: app.WidgetSettings{Color: "#000000", Position: "right"},
			})
			return err
		}},
		{"운영시간 구간 오류도 키 오름차순의 첫 요일", "시작이 끝보다 앞서야 합니다: 10:00~09:00", func() error {
			_, err := svc.Commands.UpdateSettings(ctx, admin, app.Settings{
				Hours: app.BusinessHours{Timezone: "Asia/Seoul", Days: map[string][]app.HoursRange{
					"wed": {{From: "x", To: "y"}}, "fri": {{From: "10:00", To: "09:00"}}, "tue": {{From: "1", To: "2"}},
				}},
				Widget: app.WidgetSettings{Color: "#000000", Position: "right"},
			})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				var inv app.InvalidError
				if err := tc.call(); !errors.As(err, &inv) || inv.Reason != tc.want {
					t.Fatalf("%d번째: %v, %q 여야 한다", i+1, err, tc.want)
				}
			}
		})
	}
}

// 허용 목록이 있을 때 우리 자신(독립 위젯 페이지)의 오리진은 목록과 같은 규칙으로 비교한다 — 대소문자와 끝 슬래시를 가리지 않는다.
// 자기 오리진을 모르면(공개 주소 설정 누락) 자기 오리진 통로는 닫힌다. 빈 호스트 오리진이 빈 자기 오리진과 같다고 열리면 안 된다.
func TestWidgetOriginAllowed_자기오리진(t *testing.T) {
	t.Run("위젯 오리진 판정에서 자기 오리진은 목록과 같은 규칙으로 비교하고(대소문자·끝 슬래시 무시), 자기 오리진을 모르면 그 통로를 닫는다", func(t *testing.T) {
		listed := core.Workspace{AllowedOrigins: []string{"https://shop.example.com"}}
		open := core.Workspace{}
		cases := []struct {
			name       string
			ws         core.Workspace
			host, self string
			want       bool
		}{
			{"자기 오리진과 같으면 연다", listed, "https://chat.example.com", "https://chat.example.com", true},
			{"자기 오리진은 대소문자를 가리지 않는다", listed, "HTTPS://Chat.Example.COM", "https://chat.example.com", true},
			{"자기 오리진은 끝 슬래시를 가리지 않는다", listed, "https://chat.example.com/", "https://chat.example.com", true},
			{"설정의 자기 오리진이 대문자여도 같다", listed, "https://chat.example.com", "https://Chat.Example.com", true},
			{"목록의 오리진은 연다", listed, "https://SHOP.example.com/", "https://chat.example.com", true},
			{"목록에도 없고 자기도 아니면 닫는다", listed, "https://evil.example", "https://chat.example.com", false},
			{"호스트 오리진이 없으면 닫는다", listed, "", "https://chat.example.com", false},
			{"자기 오리진을 모르고 호스트 오리진도 없으면 닫는다", listed, "", "", false},
			{"자기 오리진을 모르고 호스트 오리진이 공백뿐이어도 닫는다", listed, "  ", "", false},
			{"자기 오리진을 모르면 목록 밖은 닫는다", listed, "https://evil.example", "", false},
			{"자기 오리진을 몰라도 목록 안은 연다", listed, "https://shop.example.com", "", true},
			{"목록이 비면 어디서든 연다", open, "", "", true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := app.WidgetOriginAllowed(tc.ws, tc.host, tc.self); got != tc.want {
					t.Errorf("host=%q self=%q: %v, %v 여야 한다", tc.host, tc.self, got, tc.want)
				}
			})
		}
	})
}
