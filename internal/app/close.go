package app

import (
	"context"
	"log/slog"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 고객이 상담을 직접 종료한다(D48). 잘못 눌러 연 상담이나 문의하기 전에 해결된 상담을 고객이 닫는다.
//
//   - 처리된 상담이다. 처리 주체는 지금처럼 응답으로 정한다 — 상담원이 답했으면 AGENT, AI 만 답했으면 AI,
//     아무 답도 없으면 고객 자체 해결(SELF)이다. 미응답(UNANSWERED)이 되지 않는다(core.ClassifyHandler).
//   - 닫은 상담은 고객 쪽에서 읽기 전용이다. 고객의 새 글은 거절하고(core.ErrClosedByCustomer) 다시 열지 않는다.
//     상담원이 데스크에서 다시 열면 풀린다. 상담원이 닫은 상담은 지금처럼 고객 글이 다시 연다.
//   - 만족도는 상담원·AI 의 공개 답이 있었을 때만 받는다(core.ErrNoReply).

// ClosedByCustomerNote 는 고객이 닫았을 때 남는 시스템 줄이다. 시스템 줄은 상담원만 본다(D25) — 고객은 상태를 대화 프레임으로 받는다.
const ClosedByCustomerNote = "고객이 상담을 종료했습니다."

// ErrClosedByCustomer 는 고객이 닫아 둔 상담에 고객이 새 글을 쓰려 할 때다. 409 closed_by_customer 다.
var ErrClosedByCustomer = core.NewError("closed_by_customer", "app: 고객이 직접 닫은 상담이라 고객의 새 글을 받지 않았다")

// ErrNoReply 는 상담원·AI 의 답 없이 고객이 닫은 상담에 만족도를 내려 할 때다. 409 no_reply 다.
var ErrNoReply = core.NewError("no_reply", "app: 상담원·AI 의 답 없이 고객이 닫은 상담이라 만족도를 받지 않았다")

// ErrNoCloser 는 저장소에 고객 종료 포트가 없을 때다. 운영 조립에서는 나지 않는다.
var ErrNoCloser = core.NewError("no_closer", "app: 저장소에 고객 종료 포트(CustomerCloser)가 없다")

// CloseByCustomer 는 그 상담의 고객이 상담을 닫는다. 이미 닫혀 있으면 같은 결과(지금 대화)로 답한다 — 두 번 눌러도, 다른 탭이 먼저 닫아도 200 이다.
// 권한은 permission.go 의 authorizeCustomerOn 한 곳이다(고객만, 자기 상담만 — I6·I11). 저장소가 잠근 행의 고객과 한 번 더 맞춘다.
func (s *Commands) CloseByCustomer(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	conv, err := s.q.authorizeCustomerOn(ctx, actor, convID)
	if err != nil {
		return core.Conversation{}, err
	}
	if s.closer == nil {
		return core.Conversation{}, ErrNoCloser
	}
	changed := false
	out, err := s.closer.CloseByCustomer(ctx, conv.ID, actor.CustomerID, func(_, _ core.Conversation) core.ChangeRecord {
		changed = true
		// 업무 기록의 주체는 상담원이 아니라 고객이다 — agent_id 를 비우고 누가 닫았는지를 detail 에 적는다.
		// 상담원별 집계(종료 수)는 상담원의 종료만 센다.
		return core.ChangeRecord{
			Note:  ClosedByCustomerNote,
			Event: core.ConversationEvent{Kind: core.EventClosed, Detail: map[string]any{"by": "customer"}},
		}
	})
	if err != nil {
		// 거절의 code·reason 은 http.request 한 줄이 싣는다(D47).
		return core.Conversation{}, translate(err)
	}
	if changed {
		slog.InfoContext(ctx, "conversation.state_changed", "conversation_id", out.ID, "from", conv.State, "to", core.StateClosed, "by", "customer")
	}
	return out, nil
}
