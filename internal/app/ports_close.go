package app

import (
	"context"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 고객이 상담을 직접 종료하는 포트다(D48). 쓰는 쪽(close.go)이 정의하고 Store 에 더하지 않는다.
// 저장소가 이 능력을 가지면 New 가 꽂는다(pgstore 와 그것을 감싼 테스트 대역이 가진다). 없으면 고객 종료는 ErrNoCloser 다.

// CustomerCloser 는 그 상담의 고객이 상담을 닫는다.
type CustomerCloser interface {
	// CloseByCustomer 는 잠근 대화 행에서 판정하고 닫는다. record 가 정한 시스템 줄·업무 기록과 처리 주체(종료 기록)를 같은 트랜잭션에 남긴다.
	// 이미 닫혀 있으면(누가 닫았든) 아무것도 쓰지 않고 record 도 부르지 않고 지금 대화를 준다.
	// 그 상담의 고객이 아니거나 회원의 지난 상담이거나 가린 상담이면 core.ErrNotFound 다.
	CloseByCustomer(ctx context.Context, convID, customerID string, record core.ChangeRecorder) (core.Conversation, error)
}
