package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

const (
	MaxBodyRunes     = 4000
	MaxClientMsgID   = 128
	welcomeClientMsg = "system-welcome"
	WelcomeBody      = "안녕하세요. 무엇을 도와드릴까요? 문의를 남겨 주시면 상담원이 확인하고 답해 드립니다."
)

var (
	ErrForbidden = core.NewError("forbidden", "app: 이 주체는 이 대화나 기능에 접근할 수 없다")
	ErrNotFound  = core.ErrNotFound
	// ErrEnded 는 종료된 지난 상담에 쓰거나 그 상담을 다시 열려 할 때다. 다시 보내도 결과가 같다.
	ErrEnded    = core.NewError("ended", "app: 종료된 지난 상담이라 쓰거나 다시 열지 않았다")
	ErrConflict = core.NewError("conflict", "app: 같은 값이 이미 있어 만들거나 바꾸지 않았다")
	// ErrAlreadyTaken 은 미배정을 보고 "나에게" 를 눌렀는데 그 사이 다른 상담원이 가져갔을 때다(D39).
	ErrAlreadyTaken = core.WrapError(ErrConflict, "already_taken", "app: 미배정 상담을 가져가려 했는데 다른 상담원이 먼저 가져갔다")
	// ErrAssigneeChanged 는 이관할 때 화면이 보던 담당자가 그 사이 바뀌었을 때다(D39).
	ErrAssigneeChanged = core.WrapError(ErrConflict, "assignee_changed", "app: 화면이 보던 담당자가 그 사이 바뀌어 배정을 바꾸지 않았다")
	// ErrAgentOnly 는 상담원 전용 기능을 상담원이 아닌 주체(고객)가 부를 때다. ErrForbidden 으로도 걸린다 —
	// 403 이라는 뜻은 같고 문구만 다르다. 남의 대화가 아니라 자리가 틀린 것이라 "이 대화에 접근할 수 없다" 로 답하지 않는다.
	ErrAgentOnly = core.WrapError(ErrForbidden, "agent_only", "app: 상담원 전용 기능을 상담원이 아닌 주체가 불렀다")
	// ErrNotOwner 는 자기가 보내지 않은 메시지를 고치거나 지우려 할 때다. 대화에는 들어갈 수 있으니 "이 대화에 접근할 수 없다" 가 아니다.
	ErrNotOwner = core.WrapError(ErrForbidden, "not_owner", "app: 자기가 보낸 메시지가 아니라 고치거나 지우지 않았다")
	// ErrSystemLine 은 시스템 줄(배정·종료·보류 기록)을 고치거나 지우려 할 때다. 권한의 문제가 아니라 대상이 운영 기록이라 관리자도 같다 — 409 system_line.
	ErrSystemLine = core.NewError("system_line", "app: 시스템 줄은 운영 기록이라 누구도 고치거나 지우지 않는다")
)

// InvalidError 는 사용자 입력이 정책을 어겼을 때 쓴다. Reason 은 사용자에게 그대로 보여도 되는 문구다.
// Code 는 로그에 싣는 고정 사유 코드다(D47). 문구에는 입력값이 섞일 수 있어 로그에 싣지 않으므로, 로그만으로 사유를 가르려면 코드가 있어야 한다.
// 모든 입력 거절에 붙는다(감사 24) — 필드 이름 수준(email_format·name_required·body_too_long …)이라 http.request·ws.rejected 한 줄로 어느 필드가
// 틀렸는지 갈린다. 코드는 리포에 고정된 리터럴이다(internal/archtest 의 TestInput_… 가 막는다). 원문·입력값을 코드에 넣지 않는다.
type InvalidError struct{ Reason, Code string }

func (e InvalidError) Error() string { return "app: 입력이 규칙을 어겼다: " + e.Reason }

// ErrCode 는 고정 사유 코드다. 없으면 invalid 다(app 밖에서 만든 InvalidError).
func (e InvalidError) ErrCode() string {
	if e.Code != "" {
		return e.Code
	}
	return "invalid"
}

