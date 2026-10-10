package core

import "time"

type Snippet struct {
	ID       string `json:"id"`
	Shortcut string `json:"shortcut"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

type Customer struct {
	ID            string         `json:"id"`
	WorkspaceID   string         `json:"-"`
	ExternalID    string         `json:"external_id,omitempty"`
	DisplayName   string         `json:"display_name"`
	Email         string         `json:"email,omitempty"`
	Phone         string         `json:"phone,omitempty"`
	Note          string         `json:"note"`
	Attributes    map[string]any `json:"attributes"`
	CreatedAt     time.Time      `json:"created_at"`
	Conversations int64          `json:"conversations"`
}

type CustomerProfile struct {
	DisplayName string
	Email       string
	Phone       string
	Note        string
	Attributes  map[string]any
}

// MemberProfile 은 호스트 사이트가 서명해서 넘긴 회원 정보다.
type MemberProfile struct {
	ExternalID  string
	DisplayName string
	Email       string
	Phone       string
}

type SearchHit struct {
	ConversationID string    `json:"conversation_id"`
	CustomerName   string    `json:"customer_name"`
	Seq            int64     `json:"seq"`
	Body           string    `json:"body"`
	SenderName     string    `json:"sender_name"`
	CreatedAt      time.Time `json:"created_at"`
}
