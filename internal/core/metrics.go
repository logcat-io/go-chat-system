package core

import (
	"time"
)

// 상담 지표와 만족도 조사(D45). 정의는 design.md D45 가 정본이다.

// Handler 는 종료한 상담을 누가 처리했는지다. 상담을 끝낼 때 정하고, 다시 열었다 닫으면 마지막 종료의 것으로 덮는다.
type Handler string

const (
	HandlerAgent      Handler = "AGENT"      // 상담원의 공개 답이 하나라도 있다
	HandlerAI         Handler = "AI"         // 상담원 답은 없고 AI 응대의 공개 답만 있다
	HandlerUnanswered Handler = "UNANSWERED" // 고객에게 간 사람·AI 의 답이 없다(접수 안내·시스템 줄·내부 메모는 답이 아니다)
	// HandlerSelf 는 고객 자체 해결이다 — 사람·AI 의 답 없이 고객이 직접 닫았다(D48). 잘못 눌렀거나 문의 전에 해결된 상담이다.
	// 상담원 없이 끝난 상담이라 AI 처리율의 분자에 든다. 고객이 닫았으면 미응답(UNANSWERED)이 되지 않는다.
	HandlerSelf Handler = "SELF"
)

// ClosedBy 는 상담을 마지막으로 닫은 쪽이다(D48). 처리 주체(Handler)와 다른 축이다 — 고객이 닫아도 상담원이 답했으면 처리 주체는 상담원이다.
type ClosedBy string

const (
	ClosedByAgent    ClosedBy = "AGENT"    // 상담원이 완료를 눌렀다
	ClosedByCustomer ClosedBy = "CUSTOMER" // 고객이 위젯에서 상담 종료를 눌렀다. 고객 쪽에서는 읽기 전용이다
	ClosedBySystem   ClosedBy = "SYSTEM"   // 사람이 누르지 않았다(지난 상담을 되살릴 때 종료로 돌린 것, 0020 이전의 기록 중 상담원을 모르는 것)
)

// ClassifyHandler 는 처리 주체를 정한다. 상담원이 한 번이라도 공개로 답했으면 상담원 개입이다 — AI 가 먼저 답했어도 그렇다.
// 답이 하나도 없으면 누가 닫았는지로 가른다: 고객이 닫았으면 고객 자체 해결(SELF), 아니면 미응답(UNANSWERED)이다(D48).
func ClassifyHandler(agentReplied, aiReplied bool, by ClosedBy) Handler {
	switch {
	case agentReplied:
		return HandlerAgent
	case aiReplied:
		return HandlerAI
	case by == ClosedByCustomer:
		return HandlerSelf
	default:
		return HandlerUnanswered
	}
}

var (
	// ErrNotClosed 는 아직 끝나지 않은(또는 다시 열린) 상담에 만족도를 내려 할 때다.
	ErrNotClosed = NewError("not_closed", "core: 상담이 종료 상태가 아니라(진행 중이거나 다시 열렸다) 만족도를 받지 않았다")
	// ErrRatingExpired 는 종료 뒤 고칠 수 있는 시간이 지난 만족도다.
	ErrRatingExpired = NewError("rating_closed", "core: 종료 뒤 만족도를 내거나 고칠 수 있는 시간이 지났다")
	// ErrNoReply 는 상담원·AI 의 답 없이 고객이 직접 닫은 상담(처리 주체 SELF)에 만족도를 내려 할 때다(D48). 평가할 상담이 없었다.
	ErrNoReply = NewError("no_reply", "core: 상담원·AI 의 답 없이 고객이 닫은 상담이라 만족도를 받지 않았다")
)

