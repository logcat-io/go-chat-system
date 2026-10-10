// Package httpapi 는 REST 엔드포인트와 인증 미들웨어, 정적 파일 서빙을 조립한다.
//
// 라우팅은 이 파일, 핸들러는 책임별 파일에 둔다.
// 응답은 map 이 아니라 타입으로 쓴다. 필드 이름이 오타여도 컴파일이 통과하는 걸 막고,
// 고객에게 나가는 표현(app 패키지의 뷰 타입)을 바꾸면 여기가 따라 바뀌게 하기 위해서다.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
	"github.com/logcat-io/go-chat-system/web"
)

// Realtime 은 WebSocket 을 맡는 쪽에 바라는 것이다. 이 패키지는 그 구현을 모른다.
type Realtime interface {
	// revalidate 는 nil(세션 살아 있음), app.ErrNotFound(세션 없음), 그 밖의 오류(확인 못 함) 중 하나를 돌려준다.
	Handle(w http.ResponseWriter, r *http.Request, actor app.Actor, revalidate func(context.Context) error)
	Connections() int
}

type API struct {
	cmd *app.Commands
	qry *app.Queries
	rt  Realtime
	// localFiles 는 로컬 디스크 저장소가 발급한 주소를 받는 핸들러다. 객체 저장소면 nil 이다.
	localFiles http.Handler
	cfg        config.Config
	log        *slog.Logger
	// entry 는 토큰 없이 들어오는 입구의 한도다. 로그인·세션 생성·초대 수락·상담원 가입.
	entry *rateLimiter
	// draining 이 켜지면 /readyz 가 503 이다. 서버가 내려가기 시작했다는 표식이다(Drain).
	draining atomic.Bool
}

// Drain 은 서버가 내려가기 시작했을 때 부른다. 그 뒤로 /readyz 는 DB 를 묻지 않고 503 이다.
// 로드밸런서가 이 서버를 빼서 새 연결이 다른 서버로 가게 한다. 다른 요청은 그대로 받는다.
func (a *API) Drain() { a.draining.Store(true) }

// New 는 조립이 끝난 유스케이스를 받는다. 첨부 저장소와 한도는 조립부(cmd/server)가 app 에 이미 꽂았다.
// localFiles 는 로컬 디스크 저장소의 주소 핸들러다. 객체 저장소를 쓰면 nil 을 넘긴다.
func New(a *app.App, rt Realtime, localFiles http.Handler, cfg config.Config, log *slog.Logger) *API {
	return &API{
		cmd: a.Commands, qry: a.Queries, rt: rt, localFiles: localFiles, cfg: cfg, log: log,
		entry: newRateLimiter(cfg.EntryRatePerMinute, cfg.EntryRatePerMinute, cfg.TrustedProxies),
	}
}

