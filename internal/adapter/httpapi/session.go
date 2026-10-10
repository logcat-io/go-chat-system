package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// entryWorkspace 는 토큰 없이 들어오는 입구(위젯 설정·고객 세션·회원 세션·/widget 페이지·로그인·상담원 가입)가 가는 워크스페이스다.
// 설치형 단일 워크스페이스라 설치 하나에 하나로 고정한다(D35). 위젯 키로 테넌트를 고르던 길과 CHAT_DEFAULT_WORKSPACE 는 걷어냈다.
// 토큰이 있는 요청은 세션의 소속을 따르고, 내부 workspace_id 와 격리(I11)는 그대로 남아 있다.
const entryWorkspace = core.DefaultWorkspaceID

type customerSessionRequest struct {
	Token       string `json:"token"`
	DisplayName string `json:"display_name"`
	// WidgetKey 는 옛 설치 코드(data-key)가 보내는 값이다. 읽지 않는다 — 받기만 해서 옛 위젯이 400 으로 깨지지 않게 한다(D35).
	WidgetKey string `json:"widget_key"`
	// HostOrigin 은 임베드한 페이지의 오리진이다. 허용 목록이 있으면 거기 있어야 한다.
	HostOrigin string `json:"host_origin"`
}

// customerSession 은 위젯 진입점이다. 토큰을 들고 오면 같은 고객으로 이어 붙인다.
// 이어 붙이는 것은 익명 고객의 세션뿐이다. 회원 세션 토큰을 들고 오면 버리고 새 익명 고객을 연다(D46, 공유 PC) —
// 회원은 회원 입구(member-session)로만 들어온다.
func (a *API) customerSession(w http.ResponseWriter, r *http.Request) {
	var req customerSessionRequest
	if !decodeOr(w, r, &req) {
		return
	}
	ws, ok := a.widgetWorkspace(w, r, req.HostOrigin)
	if !ok {
		return
	}

	// 들고 온 토큰이 이 워크스페이스의 익명 고객 세션이면 이어 쓰고, 아니면 버리고 새로 발급한다.
	var customerID, token string
	if held, ok := a.qry.ResumableCustomer(r.Context(), strings.TrimSpace(req.Token), ws.ID); ok {
		customerID, token = held.CustomerID, strings.TrimSpace(req.Token)
		a.cmd.NoteSessionUse(r.Context(), held)
	}

	customerID, conv, err := a.cmd.StartCustomerSession(r.Context(), ws.ID, customerID, req.DisplayName)
	if err != nil {
		a.fail(w, err)
		return
	}
	created := token == ""
	if created {
		if token, err = a.cmd.IssueCustomerSession(r.Context(), customerID); err != nil {
			a.fail(w, err)
			return
		}
	}
	a.respondCustomerSession(w, r, customerID, token, created, conv)
}

// widgetWorkspace 는 입구 워크스페이스를 읽고 호스트 오리진을 검사한다. 실패하면 응답을 쓰고 false 를 준다.
func (a *API) widgetWorkspace(w http.ResponseWriter, r *http.Request, hostOrigin string) (core.Workspace, bool) {
	ws, err := a.qry.EntryWorkspace(r.Context(), entryWorkspace)
	if err != nil {
		a.fail(w, err)
		return core.Workspace{}, false
	}
	if !app.WidgetOriginAllowed(ws, hostOrigin, a.publicOrigin()) {
		writeErr(w, http.StatusForbidden, "origin_not_allowed", "이 사이트에서는 위젯을 쓸 수 없습니다")
		return core.Workspace{}, false
	}
	return ws, true
}

// publicOrigin 은 우리 자신의 오리진이다. 독립 위젯 페이지(/widget)는 여기서 뜬다.
func (a *API) publicOrigin() string {
	u, err := url.Parse(a.cfg.PublicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// widgetAncestors 는 /widget 페이지의 문이다. 허용 목록이 있으면 그 사이트만 iframe 으로 띄울 수 있게
// CSP frame-ancestors 의 값을 준다. 브라우저가 강제하므로 Referer·embed 파라미터·경로(.html)에 기대지 않는다.
// 허용 목록이 비어 있으면 빈 값이다 — 어디서든 된다. 옛 설치 코드가 붙이는 ?key= 는 읽지 않는다(D35).
// 워크스페이스를 못 읽으면 응답을 쓰고 false 다.
func (a *API) widgetAncestors(w http.ResponseWriter, r *http.Request) (string, bool) {
	ws, err := a.qry.EntryWorkspace(r.Context(), entryWorkspace)
	if err != nil {
		// 위젯 iframe 에 그대로 뜨는 글이다. 고객이 읽는다. 원인 줄은 widget.unavailable 이다(D47).
		a.log.WarnContext(r.Context(), "widget.unavailable", "err_kind", logctx.ErrKind(err))
		noteErrorReason(w, logctx.ErrKind(err))
		http.Error(w, "상담 창을 열지 못했습니다. 잠시 후 다시 시도해 주세요", http.StatusServiceUnavailable)
		return "", false
	}
	if len(ws.AllowedOrigins) > 0 {
		return "'self' " + strings.Join(ws.AllowedOrigins, " "), true
	}
	return "", true
}

// respondCustomerSession 은 익명·회원 두 입구가 같은 모양으로 답하게 한다. 토큰은 부른 쪽이 이미 정했다(이어 쓰거나 새로 발급).
// created 면 새로 발급한 세션이라 session.created 를 한 줄 남긴다.
func (a *API) respondCustomerSession(w http.ResponseWriter, r *http.Request, customerID, token string, created bool, conv *core.Conversation) {
	customer, err := a.qry.CustomerProfile(r.Context(), customerID)
	if err != nil {
		a.fail(w, err)
		return
	}
	// 대화가 생기기 전의 고객은 customer_id 로만 이어진다. 새 세션을 발급했을 때 한 줄 남긴다.
	// 회원 여부는 방금 읽은 프로필에 있다. 이 한 줄 때문에 조회를 더하지 않는다.
	if created {
		a.log.InfoContext(r.Context(), "session.created", "workspace_id", customer.WorkspaceID,
			"customer_id", customerID, "member", customer.ExternalID != "")
	}
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: customerID}
	var convView any
	if conv != nil {
		convView = app.ForActor(actor, *conv)
	}
	writeJSON(w, http.StatusOK, customerSessionResponse{
		Token:        token,
		Customer:     app.CustomerForActor(actor, customer),
		Conversation: convView,
	})
}

type agentLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Workspace 는 옛 데스크가 보내던 슬러그다. 읽지 않는다 — 로그인은 늘 입구 워크스페이스다(D35).
	Workspace string `json:"workspace"`
}

func (a *API) agentLogin(w http.ResponseWriter, r *http.Request) {
	var req agentLoginRequest
	if !decodeOr(w, r, &req) {
		return
	}
	agent, err := a.qry.VerifyLogin(r.Context(), entryWorkspace, req.Email, req.Password)
	if errors.Is(err, core.ErrNotFound) {
		// 이메일이 없는 것과 비밀번호가 틀린 것을 구분해 주지 않는다.
		noteErrorReason(w, "invalid_credentials")
		writeErr(w, http.StatusUnauthorized, "unauthorized", "이메일 또는 비밀번호가 올바르지 않습니다")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	token, err := a.cmd.IssueAgentSession(r.Context(), agent.ID, a.cfg.SessionTTL)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentLoginResponse{Token: token, Agent: agent})
}
