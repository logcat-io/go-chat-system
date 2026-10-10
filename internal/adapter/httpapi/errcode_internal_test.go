package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// D47: 로그만으로 무엇이(라우트) 왜(응답 code 와 더 좁은 고정 사유) 실패했는지 가른다. 오류 하나가 http.request 한 줄에
// code·reason 으로 남고, 서버 잘못이면 원인 줄(http.failed)의 err_kind 가 고정 코드다. 원문은 어디에도 싣지 않는다(D30).

func logLinesOf(buf *lockedBuf, msg string) []map[string]any {
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// failVia 는 err 하나를 fail 로 응답하는 요청을 withTrace 아래에서 돌린다. 응답과 로그를 돌려준다.
func failVia(t *testing.T, pattern, path string, err error) (*httptest.ResponseRecorder, *lockedBuf) {
	t.Helper()
	logs := &lockedBuf{}
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))
	a := &API{log: log}
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { a.fail(w, err) })
	rec := httptest.NewRecorder()
	withTrace(log, withRecover(log, mux)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec, logs
}

func TestHTTP_오류응답의http_request는응답code와고정사유를싣고원인줄의err_kind는고정코드다(t *testing.T) {
	cases := []struct {
		name                 string
		err                  error
		status               int
		code, reason, failed string // failed 는 http.failed 의 err_kind(빈 값이면 그 줄이 없어야 한다)
		message              string
	}{
		{"임대를 잃은 완료는 응답이 409 busy 이고 로그는 lease_lost 다", app.ErrUploadLeaseLost, 409, "busy", "lease_lost", "", "이 첨부를 확인하는 중입니다. 잠시 후 다시 시도해 주세요"},
		{"다른 요청이 확인 중이면 409 busy 이고 사유가 따로 없다", app.ErrUploadBusy, 409, "busy", "", "", ""},
		{"먼저 가져간 배정은 conflict 에 already_taken", app.ErrAlreadyTaken, 409, "conflict", "already_taken", "", "다른 상담원이 먼저 가져갔습니다"},
		{"바뀐 담당자는 conflict 에 assignee_changed", app.ErrAssigneeChanged, 409, "conflict", "assignee_changed", "", ""},
		{"남의 메시지는 forbidden 에 not_owner 이고 자기 메시지만 고칠 수 있다고 답한다", app.ErrNotOwner, 403, "forbidden", "not_owner", "", "자기가 보낸 메시지만 고치거나 지울 수 있습니다"},
		{"파일이 없는 완료는 invalid 에 file_missing", app.InvalidError{Reason: "파일을 찾지 못했습니다. 다시 올려 주세요", Code: "file_missing"}, 400, "invalid", "file_missing", "", ""},
		{"되풀이된 경합은 500 이고 원인 줄의 err_kind 가 too_many_retries", core.ErrTooManyRetries, 500, "internal", "too_many_retries", "too_many_retries", msgInternal},
		{"옮길 자리에 파일이 있으면 500 이고 err_kind 가 object_exists", core.ErrObjectExists, 500, "internal", "object_exists", "object_exists", ""},
		{"세션 보안 저장소가 없으면 500 이고 err_kind 가 no_session_store", app.ErrNoSessionStore, 500, "internal", "no_session_store", "no_session_store", ""},
		{"저장소의 403 은 500 이고 err_kind 가 s3_403", core.NewError("s3_403", "s3store: 확인 요청을 저장소가 HTTP 403 으로 거절했다(AccessDenied)"), 500, "internal", "s3_403", "s3_403", ""},
		{"감싼 DB 오류처럼 코드가 없으면 err_kind 는 error", errors.New("pgx: user=secret host=10.0.0.1"), 500, "internal", "error", "error", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, logs := failVia(t, "POST /x", "/x", tc.err)
			var body errorResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.status || body.Error.Code != tc.code {
				t.Fatalf("응답 %d %s, %d %s 여야 한다", rec.Code, body.Error.Code, tc.status, tc.code)
			}
			if tc.message != "" && body.Error.Message != tc.message {
				t.Errorf("문구 %q, %q 여야 한다", body.Error.Message, tc.message)
			}
			req := logLinesOf(logs, "http.request")
			if len(req) != 1 {
				t.Fatalf("http.request 가 %d 줄이다: %s", len(req), logs.String())
			}
			if reason, _ := req[0]["reason"].(string); req[0]["code"] != tc.code || reason != tc.reason {
				t.Fatalf("http.request 는 code=%s reason=%s 한 줄이어야 한다: %v", tc.code, tc.reason, req)
			}
			failed := logLinesOf(logs, "http.failed")
			switch {
			case tc.failed == "" && len(failed) != 0:
				t.Errorf("거절인데 http.failed 를 남겼다: %v", failed)
			case tc.failed != "" && (len(failed) != 1 || failed[0]["err_kind"] != tc.failed || failed[0]["level"] != "ERROR"):
				t.Errorf("http.failed 는 err_kind=%s 인 ERROR 한 줄이어야 한다: %v", tc.failed, failed)
			case tc.failed != "" && failed[0]["http_route"] != "POST /x":
				// 인증 전(세션 조회)에 실패하면 ctx 에 라우트가 아직 없다. 원인 줄만으로도 어디인지 보이게 라우트를 싣는다(감사 23 N20).
				t.Errorf("http.failed 에 http_route=POST /x 가 없다: %v", failed)
			}
			for _, leak := range []string{"secret", "10.0.0.1", "AccessDenied"} {
				if strings.Contains(logs.String(), leak) {
					t.Errorf("로그에 원문 %q 가 실렸다:\n%s", leak, logs.String())
				}
			}
		})
	}
}