// Rating 은 상담 하나의 만족도다. 상담당 한 행이고 마지막 값이다.
type Rating struct {
	Score   int    `json:"score"`
	Comment string `json:"comment"`
	// Handler·AgentID 는 낼 때의 종료 기록이다. 주체별·상담원별 집계가 쓴다. 고객 응답에는 싣지 않는다.
	Handler   Handler `json:"handler,omitempty"`
	AgentID   string  `json:"agent_id,omitempty"`
	AgentName string  `json:"agent_name,omitempty"`
	// ClosedAt 은 이 평가가 속한 종료 시각이다. 다시 열었다 닫으면 지금 종료와 달라진다.
	ClosedAt  time.Time `json:"closed_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RatingInput 은 고객이 내는 만족도다. 검증은 app 이 한다.
type RatingInput struct {
	ConversationID string
	CustomerID     string
	Score          int
	Comment        string
	// Window 는 종료 뒤 고칠 수 있는 시간이다. 저장소가 DB 시계로 판정한다.
	Window time.Duration
}

// RatingState 는 상담 하나의 만족도 조사 상태다. 저장소가 한 번에 읽는다.
type RatingState struct {
	// Closed 는 지금 종료 상태인지다. 다시 열린 상담은 조사를 받지 않는다.
	Closed bool
	// ClosedAt 은 마지막 종료 시각이다. 한 번도 닫힌 적 없으면 nil.
	ClosedAt *time.Time
	Handler  Handler
	// Now 는 DB 시계다. 고칠 수 있는 시간의 판정은 이 시계로 한다.
	Now time.Time
	// Rating 은 낸 평가다. 없으면 nil. 지난 종료의 평가일 수 있다(Rating.ClosedAt 과 ClosedAt 이 다르다).
	Rating *Rating
}

// DurationStats 는 시간 지표의 요약이다. 밀리초다. 건수가 0 이면 값들은 nil 이다 — 0초와 "없음" 을 가른다.
type DurationStats struct {
	Count int64    `json:"count"`
	AvgMs *float64 `json:"avg_ms"`
	P50Ms *float64 `json:"p50_ms"`
	P90Ms *float64 `json:"p90_ms"`
}

// SatisfactionStats 는 만족도 요약이다. 응답 수가 0 이면 비율·평균은 nil 이다.
type SatisfactionStats struct {
	Responses int64    `json:"responses"`
	Average   *float64 `json:"average"`
	// CSAT 는 4·5점의 비율(0~1)이다.
	CSAT *float64 `json:"csat"`
}

// ClosedCounts 는 기간 안에 (마지막으로) 종료된 상담 수와 처리 주체 분포다. 지금 종료 상태인 것만 센다.
type ClosedCounts struct {
	Total      int64 `json:"total"`
	Agent      int64 `json:"agent"`
	AI         int64 `json:"ai"`
	Unanswered int64 `json:"unanswered"`
	// Self 는 처리 주체가 고객 자체 해결(SELF)인 종료 수다 — 답 없이 고객이 닫았다(D48).
	Self int64 `json:"self"`
	// ByCustomer 는 고객이 직접 닫은 종료 수다(누가 닫았나). 처리 주체와 다른 축이라 상담원·AI 가 답한 뒤 고객이 닫은 것도 든다(D48).
	ByCustomer int64 `json:"by_customer"`
}

// ResponderMetrics 는 답한 주체 하나(상담원 한 명 또는 AI)의 지표다.
type ResponderMetrics struct {
	Kind      SenderKind `json:"kind"` // AGENT 또는 AI
	AgentID   string     `json:"agent_id,omitempty"`
	AgentName string     `json:"agent_name,omitempty"`
	// Handled 는 이 주체가 처리 주체인 종료 상담 수다(상담원은 담당·마지막 답 기준).
	Handled       int64             `json:"handled"`
	FirstResponse DurationStats     `json:"first_response"`
	Response      DurationStats     `json:"response"`
	Satisfaction  SatisfactionStats `json:"satisfaction"`
}

// Metrics 는 기간 하나의 상담 지표다(D45).
type Metrics struct {
	From   time.Time    `json:"from"`
	To     time.Time    `json:"to"`
	Closed ClosedCounts `json:"closed"`
	// AIRate 는 상담원 없이 끝난 비율 (AI + SELF) / (AI + SELF + AGENT) 다(D45·D48). 셋 다 0 이면 nil — 나눌 것이 없다.
	AIRate *float64 `json:"ai_rate"`
	// AIActive 는 이 워크스페이스에 AI 응대가 한 번이라도 있었는지다. 없으면 화면은 "AI 응대가 아직 없다" 고 쓴다 — 0% 로 읽히지 않게.
	AIActive      bool              `json:"ai_active"`
	FirstResponse DurationStats     `json:"first_response"`
	Response      DurationStats     `json:"response"`
	Satisfaction  SatisfactionStats `json:"satisfaction"`
	// ResponseRate 는 조사를 받은 종료 상담 중 만족도를 낸 비율이다. 고객 자체 해결(SELF)은 조사를 받지 않아 분모에서 뺀다(D48).
	// 조사를 받은 종료 상담이 0 이면 nil.
	ResponseRate *float64           `json:"response_rate"`
	Responders   []ResponderMetrics `json:"responders"`
}
