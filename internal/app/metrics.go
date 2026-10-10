package app

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 상담 지표와 만족도 조사(D45). 포트는 ports_metrics.go 에 있다.
//
//   AI 처리율       종료 상담 중 상담원 개입 없이 끝난 비율 = (AI + SELF) / (AI + SELF + AGENT). SELF 는 답 없이 고객이 닫은 고객 자체 해결이다(D48).
//                  처리 주체는 끝낼 때 정한다(core.ClassifyHandler).
//   첫 응답 시간     상담의 첫 고객 공개 글 → 첫 상담원·AI 공개 답
//   평균 응답 시간   고객이 기다린 모든 차례(답이 없던 고객 글들 중 첫 글 → 다음 상담원·AI 공개 답)의 평균
//   만족도          종료 뒤 고객이 낸 별점 1~5 와 한 줄 의견. CSAT = 4·5점 비율, 응답률 = 낸 수 / 조사를 받은 종료 상담 수(종료 상담 − SELF. 고객 자체 해결은 답이 없어 조사를 받지 않는다, D48)

const (
	// RatingWindow 는 종료 뒤 만족도를 내거나 고칠 수 있는 시간이다. 다시 열었다 닫으면 새 종료부터 다시 센다.
	RatingWindow = 24 * time.Hour
	// MaxRatingComment 는 한 줄 의견의 글자 수 상한이다.
	MaxRatingComment = 200
	// MetricsDefaultRange 는 기간을 주지 않았을 때의 길이다(끝은 지금).
	MetricsDefaultRange = 7 * 24 * time.Hour
	// MetricsMaxRange 는 한 번에 볼 수 있는 기간의 상한이다. 집계가 기간 인덱스를 타도 범위가 끝없이 넓으면 느려진다.
	MetricsMaxRange = 366 * 24 * time.Hour
)

var (
	// ErrNotClosed 는 아직 끝나지 않은(다시 열린) 상담에 만족도를 내려 할 때다.
	ErrNotClosed = core.NewError("not_closed", "app: 상담이 종료 상태가 아니라 만족도를 받지 않았다")
	// ErrRatingExpired 는 종료 뒤 RatingWindow 가 지난 만족도다.
	ErrRatingExpired = core.NewError("rating_closed", "app: 종료 뒤 만족도를 내거나 고칠 수 있는 시간이 지났다")
	// ErrCustomerOnly 는 고객만 할 수 있는 일(만족도 내기)을 상담원이 부를 때다. 403 이다.
	ErrCustomerOnly = core.WrapError(ErrForbidden, "customer_only", "app: 고객 전용 기능을 고객이 아닌 주체가 불렀다")
)

