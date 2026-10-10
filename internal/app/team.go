package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 팀 관리. 초대·직접 가입 승인·역할·비활성·비밀번호. 팀 관리 권한(team.manage)이 손댄다.

const (
	inviteTTL = 72 * time.Hour
	// adminInviteTTL 은 관리자 역할 초대의 수명이다(D46). 관리자 링크 하나로 팀·회원 서명 비밀·내보내기까지 손에 넣는다.
	// 링크가 메신저·메일에 남아 새는 창을 줄인다. 하루면 "내일 출근해서 연다" 까지 덮고, 넘기면 관리자가 다시 보낸다.
	adminInviteTTL    = 24 * time.Hour
	minPasswordLength = 8
	// maxPendingSignups 는 워크스페이스당 승인 대기 상한이다. 토큰 없는 입구라 넘치면 관리자 화면이 가입 폭탄에 묻힌다.
	maxPendingSignups = 50
)

const roleInvalid = "역할은 ADMIN·STAFF·PART_TIME 중 하나여야 합니다"

// Team 은 팀 화면 한 장이다. 구성원, 초대 대기, 가입 승인 대기.
type Team struct {
	Members []core.TeamMember
	Invites []core.Invite
	Pending []core.TeamMember
}

func (s *Queries) Team(ctx context.Context, actor Actor) (Team, error) {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return Team{}, err
	}
	members, err := s.read.ListTeam(ctx, actor.WorkspaceID)
	if err != nil {
		return Team{}, translate(err)
	}
	invites, err := s.read.ListInvites(ctx, actor.WorkspaceID)
	if err != nil {
		return Team{}, translate(err)
	}
	pending, err := s.read.ListPendingAgents(ctx, actor.WorkspaceID)
	if err != nil {
		return Team{}, translate(err)
	}
	return Team{Members: members, Invites: invites, Pending: pending}, nil
}

// Invite 는 초대 링크의 토큰을 만든다. 토큰은 이 응답에만 실리고 다시 볼 수 없다.
func (s *Commands) Invite(ctx context.Context, actor Actor, email, name string, role core.Role) (core.Invite, error) {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return core.Invite{}, err
	}
	email, err := parseEmail(email)
	if err != nil {
		return core.Invite{}, err
	}
	name, err = trimmedRunes(name, maxNameRunes, "name_too_long", "이름")
	if err != nil {
		return core.Invite{}, err
	}
	if name == "" {
		return core.Invite{}, invalid("name_required", "이름이 비어 있습니다")
	}
	if !role.Valid() {
		return core.Invite{}, invalid("role_invalid", roleInvalid)
	}
	// 승인된 구성원(활성·비활성)만 막는다. 승인 대기 행은 막지 않는다 — 가입 입구는 이메일 소유를 확인하지 않아서,
	// 남이 그 이메일로 먼저 신청해 두면 진짜 주인을 초대하지 못하게 된다. 수락하면 대기 행을 대체한다.
	members, err := s.read.ListTeam(ctx, actor.WorkspaceID)
	if err != nil {
		return core.Invite{}, translate(err)
	}
	for _, m := range members {
		if strings.EqualFold(m.Email, email) {
			return core.Invite{}, invalid("email_taken", "이미 팀에 있는 이메일입니다")
		}
	}
	ttl := inviteTTL
	if role == core.RoleAdmin {
		ttl = adminInviteTTL
	}
	inv, err := s.write.CreateInvite(ctx, actor.WorkspaceID, email, name, role, actor.AgentID, ttl)
	if err != nil {
		return core.Invite{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "invite.create", email, map[string]any{"role": role})
	return inv, nil
}

func (s *Commands) RevokeInvite(ctx context.Context, actor Actor, email string) error {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return err
	}
	if err := translate(s.write.RevokeInvite(ctx, actor.WorkspaceID, email)); err != nil {
		return err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "invite.revoke", email, nil)
	return nil
}

