package core

import (
	"time"
)

// ErrNotOwner 는 남의 메시지를 고치려는 시도다.
var ErrNotOwner = NewError("not_owner", "core: 이 메시지를 보낸 사람이 아니라 고치거나 지우지 않았다")

// ErrSystemLine 은 시스템 줄(배정·종료·보류 기록)을 고치거나 지우려는 시도다. 시스템 줄은 운영 기록이라 누구도(관리자도) 바꾸지 못한다(감사 28).
var ErrSystemLine = NewError("system_line", "core: 시스템 줄은 운영 기록이라 고치거나 지우지 않았다")

// ReviseParams 는 수정·삭제 공통 입력이다.
type ReviseParams struct {
	ConversationID string
	MessageID      string
	// 요청자. 메시지 발신자와 같아야 한다. AdminOverride 면 상담원·AI·봇 메시지를 관리자가 지울 수 있다(고객 글과 시스템 줄은 아니다).
	ActorKind     SenderKind
	ActorAgentID  string
	ActorCustomer string
	AdminOverride bool
	// Body 는 수정일 때 새 본문이다. 삭제는 비운다.
	Body string
}

// Revisions 는 한 메시지의 원문 이력이다. 최신이 마지막이다.
type Revision struct {
	Body      string    `json:"body"`
	Reason    string    `json:"reason"`
	ActorKind string    `json:"actor_kind"`
	ActorID   string    `json:"actor_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EventKind 는 업무 기록(conversation_events.kind)의 종류다. 값은 DB 에 그대로 저장되고 업무 기록 API 로 그대로 나간다.
// 상담원별 집계(pgstore 의 WorkSummary)가 이 값들을 SQL 문자열로 센다. 값을 바꾸거나 세는 종류를 더하면 그 SQL 도 같이 고친다.
type EventKind string

const (
	EventOpened      EventKind = "opened"      // 상담이 열렸다
	EventAssigned    EventKind = "assigned"    // 담당자가 생겼다. 집계의 배정에 든다
	EventTransferred EventKind = "transferred" // 담당자가 다른 상담원으로 바뀌었다. 집계의 배정에 든다
	EventUnassigned  EventKind = "unassigned"  // 담당자가 풀렸다
	EventAnswered    EventKind = "answered"    // 기다리던 고객에게 첫 공개 답변이 갔다. 집계의 응답
	EventClosed      EventKind = "closed"      // 종료. 집계의 종료
	EventSnoozed     EventKind = "snoozed"     // 보류. 집계의 보류
	EventReopened    EventKind = "reopened"    // 상담원이 다시 열었다
	EventTagged      EventKind = "tagged"      // 태그를 붙였다
	EventDeleted     EventKind = "deleted"     // 상담을 가렸다
	EventRestored    EventKind = "restored"    // 가린 상담을 되살렸다
)

type ConversationEvent struct {
	ID             int64          `json:"id"`
	WorkspaceID    string         `json:"-"`
	ConversationID string         `json:"conversation_id"`
	CustomerID     string         `json:"customer_id,omitempty"`
	CustomerName   string         `json:"customer_name,omitempty"`
	AgentID        string         `json:"agent_id,omitempty"`
	AgentName      string         `json:"agent_name,omitempty"`
	Kind           EventKind      `json:"kind"`
	Detail         map[string]any `json:"detail"`
	CreatedAt      time.Time      `json:"created_at"`
}

type EventFilter struct {
	WorkspaceID    string
	AgentID        string
	ConversationID string
	From, To       time.Time
	Limit          int
	// IncludeDeleted 가 false 면 삭제된 상담의 이벤트를 뺀다. 삭제한 상담은 그 권한이 있는 사람만 본다(D29).
	IncludeDeleted bool
}

// AgentWork 는 상담원 한 명의 기간 집계다. 대시보드·칸반의 첫 숫자다.
type AgentWork struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Assigned  int64  `json:"assigned"`
	Answered  int64  `json:"answered"`
	Closed    int64  `json:"closed"`
	Snoozed   int64  `json:"snoozed"`
	Customers int64  `json:"customers"` // 기간 안에 손댄 고객 수
}

// OpenResult 는 회원의 방 열기가 무엇을 했는지다.
type OpenResult int

const (
	OpenedExisting OpenResult = iota // 이미 열린 상담이 있다
	OpenedNew                        // 새 상담을 열었다
)

// PriorCursor 는 "이전 상담 더 보기" 의 위치다. 이 대화의 이 번호 앞, 그리고 그보다 먼저 열린 대화들을 가리킨다.
// Seq 가 0 이면 그 대화는 통째로 건너뛰고 더 먼저 열린 대화부터다.
type PriorCursor struct {
	ConversationID string
	Seq            int64
}