// handler 는 주체가 확인된 뒤 불리는 핸들러다. 모든 인증 경로가 같은 모양을 쓴다.
type handler func(http.ResponseWriter, *http.Request, app.Actor)

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)

	// 토큰 없이 들어오는 입구. 전부 IP 별 한도를 건다. 한도는 입구마다 따로 센다(ratelimit.go).
	// 입구는 전부 기본 워크스페이스로 간다(entryWorkspace, D35). 회사 셀프 가입(POST /api/signup)은 걷어냈다.
	mux.HandleFunc("POST /api/customer/session", a.entry.limited(a.customerSession))
	mux.HandleFunc("POST /api/customer/member-session", a.entry.limited(a.memberSession))
	mux.HandleFunc("POST /api/agent/login", a.entry.limited(a.agentLogin))
	mux.HandleFunc("POST /api/agent/invites/preview", a.entry.limited(a.invitePreview))
	mux.HandleFunc("POST /api/agent/invites/accept", a.entry.limited(a.acceptInvite))
	mux.HandleFunc("POST /api/agent/signup", a.entry.limited(a.agentSignup))
	mux.HandleFunc("GET /api/widget/settings", a.widgetSettings)

	mux.Handle("GET /api/me", a.authed(a.me))
	mux.Handle("POST /api/ws-ticket", a.authed(a.wsTicket))
	mux.Handle("GET /api/workspace", a.authed(a.workspaceSummary))
	mux.Handle("POST /api/logout", a.authed(a.logout))
	mux.Handle("POST /api/agent/password", a.authed(a.changePassword))
	mux.Handle("POST /api/agent/name", a.authed(a.renameSelf))

	// 관리자
	mux.Handle("GET /api/admin/team", a.authed(a.team))
	mux.Handle("POST /api/admin/invites", a.authed(a.createInvite))
	mux.Handle("DELETE /api/admin/invites/{email}", a.authed(a.revokeInvite))
	mux.Handle("POST /api/admin/agents/{id}/role", a.authed(a.setRole))
	mux.Handle("POST /api/admin/agents/{id}/active", a.authed(a.setActive))
	mux.Handle("POST /api/admin/agents/{id}/approve", a.authed(a.approveAgent))
	mux.Handle("POST /api/admin/agents/{id}/reject", a.authed(a.rejectAgent))
	mux.Handle("POST /api/admin/agents/{id}/name", a.authed(a.renameAgent))
	mux.Handle("GET /api/admin/settings", a.authed(a.settings))
	mux.Handle("PUT /api/admin/settings", a.authed(a.updateSettings))
	mux.Handle("GET /api/admin/widget-faq", a.authed(a.widgetFAQ))
	mux.Handle("PUT /api/admin/widget-faq", a.authed(a.updateWidgetFAQ))
	mux.Handle("GET /api/admin/workspace", a.authed(a.workspace))
	mux.Handle("PUT /api/admin/workspace", a.authed(a.updateWorkspace))
	mux.Handle("POST /api/admin/workspace/rotate-member-secret", a.authed(a.rotateMemberSecret))
	mux.Handle("GET /api/admin/audit", a.authed(a.auditLog))
	mux.Handle("GET /api/admin/export", a.authed(a.export))
	mux.Handle("DELETE /api/customers/{id}", a.authed(a.deleteCustomer))

	// 고객 위젯
	mux.Handle("GET /api/customer/conversations", a.authed(a.customerConversations))
	mux.Handle("POST /api/customer/conversations", a.authed(a.startConversation))
	mux.Handle("POST /api/customer/conversations/{id}/close", a.authed(a.closeByCustomer))

	// 대화 — 고객과 상담원이 같이 쓴다. 보이는 내용은 주체에 따라 갈린다.
	mux.Handle("GET /api/conversations/{id}", a.authed(a.conversation))
	mux.Handle("GET /api/conversations/{id}/messages", a.authed(a.messages))
	mux.Handle("POST /api/conversations/{id}/messages", a.authed(a.sendMessage))
	mux.Handle("PATCH /api/conversations/{id}/messages/{msgId}", a.authed(a.editMessage))
	mux.Handle("DELETE /api/conversations/{id}/messages/{msgId}", a.authed(a.deleteMessage))
	mux.Handle("GET /api/agent/conversations/{id}/messages/{msgId}/revisions", a.authed(a.revisions))
	mux.Handle("GET /api/agent/conversations/{id}/prior", a.authed(a.priorHistory))
	mux.Handle("GET /api/conversations/{id}/reads", a.authed(a.reads))
	mux.Handle("POST /api/conversations/{id}/read", a.authed(a.markRead))
	mux.Handle("GET /api/conversations/{id}/rating", a.authed(a.rating))
	mux.Handle("PUT /api/conversations/{id}/rating", a.authed(a.rate))
	mux.Handle("POST /api/conversations/{id}/attachments", a.authed(a.startUpload))
	mux.Handle("POST /api/attachments/{id}/complete", a.authed(a.completeUpload))
	mux.Handle("GET /api/attachments/{id}", a.authed(a.downloadAttachment))
	mux.Handle("GET /api/attachments/{id}/url", a.authed(a.attachmentURL))
	// 로컬 디스크 어댑터는 발급한 주소를 자기가 받는다. 객체 저장소 어댑터에는 이 핸들러가 없다.
	if a.localFiles != nil {
		mux.Handle("POST /files/upload", a.localFiles)
		mux.Handle("GET /files/get", a.localFiles)
	}
	mux.Handle("GET /api/customers/{id}", a.authed(a.customer))
	mux.Handle("POST /api/customers/{id}", a.authed(a.updateCustomer))

	// 상담원 데스크
	mux.Handle("GET /api/agent/conversations", a.authed(a.inbox))
	mux.Handle("GET /api/agent/inbox/counts", a.authed(a.inboxCounts))
	mux.Handle("POST /api/agent/conversations/{id}/state", a.authed(a.setState))
	mux.Handle("DELETE /api/agent/conversations/{id}", a.authed(a.deleteConversation))
	mux.Handle("POST /api/agent/conversations/{id}/restore", a.authed(a.restoreConversation))
	mux.Handle("GET /api/agent/conversations/{id}/deleted", a.authed(a.deletedConversation))
	mux.Handle("POST /api/agent/conversations/{id}/assign", a.authed(a.assign))
	mux.Handle("POST /api/agent/conversations/{id}/follow", a.authed(a.follow))
	mux.Handle("GET /api/agent/conversations/{id}/followers", a.authed(a.followers))
	mux.Handle("POST /api/agent/conversations/{id}/tags", a.authed(a.addTag))
	mux.Handle("DELETE /api/agent/conversations/{id}/tags/{tagId}", a.authed(a.removeTag))
	mux.Handle("GET /api/agent/search", a.authed(a.search))
	mux.Handle("GET /api/agent/snippets", a.authed(a.listSnippets))
	mux.Handle("POST /api/agent/snippets", a.authed(a.createSnippet))
	mux.Handle("PUT /api/agent/snippets/{id}", a.authed(a.updateSnippet))
	mux.Handle("DELETE /api/agent/snippets/{id}", a.authed(a.deleteSnippet))
	mux.Handle("GET /api/agent/agents", a.authed(a.agents))
	mux.Handle("GET /api/agent/tags", a.authed(a.tags))
	mux.Handle("POST /api/agent/conversations/{id}/draft", a.authed(a.draftReply))
	mux.Handle("GET /api/agent/work", a.authed(a.workLog))
	mux.Handle("GET /api/agent/work/summary", a.authed(a.workSummary))
	mux.Handle("GET /api/agent/metrics", a.authed(a.metrics))

	mux.HandleFunc("GET /ws", a.websocketEntry)
	mux.Handle("GET /", a.page(revalidated(web.FS(), http.FileServerFS(web.FS()))))

	return withSpan(spanServerName(a.cfg.PublicURL), withTrace(a.log, withRecover(a.log, withSecurityHeaders(mux))))
}