// InvitePreview 는 수락 화면이 "누구를 어떤 역할로 초대했는지" 를 보여 줄 때 쓴다. 토큰이 곧 권한이다.
func (s *Queries) InvitePreview(ctx context.Context, token string) (core.Invite, error) {
	if strings.TrimSpace(token) == "" {
		return core.Invite{}, ErrNotFound
	}
	inv, err := s.read.InviteByToken(ctx, token)
	return inv, translate(err)
}

// AcceptInvite 는 초대를 계정으로 바꾼다. 같은 이메일의 승인 대기 행이 있으면 저장소가 그 행을 대체한다.
func (s *Commands) AcceptInvite(ctx context.Context, token, password string) (core.Agent, error) {
	if err := validatePassword(password); err != nil {
		return core.Agent{}, err
	}
	// 없는 토큰은 비밀번호 해시 전에 돌려보낸다.
	if _, err := s.read.InviteByToken(ctx, strings.TrimSpace(token)); err != nil {
		return core.Agent{}, translate(err)
	}
	agent, replaced, err := s.write.AcceptInvite(ctx, strings.TrimSpace(token), password)
	if err != nil {
		return core.Agent{}, translate(err)
	}
	// 대기 행을 대체했으면 남긴다. 가입 입구는 이메일 소유를 확인하지 않아서, 누가 먼저 그 이메일로 신청했었다는 사실이 나중에 필요할 수 있다.
	var detail map[string]any
	if replaced {
		detail = map[string]any{"replaced_pending": true}
	}
	s.audit(ctx, agent.WorkspaceID, Actor{Kind: ActorAgent, AgentID: agent.ID, Name: agent.Name}, "invite.accept", agent.Email, detail)
	return agent, nil
}

func (s *Commands) SetRole(ctx context.Context, actor Actor, agentID string, role core.Role) error {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return err
	}
	agentID, err := id.Normalize(agentID)
	if err != nil {
		return invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
	}
	if !role.Valid() {
		return invalid("role_invalid", roleInvalid)
	}
	// 마지막 관리자 보호는 저장소가 같은 트랜잭션에서 건다. 여기서 세고 넘기면 동시 요청에 뚫린다.
	if err := translate(s.write.SetAgentRole(ctx, actor.WorkspaceID, agentID, role)); err != nil {
		return err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.role", agentID, map[string]any{"role": role})
	return nil
}

func (s *Commands) SetActive(ctx context.Context, actor Actor, agentID string, active bool) error {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return err
	}
	agentID, err := id.Normalize(agentID)
	if err != nil {
		return invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
	}
	if agentID == actor.AgentID && !active {
		return invalid("self_deactivate", "자기 자신을 비활성화할 수 없습니다")
	}
	if err := translate(s.write.SetAgentActive(ctx, actor.WorkspaceID, agentID, active)); err != nil {
		return err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.active", agentID, map[string]any{"active": active})
	return nil
}

