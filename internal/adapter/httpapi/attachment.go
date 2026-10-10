package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 첨부는 세 걸음이다. 주소를 받고(여기), 브라우저가 저장소로 직접 올리고, 완료를 알린다(여기).
// 서버는 파일 바이트를 받지 않는다. 규칙은 app 의 StartUpload·CompleteUpload·DownloadURL 에 있다.

type uploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

type uploadResponse struct {
	Attachment core.Attachment `json:"attachment"`
	Upload     core.UploadForm `json:"upload"`
}

// startUpload 는 첨부 행을 만들고 저장소로 올릴 양식을 돌려준다.
func (a *API) startUpload(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req uploadRequest
	if !decodeOr(w, r, &req) {
		return
	}
	att, form, err := a.cmd.StartUpload(r.Context(), actor, r.PathValue("id"), app.UploadRequest{
		Filename: req.Filename, ContentType: req.ContentType, ByteSize: req.ByteSize, Width: req.Width, Height: req.Height,
	})
	if err != nil {
		a.failUpload(w, actor, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadResponse{Attachment: att, Upload: form})
}

// completeUpload 는 올라온 파일을 확인한다. 여기를 지나야 메시지에 붙일 수 있다.
func (a *API) completeUpload(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	att, err := a.cmd.CompleteUpload(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.failUpload(w, actor, err)
		return
	}
	writeJSON(w, http.StatusCreated, attachmentResponse{Attachment: att})
}

// downloadAttachment 는 권한을 확인한 뒤 잠깐 살아 있는 주소로 넘긴다. Authorization 헤더가 필요하므로 브라우저가 링크로
// 바로 여는 길은 아니다. 토큰을 실을 수 있는 클라이언트(API 호출, 테스트)가 리다이렉트를 따라가는 길이다. 화면은 attachmentURL 을 쓴다.
func (a *API) downloadAttachment(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	target, err := a.qry.DownloadURL(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.failUpload(w, actor, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// attachmentURL 은 같은 주소를 JSON 으로 준다. 화면은 이걸 img 의 src 와 내려받기 링크에 꽂는다.
// 저장소가 다른 출처일 때 Authorization 을 실은 fetch 로 리다이렉트를 따라가지 않기 위해서다.
func (a *API) attachmentURL(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	target, err := a.qry.DownloadURL(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.failUpload(w, actor, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"url": target})
}

// failUpload 는 첨부 길의 실패다. 저장소가 없을 때(no_file_store)의 문구만 읽는 사람에 따라 다르고 나머지는 fail 과 같다.
// 고객에게 "관리자에게 문의" 는 할 수 없는 일이다 — 사실(지금은 안 된다)과 할 수 있는 일(메시지로 남기기)만 말한다(D47-8, 감사 23).
func (a *API) failUpload(w http.ResponseWriter, actor app.Actor, err error) {
	if errors.Is(err, app.ErrNoFileStore) && actor.Kind != app.ActorAgent {
		writeErr(w, http.StatusServiceUnavailable, "no_file_store", msgNoFileStoreCustomer)
		return
	}
	a.fail(w, err)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatInt(n/(1<<20), 10) + "MB"
	case n >= 1<<10:
		return strconv.FormatInt(n/(1<<10), 10) + "KB"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}
