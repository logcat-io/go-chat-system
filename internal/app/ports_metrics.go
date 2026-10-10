package app

import (
	"context"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 상담 지표와 만족도 조사(D45)의 포트다. 쓰는 쪽(metrics.go)이 정의하고 Store 에 더하지 않는다.
// 응답 기록과 종료 기록은 메시지 저장·상태 변경 트랜잭션 안에서 저장소가 남긴다 — 여기에는 읽기와 만족도 쓰기만 있다.

// RatingReader 는 상담 하나의 만족도 조사 상태를 읽는다.
type RatingReader interface {
	// RatingState 는 지금 종료인지, 마지막 종료 시각, 낸 평가를 한 번에 준다. 없는 대화는 core.ErrNotFound 다.
	RatingState(ctx context.Context, convID string) (core.RatingState, error)
}

// RatingWriter 는 고객의 만족도를 남긴다.
type RatingWriter interface {
	// PutRating 은 상담당 한 행을 덮어쓴다. 그 상담의 고객이 아니거나 가린 상담이면 core.ErrNotFound,
	// 지금 종료가 아니면 core.ErrNotClosed, 마지막 종료에서 in.Window 가 지났으면 core.ErrRatingExpired 다(DB 시계).
	PutRating(ctx context.Context, in core.RatingInput) (core.RatingState, error)
}

// MetricsReader 는 기간의 상담 지표를 읽는다.
type MetricsReader interface {
	// Metrics 는 [from, to) 의 지표다. 가린 상담은 세지 않는다.
	Metrics(ctx context.Context, wsID string, from, to time.Time) (core.Metrics, error)
}

// MetricsStore 는 조립부가 한 번 꽂는 지표·만족도 저장소다.
type MetricsStore interface {
	RatingReader
	RatingWriter
	MetricsReader
}

// UseMetrics 는 지표·만족도 저장소를 꽂는다. 조립부(cmd/server)가 한 번 부른다. 조회는 읽기 포트만 받는다.
func (a *App) UseMetrics(store MetricsStore) {
	a.Queries.ratings = store
	a.Queries.metrics = store
	a.Commands.ratings = store
}
