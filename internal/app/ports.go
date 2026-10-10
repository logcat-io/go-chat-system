package app

import (
	"context"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 포트. 애플리케이션이 바깥(저장소)에 바라는 것을 **쓰는 쪽에서** 정의한다 — Go 의 관례이자 의존 방향의 근거다.
// 구현(adapter/pgstore)은 이 패키지를 import 하지 않는다. 메서드 집합이 맞으면 그걸로 충족된다.
//
// 읽기와 쓰기를 타입으로 가른다. Queries 는 ReadStore 만 받는다.

type AttachmentReader interface {
	AttachmentFile(ctx context.Context, attachmentID string) (core.AttachmentRef, error)
	// ConfirmedAttachment 는 확인된 첨부다. 확인 전이거나 대화가 삭제됐으면 core.ErrNotFound 다.
	ConfirmedAttachment(ctx context.Context, attachmentID string) (core.Attachment, error)
}

type AttachmentWriter interface {
	// CreateAttachment 는 대화의 미전송 첨부가 maxPending 에 닿았으면 core.ErrTooManyPending 이다. 세기와 넣기가 한 덩어리다.
	CreateAttachment(ctx context.Context, a core.NewAttachment, maxPending int) (core.Attachment, error)
	// ClaimAttachment 는 확인 전 첨부의 완료를 lease 동안 맡는다. 이미 확인됐거나 다른 요청이 맡고 있으면 claimed 가 false 다.
	// token 은 이 임대의 주인 표식이다. 도장과 해제는 같은 token 일 때만 걸린다.
	// 기다리지 않는다. 커넥션을 쥔 채 다른 요청을 기다리면 동시 요청이 풀을 다 쥐고 서버가 멈춘다.
	// 호출자의 취소와 떼어 내 결과를 반드시 돌려준다 — 커밋된 임대를 호출자가 모르면 아무도 비우지 못한다.
	ClaimAttachment(ctx context.Context, attachmentID, token string, lease time.Duration) (ref core.AttachmentRef, claimed bool, err error)
	// ReleaseAttachment 는 완료가 실패했을 때 token 의 임대를 비운다. 다른 주인의 임대와 확인된 첨부는 건드리지 않는다.
	// released 가 false 면 이 token 의 도장은 앞으로 걸리지 않는다.
	ReleaseAttachment(ctx context.Context, attachmentID, token string) (released bool, err error)
	// CompleteAttachment 는 token 이 임대의 주인일 때 확인 전 첨부에 도장을 찍고 임대를 비운다. stamped 는 이 호출이 찍었는지다.
	// 이미 확인된 첨부면 바꾸지 않고 그 첨부를 stamped=false 로 준다. 확인 전인데 주인이 바뀌었으면 core.ErrLeaseLost,
	// 대화가 삭제됐으면 core.ErrNotFound 다.
	CompleteAttachment(ctx context.Context, attachmentID, token, storagePath, contentType string, width, height int) (att core.Attachment, stamped bool, err error)
}

type ConversationReader interface {
	Conversation(ctx context.Context, convID string) (core.Conversation, error)
	ConversationMeta(ctx context.Context, convID string) (core.Conversation, error)
	ConversationDeletion(ctx context.Context, convID string) (core.Deletion, error)
	CountInbox(ctx context.Context, wsID, agentID string) (core.InboxCounts, error)
	// CustomerConversations 는 limit 이 0 이하면 core.CustomerConversationsPageDefault 개를, core.CustomerConversationsPageMax 를 넘으면 core.CustomerConversationsPageMax 개를 준다.
	CustomerConversations(ctx context.Context, customerID string, limit int) ([]core.Conversation, error)
	LatestOpenConversation(ctx context.Context, customerID string) (core.Conversation, error)
	CustomerConversationCount(ctx context.Context, customerID string) (int, error)
	// ListCustomerHistory 는 limit 이 0 이하면 core.PriorHistoryPageDefault 개를, core.PriorHistoryPageMax 를 넘으면 core.PriorHistoryPageMax 개를 준다.
	ListCustomerHistory(ctx context.Context, wsID, customerID, currentID string, before core.PriorCursor, limit int) ([]core.Message, error)
	// ListConversations 는 f.Limit 이 0 이하면 core.InboxPageDefault 개를, core.InboxPageMax 를 넘으면 core.InboxPageMax 개를 준다.
	ListConversations(ctx context.Context, f core.InboxFilter) ([]core.Conversation, error)
}

type ConversationWriter interface {
	// AssignConversation 은 잠근 행의 담당자가 이미 agentID 면(빈 문자열 = 이미 미배정) expected 와 무관하게 아무것도 쓰지 않고
	// record 도 부르지 않고 그 대화를 돌려준다. 아니면 expected 가 있을 때 잠근 행의 담당자가 그 값일 때만 바꾸고,
	// 아니면 core.ErrAssigneeChanged 다(빈 문자열은 미배정). expected 가 nil 이면 조건 없이 바꾼다. record 가 정한 시스템 줄·업무 기록은 같은 트랜잭션에 남는다(D39).
	// 돌려주는 대화는 커밋 전에 트랜잭션 안에서 읽은 값이다. 삭제된 대화면 core.ErrNotFound 다.
	AssignConversation(ctx context.Context, convID, agentID string, expected *string, record core.ChangeRecorder) (core.Conversation, error)
	CreateConversation(ctx context.Context, wsID, customerID, subject string) (core.Conversation, error)
	CreateCustomer(ctx context.Context, wsID, displayName string) (string, error)
	// DeleteConversation·RestoreConversation 은 같은 회원의 상담 열기(OpenMemberConversation)·다시 열기와 한 줄로 선다.
	// 가리는 사이에 같은 회원의 새 문의가 가려질 방으로 들어가지 않는다.
	DeleteConversation(ctx context.Context, convID, agentID, reason string) error
	RestoreConversation(ctx context.Context, convID string) error
	// SetConversationState 는 CLOSED 가 아닌 상태로 바꿀 때 회원의 상담 열기·가리기와 한 줄로 선 뒤에 판정한다.
	// 그 회원에게 지난 상담이 됐으면 core.ErrConversationEnded 다. 삭제된 대화면 core.ErrNotFound 다.
	// 잠근 행이 이미 그 상태면(보류는 보류 시각까지 같으면) 아무것도 쓰지 않고 record 도 부르지 않는다.
	// record 와 돌려주는 대화는 AssignConversation 과 같다 — 변경·시스템 줄·업무 기록이 한 트랜잭션이다(D39).
	SetConversationState(ctx context.Context, convID string, state core.State, until *time.Time, record core.ChangeRecorder) (core.Conversation, error)
}

type DirectoryReader interface {
	ConversationTags(ctx context.Context, convID string) ([]core.Tag, error)
	Customer(ctx context.Context, customerID string) (core.Customer, error)
	Followers(ctx context.Context, convID string) ([]core.Agent, error)
	ListAgents(ctx context.Context, wsID string) ([]core.Agent, error)
	ListSnippets(ctx context.Context, wsID string) ([]core.Snippet, error)
	ListTags(ctx context.Context, wsID string) ([]core.Tag, error)
	// SearchMessages 는 limit 이 0 이하면 core.SearchPageDefault 개를, core.SearchPageMax 를 넘으면 core.SearchPageMax 개를 준다.
	SearchMessages(ctx context.Context, wsID, query string, internal bool, limit int) ([]core.SearchHit, error)
}

type DirectoryWriter interface {
	AddConversationTag(ctx context.Context, convID, tagID string) error
	CreateSnippet(ctx context.Context, wsID, shortcut, title, body string) (core.Snippet, error)
	DeleteSnippet(ctx context.Context, wsID, snippetID string) error
	EnsureTag(ctx context.Context, wsID, name string) (core.Tag, error)
	Follow(ctx context.Context, convID, agentID string) error
	RemoveConversationTag(ctx context.Context, convID, tagID string) error
	TouchCustomer(ctx context.Context, customerID string) error
	Unfollow(ctx context.Context, convID, agentID string) error
	UpdateCustomer(ctx context.Context, customerID string, p core.CustomerProfile) (core.Customer, error)
	UpsertMember(ctx context.Context, wsID string, m core.MemberProfile) (string, error)
}

type HealthReader interface {
	Ping(ctx context.Context) error
}

type HistoryReader interface {
	AgentReplyPoint(ctx context.Context, convID string) (int64, error)
	// ListEvents 는 f.Limit 이 0 이하면 core.EventsPageDefault 개를, core.EventsPageMax 를 넘으면 core.EventsPageMax 개를 준다.
	ListEvents(ctx context.Context, f core.EventFilter) ([]core.ConversationEvent, error)
	// ListMessagesBefore 는 limit 이 0 이하면 core.MessagesBeforePageDefault 개를, core.MessagesBeforePageMax 를 넘으면 core.MessagesBeforePageMax 개를 준다.
	ListMessagesBefore(ctx context.Context, convID string, beforeSeq int64, limit int, internal bool) ([]core.Message, error)
	Revisions(ctx context.Context, convID, messageID string) ([]core.Revision, error)
	// WorkSummary 는 includeDeleted 가 false 면 삭제된 상담의 이벤트를 세지 않는다.
	WorkSummary(ctx context.Context, wsID string, from, to time.Time, includeDeleted bool) ([]core.AgentWork, error)
}

type HistoryWriter interface {
	// OpenMemberConversation 은 회원별로 한 줄로 서서 연다. 동시에 여러 번 불러도 열린 상담은 하나다.
	OpenMemberConversation(ctx context.Context, wsID, customerID string) (core.Conversation, core.OpenResult, error)
	RecordEvent(ctx context.Context, e core.ConversationEvent) error
	ReviseMessage(ctx context.Context, kind core.MessageKind, p core.ReviseParams) (core.AppendResult, error)
}

type MessageReader interface {
	// ListMessages 는 limit 이 0 이하면 core.MessagesPageDefault 개를, core.MessagesPageMax 를 넘으면 core.MessagesPageMax 개를 준다.
	// 넘친 limit 은 Max 로 자른다 — 1000 을 달라고 하면 500 개가 온다.
	ListMessages(ctx context.Context, convID string, afterSeq int64, limit int, internal bool) ([]core.Message, error)
}

type MessageWriter interface {
	AppendMessage(ctx context.Context, p core.AppendParams) (core.AppendResult, error)
	MarkAutoReplied(ctx context.Context, convID string) (bool, error)
}

type ReadReader interface {
	ReadStates(ctx context.Context, convID string) ([]core.ReadState, error)
}

type ReadWriter interface {
	MarkRead(ctx context.Context, convID string, ev core.LiveEvent, seq int64, internal bool) (int64, error)
	PublishLive(ctx context.Context, ev core.LiveEvent) error
}

type SessionReader interface {
	AgentByID(ctx context.Context, agentID string) (core.Agent, error)
	PendingAgentMatches(ctx context.Context, wsID, email, password string) (bool, error)
	SubjectByToken(ctx context.Context, token string) (core.Subject, error)
	VerifyAgent(ctx context.Context, wsID, email, password string) (core.Agent, error)
}

type SessionWriter interface {
	// CreateSession 은 세션을 만들고 평문 토큰을 준다. ttl 은 절대 수명, idle 은 유휴 한도이고 0 이면 그 만료가 없다(D46).
	CreateSession(ctx context.Context, kind, agentID, customerID string, ttl, idle time.Duration) (string, error)
}

type SettingReader interface {
	Setting(ctx context.Context, wsID, key string, out any) error
}

type SettingWriter interface {
	PutSettings(ctx context.Context, wsID string, values map[string]any, updatedBy string) error
}

type TeamReader interface {
	InviteByToken(ctx context.Context, token string) (core.Invite, error)
	ListInvites(ctx context.Context, wsID string) ([]core.Invite, error)
	ListPendingAgents(ctx context.Context, wsID string) ([]core.TeamMember, error)
	ListTeam(ctx context.Context, wsID string) ([]core.TeamMember, error)
}

type TeamWriter interface {
	// AcceptInvite 는 초대를 계정으로 바꾼다. replacedPending 은 같은 이메일의 승인 대기 행을 대체했는지다.
	AcceptInvite(ctx context.Context, token, password string) (agent core.Agent, replacedPending bool, err error)
	ApproveAgent(ctx context.Context, wsID, agentID string, role core.Role) (core.TeamMember, error)
	ChangePassword(ctx context.Context, agentID, password, keepToken string) error
	CreateInvite(ctx context.Context, wsID, email, name string, role core.Role, invitedBy string, ttl time.Duration) (core.Invite, error)
	// CreatePendingAgent 는 만들었으면 true 다. 이메일이 이미 있거나 대기가 maxPending 에 닿았으면 false 고 오류가 아니다.
	CreatePendingAgent(ctx context.Context, p core.PendingSignup, maxPending int) (bool, error)
	DeleteSession(ctx context.Context, token string) error
	RejectAgent(ctx context.Context, wsID, agentID string) (email string, err error)
	RevokeInvite(ctx context.Context, wsID, email string) error
	SetAgentActive(ctx context.Context, wsID, agentID string, active bool) error
	SetAgentRole(ctx context.Context, wsID, agentID string, role core.Role) error
}

type WorkspaceReader interface {
	// AuditLog 는 limit 이 0 이하면 core.AuditPageDefault 개를, core.AuditPageMax 를 넘으면 core.AuditPageMax 개를 준다.
	AuditLog(ctx context.Context, wsID string, limit int) ([]core.AuditEntry, error)
	EachConversation(ctx context.Context, wsID string, fn func(core.Conversation) error) error
	Workspace(ctx context.Context, wsID string) (core.Workspace, error)
}

type WorkspaceWriter interface {
	AnonymizeCustomer(ctx context.Context, wsID, customerID string) (paths []string, err error)
	Audit(ctx context.Context, e core.AuditEntry) error
	RotateMemberSecret(ctx context.Context, wsID string) (string, error)
	UpdateWorkspace(ctx context.Context, wsID, name string, allowedOrigins []string) (core.Workspace, error)
}

// ReadStore 는 조회 포트 전부다.
type ReadStore interface {
	AttachmentReader
	ConversationReader
	DirectoryReader
	HealthReader
	HistoryReader
	MessageReader
	ReadReader
	SessionReader
	SettingReader
	TeamReader
	WorkspaceReader
}

// WriteStore 는 명령 포트 전부다.
type WriteStore interface {
	AttachmentWriter
	ConversationWriter
	DirectoryWriter
	HistoryWriter
	MessageWriter
	ReadWriter
	SessionWriter
	SettingWriter
	TeamWriter
	WorkspaceWriter
}

// Store 는 조립할 때 한 번 받는 전체 포트다.
type Store interface {
	ReadStore
	WriteStore
}

// FileStore 는 첨부 파일이 사는 곳이다. 서버는 파일 바이트를 만지지 않는다 — 주소를 발급하고, 올라온 것을 확인하고, 지운다.
// 로컬 디스크 어댑터와 S3 호환 어댑터(AWS S3·MinIO 등)가 같은 계약을 채운다. 브라우저는 세 걸음(신청·올리기·완료)이
// 같고, 올리기의 방식(POST 양식 또는 PUT)만 UploadForm.Method 를 따른다(D42).
type FileStore interface {
	// PresignUpload 는 key 에 신청한 크기 size 의 파일 하나를 올릴 수 있는 양식(또는 서명된 PUT 주소)을 만든다.
	// 크기는 저장소가 지킬 수도(POST policy 의 content-length-range, 로컬의 허가 한도, 서명된 Content-Length) 못 지킬 수도 있다.
	// 앱은 저장소를 믿지 않는다 — 완료 때 Stat 크기가 신청한 크기와 다르면 파일을 지우고 거절한다. 신청한 크기는 한도 이하라
	// 한도를 넘는 파일은 저장소가 무엇이든 확인을 통과하지 못한다. Content-Type 은 서명에 묶는다.
	PresignUpload(key, contentType string, size int64, ttl time.Duration) (core.UploadForm, error)
	// PresignDownload 는 ttl 동안 살아 있는 내려받기 주소를 만든다. 권한 검사는 호출 전에 끝낸다.
	PresignDownload(key, filename, contentType string, inline bool, ttl time.Duration) (string, error)
	// Stat 은 올라온 파일의 크기와 앞부분을 준다. 없으면 core.ErrNotFound.
	Stat(ctx context.Context, key string) (core.ObjectInfo, error)
	// Promote 는 확인한 임시 파일을 확정 위치로 옮긴다. 원본이 없으면 core.ErrNotFound.
	// 확정 위치에 이미 파일이 있을 때 덮어쓰지 않는 저장소는 core.ErrObjectExists 를 낸다. 앱은 임대마다 새 확정 키를 쓰므로
	// 정상 흐름에서는 이 오류가 나지 않는다.
	// 올리기 양식은 임시 위치에만 쓸 수 있으므로, 옮긴 뒤에는 같은 양식으로 다시 올려도 확정 파일이 바뀌지 않는다.
	// Stat·Promote·Remove 는 ctx 가 이미 끝났으면 일하지 않고 ctx.Err() 를 돌려준다.
	Promote(ctx context.Context, from, to string) error
	Remove(ctx context.Context, key string) error
}
