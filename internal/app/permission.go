package app

import (
	"context"
	"log/slog"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 권한(D29). 유스케이스는 역할을 묻지 않고 권한을 묻는다 — actor.Can(PermX).
// 역할과 권한의 대응은 이 파일의 rolePermissions 한 곳에만 있다. 역할을 더 잘게 쪼개거나 워크스페이스마다 바꾸고 싶어지면
// 권한 목록은 그대로 두고 이 표만 설정으로 뺀다. 역할 이름을 비교하는 코드가 다른 곳에 생기면 그 길이 막힌다.

// Permission 은 상담원이 할 수 있는 일 하나다. 값은 화면이 받는 문자열 그대로다(GET /api/me 의 permissions).
type Permission string

const (
	// PermConversationWork 는 응대 전부다. 받은함, 답변, 내부 메모, 태그 붙이기, 종료·보류·재개, 자기에게 배정, 이전 상담 이어 보기.
	PermConversationWork Permission = "conversation.work"
	// PermConversationAssign 은 남에게 배정·이관, 남의 담당 해제, 남이 맡은 상담 가져오기다.
	PermConversationAssign Permission = "conversation.assign"
	// PermConversationDelete 는 상담 가리기와 되살리기다.
	PermConversationDelete Permission = "conversation.delete"
	// PermMessageModerate 는 다른 상담원·AI·봇의 메시지 삭제다. 고객 메시지는 여전히 고객의 것이고, 시스템 줄은 운영 기록이라 이 권한으로도 지우지 못한다(감사 28).
	PermMessageModerate Permission = "message.moderate"
	// PermMessageRevisions 는 수정·삭제 전 원문 이력 조회다.
	PermMessageRevisions Permission = "message.revisions"
	// PermSearchAll 은 워크스페이스 전체 메시지 검색이다.
	PermSearchAll Permission = "search.all"
	// PermCustomerEdit 는 고객 프로필·운영 메모 수정이다. 보기는 응대에 필요해 누구나 한다.
	PermCustomerEdit Permission = "customer.edit"
	// PermCustomerDelete 는 고객 삭제(익명화)다.
	PermCustomerDelete Permission = "customer.delete"
	// PermLibraryManage 는 빠른 답변·태그 정의를 만들고 고치고 지우는 일이다.
	PermLibraryManage Permission = "library.manage"
	// PermWorkViewAll 은 다른 상담원의 업무 기록과 집계 조회다. 없으면 자기 것만 본다.
	PermWorkViewAll Permission = "work.view_all"
	// PermTeamManage 는 초대, 가입 승인·거절, 역할 변경, 비활성화, 다른 상담원의 이름 변경이다. 자기 이름은 권한 없이 바꾼다.
	PermTeamManage Permission = "team.manage"
	// PermWorkspaceManage 는 워크스페이스 설정, 허용 출처, 위젯 FAQ, 회원 서명 비밀, 자동 응답, 내보내기, 감사 로그다.
	PermWorkspaceManage Permission = "workspace.manage"
)

// allPermissions 는 표의 행 순서다. 화면에 내려가는 목록도 이 순서다.
var allPermissions = []Permission{
	PermConversationWork, PermConversationAssign, PermConversationDelete, PermMessageModerate,
	PermMessageRevisions, PermSearchAll, PermCustomerEdit, PermCustomerDelete,
	PermLibraryManage, PermWorkViewAll, PermTeamManage, PermWorkspaceManage,
}

// rolePermissions 는 역할→권한 대응표다. 역할을 직접 보는 곳은 여기뿐이다.
var rolePermissions = map[core.Role]map[Permission]bool{
	core.RoleAdmin: set(allPermissions...),
	core.RoleStaff: set(
		PermConversationWork, PermConversationAssign, PermMessageRevisions, PermSearchAll,
		PermCustomerEdit, PermLibraryManage, PermWorkViewAll,
	),
	core.RolePartTime: set(PermConversationWork),
}

func set(perms ...Permission) map[Permission]bool {
	out := make(map[Permission]bool, len(perms))
	for _, p := range perms {
		out[p] = true
	}
	return out
}

// ErrPermission 은 상담원이지만 그 일을 할 권한이 없을 때다. ErrForbidden 으로도 걸린다 — 403 이라는 뜻은 같고 문구만 다르다.
var ErrPermission = core.WrapError(ErrForbidden, "permission", "app: 상담원에게 이 작업의 권한이 없다")

// Can 은 이 주체가 그 일을 할 수 있는지다. 고객과 신원 없는 주체는 아무 권한도 없다.
func (a Actor) Can(p Permission) bool {
	if a.Kind != ActorAgent || a.AgentID == "" {
		return false
	}
	return rolePermissions[a.Role][p]
}

// Permissions 는 이 주체가 가진 권한 목록이다. 화면은 이걸로 버튼을 그릴지 정한다 — 역할 이름으로 분기하지 않는다.
func (a Actor) Permissions() []Permission {
	out := make([]Permission, 0, len(allPermissions))
	for _, p := range allPermissions {
		if a.Can(p) {
			out = append(out, p)
		}
	}
	return out
}

// require 는 권한 문지기다. 상담원이 아니면 ErrForbidden, 상담원인데 권한이 없으면 ErrPermission.
// 권한 거절은 여기서 auth.denied 로 남긴다 — 어느 권한이 모자랐는지는 여기서만 안다.
// 상담원이 아닌 주체의 거절은 권한 문제가 아니라 남의 자리에 들어온 것이라, 전달 계층이 오류 코드와 함께 남긴다.
func require(ctx context.Context, actor Actor, p Permission) error {
	if err := requireAgent(actor); err != nil {
		return err
	}
	if !actor.Can(p) {
		return denied(ctx, p)
	}
	return nil
}

// denied 는 권한 거절 한 건을 남기고 ErrPermission 을 돌려준다. 같은 요청에서 auth.denied 가 두 번 나오지 않게,
// 전달 계층은 ErrPermission 이면 따로 남기지 않는다.
func denied(ctx context.Context, p Permission) error {
	slog.WarnContext(ctx, "auth.denied", "permission", string(p))
	return ErrPermission
}

// Require 는 HTTP 계층이 유스케이스를 부르기 전에 막아야 할 때 쓴다(응답 헤더를 먼저 쓰는 내보내기 등).
func (s *Queries) Require(ctx context.Context, actor Actor, p Permission) error {
	return require(ctx, actor, p)
}

// requireCustomer 는 고객 전용 기능(만족도 내기, 상담 직접 종료)의 문지기다. 상담원이 부르면 ErrCustomerOnly(403)다.
func requireCustomer(actor Actor) error {
	if actor.Kind != ActorCustomer || actor.CustomerID == "" {
		return ErrCustomerOnly
	}
	return nil
}

// authorizeCustomerOn 은 고객 전용 기능이 대화에 손대기 전에 거치는 길이다(D48 고객 종료). 고객만, 자기 상담만이다 —
// 남의 상담은 403, 다른 워크스페이스·가린 상담·회원의 지난 상담은 404 다(Authorize, I6·I11).
func (s *Queries) authorizeCustomerOn(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	if err := requireCustomer(actor); err != nil {
		return core.Conversation{}, err
	}
	return s.Authorize(ctx, actor, convID)
}

// ErrPendingApproval 은 비밀번호는 맞지만 관리자가 아직 가입을 승인하지 않은 계정이다.
var ErrPendingApproval = core.NewError("pending_approval", "app: 비밀번호는 맞지만 관리자가 아직 가입을 승인하지 않았다")