// page 는 /widget, /desk 를 각 html 로 보내고 나머지는 정적 서버에 넘긴다.
// HTML 페이지에는 CSP 를 건다(D46): 스크립트는 우리 출처의 파일만(script-src 'self'), 플러그인 없음, <base> 로 주소를 돌리지 못하게.
// 페이지에 인라인 스크립트·이벤트 속성이 없어서(web 패키지 테스트가 지킨다) 이것만으로 깨지는 화면이 없다.
// XSS 가 생겨도 주입한 인라인 스크립트가 돌지 않아 localStorage 의 세션 토큰을 읽어 가지 못한다.
// 스타일·그림·연결은 묶지 않는다 — 첨부 그림과 올리기는 저장소(S3 호환) 출처라 설치마다 다르다.
func (a *API) page(static http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "/" 는 손대지 않는다. 파일 서버가 index.html 을 알아서 찾고,
		// 여기서 /index.html 로 바꾸면 정규화 리다이렉트가 "/" 로 되돌려 루프가 생긴다.
		// 없는 API(GET)는 정적 서버에 넘기지 않고 JSON 404 로 답한다. 정적 서버는 no-cache 를 걸고, 404 를 쓰며 Cache-Control 을 지운다(Go 1.23 의
		// 오류 응답) — API 응답은 저장하지 않는다는 약속(D46, 정책 691)이 이 길에서 풀렸다(감사 20). 다른 메서드의 없는 API 는 mux 의 404·405 다.
		if apiPath(r.URL.Path) {
			writeErr(w, http.StatusNotFound, "not_found", "없는 API 입니다")
			return
		}
		switch r.URL.Path {
		case "/widget", "/widget.html":
			ancestors, ok := a.widgetAncestors(w, r)
			if !ok {
				return
			}
			setPageCSP(w, ancestors)
			r.URL.Path = "/widget.html"
		case "/desk", "/desk.html":
			// 데스크는 어디에도 끼워 넣을 수 없다. 클릭재킹 방어.
			setPageCSP(w, "'none'")
			r.URL.Path = "/desk.html"
		case "/", "/index.html":
			setPageCSP(w, "")
		}
		static.ServeHTTP(w, r)
	})
}

// pageCSP 는 HTML 페이지 공통의 CSP 다. frame-ancestors 는 페이지마다 다르다.
const pageCSP = "script-src 'self'; object-src 'none'; base-uri 'none'"

