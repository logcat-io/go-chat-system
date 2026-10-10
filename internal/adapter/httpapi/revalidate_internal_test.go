package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// DEF-02: REST 의 authed 는 세션 없음(401)과 DB 오류(500)를 가르는데 WebSocket 의 재확인 훅만 "오류면 세션 없음" 이었다.
// 훅은 realtime 에 넘어가 열린 연결이 살지 끊길지를 정한다. 여기서는 이 패키지가 넘기는 훅의 답만 본다(끊고 잇는 것은 realtime 의 정책 561).
// 저장소 대역이 세션 조회만 DB 오류처럼 실패하게 한다.
func TestWebSocket세션재확인은DB오류를세션없음으로보지않는다_만료나로그아웃한토큰만세션없음이다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx := context.Background()
	custID, err := shared.CreateCustomer(ctx, core.DefaultWorkspaceID, "재확인고객")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &subjectOutage{Store: shared}
	svc := app.New(flaky)
	token, err := svc.Commands.IssueCustomerSession(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	rt := &hookCatcher{}
	a := New(svc, rt, nil, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/ws?token="+token, nil)
	a.websocket(httptest.NewRecorder(), req, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: core.DefaultWorkspaceID})
	if rt.hook == nil {
		t.Fatal("재확인 훅을 넘기지 않았다")
	}

	if err := rt.hook(ctx); err != nil {
		t.Fatalf("살아 있는 세션인데 %v", err)
	}
	flaky.down.Store(true)
	if err := rt.hook(ctx); err == nil || errors.Is(err, app.ErrNotFound) {
		t.Fatalf("DB 오류를 %v 로 답했다. 세션 없음이 아닌 오류여야 한다(확인 못 함)", err)
	}
	flaky.down.Store(false)
	if err := rt.hook(ctx); err != nil {
		t.Fatalf("DB 가 돌아왔는데 %v", err)
	}
	if err := svc.Commands.EndSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := rt.hook(ctx); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("로그아웃한 토큰을 %v 로 답했다. 세션 없음(ErrNotFound)이어야 한다", err)
	}
}

// subjectOutage 는 down 이면 세션 조회가 DB 연결 오류로 실패하는 저장소 대역이다.
type subjectOutage struct {
	*pgstore.Store
	down atomic.Bool
}

func (s *subjectOutage) SubjectByToken(ctx context.Context, token string) (core.Subject, error) {
	if s.down.Load() {
		return core.Subject{}, errors.New("failed to connect to `host=127.0.0.1`: dial error (connection refused)")
	}
	return s.Store.SubjectByToken(ctx, token)
}

// hookCatcher 는 WebSocket 을 맡는 쪽 대역이다. 넘겨받은 재확인 훅만 쥔다.
type hookCatcher struct{ hook func(context.Context) error }

func (h *hookCatcher) Handle(_ http.ResponseWriter, _ *http.Request, _ app.Actor, revalidate func(context.Context) error) {
	h.hook = revalidate
}
func (h *hookCatcher) Connections() int { return 0 }
