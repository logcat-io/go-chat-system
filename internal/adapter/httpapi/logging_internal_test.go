package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// http.request 의 수준은 응답 상태로 정한다. 500·502·504 는 서버 잘못이라 ERROR, 503 은 의도한 "지금은 못 한다" 라 WARN 이다.
// 5xx 를 일부러 내는 길이 없어 미들웨어만 띄워 본다.
func TestHTTPRequest의수준(t *testing.T) {
	t.Run("http.request 는 500·502·504 가 ERROR, 503 이 WARN, 그 밖은 INFO 다", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			level  string
		}{
			{http.StatusInternalServerError, "ERROR"}, {http.StatusBadGateway, "ERROR"}, {http.StatusGatewayTimeout, "ERROR"},
			{http.StatusServiceUnavailable, "WARN"},
			{http.StatusNotFound, "INFO"}, {http.StatusOK, "INFO"},
		} {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
			withTrace(log, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("로그 한 줄이 아니다: %s", buf.String())
			}
			if line["msg"] != "http.request" || line["level"] != tc.level {
				t.Errorf("%d → %v %v, 기대 %s", tc.status, line["msg"], line["level"], tc.level)
			}
		}
	})
}

// 이 패키지 밖의 핸들러(로컬 파일 저장소 /files/*)는 JSON 오류 본문을 쓰지 않는다. NoteError 로 적은 code·reason 이
// http.request 에 실린다(감사 23 — 전에는 /files/* 500 의 http.request 에 reason 이 없었다).
func TestHTTPRequest_밖의핸들러가NoteError로적은code와reason을http_request에싣는다(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files/upload", func(w http.ResponseWriter, _ *http.Request) {
		n, ok := w.(interface{ NoteError(code, reason string) })
		if !ok {
			t.Fatal("ResponseWriter 에 NoteError 가 없다")
		}
		n.NoteError("internal", "no_space")
		http.Error(w, "x", http.StatusInternalServerError)
	})
	withTrace(log, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/files/upload", nil))
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("로그 한 줄이 아니다: %s", buf.String())
	}
	if line["code"] != "internal" || line["reason"] != "no_space" || line["level"] != "ERROR" {
		t.Errorf("http.request %v — code=internal reason=no_space ERROR 여야 한다", line)
	}
}