// invalid 는 고정 사유 코드가 붙은 입력 거절이다. code 는 소문자 리터럴(필드_무엇)이고 format 은 사용자 문구다.
func invalid(code, format string, args ...any) error {
	return InvalidError{Reason: fmt.Sprintf(format, args...), Code: code}
}

type ActorKind string

const (
	ActorAgent    ActorKind = "AGENT"
	ActorCustomer ActorKind = "CUSTOMER"
)

// Actor 는 요청을 보낸 주체다.
type Actor struct {
	Kind       ActorKind
	AgentID    string
	CustomerID string
	Name       string
	// Role 은 상담원일 때만 있다. 판정은 Role 이 아니라 Can(권한)으로 한다 (permission.go).
	Role core.Role
	// WorkspaceID 는 소속 테넌트다. 모든 조회가 이 안에서만 돈다 (불변식 I11).
	WorkspaceID string
	// Session 은 이 요청을 인증한 세션의 참조다(D46). WebSocket 티켓 발급과, 세션이 끝났을 때 그 세션의 연결을 찾는 데 쓴다.
	// 권한 판정에는 쓰지 않는다.
	Session core.SessionRef
}

// Internal 은 내부 메모를 볼 수 있는 주체인지다. 번호 공간도 이걸로 갈린다.
func (a Actor) Internal() bool { return a.Kind == ActorAgent }

// ActorKey 는 로그에서 행위자를 한 값으로 가리킬 때 쓴다. 자기 이벤트 판정은 LiveVisible 이 kind 와 id 를 직접 비교한다.
func ActorKey(actor Actor) string {
	if actor.Kind == ActorAgent {
		return string(ActorAgent) + ":" + actor.AgentID
	}
	return string(ActorCustomer) + ":" + actor.CustomerID
}

// LiveVisible 은 휘발성 이벤트(타이핑·읽음)를 이 사람에게 보여도 되는지다.
// 전달 루프가 이벤트마다 연결마다 부른다. 조회도 할당도 하지 않는다.
func LiveVisible(viewer Actor, ev core.LiveEvent) bool {
	actor := ActorKind(ev.ActorKind)
	// 내가 낸 이벤트는 나에게 돌려보내지 않는다. 연결이 아니라 행위자로 가르므로 같은 사람의 다른 탭에도 가지 않는다.
	if viewer.isActor(actor, ev.ActorID) {
		return false
	}
	if ev.Kind == core.LiveRead && agentReadHiddenFrom(viewer, actor) {
		return false
	}
	return true
}

// isActor 는 kind·id 가 이 행위자인지다. ActorKey 와 같은 판정을 문자열을 만들지 않고 한다.
func (a Actor) isActor(kind ActorKind, actorID string) bool {
	if a.Kind == ActorAgent {
		return kind == ActorAgent && actorID == a.AgentID
	}
	return kind == ActorCustomer && actorID == a.CustomerID
}

// agentReadHiddenFrom 은 D25 다. 상담원이 읽기만 한 것은 고객에게 알리지 않는다.
// 고객이 보는 상담원의 "읽음" 은 답변 지점이고, 그건 메시지와 ReadStates 의 답변 지점으로 전달된다.
func agentReadHiddenFrom(viewer Actor, reader ActorKind) bool {
	return !viewer.Internal() && reader == ActorAgent
}

// requireAgent 는 상담원 전용 기능의 문지기다. 같은 세 줄을 기능마다 반복하지 않는다.
func requireAgent(actor Actor) error {
	if actor.Kind != ActorAgent || actor.AgentID == "" {
		return ErrAgentOnly
	}
	return nil
}

// App 은 조립 결과다. 명령과 조회를 나눠 든다 (CQRS — 복제가 아니라 책임의 분리다).
//
//	Queries   읽기만 한다. 쓰기 포트를 아예 들고 있지 않아서, 조회 경로에 쓰기가 끼어들면 컴파일이 안 된다.
//	Commands  상태를 바꾼다. 판단에 필요한 읽기는 저장소에서 직접 하거나 Queries 의 권한 검사·조회를 쓴다.
type App struct {
	Commands *Commands
	Queries  *Queries
}