// setPageCSP 는 HTML 페이지의 CSP 를 건다. ancestors 가 비어 있으면 frame-ancestors 를 걸지 않는다(어디서든 끼워 넣을 수 있다).
func setPageCSP(w http.ResponseWriter, ancestors string) {
	csp := pageCSP
	if ancestors != "" {
		csp += "; frame-ancestors " + ancestors
	}
	w.Header().Set("Content-Security-Policy", csp)
}

// revalidated 는 정적 자산(페이지·JS·CSS)을 매번 검증하게 한다.
//
// 배포 뒤 옛 chat.js 가 브라우저나 엣지(CDN)에 남으면 새 서버와 옛 유실 방지 클라이언트가 섞인다.
// 그래서 max-age 를 주지 않고 no-cache(쓰기 전에 매번 묻는다)를 단다. 묻는 비용은 ETag 로 줄인다 — 바뀌지 않았으면
// 파일 서버가 If-None-Match 를 보고 본문 없이 304 를 준다. 임베드 파일은 수정 시각이 없어서(Last-Modified 를 못 쓴다)
// 내용의 해시를 기동 때 한 번 계산해 ETag 로 쓴다. 바이너리가 바뀌어야 내용이 바뀌므로 다시 계산할 일이 없다.
func revalidated(fsys fs.FS, static http.Handler) http.Handler {
	etags := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		etags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		w.Header().Set("Cache-Control", "no-cache")
		if etag, ok := etags[name]; ok {
			w.Header().Set("ETag", etag)
		}
		static.ServeHTTP(w, r)
	})
}

// requestToken 은 헤더에서 토큰을 꺼낸다.
//
// 쿼리 토큰은 WebSocket 의 옛 길(/ws?token=)에서만 받는다. 브라우저가 WS 에 헤더를 못 붙이기 때문이었는데, 지금은
// 일회용 티켓(/ws?ticket=)을 쓴다(D46, DEF-91). 다른 엔드포인트까지 허용하면 주소 하나를 복사하는 것만으로 세션이 통째로 넘어간다.
func (a *API) requestToken(r *http.Request) string {
	if token, err := auth.BearerToken(r.Header.Get("Authorization")); err == nil {
		return token
	}
	if r.URL.Path == "/ws" {
		return r.URL.Query().Get("token")
	}
	return ""
}

// actorOf 는 세션의 주체를 요청의 행위자로 옮긴다. REST 와 WebSocket(토큰·티켓) 세 길이 같은 모양을 쓴다.
func actorOf(sub core.Subject) app.Actor {
	return app.Actor{
		Kind:        app.ActorKind(sub.Kind),
		AgentID:     sub.AgentID,
		CustomerID:  sub.CustomerID,
		Name:        sub.Name,
		Role:        sub.Role,
		WorkspaceID: sub.WorkspaceID,
		Session:     sub.Session,
	}
}

func (a *API) authed(next handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := a.requestToken(r)
		if token == "" {
			// 401 의 두 갈래를 http.request 의 reason 으로 가른다(D47, 감사 23) — 토큰 없음과 틀리거나 끝난 토큰.
			noteErrorReason(w, "no_token")
			writeErr(w, http.StatusUnauthorized, "unauthorized", msgNoToken)
			return
		}
		sub, err := a.qry.Subject(r.Context(), token)
		if errors.Is(err, core.ErrNotFound) {
			noteErrorReason(w, "bad_session")
			writeErr(w, http.StatusUnauthorized, "unauthorized", msgBadSession)
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		r = annotate(r, sub)
		// 유휴 만료의 기준(D46). 1분 안에 이미 적었으면 아무것도 하지 않는다.
		a.cmd.NoteSessionUse(r.Context(), sub)
		next(w, r, actorOf(sub))
	})
}

// wsTicketResponse 는 WebSocket 티켓이다. 30초 안에 /ws?ticket= 으로 한 번 쓴다.
type wsTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// wsTicket 은 지금 세션에 묶인 WebSocket 티켓을 준다(D46, DEF-91). 세션 토큰은 Authorization 헤더로만 오간다.
func (a *API) wsTicket(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ticket, expires, err := a.cmd.IssueWSTicket(r.Context(), actor)
	if errors.Is(err, core.ErrNotFound) {
		// 인증을 지난 뒤 세션이 사라졌다(그 사이 로그아웃). 다른 401 과 같은 답이다.
		noteErrorReason(w, "bad_session")
		writeErr(w, http.StatusUnauthorized, "unauthorized", msgBadSession)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wsTicketResponse{Ticket: ticket, ExpiresAt: expires})
}

