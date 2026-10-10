package core

import (
	"time"
)

type SenderKind string

const (
	SenderCustomer SenderKind = "CUSTOMER"
	SenderAgent    SenderKind = "AGENT"
	SenderSystem   SenderKind = "SYSTEM"
	// SenderBot 은 자동 응답이다. 말풍선으로 보이되 사람이 아닌 걸 표시한다.
	SenderBot SenderKind = "BOT"
	// SenderAI 는 AI 응대의 답이다(D45). 접수 안내·부재중 같은 정해진 문장(BOT)과 다르다 — 고객의 물음에 답한 것이라
	// 지표가 응답으로 세고 처리 주체를 AI 로 가른다. 지금은 쓰는 길이 없고 상담 흐름 개편의 AI 모듈이 Commands.AIReply 로 쓴다.
	SenderAI SenderKind = "AI"
)

// Responds 는 이 발신 종류의 공개 메시지가 고객을 기다리게 하던 차례를 닫는 답인지다. 상담원과 AI 응대만 답이다 —
// 접수 안내(BOT)·시스템 줄(SYSTEM)은 아니다(D45).
func (k SenderKind) Responds() bool { return k == SenderAgent || k == SenderAI }

type Visibility string

const (
	// Public 은 고객에게 전달되는 메시지다. 고객용 번호(public_seq)를 받는다.
	Public Visibility = "PUBLIC"
	// Private 는 내부 메모다. 고객에게 전달되지 않고 고객용 번호도 받지 않는다.
	Private Visibility = "PRIVATE"
)

// MessageKind 는 로그 행의 종류다. EDIT·DELETE 는 ref_message_id 가 가리키는 메시지를 고친 이벤트다.
type MessageKind string

const (
	KindMessage MessageKind = "MESSAGE"
	KindEdit    MessageKind = "EDIT"
	KindDelete  MessageKind = "DELETE"
)

type Message struct {
	ConversationID string `json:"conversation_id"`
	// Seq 는 보는 사람의 번호다. 상담원에게는 전체 번호, 고객에게는 공개 번호를 담는다.
	Seq         int64       `json:"seq"`
	ID          string      `json:"id"`
	ClientMsgID string      `json:"client_msg_id"`
	SenderKind  SenderKind  `json:"sender_kind"`
	SenderID    string      `json:"sender_id,omitempty"`
	SenderName  string      `json:"sender_name"`
	Visibility  Visibility  `json:"visibility"`
	Body        string      `json:"body"`
	Attachment  *Attachment `json:"attachment,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	// Kind 가 EDIT/DELETE 면 RefMessageID 의 메시지가 바뀐 것이다. 화면은 이 행을 줄로 그리지 않고 그 메시지를 고친다.
	Kind         MessageKind `json:"kind"`
	RefMessageID string      `json:"ref_message_id,omitempty"`
	EditedAt     *time.Time  `json:"edited_at,omitempty"`
	DeletedAt    *time.Time  `json:"deleted_at,omitempty"`
}

type AppendParams struct {
	ConversationID string
	ClientMsgID    string
	// Kind 가 비어 있으면 MESSAGE 다.
	Kind           MessageKind
	RefMessageID   string
	SenderKind     SenderKind
	SenderAgentID  string
	SenderCustomer string
	Visibility     Visibility
	Body           string
	// AttachmentID 가 있으면 업로드해 둔 파일을 이 메시지에 이어 붙인다.
	AttachmentID string
	// Reopen 이 true 면 닫히거나 보류된 대화를 다시 연다.
	Reopen bool
}

type AppendResult struct {
	Message        Message
	Created        bool
	NeedsAutoReply bool
	// Answered 는 기다리던 고객에게 상담원의 공개 답변이 간 것이다. 업무 기록이 쓴다.
	Answered bool
	// Responded 는 이 메시지가 고객이 기다리던 차례를 닫은 답(상담원·AI 공개 답)이라 응답 기록이 같은 트랜잭션에 남은 것이다(D45).
	Responded bool
	// Seq·PublicSeq 는 새로 저장한 행의 두 번호다. Message.Seq 는 보낸 사람 쪽 번호라 둘을 가를 수 없어 따로 둔다.
	// 운영 로그가 쓴다. 내부 메모는 PublicSeq 가 0 이고, 재전송(Created=false)이면 둘 다 0 이다.
	Seq       int64
	PublicSeq int64
}

// ErrClientMsgIDTaken 은 같은 client_msg_id 를 다른 발신자가 쓴 것이다. 재전송이 아니라 충돌이다.
var ErrClientMsgIDTaken = NewError("client_msg_id_taken", "core: 이 client_msg_id 는 같은 대화에서 다른 발신자가 이미 썼다")
