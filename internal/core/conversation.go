package core

import (
	"time"
)

// State 는 대화 상태다. 채널톡의 진행·보류·완료와 같은 자리다.
// 문자열 그대로 두면 오타가 컴파일을 통과하고 런타임 CHECK 제약에서야 터진다.
type State string

const (
	StateOpen    State = "OPEN"
	StateSnoozed State = "SNOOZED"
	StateClosed  State = "CLOSED"
)

// Valid 는 정해진 셋 중 하나인지 본다. 빈 값은 "거르지 않음" 이라 따로 다룬다.
func (s State) Valid() bool {
	switch s {
	case StateOpen, StateSnoozed, StateClosed:
		return true
	default:
		return false
	}
}

func (s State) String() string { return string(s) }

type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Conversation struct {
	ID            string     `json:"id"`
	CustomerID    string     `json:"customer_id"`
	CustomerName  string     `json:"customer_name"`
	State         State      `json:"state"`
	Subject       string     `json:"subject"`
	LastSeq       int64      `json:"last_seq"`
	LastPublicSeq int64      `json:"last_public_seq"`
	LastMessageAt *time.Time `json:"last_message_at"`
	SnoozedUntil  *time.Time `json:"snoozed_until,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// UpdatedAt 은 변화 감지 표식이다. 상태·담당자·태그가 바뀌면 올라간다.
	UpdatedAt time.Time `json:"updated_at"`

	AssigneeAgentID string `json:"assignee_agent_id,omitempty"`
	AssigneeName    string `json:"assignee_name,omitempty"`
	// WorkspaceID 는 소속이다. 권한 판정에만 쓰고 응답에는 싣지 않는다.
	WorkspaceID string `json:"-"`
	// PastForCustomer 는 회원이 더 뒤의 상담을 연 지난 상담이다. 상담원은 계속 보고, 그 회원은 다시 볼 수 없다.
	PastForCustomer bool `json:"-"`
	// DeletedAt 은 관리자가 가린 상담이다. 행과 메시지는 남고, 조회 경로마다 없는 것으로 본다. 내보내기에만 이 값과 함께 나간다.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// ClosedBy 는 지금 종료 상태일 때 누가 닫았는지다(D48). 종료가 아니면 비어 있다. 고객이 닫았으면(CUSTOMER) 고객 쪽에서는 읽기 전용이고,
	// 상담원이 다시 열면 풀린다. 데스크는 상태 옆에 "고객 종료" 를 보인다.
	ClosedBy ClosedBy `json:"closed_by,omitempty"`

	Tags []Tag `json:"tags"`

	// 목록 표시용
	LastBody       string `json:"last_body,omitempty"`
	LastSenderKind string `json:"last_sender_kind,omitempty"`
	Unread         int64  `json:"unread"`
}

// CustomerClosed 는 지금 고객이 닫아 둔 상담인지다(D48). 고객의 새 글을 받지 않는다.
func (c Conversation) CustomerClosed() bool {
	return c.State == StateClosed && c.ClosedBy == ClosedByCustomer
}

// ViewerSeq 는 보는 사람 기준의 마지막 번호다.
func (c Conversation) ViewerSeq(internal bool) int64 {
	if internal {
		return c.LastSeq
	}
	return c.LastPublicSeq
}

// InboxFilter 는 상담원 받은함의 조건이다. 채널톡의 좌측 필터와 같은 자리다.
type InboxFilter struct {
	// WorkspaceID 는 필수다. 받은함은 테넌트 안에서만 본다.
	WorkspaceID string
	State       State  // 빈 값이면 거르지 않는다
	AssigneeID  string // 특정 상담원이 맡은 것만
	Unassigned  bool   // 담당자가 없는 것만
	FollowedBy  string // 내가 팔로우한 것만
	TagID       string
	Query       string // 고객 이름 또는 메시지 본문
	// NameOnly 면 Query 를 고객 이름에서만 찾는다. 전체 검색 권한(search.all)이 없는 상담원은 본문을 뒤지지 못한다.
	NameOnly bool
	// Deleted 면 가린 상담만 본다. 되살리기 화면이 쓴다. 꺼져 있으면 가린 상담은 빠진다.
	Deleted     bool
	ReaderID    string // 안 읽은 수를 셀 상담원
	OnlyWaiting bool   // 고객이 마지막으로 말한 뒤 상담원의 공개 답변이 없는 것만. 읽음과 무관하다
	Limit       int
}

// Deletion 은 상담을 가린 기록이다. 되살리기 화면의 머리에 보인다.
type Deletion struct {
	DeletedAt     time.Time `json:"deleted_at"`
	DeletedByName string    `json:"deleted_by_name"`
	Reason        string    `json:"reason"`
}

// InboxCounts 는 상담원이 좌측 필터에서 한눈에 보는 숫자다.
// "지금 내가 봐야 할 것이 몇 건인가" 에 답하는 게 목적이라 안 읽은 수를 같이 센다.
type InboxCounts struct {
	Todo       int64 `json:"todo"`       // 내게 배정됐고 고객이 답을 기다리는 것 (읽음과 무관)
	Mine       int64 `json:"mine"`       // 내가 맡은 진행중
	Unassigned int64 `json:"unassigned"` // 아무도 안 맡은 진행중
	Following  int64 `json:"following"`
	All        int64 `json:"all"`
	Open       int64 `json:"open"`
	Snoozed    int64 `json:"snoozed"`
	Closed     int64 `json:"closed"`
}

// ChangeCursor 는 "여기까지 훑었다" 는 자리다. 시각과 id 를 같이 들어
// 같은 시각에 여러 대화가 바뀌어도 건너뛰지 않는다.
type ChangeCursor struct {
	At time.Time
	ID string
}

// ChangedSince 는 커서 뒤에 바뀐 대화를 가져온다.
//
// 알림(NOTIFY)을 놓쳤을 때의 안전망이다. 연결마다 자기 대화를 되묻게 하면
// 3000 연결에서 초당 수백 질의가 되지만, 이렇게 모아 물으면 주기당 한 번이면 된다.
//
// 한도에 걸리면 더 있다고 알려 준다. 안전망이 조용히 잘리면 안전망이 아니다.
// ChangedConv 는 안전망이 깨울 대화와 그 소속, 그리고 "어디까지 왔는지" 다.
// 번호를 같이 실어야 구독자가 이미 따라잡은 대화에서 DB 를 다시 읽지 않는다 — 3000개 대화가 매 주기 깨어나면
// 그 되읽기 3000번이 곧 지연 스파이크다. UpdatedAt 은 상태·담당자·태그 같은 메시지 밖 변화의 시각이다 — 안전망이 이 값이
// 바뀌었는지 보고 메타 힌트를 켠다. 저장소가 "마지막 변경이 메시지인가" 로 가르면 상태를 바꾼 뒤 시스템 줄이 따라
// 붙는 변경(닫기·배정)이 메시지로 보여 대화 프레임이 안 나간다(D4).
type ChangedConv struct {
	ID          string
	WorkspaceID string
	Seq         int64
	PublicSeq   int64
	UpdatedAt   time.Time
	// At 은 이 변경의 시각(마지막 메시지와 마지막 갱신 중 늦은 쪽)이다. 안전망이 겹침 구간에서 이미 알린 변경을 가를 때 쓴다.
	At time.Time
}

// ChangeRecord 는 상태·담당 변경과 같은 트랜잭션에 남길 기록이다(D39). 변경이 커밋되면 둘 다 있고, 되돌려지면 둘 다 없다.
type ChangeRecord struct {
	// Note 는 상담원만 보는 시스템 줄이다. 비면 남기지 않는다.
	Note string
	// Event 는 업무 기록이다. Kind 가 비면 남기지 않는다. ConversationID 는 저장소가 채운다.
	Event ConversationEvent
}

// ChangeRecorder 는 바뀌기 전과 뒤의 대화(같은 트랜잭션 안에서 읽은 값)를 보고 남길 기록을 정한다.
// 무엇을 남길지는 유스케이스가 정하고, 실제 값은 저장소가 잠근 행에서 준다 — 화면이 본 값이 아니라 DB 가 바꾼 값이 기록에 남는다.
// 트랜잭션 안에서 불리므로 DB 나 다른 자원을 부르지 않는다(D28). nil 이면 아무것도 남기지 않는다.
type ChangeRecorder func(before, after Conversation) ChangeRecord
