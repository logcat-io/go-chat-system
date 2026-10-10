package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 감사 14(D30): 500 의 원인 줄(옛 "요청 처리 실패", 지금 http.failed) ERROR 가 오류 원문을 실었다. DB 연결 오류 문구에는 접속 정보(user=·database=·호스트)가 들어간다.
// 저장소 대역이 세션 조회를 접속 정보가 든 오류로 실패시키고, 실제 요청 경로로 500 을 받은 뒤 로그를 본다.
func TestHTTP_요청처리실패ERROR에는오류원문없이종류만싣는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx := context.Background()
	custID, err := shared.CreateCustomer(ctx, core.DefaultWorkspaceID, "오류로그고객")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &subjectOutage{Store: shared}
	svc := app.New(flaky)
	token, err := svc.Commands.IssueCustomerSession(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	a := New(svc, &hookCatcher{}, nil, config.Config{}, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	flaky.down.Store(true)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("세션 조회가 DB 오류인데 %d 다. 500 이어야 한다", rec.Code)
	}
	var found []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "http.failed" {
			found = append(found, l)
		}
	}
	if len(found) != 1 || found[0]["level"] != "ERROR" || found[0]["err_kind"] != "error" {
		t.Fatalf("http.failed 는 err_kind=error 인 ERROR 한 줄이어야 한다:\n%s", logs.String())
	}
	if _, ok := found[0]["err"]; ok {
		t.Errorf("오류 원문을 실었다: %v", found[0])
	}
	for _, leak := range []string{"failed to connect", "host=", "dial error", "127.0.0.1"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("로그에 접속 정보 %q 가 실렸다:\n%s", leak, logs.String())
		}
	}
}

// lockedBuf 는 핸들러와 테스트가 같이 쓰는 로그 버퍼다.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
