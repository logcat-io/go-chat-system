package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 상담 추적 로깅의 HTTP 쪽 (docs/proposals/2026-10-03-권한과-로깅.md 3절).
//
// 요청마다 trace_id 를 얹고, 인증을 지나면 누가(workspace_id·actor_kind·actor_id) 무엇에(conversation_id) 손댔는지를
// ctx 에 얹는다. 그 아래 app 의 사건 로그는 이 키를 저절로 달고 나온다. 요청이 끝나면 http.request 한 줄을 남긴다.

// requestScope 는 요청 하나의 로그 ctx 를 바깥 미들웨어까지 돌려보내는 상자다.
// authed 가 키를 얹은 ctx 는 안쪽 요청에만 있어서, 요청이 끝난 뒤 http.request 를 쓰는 미들웨어는 그 키를 못 본다.
// 같은 goroutine 안에서만 쓰고 읽으므로 잠금이 없다.
type requestScope struct {
	ctx context.Context
	// req 는 mux 에 넘긴 요청이다. mux 가 여기에 Pattern(라우트)을 적는다 — 응답 쓰기 쪽(fail)이 무엇에 대한 오류인지 말할 때 쓴다.
	req *http.Request
}

type scopeKey struct{}

// logWriter 는 상태 코드·바이트·오류 코드를 기록한다.
// WebSocket 업그레이드(Hijack)와 http.ResponseController 는 Unwrap 으로 원래 ResponseWriter 에 닿는다.
type logWriter struct {
	http.ResponseWriter
	scope  *requestScope
	status int
	bytes  int64
	// code 는 writeErr 가 쓴 오류 코드다. 권한 거절(auth.denied)을 403 의 다른 사유(승인 대기·오리진·가입 닫힘)와 가른다.
	// http.request 에도 싣는다 — 상태 코드만으로는 400 이 어떤 거절이었는지 모른다(D47).
	code string
	// reason 은 code 보다 좁은 고정 사유다(lease_lost·already_taken·file_missing …). 500 이면 오류 종류(err_kind)다.
	reason string
	// deniedLogged 는 app 이 이 거절을 auth.denied 로 이미 남겼다는 표시다. 같은 요청에서 두 번 남기지 않는다.
	deniedLogged bool
}

func (w *logWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// NoteError 는 이 패키지 밖의 핸들러(로컬 파일 저장소 /files/*)가 http.request 에 실을 오류 code 와 고정 사유를 적는 길이다.
// 그 핸들러는 JSON 오류 본문(writeErr)을 쓰지 않아 code 가 비고, 500 이면 원인의 종류를 http.request 가 몰랐다(감사 23).
// 인터페이스로 찾는다 — filestore 는 httpapi 를 모른다(어댑터끼리는 서로를 모른다).
func (w *logWriter) NoteError(code, reason string) {
	w.code = code
	if reason != "" {
		w.reason = reason
	}
}

// quietRoutes 는 http.request 를 debug 로 낮추는 경로다. 상태 확인과 정적 자산은 장애 추적에 쓸 일이 거의 없고 양만 많다.
var quietRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /readyz":  true,
	"GET /":        true,
}

// withTrace 는 요청에 trace 를 얹고 끝나면 http.request 를 남긴다.
// trace_id 는 요청마다 새로 만든다. 들어온 traceparent 는 읽지 않는다 — 클라이언트가 trace id 를 정하면 문의 번호(D47)를
// 남의 요청과 같게 만들 수 있다(감사 24). 트레이스를 내보내면 withSpan 이 연 새 루트 스팬의 id 가 이 값을 대신한다(D33).
func withTrace(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := logctx.NewTrace(r.Context())
		// 응답 헤더에 돌려준다. 고객이 오류 화면에서 이 값을 알려주면 그 요청을 바로 찾는다.
		w.Header().Set("traceparent", logctx.Traceparent(ctx))
		w.Header().Set("X-Trace-Id", logctx.TraceID(ctx))

		scope := &requestScope{ctx: ctx}
		lw := &logWriter{ResponseWriter: w, scope: scope}
		inner := r.WithContext(context.WithValue(ctx, scopeKey{}, scope))
		scope.req = inner
		next.ServeHTTP(lw, inner)

		// ServeMux 는 넘겨받은 요청에 Pattern 을 적는다. 그래서 안쪽 요청에서 읽는다.
		route := inner.Pattern
		nameSpan(r.Context(), route)
		status := lw.status
		if status == 0 {
			status = http.StatusOK
		}
		logCtx := scope.ctx
		// 권한 거절(permission)은 app 이 남겼다. 여기서는 그 밖의 forbidden — 남의 대화, 상담원 자리에 들어온 고객 — 만 남긴다.
		if lw.code == "forbidden" && !lw.deniedLogged {
			log.WarnContext(logCtx, "auth.denied", "http_route", route, "code", lw.code)
		}
		level := requestLevel(status, route)
		args := []any{"method", knownMethod(r.Method), "http_route", route, "status", status,
			"duration_ms", time.Since(start).Milliseconds(), "bytes", lw.bytes}
		// 오류 응답이면 응답 본문의 code 와 더 좁은 사유를 싣는다. 이 한 줄만으로 무엇이(route) 왜(code·reason) 실패했는지 거른다(D47).
		if lw.code != "" {
			args = append(args, "code", lw.code)
		}
		if lw.reason != "" && lw.reason != lw.code {
			args = append(args, "reason", lw.reason)
		}
		log.Log(logCtx, level, "http.request", args...)
	})
}

