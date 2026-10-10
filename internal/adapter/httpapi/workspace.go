package httpapi

import (
	"net/http"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 워크스페이스(테넌트) 관리. 설정·감사 로그·내보내기·고객 삭제.
// 회사 셀프 가입(POST /api/signup)은 2026-10-05 에 걷어냈다 — 설치형 단일 워크스페이스다(D35).

// workspaceSummary 는 상담원 누구나 보는 소속 정보다.
func (a *API) workspaceSummary(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ws, err := a.qry.WorkspaceSummary(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (a *API) workspace(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ws, err := a.qry.Workspace(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	// 설치 코드는 고객 사이트가 부를 공개 주소로 만든다. 데스크를 연 주소(사내·localhost)를 쓰면 고객이 닿지 못한다.
	ws.PublicURL = a.cfg.PublicURL
	writeJSON(w, http.StatusOK, ws)
}

type workspaceUpdateRequest struct {
	Name           string   `json:"name"`
	AllowedOrigins []string `json:"allowed_origins"`
}

func (a *API) updateWorkspace(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req workspaceUpdateRequest
	if !decodeOr(w, r, &req) {
		return
	}
	ws, err := a.cmd.UpdateWorkspace(r.Context(), actor, req.Name, req.AllowedOrigins)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (a *API) rotateMemberSecret(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ws, err := a.cmd.RotateMemberSecret(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

type auditResponse struct {
	Entries []core.AuditEntry `json:"entries"`
}

func (a *API) auditLog(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	entries, err := a.qry.AuditLog(r.Context(), actor, intQuery(r, "limit", core.AuditPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, auditResponse{Entries: entries})
}

// export 는 파일로 내려준다. 권한 오류는 헤더를 쓰기 전에 나므로 JSON 오류로 답할 수 있다.
func (a *API) export(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.qry.Require(r.Context(), actor, app.PermWorkspaceManage); err != nil {
		a.fail(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		noteErrorReason(w, "bad_format")
		writeErr(w, http.StatusBadRequest, "invalid", "format 은 json 또는 csv 여야 합니다")
		return
	}
	contentType := "application/json; charset=utf-8"
	if format == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="conversations-`+time.Now().Format("20060102")+`.`+format+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := a.cmd.Export(r.Context(), actor, format, w); err != nil {
		// 이미 본문이 나가기 시작했을 수 있다. 200 + 잘린 파일로 끝나면 안 되니 연결을 끊는다.
		// 로그 미들웨어가 w 를 감싸므로 Unwrap 을 따라가는 ResponseController 로 잡는다.
		a.log.ErrorContext(r.Context(), "export.failed", "err_kind", logctx.ErrKind(err))
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close() // 끊는 것이 목적이다. 닫기 실패도 끊긴 것이다.
		} else {
			panic(http.ErrAbortHandler)
		}
	}
}

// deleteCustomer 는 익명화다. 대화 번호는 남고 개인정보와 첨부 파일이 사라진다.
func (a *API) deleteCustomer(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.cmd.DeleteCustomer(r.Context(), actor, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
