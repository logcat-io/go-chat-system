package ollama_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/adapter/ollama"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// D47(감사 23 N7): 모델 호출의 실패는 고정 코드로 가른다. 로그의 err_kind(draft.failed)와 http.request 의 reason 이 이 값이다.
// HTTP 실패는 ollama_http_<상태>, 읽을 수 없는 답은 ollama_bad_response, 모델이 돌려준 오류는 ollama_error, 빈 답은 ollama_empty 다.
// 모델 서버의 답(오류 본문)은 코드에 넣지 않는다 — 바깥이 정한 글자다.
func TestOllama_모델호출의실패는상태별고정코드다(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"503", http.StatusServiceUnavailable, "OLLAMA-SECRET-ERRBODY", "ollama_http_503"},
		{"404(모델 없음)", http.StatusNotFound, `{"error":"model not found"}`, "ollama_http_404"},
		{"500", http.StatusInternalServerError, "boom", "ollama_http_500"},
		{"JSON 아님", http.StatusOK, "<html>", "ollama_bad_response"},
		{"모델 오류", http.StatusOK, `{"error":"OLLAMA-SECRET-ERRBODY"}`, "ollama_error"},
		{"빈 답", http.StatusOK, `{"message":{"role":"assistant","content":"  "}}`, "ollama_empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			_, err := ollama.New(srv.URL, "m").Draft(context.Background(), app.DraftRequest{})
			if got := core.ErrCode(err); got != c.want {
				t.Fatalf("코드 %q — %q 여야 한다(err=%v)", got, c.want, err)
			}
			if kind := logctx.ErrKind(err); kind != c.want || strings.Contains(kind, "SECRET") {
				t.Errorf("err_kind %q — %q 여야 한다", kind, c.want)
			}
		})
	}
}

// 성공하면 모델의 답을 앞뒤 공백 없이 돌려준다. /api/chat 에 스트림 없이 한 번 묻는다.
func TestOllama_모델호출이성공하면답을다듬어돌려준다(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"  확인해서 안내드리겠습니다 \n"}}`)
	}))
	defer srv.Close()
	got, err := ollama.New(srv.URL+"/", "m").Draft(context.Background(), app.DraftRequest{})
	if err != nil || got != "확인해서 안내드리겠습니다" || path != "/api/chat" {
		t.Fatalf("답 %q, 오류 %v, 경로 %q", got, err, path)
	}
}