// websocketEntry 는 /ws 의 입구다. 셋을 받는다.
//
//	?ticket=  일회용 티켓(D46). 브라우저(chat.js)와 부하 도구의 길이다. 쓰면 끝이고 30초 뒤 만료된다.
//	Bearer    Authorization 헤더. 헤더를 붙일 수 있는 클라이언트의 길이다.
//	?token=   옛 길(DEF-91). 세션 토큰이 주소에 실려 앞단 접근 로그에 남는다. 배포 직후 남은 옛 chat.js 를 위해 당분간 받고
//	          ws.token_in_query 사건(id 만)으로 남긴다. 그 사건이 0 이 되면 걷어낸다(docs/api.md 에 제거 예정으로 적었다).
func (a *API) websocketEntry(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		a.authed(a.websocket).ServeHTTP(w, r)
		return
	}
	sub, err := a.cmd.RedeemWSTicket(r.Context(), ticket)
	if errors.Is(err, core.ErrNotFound) {
		noteErrorReason(w, "bad_ticket")
		writeErr(w, http.StatusUnauthorized, "unauthorized", "티켓이 없거나 만료되었거나 이미 쓰였습니다")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	r = annotate(r, sub)
	a.cmd.NoteSessionUse(r.Context(), sub)
	ref := sub.Session
	// 티켓은 한 번 쓰면 끝이라 재확인은 세션 참조로 한다. 답은 토큰 길과 같은 셋이다(DEF-02). 열린 연결은 세션을 쓰는 중이다 — 확인하며 사용 시각도 적는다.
	revalidate := func(ctx context.Context) error {
		s, err := a.qry.SubjectBySession(ctx, ref)
		if err == nil {
			a.cmd.NoteSessionUse(ctx, s)
		}
		return err
	}
	a.rt.Handle(w, r, actorOf(sub), revalidate)
}

func (a *API) websocket(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	token := a.requestToken(r)
	if r.URL.Query().Get("token") != "" {
		// 옛 길이다. 토큰 값은 싣지 않는다 — 행위자 id 는 annotate 가 이미 얹었다(D30).
		a.log.InfoContext(r.Context(), "ws.token_in_query")
	}
	// 세션 없음(ErrNotFound)과 DB 오류를 가른다. REST 의 authed 가 401 과 500 을 가르는 것과 같다(DEF-02).
	// 오류를 그대로 넘기면 realtime 이 세션 없음이면 끊고, 확인 못 함이면 이어 가며 다음 틱에 다시 묻는다.
	revalidate := func(ctx context.Context) error {
		sub, err := a.qry.Subject(ctx, token)
		if err == nil {
			a.cmd.NoteSessionUse(ctx, sub)
		}
		return err
	}
	a.rt.Handle(w, r, actor, revalidate)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	// 연결 수는 운영 정보다. 무인증 엔드포인트에서 내보내지 않는다.
	writeJSON(w, http.StatusOK, statusResponse{Status: "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if a.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable, "draining", "서버가 내려가는 중입니다")
		return
	}
	if err := a.qry.Ready(r.Context()); err != nil {
		// 스키마가 모자라면 배포의 마이그레이션 단계가 아직 돌지 않은 것이다(D49). DB 는 살아 있다.
		if errors.Is(err, app.ErrSchemaBehind) {
			writeErr(w, http.StatusServiceUnavailable, "schema_behind", "DB 스키마가 이 서버보다 뒤에 있습니다")
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "db_down", "DB 에 접속할 수 없습니다")
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "ready"})
}

