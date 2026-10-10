package httpapi

import (
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 응답 타입. map[string]any 를 쓰면 필드 이름 오타가 컴파일을 통과하고,
// 고객용 뷰와 상담원용 뷰가 뒤섞여도 아무도 모른다.

type statusResponse struct {
	Status string `json:"status"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type meResponse struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	AgentID    string `json:"agent_id,omitempty"`
	CustomerID string `json:"customer_id,omitempty"`
	Role       string `json:"role,omitempty"`
	// Permissions 는 화면이 버튼·메뉴·탭을 그릴지 정하는 근거다. 고객은 빈 목록이다. 막는 것은 서버다.
	Permissions []app.Permission `json:"permissions"`
	Connections int              `json:"connections,omitempty"` // 상담원만. 고객 응답에는 키 자체가 없다
	// DraftModel 이 비어 있으면 화면이 초안 버튼을 숨긴다.
	DraftModel string `json:"draft_model,omitempty"`
	// Workspace 는 소속이다. 데스크가 상단에 이름을 띄운다.
	Workspace *app.WorkspaceSummary `json:"workspace,omitempty"`
}

type customerSessionResponse struct {
	Token        string `json:"token"`
	Customer     any    `json:"customer"`
	Conversation any    `json:"conversation"`
}

type agentLoginResponse struct {
	Token string     `json:"token"`
	Agent core.Agent `json:"agent"`
}

type conversationsResponse struct {
	Conversations []any `json:"conversations"`
}

type conversationResponse struct {
	Conversation any `json:"conversation"`
}

type messagesResponse struct {
	Messages []core.Message `json:"messages"`
}

type sendResponse struct {
	Message core.Message `json:"message"`
	Created bool         `json:"created"`
}

type readsResponse struct {
	Reads []app.ReadView `json:"reads"`
}

type readResponse struct {
	LastReadSeq int64 `json:"last_read_seq"`
}

type attachmentResponse struct {
	Attachment core.Attachment `json:"attachment"`
}

type followersResponse struct {
	Followers []core.Agent `json:"followers"`
}

type followResponse struct {
	Following bool `json:"following"`
}

type tagsResponse struct {
	Tags []core.Tag `json:"tags"`
}

type searchResponse struct {
	Hits []core.SearchHit `json:"hits"`
}

type snippetsResponse struct {
	Snippets []core.Snippet `json:"snippets"`
}

type agentsResponse struct {
	Agents []core.Agent `json:"agents"`
}
