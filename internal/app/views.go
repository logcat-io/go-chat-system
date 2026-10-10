package app

import (
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 고객에게 나가는 표현은 저장소 구조체를 그대로 쓰지 않는다.
//
// 공개 범위를 코어가 소유한다는 원칙(docs/design.md D6)을 메시지 본문에만 적용하면,
// 태그·담당자·운영 메모처럼 그 주변에 붙은 운영 정보가 그대로 샌다.
// "악성민원" 같은 태그 이름과 담당자 실명이 고객 브라우저에서 읽히면 그것도 유출이다.
// 그래서 고객용 표현을 따로 둔다. 새 필드를 추가해도 기본값은 "고객에게 안 보임" 이다.

// CustomerConversation 은 고객이 보는 대화다. 번호는 고객 번호 공간으로 맞춰 준다.
type CustomerConversation struct {
	ID             string     `json:"id"`
	CustomerID     string     `json:"customer_id"`
	State          core.State `json:"state"`
	Subject        string     `json:"subject"`
	LastSeq        int64      `json:"last_seq"`
	LastMessageAt  *time.Time `json:"last_message_at"`
	CreatedAt      time.Time  `json:"created_at"`
	LastBody       string     `json:"last_body,omitempty"`
	LastSenderKind string     `json:"last_sender_kind,omitempty"`
	Unread         int64      `json:"unread"`
	// ClosedBy 는 지금 종료일 때 누가 닫았는지다(D48). CUSTOMER 면 위젯은 입력창 대신 "새 문의하기" 를 보인다.
	ClosedBy core.ClosedBy `json:"closed_by,omitempty"`
}

// ForActor 는 보는 사람에 맞는 대화 표현을 돌려준다.
// 상담원에게는 저장소 구조체 그대로, 고객에게는 운영 정보를 뺀 것.
func ForActor(actor Actor, c core.Conversation) any {
	if actor.Internal() {
		return c
	}
	return CustomerConversation{
		ID: c.ID, CustomerID: c.CustomerID, State: c.State, Subject: c.Subject,
		LastSeq: c.LastPublicSeq, LastMessageAt: c.LastMessageAt, CreatedAt: c.CreatedAt,
		LastBody: c.LastBody, LastSenderKind: c.LastSenderKind, Unread: c.Unread, ClosedBy: c.ClosedBy,
	}
}

func ForActorList(actor Actor, convs []core.Conversation) []any {
	out := make([]any, 0, len(convs))
	for _, c := range convs {
		out = append(out, ForActor(actor, c))
	}
	return out
}

// CustomerSelf 는 고객이 보는 자기 프로필이다. 운영 메모와 속성은 뺀다.
type CustomerSelf struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
}

func CustomerForActor(actor Actor, c core.Customer) any {
	if actor.Internal() {
		return c
	}
	return CustomerSelf{ID: c.ID, DisplayName: c.DisplayName, Email: c.Email, Phone: c.Phone}
}

// ReadView 는 읽음 지점을 보는 사람의 번호 공간으로 옮긴 것이다.
// 두 공간을 그대로 흘리면 숨긴 메시지 수가 번호 차이로 드러난다.
type ReadView struct {
	ReaderKind  string `json:"reader_kind"`
	ReaderID    string `json:"reader_id"`
	ReaderName  string `json:"reader_name"`
	LastReadSeq int64  `json:"last_read_seq"`
}

func ReadViews(states []core.ReadState, internal bool) []ReadView {
	out := make([]ReadView, 0, len(states))
	for _, s := range states {
		out = append(out, ReadView{
			ReaderKind:  s.ReaderKind,
			ReaderID:    s.ReaderID,
			ReaderName:  s.ReaderName,
			LastReadSeq: s.ViewerSeq(internal),
		})
	}
	return out
}