func TestHTTP_404와409conflict문구는무엇을찾지못했거나무엇이이미있는지말한다(t *testing.T) {
	cases := []struct {
		pattern, path string
		err           error
		want          string
	}{
		{"GET /api/conversations/{id}", "/api/conversations/c1", app.ErrNotFound, "상담을 찾을 수 없습니다. 이미 지워졌을 수 있습니다"},
		{"PATCH /api/conversations/{id}/messages/{msgId}", "/api/conversations/c1/messages/m1", app.ErrNotFound, "메시지를 찾을 수 없습니다. 이미 지워졌을 수 있습니다"},
		{"GET /api/attachments/{id}", "/api/attachments/a1", app.ErrNotFound, "첨부를 찾을 수 없습니다. 이미 지워졌을 수 있습니다"},
		{"POST /api/agent/snippets", "/api/agent/snippets", app.ErrConflict, "같은 단축어의 빠른 답변이 이미 있습니다. 다른 단축어를 써 주세요"},
		{"POST /api/admin/invites", "/api/admin/invites", app.ErrConflict, "이미 초대했거나 가입한 이메일입니다"},
		{"POST /api/agent/signup", "/api/agent/signup", app.ErrConflict, "이미 가입한 이메일입니다. 로그인해 주세요"},
		{"POST /api/agent/conversations/{id}/tags", "/api/agent/conversations/c1/tags", app.ErrConflict, "같은 이름의 태그가 이미 있습니다"},
	}
	for _, tc := range cases {
		_, route, _ := strings.Cut(tc.pattern, " ")
		rec, _ := failVia(t, "POST "+route, tc.path, tc.err)
		var body errorResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error.Message != tc.want {
			t.Errorf("%s: %q, %q 여야 한다", tc.pattern, body.Error.Message, tc.want)
		}
	}
}