// knownMethod 는 로그·스팬 이름에 싣는 메서드다. 표준 메서드면 그대로, 아니면 _OTHER(OpenTelemetry 표준과 같은 값)다.
// 메서드는 클라이언트가 정한 글자다. net/http 는 토큰 글자이기만 하면 길이(헤더 한도 1MB까지)를 묻지 않고 받는다(D30, 감사 23).
func knownMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "_OTHER"
}

// requestLevel 은 http.request 한 줄의 수준이다. 서버 잘못(500·502·504 와 그 밖의 5xx)은 ERROR 다.
// 503 은 WARN 이다. 초안·첨부 저장소·회원 연동이 설정에서 꺼져 있거나 준비 확인에서 DB 가 닿지 않을 때 내는 값이라
// 코드의 잘못이 아니라 지금 상태다.
// 상태 확인·정적 자산은 양만 많아 DEBUG 로 낮춘다.
func requestLevel(status int, route string) slog.Level {
	switch {
	case status == http.StatusServiceUnavailable:
		return slog.LevelWarn
	case status >= 500:
		return slog.LevelError
	case quietRoutes[route] && status < 400:
		// 정적 자산·상태 확인의 성공만 낮춘다. 없는 API(GET /api/nope 는 "GET /" 라우트다)의 404 같은 실패는 INFO 로 남겨
		// 문의 번호로 찾을 수 있게 한다(D47).
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// annotate 는 인증을 지난 요청의 ctx 에 주체와 대화를 얹는다. authed 가 부른다.
func annotate(r *http.Request, sub core.Subject) *http.Request {
	args := []any{"http_route", r.Pattern, "workspace_id", sub.WorkspaceID}
	switch app.ActorKind(sub.Kind) {
	case app.ActorAgent:
		args = append(args, "actor_kind", "agent", "actor_id", sub.AgentID)
	case app.ActorCustomer:
		// 고객은 대화가 생기기 전부터 customer_id 로 이어진다. 같은 값이지만 그 키로도 거를 수 있게 둘 다 싣는다.
		args = append(args, "actor_kind", "customer", "actor_id", sub.CustomerID, "customer_id", sub.CustomerID)
	}
	// 경로의 {id} 가 대화일 때만 얹는다. 첨부·고객·상담원의 {id} 는 대화가 아니다.
	if strings.Contains(r.Pattern, "/conversations/{id}") {
		if convID, err := id.Normalize(r.PathValue("id")); err == nil {
			args = append(args, "conversation_id", convID)
		}
	}
	ctx := logctx.With(r.Context(), args...)
	if scope, ok := ctx.Value(scopeKey{}).(*requestScope); ok {
		scope.ctx = ctx
	}
	return r.WithContext(ctx)
}

// requestLogCtx 는 응답 쓰기 쪽(fail)이 요청의 로그 ctx 를 찾는 길이다. fail 은 요청을 받지 않는다.
func requestLogCtx(w http.ResponseWriter) context.Context {
	if lw := findLogWriter(w); lw != nil {
		return lw.scope.ctx
	}
	return context.Background()
}

// noteErrorCode 는 writeErr 가 쓴 오류 코드를 기록한다.
func noteErrorCode(w http.ResponseWriter, code string) {
	if lw := findLogWriter(w); lw != nil {
		lw.code = code
	}
}

// noteErrorReason 은 응답 code 보다 좁은 고정 사유를 기록한다. http.request 에 reason 으로 실린다.
func noteErrorReason(w http.ResponseWriter, reason string) {
	if lw := findLogWriter(w); lw != nil && reason != "" {
		lw.reason = reason
	}
}

// requestRoute 는 이 응답이 답하는 요청의 라우트(mux 의 패턴)다. 아직 라우팅 전이거나 모르면 빈 문자열이다.
func requestRoute(w http.ResponseWriter) string {
	if lw := findLogWriter(w); lw != nil && lw.scope.req != nil {
		return lw.scope.req.Pattern
	}
	return ""
}

// noteDeniedLogged 는 app 이 auth.denied 를 이미 남긴 거절이라고 적는다.
func noteDeniedLogged(w http.ResponseWriter) {
	if lw := findLogWriter(w); lw != nil {
		lw.deniedLogged = true
	}
}

func findLogWriter(w http.ResponseWriter) *logWriter {
	for w != nil {
		if lw, ok := w.(*logWriter); ok {
			return lw
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}