type Queries struct {
	read   ReadStore
	drafts DraftProvider
	files  FileStore
	faq    WidgetFAQReader
	// ratings·metrics 는 지표·만족도 저장소다(D45, UseMetrics).
	ratings RatingReader
	metrics MetricsReader
	// sessions 는 세션 참조로 찾는 길이다(D46). 저장소가 가지면 New 가 꽂는다.
	sessions SessionLookup
	// clock 은 기간의 끝을 정하지 않은 조회의 "지금" 이다. 저장소가 DB 시계를 가지면 그것이다(DEF-148).
	clock DBClock
	// schema 는 스키마가 이 코드를 따라왔는지 읽는 길이다(D49, UseSchema).
	schema SchemaStore
}

type Commands struct {
	read   ReadStore
	write  WriteStore
	q      *Queries
	files  FileStore
	limits UploadLimits
	manage ManageWriter
	// ratings 는 만족도 쓰기 포트다(D45, UseMetrics).
	ratings RatingWriter
	// sessions 는 세션 보안 포트다(D46). 저장소가 가지면 New 가 꽂는다. policy 는 조립부가 꽂는 수명의 숫자다.
	sessions SessionKeeper
	policy   SessionPolicy
	// idleJudge 는 고객 유휴 한도를 판정 시점에 보는 저장소다(감사 21). 저장소가 가지면 New 가 꽂는다.
	idleJudge CustomerIdleJudge
	// closer 는 고객 종료 포트다(D48, ports_close.go). 저장소가 가지면 New 가 꽂는다.
	closer CustomerCloser
}

// New 는 포트 구현 하나(Store)로 명령·조회를 조립한다.
func New(store Store) *App {
	q := &Queries{read: store, drafts: noDraft{}, clock: appClock{}}
	if c, ok := store.(DBClock); ok {
		q.clock = c
	}
	cmd := &Commands{read: store, write: store, q: q}
	// 세션 보안 포트는 따로 정의했다(ports_session.go). 저장소가 그 능력을 가지면 여기서 꽂는다 — 첨부의 TempSweeper 와 같은 방식이다.
	if ss, ok := store.(SessionStore); ok {
		q.sessions, cmd.sessions = ss, ss
	}
	if j, ok := store.(CustomerIdleJudge); ok {
		cmd.idleJudge = j
	}
	if c, ok := store.(CustomerCloser); ok {
		cmd.closer = c
	}
	return &App{Queries: q, Commands: cmd}
}

// Authorize 는 actor 가 이 대화를 읽고 쓸 수 있는지 판정하고 대화를 돌려준다.
// 고객은 자기 대화만 볼 수 있다 (불변식 I6).
func (s *Queries) Authorize(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	convID, err := id.Normalize(convID)
	if err != nil {
		return core.Conversation{}, invalid("conversation_id_format", "conversation_id 형식이 올바르지 않습니다")
	}
	conv, err := s.read.Conversation(ctx, convID)
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	if err := s.authorizeConversation(actor, conv); err != nil {
		return core.Conversation{}, err
	}
	return conv, nil
}

func (s *Queries) authorizeConversation(actor Actor, conv core.Conversation) error {
	// 다른 워크스페이스의 대화는 존재 자체를 알려주지 않는다 (불변식 I11). 전달 루프의 Stream 도 여기를 지난다.
	if conv.WorkspaceID != actor.WorkspaceID {
		return ErrNotFound
	}
	// 가린 상담은 상담원에게도 고객에게도 없는 것이다(D29). 조회·구독·전송·첨부가 전부 이 문을 지난다.
	// 되살리기만 이 문을 거치지 않고 따로 본다.
	if conv.DeletedAt != nil {
		return ErrNotFound
	}
	switch actor.Kind {
	case ActorAgent:
		if actor.AgentID == "" {
			return ErrForbidden
		}
	case ActorCustomer:
		if actor.CustomerID == "" || conv.CustomerID != actor.CustomerID {
			// 남의 대화를 물어봤을 때 존재 여부를 알려주지 않는다.
			return ErrForbidden
		}
		// 회원에게 지난 상담은 없는 것이다. 목록에서만 빼면 id 를 아는 옛 탭이 계속 읽고 구독한다.
		// 조회·구독·전송·첨부가 전부 이 문을 지나므로 여기 한 곳에서 막는다.
		if conv.PastForCustomer {
			return ErrNotFound
		}
	default:
		return ErrForbidden
	}
	return nil
}