// D47: 핸들러의 패닉은 그 요청만 500 으로 끝내고 panic.recovered 에 스택과 panic 값의 타입만 남긴다. 값에는 원문(접속 정보)이 있을 수 있다.
func TestHTTP_핸들러패닉은500이고스택과타입만남기며값의원문은싣지않는다_ErrAbortHandler는다시던진다(t *testing.T) {
	logs := &lockedBuf{}
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic(errors.New("failed to connect to user=chat database=secretdb"))
	})
	h := withTrace(log, withRecover(log, mux))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("패닉 요청이 %d 다. 500 이어야 한다", rec.Code)
	}
	p := logLinesOf(logs, "panic.recovered")
	if len(p) != 1 || p[0]["where"] != "http" || p[0]["panic_type"] != "*errors.errorString" || p[0]["level"] != "ERROR" {
		t.Fatalf("panic.recovered 는 where=http panic_type=*errors.errorString ERROR 한 줄이어야 한다: %v", p)
	}
	if stack, _ := p[0]["stack"].(string); !strings.Contains(stack, "TestHTTP_핸들러패닉은") {
		t.Errorf("스택에 패닉 자리가 없다: %q", stack)
	}
	if strings.Contains(logs.String(), "secretdb") {
		t.Errorf("panic 값의 원문이 실렸다:\n%s", logs.String())
	}
	req := logLinesOf(logs, "http.request")
	if len(req) != 1 || req[0]["status"] != float64(500) || req[0]["reason"] != "panic" || req[0]["trace_id"] != p[0]["trace_id"] {
		t.Errorf("http.request 는 같은 trace_id 의 500 reason=panic 이어야 한다: %v / %v", req, p)
	}
	// 같은 핸들러가 다음 요청도 받는다 — 프로세스와 서버가 산다.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("두 번째 요청이 %d 다", rec.Code)
	}

	t.Run("http.ErrAbortHandler 는 net/http 의 약속이라 다시 던진다", func(t *testing.T) {
		abort := withRecover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
		defer func() {
			if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // net/http 와 같은 비교다
				t.Errorf("다시 던지지 않았다: %v", v)
			}
		}()
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

// D47: 정적 자산·상태 확인의 성공은 DEBUG 지만 같은 라우트의 실패(없는 API 의 404 는 "GET /" 다)는 INFO 다. 문의 번호로 찾을 줄이 있어야 한다.
func TestHTTP_조용한라우트도실패한요청은INFO로남는다(t *testing.T) {
	for _, tc := range []struct {
		status int
		route  string
		want   slog.Level
	}{
		{200, "GET /", slog.LevelDebug},
		{200, "GET /healthz", slog.LevelDebug},
		{404, "GET /", slog.LevelInfo},
		{503, "GET /readyz", slog.LevelWarn},
		{500, "GET /", slog.LevelError},
	} {
		if got := requestLevel(tc.status, tc.route); got != tc.want {
			t.Errorf("%d %s: %v, %v 여야 한다", tc.status, tc.route, got, tc.want)
		}
	}
}

// D47-8(감사 23): 첨부 저장소가 없을 때(503 no_file_store) 고객에게 "관리자에게 문의" 라고 하지 않는다 — 고객은 관리자에게 물을 길이 없다.
// 고객에게는 사실(지금은 안 된다)과 할 수 있는 일을, 상담원에게는 설정이 없다는 것과 관리자에게 알리라고 말한다.
func TestHTTP_첨부저장소가없으면고객과상담원에게다른문구로503no_file_store를답한다(t *testing.T) {
	for _, tc := range []struct {
		actor app.Actor
		want  string
	}{
		{app.Actor{Kind: app.ActorCustomer, CustomerID: "c"}, "지금은 파일을 올리거나 열 수 없습니다. 내용을 메시지로 남겨 주세요"},
		{app.Actor{Kind: app.ActorAgent, AgentID: "a"}, "파일 첨부가 설정되지 않아 파일을 올리거나 열 수 없습니다. 관리자에게 알려 주세요"},
	} {
		logs := &lockedBuf{}
		a := &API{log: slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil)))}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) { a.failUpload(w, tc.actor, app.ErrNoFileStore) })
		rec := httptest.NewRecorder()
		withTrace(a.log, mux).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
		var body errorResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body.Error.Code != "no_file_store" || body.Error.Message != tc.want {
			t.Errorf("%s: %d %s %q — 503 no_file_store %q 여야 한다", tc.actor.Kind, rec.Code, body.Error.Code, body.Error.Message, tc.want)
		}
	}
}