// ChangePassword 는 본인만 한다. 현재 비밀번호를 다시 묻고, 바꾸면 다른 세션을 끊는다.
func (s *Commands) ChangePassword(ctx context.Context, actor Actor, current, next, keepToken string) error {
	if err := requireAgent(actor); err != nil {
		return err
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	agent, err := s.read.AgentByID(ctx, actor.AgentID)
	if err != nil {
		return translate(err)
	}
	if _, err := s.read.VerifyAgent(ctx, actor.WorkspaceID, agent.Email, current); err != nil {
		return invalid("current_password_wrong", "현재 비밀번호가 올바르지 않습니다")
	}
	if err := translate(s.write.ChangePassword(ctx, actor.AgentID, next, keepToken)); err != nil {
		return err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.password", actor.AgentID, nil)
	return nil
}

// ── 직접 가입 ──────────────────────────────────────────────────────

// AgentSignupInput 은 상담원 가입 양식이다. WorkspaceID 는 신청이 갈 워크스페이스다 — 신청자가 고르지 않는다.
// 설치형 단일 워크스페이스라 HTTP 입구는 늘 기본 워크스페이스를 넣는다(D35).
type AgentSignupInput struct {
	WorkspaceID string
	Email       string
	Name        string
	Password    string
}

// AgentSignup 은 승인 대기 계정을 만든다. 형식이 틀린 입력만 오류다.
//
// 그 밖의 결과는 전부 같은 "접수됐다" 다: 가입을 꺼 뒀다, 이메일이 이미 있다, 대기가 꽉 찼다(워크스페이스가 없는 길도 남겨 둔다).
// 하나라도 다르게 답하면 이 입구가 "이 이메일이 그 회사 상담원인가" 를 묻는 조회기가 된다.
func (s *Commands) AgentSignup(ctx context.Context, in AgentSignupInput) error {
	email, err := parseEmail(in.Email)
	if err != nil {
		return err
	}
	name, err := trimmedRunes(in.Name, maxNameRunes, "name_too_long", "이름")
	if err != nil {
		return err
	}
	if name == "" {
		return invalid("name_required", "이름이 비어 있습니다")
	}
	if err := validatePassword(in.Password); err != nil {
		return err
	}
	// 받지 않는 길도 받는 길과 같은 양의 해시 작업을 한다. 저장소는 이미 있는 이메일·꽉 찬 대기에서도 해시를 만든다.
	// 응답이 같아도 시간이 다르면 "가입을 받나" 가 갈린다.
	ws, err := s.read.Workspace(ctx, in.WorkspaceID)
	if errors.Is(err, core.ErrNotFound) {
		auth.SpendPasswordHash(in.Password)
		return nil
	}
	if err != nil {
		return translate(err)
	}
	settings, err := s.q.settings(ctx, ws.ID)
	if err != nil {
		return err
	}
	if !settings.signupOpen() {
		auth.SpendPasswordHash(in.Password)
		return nil
	}
	_, err = s.write.CreatePendingAgent(ctx, core.PendingSignup{WorkspaceID: ws.ID, Email: email, Name: name, Password: in.Password}, maxPendingSignups)
	return translate(err)
}

// ApproveAgent 는 대기 계정을 받아들이고 역할을 정한다. 상담원 수에는 한도가 없다(D21).
// 다른 워크스페이스의 계정이나 이미 승인된 계정은 없는 것으로 본다.
func (s *Commands) ApproveAgent(ctx context.Context, actor Actor, agentID string, role core.Role) (core.TeamMember, error) {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return core.TeamMember{}, err
	}
	agentID, err := id.Normalize(agentID)
	if err != nil {
		return core.TeamMember{}, invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
	}
	if !role.Valid() {
		return core.TeamMember{}, invalid("role_invalid", roleInvalid)
	}
	m, err := s.write.ApproveAgent(ctx, actor.WorkspaceID, agentID, role)
	if err != nil {
		return core.TeamMember{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.approve", agentID, map[string]any{"email": m.Email, "role": role})
	return m, nil
}

// RejectAgent 는 대기 계정을 지운다. 같은 이메일로 다시 가입할 수 있다.
func (s *Commands) RejectAgent(ctx context.Context, actor Actor, agentID string) error {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return err
	}
	agentID, err := id.Normalize(agentID)
	if err != nil {
		return invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
	}
	email, err := s.write.RejectAgent(ctx, actor.WorkspaceID, agentID)
	if err != nil {
		return translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.reject", agentID, map[string]any{"email": email})
	return nil
}

// validatePassword 는 하한을 글자로, 상한을 바이트로 센다. 안내가 "8자 이상" 이라 바이트로 세면 한글 3글자(9바이트)가 통과한다.
func validatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < minPasswordLength {
		return invalid("password_too_short", "비밀번호는 %d자 이상이어야 합니다", minPasswordLength)
	}
	// bcrypt 가 받는 상한이다. 넘는 비밀번호는 해시가 오류를 내 500 으로 끝났다. 글자 수가 아니라 바이트다.
	if len(pw) > auth.MaxPasswordBytes {
		return invalid("password_too_long", "비밀번호는 72바이트(한글 24자, 영문 72자)까지입니다")
	}
	return nil
}
