package core

import (
	"time"
)

// Attachment 는 대화에 올린 파일 하나다.
// 업로드와 전송을 나눈 이유는 멱등 때문이다. 업로드는 한 번만 하고,
// 메시지 전송은 같은 client_msg_id 로 몇 번이고 다시 보낼 수 있어야 한다.
type Attachment struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	MessageID      string    `json:"message_id,omitempty"`
	Filename       string    `json:"filename"`
	ContentType    string    `json:"content_type"`
	ByteSize       int64     `json:"byte_size"`
	Width          int       `json:"width,omitempty"`
	Height         int       `json:"height,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

type NewAttachment struct {
	ID             string
	ConversationID string
	UploaderKind   string
	UploaderID     string
	Filename       string
	ContentType    string
	ByteSize       int64
	StoragePath    string
	Width          int
	Height         int
}

// AttachmentRef 는 내려받기 권한 판정에 필요한 것이다.
//
// 대화만 보고 판정하면 안 된다. 내부 메모에 붙은 파일은 같은 대화에 있지만
// 고객이 받으면 안 되고, 아직 메시지에 붙지 않은 업로드도 올린 사람 외에는 안 된다.
type AttachmentRef struct {
	ConversationID string
	StoragePath    string
	Filename       string
	ContentType    string
	// Internal 이 true 면 내부 메모에 붙었거나 아직 전송되지 않은 업로드다.
	Internal     bool
	UploaderKind string
	UploaderID   string
	// 신청한 크기와 화면 자리용 크기. 올라온 뒤에 실제 값으로 확인한다.
	ByteSize int64
	Width    int
	Height   int
	// Uploaded 가 false 면 주소만 발급됐고 파일이 확인되지 않았다. 메시지에 붙일 수도, 내려받을 수도 없다.
	Uploaded bool
}