// authorizeAgentOn 은 상담원 전용 기능이 대화에 손대기 전에 거치는 길이다. 응대 권한이 기본이다.
func (s *Queries) authorizeAgentOn(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return core.Conversation{}, err
	}
	return s.Authorize(ctx, actor, convID)
}

// systemNote 는 타임라인에 남기는 시스템 줄이다. 실패해도 본 작업을 되돌리지 않는다.
func (s *Commands) systemNote(ctx context.Context, convID, body string, visibility core.Visibility) {
	if body == "" {
		return
	}
	_, _ = s.write.AppendMessage(ctx, core.AppendParams{
		ConversationID: convID,
		ClientMsgID:    "system-" + id.New(),
		SenderKind:     core.SenderSystem,
		Visibility:     visibility,
		Body:           body,
	})
}

func liveEvent(actor Actor, kind string) (core.LiveEvent, error) {
	ev := core.LiveEvent{Kind: kind, ActorKind: string(actor.Kind), ActorName: actor.Name}
	switch actor.Kind {
	case ActorAgent:
		if actor.AgentID == "" {
			return ev, ErrForbidden
		}
		ev.ActorID = actor.AgentID
	case ActorCustomer:
		if actor.CustomerID == "" {
			return ev, ErrForbidden
		}
		ev.ActorID = actor.CustomerID
	default:
		return ev, ErrForbidden
	}
	return ev, nil
}

// translate 는 저장소 오류를 도메인 오류로 옮긴다. 상위 계층이 pgstore 를 몰라도 되게 한다.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, core.ErrConversationEnded):
		return ErrEnded
	case errors.Is(err, core.ErrDuplicate):
		return ErrConflict
	case errors.Is(err, core.ErrLastAdmin):
		return invalid("last_admin", "활성 관리자가 한 명뿐이라 강등하거나 비활성화할 수 없습니다. 다른 사람을 먼저 관리자로 지정해 주세요")
	case errors.Is(err, core.ErrNotOwner):
		return ErrNotOwner
	case errors.Is(err, core.ErrSystemLine):
		return ErrSystemLine
	case errors.Is(err, core.ErrClientMsgIDTaken):
		return invalid("client_msg_id_taken", "client_msg_id 가 다른 발신자의 메시지에 이미 쓰였습니다")
	case errors.Is(err, core.ErrInvalidReference):
		return invalid("invalid_reference", "요청이 가리키는 항목이 없습니다. 화면을 새로 고친 뒤 다시 시도해 주세요")
	case errors.Is(err, core.ErrTooManyPending):
		return ErrTooManyPending
	case errors.Is(err, core.ErrLeaseLost):
		// 응답은 busy 그대로다(다시 부르면 넘겨받은 쪽의 결과를 받는다). 로그는 lease_lost 로 가른다.
		return ErrUploadLeaseLost
	case errors.Is(err, core.ErrNotClosed):
		return ErrNotClosed
	case errors.Is(err, core.ErrRatingExpired):
		return ErrRatingExpired
	case errors.Is(err, core.ErrNoReply):
		return ErrNoReply
	case errors.Is(err, core.ErrClosedByCustomer):
		return ErrClosedByCustomer
	default:
		return err
	}
}

// trimmedRunes 는 공백을 떼고 글자 수를 확인한다. 길이 검사가 바이트 수로 새는 걸 막는다.
func trimmedRunes(value string, limit int, code, what string) (string, error) {
	v := strings.TrimSpace(value)
	if utf8.RuneCountInString(v) > limit {
		return "", invalid(code, "%s은 %d자까지입니다", what, limit)
	}
	return v, nil
}
