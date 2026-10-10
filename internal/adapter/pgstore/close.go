package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 고객이 상담을 직접 종료한다(D48). 포트는 app/ports_close.go 에 있다.
//
// 상담원의 종료(SetConversationState)와 같은 길(changeConversation)을 지난다 — 대화 행 잠금 → 상태·closed_by 변경 →
// 시스템 줄·업무 기록 → 처리 주체(recordClosure)가 한 트랜잭션이다. 그래서 같은 상담에 상담원의 답·배정·종료가 겹쳐도
// 대화 행 잠금 순서대로 줄을 선다:
//   - 상담원 답이 먼저 커밋되면 종료가 그 답을 보고 처리 주체를 AGENT 로 정한다.
//   - 종료가 먼저 커밋되면 상담원 답은 지금 규칙(공개 답은 닫힌 상담을 다시 연다)대로 다시 연다. 다시 열면 고객 입력도 다시 열린다.
//   - 상담원의 종료가 먼저면 이미 닫혀 있으니 고객 종료는 아무것도 쓰지 않고 지금 대화를 준다(누가 닫았나는 상담원 그대로).
// 고객의 새 글은 번호 발급(allocate)이 잠근 행의 closed_by 를 보고 거절한다(core.ErrClosedByCustomer).

// CloseByCustomer 는 그 상담의 고객이 상담을 닫는다. 이미 닫혀 있으면(누가 닫았든) 아무것도 쓰지 않고 지금 대화를 준다 — 멱등이다.
// 그 상담의 고객이 아니거나 회원의 지난 상담이거나 가린 상담이면 core.ErrNotFound 다(앱의 권한 검사가 빠져도 남의 상담을 닫지 않는다).
func (s *Store) CloseByCustomer(ctx context.Context, convID, customerID string, record core.ChangeRecorder) (core.Conversation, error) {
	return s.changeConversation(ctx, convID, record, closeByCustomer(ctx, customerID), nil)
}

// closeByCustomer 는 잠근 행에서 판정하고 닫는다. changeOnce 의 change 다.
func closeByCustomer(ctx context.Context, customerID string) func(tx pgx.Tx, before core.Conversation) error {
	return func(tx pgx.Tx, before core.Conversation) error {
		if before.CustomerID != customerID || before.PastForCustomer {
			return core.ErrNotFound
		}
		if before.State == core.StateClosed {
			return errUnchanged
		}
		if _, err := tx.Exec(ctx, `
			UPDATE conversations
			   SET state = 'CLOSED', snoozed_until = NULL, closed_by = 'CUSTOMER', updated_at = clock_timestamp()
			 WHERE id = $1`, before.ID); err != nil {
			return err
		}
		return recordClosure(ctx, tx, before.ID, core.ClosedByCustomer)
	}
}