func (a *API) me(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ws, err := a.qry.WorkspaceSummary(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	res := meResponse{
		Kind: string(actor.Kind), Name: actor.Name,
		AgentID: actor.AgentID, CustomerID: actor.CustomerID, Role: string(actor.Role), Workspace: &ws,
		Permissions: actor.Permissions(),
	}
	// 연결 수·모델 이름은 운영 정보다. 고객에게는 주지 않는다.
	if actor.Kind == app.ActorAgent {
		res.Connections = a.rt.Connections()
		res.DraftModel = a.draftModel()
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) draftModel() string { return a.qry.DraftModel() }

// 사용자 문구. 무엇이 왜 안 됐고 사용자가 무엇을 하면 되는지를 합니다체로 적는다(D47). 같은 상황이면 HTTP 와 WebSocket 이 같은 문구다.
const (
	// msgInternal 은 분류되지 않은 서버 오류다. 화면이 응답의 X-Trace-Id 앞 8자리를 "문의 번호" 로 붙여 보여 준다.
	msgInternal = "서버 오류로 요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주세요"
	// msgNoToken·msgBadSession 은 401 이다. 고객(위젯)은 페이지를 새로 고치면 새 세션이 열리고, 상담원은 다시 로그인한다.
	msgNoToken    = "로그인 정보가 없습니다. 다시 로그인하거나 페이지를 새로 고쳐 주세요" //nolint:gosec // G101: 사용자 문구다. 자격 증명이 아니다
	msgBadSession = "로그인 정보가 만료되었거나 올바르지 않습니다. 다시 로그인하거나 페이지를 새로 고쳐 주세요"
	msgForbidden  = "이 대화에 접근할 수 없습니다"
	msgNotOwner   = "자기가 보낸 메시지만 고치거나 지울 수 있습니다"
	// 첨부 저장소가 없을 때(503 no_file_store). 읽는 사람이 할 수 있는 일이 달라 둘이다(D47-8).
	msgNoFileStoreAgent    = "파일 첨부가 설정되지 않아 파일을 올리거나 열 수 없습니다. 관리자에게 알려 주세요"
	msgNoFileStoreCustomer = "지금은 파일을 올리거나 열 수 없습니다. 내용을 메시지로 남겨 주세요"
	// 고객이 직접 종료한 상담(D48). 고객 쪽에서는 읽기 전용이다 — 다시 열지 않고 새 문의로 안내한다. WebSocket 도 같은 문구다.
	msgClosedByCustomer = "종료한 상담에는 메시지를 보낼 수 없습니다. 새로 문의하려면 새 문의하기를 눌러 주세요"
	msgNoReply          = "답변 없이 종료한 상담은 평가할 수 없습니다"
)

// fail 은 도메인 오류를 HTTP 상태로 옮긴다. 분류되지 않은 것만 500 이다.
// 응답 코드보다 좁은 고정 사유(lease_lost·already_taken·file_missing …)는 http.request 의 reason 으로 남긴다(D47).
func (a *API) fail(w http.ResponseWriter, err error) {
	ctx := requestLogCtx(w)
	route := requestRoute(w)
	var inv app.InvalidError
	switch {
	case errors.As(err, &inv):
		noteErrorReason(w, inv.Code)
		writeErr(w, http.StatusBadRequest, "invalid", inv.Reason)
	case errors.Is(err, app.ErrPermission):
		// 권한 거절은 app 이 어느 권한인지와 함께 이미 auth.denied 로 남겼다.
		noteDeniedLogged(w)
		writeErr(w, http.StatusForbidden, "forbidden", "이 작업을 할 권한이 없습니다")
	case errors.Is(err, app.ErrPendingApproval):
		writeErr(w, http.StatusForbidden, "pending_approval", "관리자의 승인을 기다리고 있습니다")
	case errors.Is(err, app.ErrAgentOnly):
		noteErrorReason(w, "agent_only")
		writeErr(w, http.StatusForbidden, "forbidden", "상담원만 할 수 있습니다")
	case errors.Is(err, app.ErrCustomerOnly):
		noteErrorReason(w, "customer_only")
		writeErr(w, http.StatusForbidden, "forbidden", "고객만 할 수 있습니다")
	case errors.Is(err, app.ErrNotOwner):
		// 남의 메시지다. 대화에는 들어갈 수 있으니 "이 대화에 접근할 수 없습니다" 는 사실과 다르다.
		noteErrorReason(w, "not_owner")
		writeErr(w, http.StatusForbidden, "forbidden", msgNotOwner)
	case errors.Is(err, app.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", msgForbidden)
	case errors.Is(err, app.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", notFoundMessage(route))
	case errors.Is(err, app.ErrFileTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "파일은 "+humanBytes(a.cmd.UploadLimits().MaxBytes)+" 까지 올릴 수 있습니다")
	case errors.Is(err, app.ErrTooManyPending):
		// 상한에 닿으면 거절한다(attachments.go 의 >=). "넘었습니다" 가 아니라 "까지" 다.
		// 보내지 않은 첨부를 지우는 API 는 없다(감사 23). 할 수 있는 일은 보내기와 기다리기다 — 고아 업로드 정리가 올린 지 6시간이 지난 것을
		// 지운다(app.orphanUploadAge, 주기 작업은 1시간마다라 늦어도 7시간 안).
		writeErr(w, http.StatusTooManyRequests, "too_many",
			"보내지 않은 첨부는 "+strconv.Itoa(a.cmd.UploadLimits().MaxPending)+"개까지 둘 수 있습니다. 올린 파일을 먼저 보내 주세요. 보내지 않은 첨부는 올린 지 6시간이 지나면 저절로 지워집니다")
	case errors.Is(err, app.ErrUploadBusy):
		// 임대를 잃은 것(lease_lost)도 응답은 busy 다 — 다시 부르면 넘겨받은 쪽의 결과를 받는다. 로그만 가른다.
		noteErrorReason(w, core.ErrCode(err))
		writeErr(w, http.StatusConflict, "busy", "이 첨부를 확인하는 중입니다. 잠시 후 다시 시도해 주세요")
	case errors.Is(err, app.ErrNoFileStore):
		// 상담원에게 하는 말이다. 고객에게는 첨부 길(failUpload)이 따로 답한다 — 고객은 관리자에게 물을 길이 없다(D47-8, 감사 23).
		writeErr(w, http.StatusServiceUnavailable, "no_file_store", msgNoFileStoreAgent)
	case errors.Is(err, app.ErrStorageTimeout):
		writeErr(w, http.StatusServiceUnavailable, "storage_timeout", "파일 서버가 제때 응답하지 않아 첨부를 마치지 못했습니다. 파일을 다시 올려 주세요")
	case errors.Is(err, app.ErrNotClosed):
		writeErr(w, http.StatusConflict, "not_closed", "종료된 상담만 평가할 수 있습니다")
	case errors.Is(err, app.ErrRatingExpired):
		writeErr(w, http.StatusConflict, "rating_closed", "평가할 수 있는 시간(종료 뒤 24시간)이 지났습니다")
	case errors.Is(err, app.ErrNoReply):
		writeErr(w, http.StatusConflict, "no_reply", msgNoReply)
	case errors.Is(err, app.ErrClosedByCustomer):
		writeErr(w, http.StatusConflict, "closed_by_customer", msgClosedByCustomer)
	case errors.Is(err, app.ErrSystemLine):
		// 403 이 아니다 — 권한을 더 받아도 같은 답이다(관리자도). 대상이 운영 기록이라는 상태의 충돌이라 409 다(closed_by_customer·ended 와 같은 꼴).
		writeErr(w, http.StatusConflict, "system_line", "시스템 기록(배정·종료 등)은 고치거나 지울 수 없습니다")
	case errors.Is(err, app.ErrEnded):
		writeErr(w, http.StatusConflict, "ended", "종료된 상담입니다. 새 문의를 시작해 주세요")
	case errors.Is(err, app.ErrAlreadyTaken):
		noteErrorReason(w, "already_taken")
		writeErr(w, http.StatusConflict, "conflict", "다른 상담원이 먼저 가져갔습니다")
	case errors.Is(err, app.ErrAssigneeChanged):
		noteErrorReason(w, "assignee_changed")
		writeErr(w, http.StatusConflict, "conflict", "그 사이 담당자가 바뀌었습니다. 다시 확인해 주세요")
	case errors.Is(err, app.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", conflictMessage(route))
	case errors.Is(err, app.ErrDraftDisabled):
		// 상담원은 서버 설정을 바꿀 수 없다. 환경 변수 이름은 운영 문서(README)에 있다.
		writeErr(w, http.StatusServiceUnavailable, "draft_disabled", "AI 초안 기능이 꺼져 있습니다. 쓰려면 관리자에게 문의해 주세요")
	case errors.Is(err, app.ErrDraftFailed):
		// 원인 줄이다. err_kind 는 모델 쪽 고정 코드(ollama_http_503 …)나 연결 종류(timeout·conn_refused)다.
		a.log.WarnContext(ctx, "draft.failed", "err_kind", logctx.ErrKind(err))
		noteErrorReason(w, logctx.ErrKind(err)) // http.request 한 줄만으로 모델 쪽 원인(ollama_http_503·timeout …)을 거른다(감사 23)
		writeErr(w, http.StatusBadGateway, "draft_failed", "AI 초안 모델이 응답하지 않아 초안을 만들지 못했습니다. 잠시 후 다시 시도해 주세요")
	case errors.Is(err, app.ErrMemberAuthDisabled):
		writeErr(w, http.StatusServiceUnavailable, "member_auth_disabled", "회원 연동이 설정되지 않아 회원으로 확인할 수 없습니다")
	case errors.Is(err, context.Canceled):
		// 클라이언트가 먼저 끊었다. 서버 잘못이 아니라 ERROR 로 남기지 않는다.
		a.log.DebugContext(ctx, "http.canceled", "err_kind", logctx.ErrKind(err))
		writeErr(w, 499, "canceled", "요청이 취소되었습니다")
	default:
		// 원인 줄이다(ERROR 한 줄). http.request 도 500 이라 ERROR 지만, 알람은 http.request 로만 센다(deploy.md 15.3).
		// 오류 원문은 싣지 않는다. DB 연결 오류 문구에는 접속 정보(user=·database=·호스트)가 들어간다(D30). 종류(logctx.ErrKind)만 남긴다 —
		// 업무 오류면 고정 코드(too_many_retries·object_exists·no_session_store …), DB 면 SQLSTATE, 연결이면 그 종류다.
		kind := logctx.ErrKind(err)
		noteErrorReason(w, kind)
		// 인증 전(세션 조회)에 실패하면 ctx 에 라우트가 아직 없다. 원인 줄만으로도 어디인지 보이게 라우트를 싣는다.
		a.log.ErrorContext(ctx, "http.failed", "http_route", route, "err_kind", kind)
		writeErr(w, http.StatusInternalServerError, "internal", msgInternal)
	}
}

// notFoundMessage 는 404 의 문구다. 경로로 무엇을 찾지 못했는지 말한다 — "대상" 만으로는 무엇이 없는지 모른다.
func notFoundMessage(route string) string {
	what := "요청한 항목을"
	switch {
	case strings.Contains(route, "{msgId}"):
		what = "메시지를"
	case strings.Contains(route, "{tagId}"):
		what = "태그를"
	case strings.Contains(route, "/conversations/{id}"):
		what = "상담을"
	case strings.Contains(route, "/attachments/{id}"):
		what = "첨부를"
	case strings.Contains(route, "/customers/{id}"):
		what = "고객을"
	case strings.Contains(route, "/snippets/{id}"):
		what = "빠른 답변을"
	case strings.Contains(route, "/admin/agents/{id}"):
		what = "상담원을"
	case strings.Contains(route, "/invites"):
		what = "초대를"
	}
	return what + " 찾을 수 없습니다. 이미 지워졌을 수 있습니다"
}

// conflictMessage 는 409 conflict(같은 값이 이미 있다)의 문구다. 무엇이 이미 있는지 경로로 말한다.
func conflictMessage(route string) string {
	switch {
	case strings.Contains(route, "/snippets"):
		return "같은 단축어의 빠른 답변이 이미 있습니다. 다른 단축어를 써 주세요"
	case strings.Contains(route, "/admin/invites"):
		return "이미 초대했거나 가입한 이메일입니다"
	case strings.Contains(route, "/agent/signup"), strings.Contains(route, "/invites/accept"):
		return "이미 가입한 이메일입니다. 로그인해 주세요"
	case strings.Contains(route, "/tags"):
		return "같은 이름의 태그가 이미 있습니다"
	}
	return "같은 값이 이미 있어 저장하지 않았습니다"
}

func intQuery(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func int64Query(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return v
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// decodeOr 는 본문 해석 실패를 400 으로 끝낸다. 핸들러마다 같은 네 줄을 쓰지 않는다.
func decodeOr(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decode(r, v); err != nil {
		noteErrorReason(w, "bad_body") // JSON 이 아니거나 모르는 필드·틀린 타입이다(감사 23)
		writeErr(w, http.StatusBadRequest, "invalid", "요청 본문을 해석할 수 없습니다")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	noteErrorCode(w, code)
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: msg}})
}

// withRecover 는 핸들러의 패닉을 그 요청의 500 으로 끝낸다. panic.recovered(where=http) 에 스택과 panic 값의 타입만 남긴다(D47) —
// 값에는 입력이나 오류 원문이 들어 있을 수 있다(D30). 경로는 http.request 의 http_route 가 같은 trace_id 로 싣는다.
// http.ErrAbortHandler 는 net/http 가 응답을 끊으려고 쓰는 약속이라 다시 던진다.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // net/http 가 같은 값으로 비교한다
				panic(v)
			}
			telemetry.Recovered(requestLogCtx(w), log, "http", v)
			noteErrorReason(w, "panic")
			writeErr(w, http.StatusInternalServerError, "internal", msgInternal)
		}()
		next.ServeHTTP(w, r)
	})
}
