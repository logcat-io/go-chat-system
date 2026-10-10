package realtime

import (
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// clientFrame 은 브라우저가 보내는 프레임이다.
type clientFrame struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id,omitempty"`
	AfterSeq       int64  `json:"after_seq,omitempty"`
	ClientMsgID    string `json:"client_msg_id,omitempty"`
	Body           string `json:"body,omitempty"`
	AttachmentID   string `json:"attachment_id,omitempty"`
	// Internal 이 true 면 내부 메모다. 상담원만 쓸 수 있고 고객에게 전달되지 않는다.
	Internal bool `json:"internal,omitempty"`
	// read 프레임에서 "여기까지 읽었다" 지점. 보내는 쪽의 번호 공간이다.
	Seq int64 `json:"seq,omitempty"`
}

// 서버가 보내는 프레임 종류.
const (
	frameReady        = "ready"         // 구독이 붙었다. 대화 정보와 읽음 지점을 같이 준다
	frameMessage      = "message"       // 메시지 한 건. 보는 사람의 번호가 붙어 있다
	frameAck          = "ack"           // 보낸 메시지를 DB 에 커밋했다
	frameConversation = "conversation"  // 상태·담당자·태그가 바뀌었다
	frameError        = "error"         // 거절 또는 실패
	frameInboxChanged = "inbox_changed" // 상담원 받은함을 다시 불러라
	framePong         = "pong"
	frameTyping       = "typing" // 상대가 입력 중이다. 잃어도 되는 신호
	frameRead         = "read"   // 상대의 읽음 지점이 올라갔다
)

type serverFrame struct {
	Type string `json:"type"`

	ConversationID string `json:"conversation_id,omitempty"`
	// 보는 사람에 맞춘 표현이 들어간다. 고객에게는 태그·담당자·전체 번호가 빠진다.
	Conversation any    `json:"conversation,omitempty"`
	LastSeq      int64  `json:"last_seq,omitempty"`
	State        string `json:"state,omitempty"`

	Seq         int64            `json:"seq,omitempty"`
	ID          string           `json:"id,omitempty"`
	ClientMsgID string           `json:"client_msg_id,omitempty"`
	SenderKind  string           `json:"sender_kind,omitempty"`
	SenderID    string           `json:"sender_id,omitempty"`
	SenderName  string           `json:"sender_name,omitempty"`
	Visibility  string           `json:"visibility,omitempty"`
	Body        string           `json:"body,omitempty"`
	Attachment  *core.Attachment `json:"attachment,omitempty"`
	CreatedAt   *time.Time       `json:"created_at,omitempty"`
	// Kind 가 EDIT/DELETE 면 RefMessageID 의 메시지를 고치라는 뜻이다.
	Kind         string         `json:"kind,omitempty"`
	RefMessageID string         `json:"ref_message_id,omitempty"`
	EditedAt     *time.Time     `json:"edited_at,omitempty"`
	DeletedAt    *time.Time     `json:"deleted_at,omitempty"`
	Reads        []app.ReadView `json:"reads,omitempty"`

	ReaderKind string `json:"reader_kind,omitempty"`
	ReaderID   string `json:"reader_id,omitempty"`

	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// TraceID 는 오류 프레임에만 싣는다. 그 프레임을 낸 처리의 trace_id(32자리 hex)이고 같은 값으로 로그 한 줄(ws.rejected·ws.failed·
	// ws.unsubscribed)이 남는다. 화면은 앞 8자리를 "문의 번호" 로 보여 준다(D47). 요청 하나를 가리키는 값이라 세션 토큰이 아니다.
	TraceID string `json:"trace_id,omitempty"`
}

func messageFrame(m core.Message) serverFrame {
	created := m.CreatedAt
	return serverFrame{
		Type:           frameMessage,
		ConversationID: m.ConversationID,
		Seq:            m.Seq,
		ID:             m.ID,
		ClientMsgID:    m.ClientMsgID,
		SenderKind:     string(m.SenderKind),
		SenderID:       m.SenderID,
		SenderName:     m.SenderName,
		Visibility:     string(m.Visibility),
		Body:           m.Body,
		Attachment:     m.Attachment,
		CreatedAt:      &created,
		Kind:           string(m.Kind),
		RefMessageID:   m.RefMessageID,
		EditedAt:       m.EditedAt,
		DeletedAt:      m.DeletedAt,
	}
}

// conversationFingerprint 는 클라이언트 화면이 실제로 신경 쓰는 값만 모은다. 지문이 같으면 대화 프레임을 다시 보내지 않는다.
// updated_at 도 넣는다. 일반 메시지 저장은 updated_at 을 올리지 않으므로(pgstore messages.go) 메시지마다 프레임이 따라붙지 않는다.
func conversationFingerprint(c core.Conversation) string {
	var b strings.Builder
	b.WriteString(c.State.String())
	b.WriteByte('|')
	b.WriteString(c.AssigneeAgentID)
	b.WriteByte('|')
	b.WriteString(c.Subject)
	b.WriteByte('|')
	if c.SnoozedUntil != nil {
		b.WriteString(c.SnoozedUntil.UTC().Format(time.RFC3339))
	}
	// 태그는 여기서 세지 않는다. 태그가 바뀌면 updated_at 이 올라가고, 그걸로 충분하다.
	// 일반 메시지 저장은 updated_at 을 건드리지 않는다. 올리는 것은 재개·수정·삭제 이벤트와 메타 변경뿐이다.
	b.WriteByte('|')
	b.WriteString(c.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return b.String()
}

// conversationFrame 은 대화 프레임이다. tagsKnown 이 거짓이면(상담원에게 보낼 태그를 읽지 못했다, DEF-83) conversation 에 tags 필드를
// 싣지 않는다 — "태그 없음"(빈 목록)과 "태그를 모름"(필드 없음)을 가른다. 화면은 필드가 없으면 가진 태그를 그대로 둔다.
func conversationFrame(actor app.Actor, c core.Conversation, tagsKnown bool) serverFrame {
	view := app.ForActor(actor, c)
	if actor.Internal() && !tagsKnown {
		view = tagsUnknown{Conversation: c}
	}
	return serverFrame{
		Type:           frameConversation,
		ConversationID: c.ID,
		Conversation:   view,
		State:          c.State.String(),
		LastSeq:        c.ViewerSeq(actor.Internal()),
	}
}

func liveFrame(ev core.LiveEvent, internal bool) serverFrame {
	switch ev.Kind {
	case core.LiveTyping:
		return serverFrame{
			Type:           frameTyping,
			ConversationID: ev.ConversationID,
			SenderKind:     ev.ActorKind,
			SenderName:     ev.ActorName,
		}
	default:
		return serverFrame{
			Type:           frameRead,
			ConversationID: ev.ConversationID,
			ReaderKind:     ev.ActorKind,
			ReaderID:       ev.ActorID,
			SenderName:     ev.ActorName,
			Seq:            ev.ViewerSeq(internal),
		}
	}
}

// tagsUnknown 은 tags 필드를 뺀 상담원용 대화다. 바깥 Tags 가 같은 JSON 이름의 안쪽 필드를 가리고(얕은 쪽이 이긴다), 비어 있어 생략된다.
type tagsUnknown struct {
	core.Conversation
	Tags *struct{} `json:"tags,omitempty"`
}