// RatingView 는 상담 하나의 만족도 조사 상태다. 위젯의 별점 카드와 데스크 대화의 평가 칸이 같이 쓴다.
type RatingView struct {
	// Closed 는 지금 종료 상태인지다. 다시 열린 상담은 조사를 받지 않는다.
	Closed   bool       `json:"closed"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	// EditableUntil 은 마지막 종료에서 RatingWindow 뒤다. 그때까지 내고 고칠 수 있다.
	EditableUntil *time.Time `json:"editable_until,omitempty"`
	// CanRate 는 지금 이 사람이 낼 수 있는지다. 상담원에게는 늘 false 다.
	CanRate bool `json:"can_rate"`
	// Handler 는 마지막 종료의 처리 주체다. 상담원에게만 싣는다.
	Handler core.Handler `json:"handler,omitempty"`
	// Rating 은 낸 평가(마지막 값)다. 없으면 null.
	Rating *core.Rating `json:"rating"`
	// Current 는 Rating 이 지금 종료의 평가인지다. 다시 열었다 닫은 뒤에는 앞선 종료의 평가라 false 다.
	Current bool `json:"current"`
}

// ratingView 는 저장소의 상태를 보는 사람에 맞게 옮긴다. 고객에게는 처리 주체·담당 상담원을 싣지 않는다(D11).
func ratingView(actor Actor, st core.RatingState) RatingView {
	v := RatingView{Closed: st.Closed, ClosedAt: st.ClosedAt}
	if st.Closed {
		until := st.ClosedAt.Add(RatingWindow)
		v.EditableUntil = &until
		// 답 없이 고객이 닫은 상담(고객 자체 해결)은 평가할 상담이 없었다 — 별점을 띄우지 않는다(D48). 저장소도 거절한다(no_reply).
		v.CanRate = actor.Kind == ActorCustomer && !st.Now.After(until) && st.Handler != core.HandlerSelf
	}
	if st.Rating != nil {
		r := *st.Rating
		v.Current = st.ClosedAt != nil && r.ClosedAt.Equal(*st.ClosedAt)
		if !actor.Internal() {
			r.Handler, r.AgentID, r.AgentName = "", "", ""
		}
		v.Rating = &r
	}
	if actor.Internal() {
		v.Handler = st.Handler
	}
	return v
}

// Rating 은 상담 하나의 만족도 조사 상태다. 그 상담의 고객과, 그 상담을 볼 수 있는 상담원이 본다(I6·I11).
func (s *Queries) Rating(ctx context.Context, actor Actor, convID string) (RatingView, error) {
	conv, err := s.Authorize(ctx, actor, convID)
	if err != nil {
		return RatingView{}, err
	}
	if actor.Kind == ActorAgent {
		if err := require(ctx, actor, PermConversationWork); err != nil {
			return RatingView{}, err
		}
	}
	st, err := s.ratings.RatingState(ctx, conv.ID)
	if err != nil {
		return RatingView{}, translate(err)
	}
	return ratingView(actor, st), nil
}

// RateParams 는 고객이 내는 만족도다.
type RateParams struct {
	Score   int
	Comment string
}

// validate 는 별점 1~5 와 한 줄 의견(앞뒤 공백을 떼고, 줄바꿈은 빈칸으로, MaxRatingComment 자까지)을 본다.
func (p RateParams) validate() (RateParams, error) {
	if p.Score < 1 || p.Score > 5 {
		return p, invalid("score_range", "score 는 1~5 여야 합니다")
	}
	p.Comment = strings.Join(strings.Fields(p.Comment), " ")
	if utf8.RuneCountInString(p.Comment) > MaxRatingComment {
		return p, invalid("comment_too_long", "의견은 %d자까지입니다", MaxRatingComment)
	}
	return p, nil
}

// Rate 는 그 상담의 고객이 만족도를 낸다. 상담당 한 번이고 RatingWindow 안에서는 고친다(마지막 값).
// 남의 상담은 권한 검사(Authorize)가 막고, 저장소가 잠근 행의 고객과 한 번 더 맞춰 본다.
func (s *Commands) Rate(ctx context.Context, actor Actor, convID string, p RateParams) (RatingView, error) {
	if err := requireCustomer(actor); err != nil {
		return RatingView{}, err
	}
	p, err := p.validate()
	if err != nil {
		return RatingView{}, err
	}
	conv, err := s.q.Authorize(ctx, actor, convID)
	if err != nil {
		return RatingView{}, err
	}
	st, err := s.ratings.PutRating(ctx, core.RatingInput{
		ConversationID: conv.ID, CustomerID: actor.CustomerID, Score: p.Score, Comment: p.Comment, Window: RatingWindow,
	})
	if err != nil {
		err = translate(err)
		logRejected(ctx, "rating.rejected", err, "conversation_id", conv.ID)
		return RatingView{}, err
	}
	// 의견 본문은 남기지 않는다(D30). 점수는 사람을 가리키지 않는다.
	slog.InfoContext(ctx, "rating.saved", "conversation_id", conv.ID, "score", p.Score)
	return ratingView(actor, st), nil
}

// Metrics 는 기간의 상담 지표다. 팀 전체의 숫자라 남의 업무 기록을 볼 수 있는 권한(work.view_all)이 본다.
// from·to 가 비면 끝은 지금, 처음은 그 7일 전이다.
func (s *Queries) Metrics(ctx context.Context, actor Actor, from, to time.Time) (core.Metrics, error) {
	if err := require(ctx, actor, PermWorkViewAll); err != nil {
		return core.Metrics{}, err
	}
	to, err := s.windowEnd(ctx, to)
	if err != nil {
		return core.Metrics{}, err
	}
	if from.IsZero() {
		from = to.Add(-MetricsDefaultRange)
	}
	switch {
	case !from.Before(to):
		return core.Metrics{}, invalid("range_order", "from 은 to 보다 앞이어야 합니다")
	case to.Sub(from) > MetricsMaxRange:
		return core.Metrics{}, invalid("range_too_long", "기간은 %d일까지입니다", int(MetricsMaxRange/(24*time.Hour)))
	}
	m, err := s.metrics.Metrics(ctx, actor.WorkspaceID, from, to)
	if err != nil {
		return core.Metrics{}, translate(err)
	}
	if m.Responders == nil {
		m.Responders = []core.ResponderMetrics{}
	}
	return m, nil
}

// AIReply 는 AI 응대의 답 한 건을 대화에 넣는 자리다(D45). 지금은 부르는 곳이 없고 상담 흐름 개편의 AI 모듈이 부른다.
// 발신 종류가 AI 라 지표가 응답으로 세고 처리 주체를 AI 로 가른다. 공개 메시지이고 닫힌 상담을 다시 열지 않는다.
// 사람의 요청이 아니라 서버 안의 호출이라 actor 가 없다 — 권한은 부르는 모듈이 정한다.
func (s *Commands) AIReply(ctx context.Context, convID, clientMsgID, body string) (core.AppendResult, error) {
	p, err := SendParams{ConversationID: convID, ClientMsgID: clientMsgID, Body: body}.validate()
	if err != nil {
		return core.AppendResult{}, err
	}
	res, err := s.write.AppendMessage(ctx, core.AppendParams{
		ConversationID: p.ConversationID,
		ClientMsgID:    p.ClientMsgID,
		SenderKind:     core.SenderAI,
		Visibility:     core.Public,
		Body:           p.Body,
	})
	if err != nil {
		return core.AppendResult{}, translate(err)
	}
	logAppended(ctx, res)
	return res, nil
}
