package httpapi

import (
	"net/http"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 관리 화면 셋(D36). 상담원 이름 바꾸기, 빠른 답변 고치기, 위젯 FAQ.

type nameRequest struct {
	Name string `json:"name"`
}

// renameSelf 는 자기 이름을 바꾼다. 권한 없이 본인만.
func (a *API) renameSelf(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req nameRequest
	if !decodeOr(w, r, &req) {
		return
	}
	out, err := a.cmd.RenameSelf(r.Context(), actor, req.Name)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// renameAgent 는 팀 관리 권한이 다른 상담원의 이름을 바꾼다.
func (a *API) renameAgent(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req nameRequest
	if !decodeOr(w, r, &req) {
		return
	}
	out, err := a.cmd.RenameAgent(r.Context(), actor, r.PathValue("id"), req.Name)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) updateSnippet(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req snippetRequest
	if !decodeOr(w, r, &req) {
		return
	}
	sn, err := a.cmd.UpdateSnippet(r.Context(), actor, r.PathValue("id"), req.Shortcut, req.Title, req.Body)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (a *API) widgetFAQ(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	faq, err := a.qry.AdminWidgetFAQ(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, faq)
}

type widgetFAQRequest struct {
	// Items 는 목록 전체다. 포인터라 "보내지 않음·null" 과 "빈 배열" 을 가른다 — 빠뜨린 본문이 FAQ 를 지우면 안 된다.
	Items *[]core.FAQItem `json:"items"`
}

func (a *API) updateWidgetFAQ(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req widgetFAQRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if req.Items == nil {
		noteErrorReason(w, "items_required") // 빈 본문({} 또는 본문 없음)이다
		writeErr(w, http.StatusBadRequest, "invalid", "items 가 필요합니다. FAQ 를 두지 않으려면 빈 배열을 보내 주세요")
		return
	}
	faq, err := a.cmd.UpdateWidgetFAQ(r.Context(), actor, *req.Items)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, faq)
}
