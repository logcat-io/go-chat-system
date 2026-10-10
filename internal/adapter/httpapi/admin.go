package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 관리자 기능과 계정. 팀·초대·가입 승인·설정·비밀번호·로그아웃.

type teamResponse struct {
	Members []core.TeamMember `json:"members"`
	Invites []core.Invite     `json:"invites"`
	// Pending 은 직접 가입하고 승인을 기다리는 사람이다. 구성원 목록에는 없다.
	Pending []core.TeamMember `json:"pending"`
}

func (a *API) team(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	team, err := a.qry.Team(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teamResponse{Members: team.Members, Invites: team.Invites, Pending: team.Pending})
}

type inviteRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

type inviteResponse struct {
	Invite core.Invite `json:"invite"`
	// AcceptURL 은 관리자가 복사해서 넘기는 링크다. 메일 발송은 이 제품 범위 밖이다.
	// 토큰은 주소 조각(#invite=)에 싣는다(D46). 조각은 서버로 가지 않아 앞단 접근 로그·Referer 에 남지 않는다.
	AcceptURL string `json:"accept_url"`
}

func (a *API) createInvite(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req inviteRequest
	if !decodeOr(w, r, &req) {
		return
	}
	role := core.Role(req.Role)
	if role == "" {
		role = core.RoleStaff
	}
	inv, err := a.cmd.Invite(r.Context(), actor, req.Email, req.Name, role)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inviteResponse{
		Invite:    inv,
		AcceptURL: a.cfg.PublicURL + "/desk#invite=" + inv.Token,
	})
}

func (a *API) revokeInvite(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.cmd.RevokeInvite(r.Context(), actor, r.PathValue("email")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type invitePreviewRequest struct {
	Token string `json:"token"`
}

// invitePreview 는 토큰 없이 부른다. 수락 화면이 "누구를 초대했나" 를 보여 준다.
// 초대 토큰은 본문으로 받는다(D46). 예전의 GET ?token= 은 접근 로그와 Referer 에 토큰을 남겨 걷어냈다(그 주소는 이제 405 다).
func (a *API) invitePreview(w http.ResponseWriter, r *http.Request) {
	var req invitePreviewRequest
	if !decodeOr(w, r, &req) {
		return
	}
	inv, err := a.qry.InvitePreview(r.Context(), req.Token)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

type acceptInviteRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// acceptInvite 는 계정을 만들고 바로 로그인시킨다.
func (a *API) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var req acceptInviteRequest
	if !decodeOr(w, r, &req) {
		return
	}
	agent, err := a.cmd.AcceptInvite(r.Context(), req.Token, req.Password)
	if err != nil {
		a.fail(w, err)
		return
	}
	token, err := a.cmd.IssueAgentSession(r.Context(), agent.ID, a.cfg.SessionTTL)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, agentLoginResponse{Token: token, Agent: agent})
}

type roleRequest struct {
	Role string `json:"role"`
}

func (a *API) setRole(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req roleRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if err := a.cmd.SetRole(r.Context(), actor, r.PathValue("id"), core.Role(req.Role)); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// approveAgent 는 가입 대기 계정을 받아들인다. 역할은 승인하는 사람이 고른다.
func (a *API) approveAgent(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req roleRequest
	if !decodeOr(w, r, &req) {
		return
	}
	m, err := a.cmd.ApproveAgent(r.Context(), actor, r.PathValue("id"), core.Role(req.Role))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *API) rejectAgent(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.cmd.RejectAgent(r.Context(), actor, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type agentSignupRequest struct {
	// Workspace 는 옛 가입 화면이 보내던 슬러그다. 읽지 않는다 — 신청은 늘 입구 워크스페이스로 간다(D35).
	Workspace string `json:"workspace"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Password  string `json:"password"`
}

// agentSignup 은 상담원 직접 가입이다. 형식 오류가 아니면 늘 같은 202 다 — 계정이 있는지 알려 주지 않는다.
func (a *API) agentSignup(w http.ResponseWriter, r *http.Request) {
	var req agentSignupRequest
	if !decodeOr(w, r, &req) {
		return
	}
	err := a.cmd.AgentSignup(r.Context(), app.AgentSignupInput{
		WorkspaceID: entryWorkspace, Email: req.Email, Name: req.Name, Password: req.Password,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, statusResponse{Status: "pending_approval"})
}

type activeRequest struct {
	Active bool `json:"active"`
}

func (a *API) setActive(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req activeRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if err := a.cmd.SetActive(r.Context(), actor, r.PathValue("id"), req.Active); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordRequest struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req passwordRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if err := a.cmd.ChangePassword(r.Context(), actor, req.Current, req.Next, a.requestToken(r)); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// logout 은 서버의 세션 행을 지운다. 브라우저에서 토큰만 지우면 서버에서는 720시간 산다.
func (a *API) logout(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.cmd.EndSession(r.Context(), a.requestToken(r)); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) settings(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	st, err := a.qry.AdminSettings(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) updateSettings(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req app.Settings
	if !decodeOr(w, r, &req) {
		return
	}
	st, err := a.cmd.UpdateSettings(r.Context(), actor, req)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// widgetSettings 는 토큰 없이 본다. 임베드 스크립트가 부트 때 색·제목·운영 여부를 받아 간다.
func (a *API) widgetSettings(w http.ResponseWriter, r *http.Request) {
	// 공개 설정은 오리진을 보지 않는다. 색·제목·운영 여부는 비밀이 아니고, 오리진 검사는 세션 생성의 몫이다.
	// 옛 설치 코드가 붙이는 ?key= 는 읽지 않는다(D35).
	ws, err := a.qry.EntryWorkspace(r.Context(), entryWorkspace)
	if err != nil {
		a.fail(w, err)
		return
	}
	st, err := a.qry.PublicWidgetSettings(r.Context(), ws.ID, time.Now())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type memberSessionRequest struct {
	MemberToken string `json:"member_token"`
	// Token 은 위젯이 이미 들고 있는 고객 세션 토큰이다. 바로 그 회원의 살아 있는 세션이면 새로 만들지 않고 이어 쓴다(D46).
	Token string `json:"token"`
	// WidgetKey 는 옛 위젯이 보내는 값이다. 읽지 않는다(D35).
	WidgetKey  string `json:"widget_key"`
	HostOrigin string `json:"host_origin"`
}

// memberSession 은 호스트가 서명한 회원 토큰으로 세션을 연다. 비밀은 입구 워크스페이스의 회원 서명 비밀이다.
// 새 세션의 수명은 세션 수명 상한과 회원 토큰 만료 중 이른 쪽이다(D46). 같은 브라우저가 그 회원의 세션을 들고 오면 이어 쓴다.
func (a *API) memberSession(w http.ResponseWriter, r *http.Request) {
	var req memberSessionRequest
	if !decodeOr(w, r, &req) {
		return
	}
	ws, ok := a.widgetWorkspace(w, r, req.HostOrigin)
	if !ok {
		return
	}
	customerID, conv, memberExp, err := a.cmd.StartMemberSession(r.Context(), ws, req.MemberToken)
	if err != nil {
		a.fail(w, err)
		return
	}
	held := strings.TrimSpace(req.Token)
	if sub, ok := a.qry.ResumableMember(r.Context(), held, ws.ID, customerID); ok {
		a.cmd.NoteSessionUse(r.Context(), sub)
		a.respondCustomerSession(w, r, customerID, held, false, conv)
		return
	}
	token, err := a.cmd.IssueMemberSession(r.Context(), customerID, a.cfg.SessionTTL, memberExp)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.respondCustomerSession(w, r, customerID, token, true, conv)
}
