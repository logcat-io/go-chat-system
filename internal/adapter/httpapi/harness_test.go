// 이 파일은 httpapi 의 E2E 테스트가 함께 쓰는 하네스다. 서버를 띄우는 길, 요청 헬퍼, 세션·첨부·WebSocket 도구를 한곳에 둔다.
// 테스트 함수는 두지 않는다. 한 파일에서만 쓰는 도구는 그 파일에 남긴다.
// 하네스는 운영 조립(cmd/server 의 assemble)과 따로 조립하므로 둘이 어긋날 수 있다. 운영 조립은 cmd/server/main_test.go 가 본다.

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/adapter/filestore"
	"github.com/logcat-io/go-chat-system/internal/adapter/httpapi"
	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/adapter/realtime"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// ---- 서버 ----

type harness struct {
	t     *testing.T
	srv   *httptest.Server
	store *pgstore.Store
	hub   *realtime.Hub
	files *filestore.Store
	svc   *app.App
}

// harnessConfig 는 하네스마다 다른 것만 담는다. 나머지(설정·파일 저장소·안전망)는 모든 하네스가 같다.
type harnessConfig struct {
	store    *pgstore.Store                 // nil 이면 공유 DB(testsupport.Store)
	wrap     func(*pgstore.Store) app.Store // nil 이면 store 를 그대로 앱에 준다
	catchup  time.Duration
	listener bool         // LISTEN/NOTIFY 리스너. 공유 DB URL 로 LISTEN 하므로 격리 DB 에선 false 여야 한다
	log      *slog.Logger // nil 이면 버린다
	entry    int          // 입구 한도(분당). 0 이면 사실상 없음(100000)
	// uploadMax 는 첨부 한도다. 0 이면 10MB — 한도를 넘는 업로드를 작은 파일로 재현하려고 운영 기본값(100MB)보다 작게 둔다.
	uploadMax int64
}

// newHarness 는 서버 전체를 띄운다. withListener=false 면 LISTEN/NOTIFY 를 끊어
// 주기 재조회만으로도 전달되는지 볼 수 있다.
func newHarness(t *testing.T, catchup time.Duration, withListener bool) *harness {
	t.Helper()
	return buildHarness(t, harnessConfig{catchup: catchup, listener: withListener})
}

// newHarnessOn 은 주어진 저장소 위에 띄운다. 전역 상태(관리자 수)를 단언하는 테스트는 격리 DB 를 넘긴다.
func newHarnessOn(t *testing.T, store *pgstore.Store, catchup time.Duration, withListener bool) *harness {
	t.Helper()
	return buildHarness(t, harnessConfig{store: store, catchup: catchup, listener: withListener})
}

// newHarnessWrapped 는 앱이 쓰는 저장소를 감싼 것으로 띄운다. 저장소 호출 사이에 끼어들어야 하는 테스트가 쓴다.
// 운영 코드에 훅을 넣지 않고 바깥에서 감싼다.
func newHarnessWrapped(t *testing.T, wrap func(*pgstore.Store) app.Store) *harness {
	t.Helper()
	return buildHarness(t, harnessConfig{wrap: wrap, catchup: time.Second})
}

// buildHarness 는 모든 하네스가 지나는 한 길이다.
func buildHarness(t *testing.T, c harnessConfig) *harness {
	t.Helper()
	store := c.store
	if store == nil {
		store = testsupport.Store(t)
	}
	var appStore app.Store = store
	if c.wrap != nil {
		appStore = c.wrap(store)
	}
	log := c.log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	catchup := c.catchup
	entry := c.entry
	if entry == 0 {
		entry = 100000 // 테스트는 한 IP 에서 수백 번 들어온다
	}

	svc := app.New(appStore)
	hub := realtime.NewHub()
	rt := realtime.NewServer(svc, hub, catchup, log)
	uploadMax := c.uploadMax
	if uploadMax == 0 {
		uploadMax = 10 << 20
	}
	cfg := config.Config{
		SessionTTL: time.Hour, CatchupInterval: catchup,
		MaxUploadBytes: uploadMax, MaxPendingUploads: 10,
		EntryRatePerMinute: entry,
		MemberSecret:       "test-member-secret",
		PublicURL:          "http://test.local",
	}
	// 회원 서명 비밀은 워크스페이스마다 다르다. 테스트는 기본 워크스페이스에 고정값을 싣는다.
	if err := store.SetMemberSecret(context.Background(), core.DefaultWorkspaceID, cfg.MemberSecret); err != nil {
		t.Fatal(err)
	}
	files, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// 알림을 놓쳤을 때의 안전망은 리스너와 별개로 항상 돈다.
	go realtime.NewChangeWatcher(store, hub, catchup, log).Run(ctx)

	if c.listener {
		l := pgstore.NewListener(testsupport.URL(), log)
		l.OnChange = rt.OnChange
		l.OnLive = rt.Live
		l.OnResync = rt.Resync
		l.OnSessionsEnded = rt.SessionsEnded
		go l.Run(ctx)
		// 리스너가 LISTEN 을 걸 시간을 준다.
		time.Sleep(200 * time.Millisecond)
	}

	svc.UseFiles(files, app.UploadLimits{MaxBytes: cfg.MaxUploadBytes, MaxPending: cfg.MaxPendingUploads})
	svc.UseManage(store)
	svc.UseMetrics(store)
	srv := httptest.NewServer(httpapi.New(svc, rt, files.Handler(), cfg, log).Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, store: store, hub: hub, files: files, svc: svc}
}

// captureLogs 는 운영과 같은 로거(logctx 로 감싼 JSON)를 만들어 모은다.
// app 계층은 slog 기본 로거를 쓰므로 기본 로거도 이 테스트 동안 바꾼다. 이걸 쓰는 테스트는 병렬로 돌리지 않는다.
func captureLogs(t *testing.T) (*logBuffer, *slog.Logger) {
	t.Helper()
	logs := &logBuffer{}
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	prev := slog.Default()
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs, log
}

// newLoggedHarness 는 newHarnessOn(공유 DB, 재조회 2초, 리스너 없음)과 같은 서버를 로그를 모으는 로거로 띄운다.
// 저장소를 기본 로거를 바꾸기 전에 연다.
func newLoggedHarness(t *testing.T) (*harness, *logBuffer) {
	t.Helper()
	store := testsupport.Store(t)
	logs, log := captureLogs(t)
	return buildHarness(t, harnessConfig{store: store, catchup: 2 * time.Second, log: log}), logs
}

// newCapturedHarness 는 newHarness 와 같은 서버(리스너 포함 여부도 같다)를 로그를 모으는 로거로 띄운다.
func newCapturedHarness(t *testing.T, catchup time.Duration, withListener bool) (*harness, *logBuffer) {
	t.Helper()
	logs, log := captureLogs(t)
	return buildHarness(t, harnessConfig{catchup: catchup, listener: withListener, log: log}), logs
}

// ---- 로그 ----

// logBuffer 는 여러 goroutine 이 쓰는 JSON 로그를 모은다.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type logLine map[string]any

func (l logLine) str(key string) string { s, _ := l[key].(string); return s }

func (b *logBuffer) lines(t *testing.T) []logLine {
	t.Helper()
	var out []logLine
	for _, raw := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if raw == "" {
			continue
		}
		var l logLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("JSON 한 줄이 아니다: %v (%s)", err, raw)
		}
		out = append(out, l)
	}
	return out
}

// where 는 key=value 인 줄만 남긴다. 운영자가 Loki 에서 `| json | conversation_id="…"` 로 거르는 것과 같다.
func (b *logBuffer) where(t *testing.T, key, value string) []logLine {
	t.Helper()
	var out []logLine
	for _, l := range b.lines(t) {
		if l.str(key) == value {
			out = append(out, l)
		}
	}
	return out
}

// await 는 조건에 맞는 줄이 나올 때까지 기다린다. WebSocket 종료처럼 응답 뒤에 남는 줄이 있다.
func (b *logBuffer) await(t *testing.T, pred func(logLine) bool) logLine {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range b.lines(t) {
			if pred(l) {
				return l
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("기다린 로그가 없다:\n%s", b.String())
	return nil
}

// linesOf 는 그 요청(trace)에서 msg 인 줄이다.
func linesOf(t *testing.T, logs *logBuffer, res *http.Response, msg string) []logLine {
	t.Helper()
	var out []logLine
	for _, l := range logs.where(t, "trace_id", res.Header.Get("X-Trace-Id")) {
		if l.str("msg") == msg {
			out = append(out, l)
		}
	}
	return out
}

// ---- 요청 ----

// roundTrip 은 모든 요청 헬퍼가 지나는 한 길이다. 요청을 만들고 보내는 데 실패하면 테스트를 끊고, 응답은 상태 코드와 상관없이 돌려준다.
// body 가 nil 이면 본문과 Content-Type 을 싣지 않는다. token 이 비면 Authorization 을 싣지 않는다. header 는 마지막에 덮어쓴다.
func (h *harness) roundTrip(method, path, token string, body any, header map[string]string) *http.Response {
	h.t.Helper()
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, payload)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		if k == "Host" { // net/http 클라이언트는 Host 를 헤더 맵이 아니라 req.Host 로 보낸다
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

// postRaw 는 POST 응답을 그대로 돌려준다. 닫는 것은 부른 쪽이다.
func (h *harness) postRaw(path, token string, body any) *http.Response {
	h.t.Helper()
	return h.roundTrip(http.MethodPost, path, token, body, nil)
}

// post 는 POST 하고 JSON 을 풀어 돌려준다. 400 이상이면 테스트를 끊는다.
func (h *harness) post(path, token string, body any) map[string]any {
	h.t.Helper()
	res := h.roundTrip(http.MethodPost, path, token, body, nil)
	defer res.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out)
	if res.StatusCode >= 400 {
		h.t.Fatalf("POST %s 가 %d: %s", path, res.StatusCode, raw)
	}
	return out
}

// get 은 GET 하고 JSON 을 풀어 돌려준다. 400 이상이면 테스트를 끊는다.
func (h *harness) get(path, token string) map[string]any {
	h.t.Helper()
	res := h.roundTrip(http.MethodGet, path, token, nil, nil)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		h.t.Fatalf("GET %s 가 %d: %s", path, res.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (h *harness) put(path, token string, body any) map[string]any {
	h.t.Helper()
	res := h.putRaw(path, token, body)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		h.t.Fatalf("PUT %s 가 %d: %s", path, res.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// putRaw 는 PUT 응답을 그대로 돌려준다. 다른 헬퍼와 달리 body 가 nil 이어도 JSON(null)과 Content-Type 을 싣는다.
func (h *harness) putRaw(path, token string, body any) *http.Response {
	h.t.Helper()
	b, _ := json.Marshal(body)
	return h.roundTrip(http.MethodPut, path, token, json.RawMessage(b), nil)
}

// status 는 상태 코드만 돌려준다. 오류 응답이어도 끊지 않는다.
func (h *harness) status(method, path, token string, body any) int {
	h.t.Helper()
	res := h.roundTrip(method, path, token, body, nil)
	res.Body.Close()
	return res.StatusCode
}

// raw 는 본문 없는 요청의 응답을 그대로 돌려준다. 닫는 것은 부른 쪽이다.
func (h *harness) raw(method, path, token string) *http.Response {
	h.t.Helper()
	return h.roundTrip(method, path, token, nil, nil)
}

func (h *harness) bodyOf(path, token string) string {
	h.t.Helper()
	res := h.raw(http.MethodGet, path, token)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return string(raw)
}

// call 은 상태 코드와 오류 코드·문구를 함께 돌려준다. 403 이 권한 거절인지 다른 거절인지 가린다.
func (h *harness) call(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	res := h.roundTrip(method, path, token, body, nil)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// request 는 응답 헤더를 같이 봐야 하는 요청이다.
func (h *harness) request(method, path, token string, body any, header map[string]string) (*http.Response, map[string]any) {
	h.t.Helper()
	res := h.roundTrip(method, path, token, body, header)
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func errorCodeOf(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func firstList(m map[string]any, key string) []any {
	list, _ := m[key].([]any)
	return list
}

func listIDs(list []any) []string {
	out := make([]string, 0, len(list))
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if v, ok := m["id"].(string); ok {
			out = append(out, v)
		}
	}
	return out
}

// ---- 세션과 워크스페이스 ----

// customerSession 은 위젯 진입과 같은 경로로 고객 토큰과 대화를 받는다.
func (h *harness) customerSession() (token, convID string) {
	h.t.Helper()
	out := h.post("/api/customer/session", "", map[string]any{"display_name": "E2E고객"})
	token, _ = out["token"].(string)
	if token == "" {
		h.t.Fatalf("세션 응답이 비었다: %+v", out)
	}
	// 위젯을 여는 것만으로는 대화가 생기지 않는다. 문의를 시작해야 열린다.
	started := h.post("/api/customer/conversations", token, nil)
	conv, _ := started["conversation"].(map[string]any)
	convID, _ = conv["id"].(string)
	if convID == "" {
		h.t.Fatalf("대화를 열지 못했다: %+v", started)
	}
	return token, convID
}

// customerSessionWith 는 기존 고객 토큰으로 새 대화를 연다.
func (h *harness) customerSessionWith(token string) (string, string) {
	h.t.Helper()
	started := h.post("/api/customer/conversations", token, nil)
	conv, _ := started["conversation"].(map[string]any)
	convID, _ := conv["id"].(string)
	return token, convID
}

func (h *harness) agentToken() string {
	h.t.Helper()
	agent := testsupport.Agent(h.t, h.store)
	out := h.post("/api/agent/login", "", map[string]any{"email": agent.Email, "password": "chat1234"})
	token, _ := out["token"].(string)
	if token == "" {
		h.t.Fatal("상담원 토큰이 비었다")
	}
	return token
}

// adminToken 은 관리자 토큰이다. 첫 시드 상담원이 관리자라 그 계정으로 들어간다.
func (h *harness) adminToken() string {
	h.t.Helper()
	ctx := context.Background()
	a, err := h.store.EnsureAgent(ctx, core.DefaultWorkspaceID, "admin-"+id.New()+"@test.local", "테스트관리자", "chat1234")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetAgentRole(ctx, core.DefaultWorkspaceID, a.ID, core.RoleAdmin); err != nil {
		h.t.Fatal(err)
	}
	out := h.post("/api/agent/login", "", map[string]any{"email": a.Email, "password": "chat1234"})
	token, _ := out["token"].(string)
	if token == "" {
		h.t.Fatal("관리자 토큰이 비었다")
	}
	return token
}

func (h *harness) agentEmail(agentID string) string {
	h.t.Helper()
	a, err := h.store.AgentByID(context.Background(), agentID)
	if err != nil {
		h.t.Fatal(err)
	}
	return a.Email
}

// tenant 는 한 워크스페이스와 그 첫 관리자다.
type tenant struct {
	token      string // 관리자
	wsID       string
	ownerEmail string // 관리자 이메일. 비밀번호는 ownerPassword
}

const ownerPassword = "owner1234"

// home 은 이 테넌트가 토큰 없는 입구(위젯·회원 세션·로그인·상담원 가입)가 가는 워크스페이스인지다.
// 설치형 단일 워크스페이스라 입구는 기본 워크스페이스로만 간다(D35).
func (tn tenant) home() bool { return tn.wsID == core.DefaultWorkspaceID }

// newTenant 는 두 번째 워크스페이스와 첫 관리자를 저장소 수준에서 만들고 관리자 세션을 발급한다.
// 회사 셀프 가입 입구(POST /api/signup)는 걷어냈다(D35). 그래도 내부 workspace_id 와 격리(I11)는 남아 있으니,
// 격리를 보거나 테스트끼리 행을 나누려면 다른 워크스페이스가 있어야 한다. 시드·테스트용 저장소 함수로 만든다.
func (h *harness) newTenant(name string) tenant {
	h.t.Helper()
	ctx := context.Background()
	suffix := strings.ToLower(strings.ReplaceAll(id.New(), "-", "")[20:])
	email := "owner-" + suffix + "@example.com"
	ws, admin, err := h.store.CreateWorkspaceWithAdmin(ctx, "t-"+suffix, name, email, "대표", ownerPassword)
	if err != nil {
		h.t.Fatalf("워크스페이스를 만들지 못했다: %v", err)
	}
	tok, err := h.svc.Commands.IssueAgentSession(ctx, admin.ID, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	return tenant{token: tok, wsID: ws.ID, ownerEmail: email}
}

// homeTenant 는 입구 워크스페이스(기본 워크스페이스)에 관리자를 하나 들이고 로그인 입구로 토큰을 받는다.
// 기본 워크스페이스의 설정·허용 오리진·승인 대기 수를 바꾸는 테스트는 격리 DB(newHarnessOn) 위에서 부른다 —
// 공유 DB 의 기본 워크스페이스는 다른 테스트도 같이 쓴다.
func (h *harness) homeTenant() tenant {
	h.t.Helper()
	ctx := context.Background()
	email := "owner-" + strings.ToLower(strings.ReplaceAll(id.New(), "-", "")[20:]) + "@example.com"
	a, err := h.store.CreateAgent(ctx, core.DefaultWorkspaceID, email, "대표", ownerPassword)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetAgentRole(ctx, core.DefaultWorkspaceID, a.ID, core.RoleAdmin); err != nil {
		h.t.Fatal(err)
	}
	out := h.post("/api/agent/login", "", map[string]any{"email": email, "password": ownerPassword})
	tok, _ := out["token"].(string)
	if tok == "" {
		h.t.Fatalf("기본 워크스페이스 관리자로 로그인하지 못했다: %+v", out)
	}
	return tenant{token: tok, wsID: core.DefaultWorkspaceID, ownerEmail: email}
}

// widgetCustomer 는 그 테넌트에 익명 고객 세션과 대화를 연다. 입구 워크스페이스면 위젯과 같은 HTTP 입구를 지나고,
// 다른 워크스페이스면 그 입구가 없으니(D35) 같은 유스케이스(StartCustomerSession)를 직접 부른다.
func (h *harness) widgetCustomer(tn tenant) (token, convID string) {
	h.t.Helper()
	return h.namedCustomer(tn, "고객")
}

// namedCustomer 는 widgetCustomer 와 같고 고객 이름을 정한다.
func (h *harness) namedCustomer(tn tenant, name string) (token, convID string) {
	h.t.Helper()
	if tn.home() {
		out := h.post("/api/customer/session", "", map[string]any{"display_name": name})
		token, _ = out["token"].(string)
	} else {
		ctx := context.Background()
		custID, _, err := h.svc.Commands.StartCustomerSession(ctx, tn.wsID, "", name)
		if err != nil {
			h.t.Fatal(err)
		}
		if token, err = h.svc.Commands.IssueCustomerSession(ctx, custID); err != nil {
			h.t.Fatal(err)
		}
	}
	started := h.post("/api/customer/conversations", token, nil)
	conv, _ := started["conversation"].(map[string]any)
	convID, _ = conv["id"].(string)
	if token == "" || convID == "" {
		h.t.Fatalf("위젯 고객을 만들지 못했다: %+v", started)
	}
	return token, convID
}

// memberSession 은 그 테넌트의 회원 서명으로 고객 세션을 연다. 입구 워크스페이스면 HTTP 입구를, 아니면 같은 유스케이스를 지난다.
func (h *harness) memberSession(tn tenant) string {
	h.t.Helper()
	secret, _ := h.get("/api/admin/workspace", tn.token)["member_secret"].(string)
	tok, _ := app.SignMember([]byte(secret), app.MemberClaims{MemberID: "m-" + id.New()[28:], Name: "김회원", Exp: time.Now().Add(time.Hour).Unix()})
	if tn.home() {
		sess := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
		cust, _ := sess["token"].(string)
		if cust == "" {
			h.t.Fatalf("회원 세션을 열지 못했다: %+v", sess)
		}
		return cust
	}
	ctx := context.Background()
	ws, err := h.store.Workspace(ctx, tn.wsID)
	if err != nil {
		h.t.Fatal(err)
	}
	custID, _, exp, err := h.svc.Commands.StartMemberSession(ctx, ws, tok)
	if err != nil {
		h.t.Fatalf("회원 세션을 열지 못했다: %v", err)
	}
	cust, err := h.svc.Commands.IssueMemberSession(ctx, custID, time.Hour, exp)
	if err != nil {
		h.t.Fatal(err)
	}
	return cust
}

func (h *harness) startConversation(cust string) map[string]any {
	h.t.Helper()
	c, _ := h.post("/api/customer/conversations", cust, nil)["conversation"].(map[string]any)
	return c
}

type member struct {
	token string
	id    string
	email string
}

// team 은 한 워크스페이스의 세 역할이다.
type team struct {
	tn       tenant
	admin    member
	staff    member
	partTime member
}

func (h *harness) teamOf(name string) team {
	h.t.Helper()
	tn := h.newTenant(name)
	me := h.get("/api/me", tn.token)
	adminID, _ := me["agent_id"].(string)
	return team{
		tn:       tn,
		admin:    member{token: tn.token, id: adminID},
		staff:    h.invited(tn, "STAFF"),
		partTime: h.invited(tn, "PART_TIME"),
	}
}

// invited 는 초대 링크로 그 역할의 상담원을 들인다. 초대 수락은 바로 활성이다.
func (h *harness) invited(tn tenant, role string) member {
	h.t.Helper()
	email := strings.ToLower(role) + "-" + strings.ToLower(id.New()[28:]) + "@example.com"
	out := h.post("/api/admin/invites", tn.token, map[string]any{"email": email, "name": role + " 상담원", "role": role})
	url, _ := out["accept_url"].(string)
	inviteToken := url[strings.Index(url, "invite=")+len("invite="):]
	acc := h.post("/api/agent/invites/accept", "", map[string]any{"token": inviteToken, "password": "member1234"})
	tok, _ := acc["token"].(string)
	agent, _ := acc["agent"].(map[string]any)
	agentID, _ := agent["id"].(string)
	if tok == "" || agentID == "" {
		h.t.Fatalf("%s 상담원을 들이지 못했다: %+v", role, acc)
	}
	return member{token: tok, id: agentID, email: email}
}

// agentSignup 은 그 테넌트에 상담원 가입을 신청하고 HTTP 상태를 준다. 입구 워크스페이스면 가입 입구(POST /api/agent/signup)를 지난다.
// 다른 워크스페이스는 입구가 없으니(D35) 같은 유스케이스(AgentSignup)를 부르고 결과를 입구와 같은 상태(202·400)로 옮긴다.
func (h *harness) agentSignup(tn tenant, email, name, password string) int {
	h.t.Helper()
	if tn.home() {
		// 공유 DB 의 기본 워크스페이스에 대기 행이 쌓이면 다음 실행에서 승인 대기 상한(50)에 닿는다. 끝나면 남은 대기 행을 지운다.
		h.t.Cleanup(func() {
			_, _ = h.store.Pool().Exec(context.Background(),
				"DELETE FROM agents WHERE workspace_id = $1 AND lower(email) = lower($2) AND approved_at IS NULL", core.DefaultWorkspaceID, email)
		})
		status, _ := h.call(http.MethodPost, "/api/agent/signup", "", map[string]any{"email": email, "name": name, "password": password})
		return status
	}
	err := h.svc.Commands.AgentSignup(context.Background(), app.AgentSignupInput{WorkspaceID: tn.wsID, Email: email, Name: name, Password: password})
	var inv app.InvalidError
	switch {
	case err == nil:
		return http.StatusAccepted
	case errors.As(err, &inv):
		return http.StatusBadRequest
	}
	h.t.Fatalf("상담원 가입이 실패했다: %v", err)
	return 0
}

func (h *harness) send(convID, token, body string, internal bool) map[string]any {
	h.t.Helper()
	out := h.post("/api/conversations/"+convID+"/messages", token, map[string]any{"client_msg_id": id.New(), "body": body, "internal": internal})
	msg, _ := out["message"].(map[string]any)
	return msg
}

func (h *harness) messagesOf(convID, token string) []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, raw := range firstList(h.get("/api/conversations/"+convID+"/messages", token), "messages") {
		m, _ := raw.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ---- 첨부 ----

type uploadStarted struct {
	Attachment struct {
		ID string `json:"id"`
	} `json:"attachment"`
	Upload struct {
		URL    string            `json:"url"`
		Fields map[string]string `json:"fields"`
	} `json:"upload"`
}

func (h *harness) startUpload(convID, token, filename, contentType string, size int) uploadStarted {
	h.t.Helper()
	res := h.postRaw("/api/conversations/"+convID+"/attachments", token, map[string]any{
		"filename": filename, "content_type": contentType, "byte_size": size,
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		h.t.Fatalf("주소 발급 %d: %s", res.StatusCode, raw)
	}
	var started uploadStarted
	if err := json.NewDecoder(res.Body).Decode(&started); err != nil {
		h.t.Fatal(err)
	}
	return started
}

// putObject 는 발급받은 양식으로 저장소에 파일을 올린다. 세션 토큰을 싣지 않는다 — 허가는 양식에 있다.
func (h *harness) putObject(started uploadStarted, filename string, data []byte) *http.Response {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range started.Upload.Fields {
		_ = mw.WriteField(k, v)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	target := started.Upload.URL
	if strings.HasPrefix(target, "/") {
		target = h.srv.URL + target
	}
	res, err := http.Post(target, mw.FormDataContentType(), &body)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func (h *harness) upload(convID, token, filename string, data []byte) map[string]any {
	h.t.Helper()
	res := h.uploadRaw(convID, token, filename, data)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		h.t.Fatalf("업로드가 %d: %s", res.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// uploadRaw 는 브라우저가 하는 세 걸음을 그대로 밟는다: 주소 발급 → 저장소로 직접 올림 → 완료.
// 어느 걸음에서든 거절되면 그 응답을 돌려준다.
func (h *harness) uploadRaw(convID, token, filename string, data []byte) *http.Response {
	h.t.Helper()
	res := h.postRaw("/api/conversations/"+convID+"/attachments", token, map[string]any{
		"filename": filename, "content_type": "application/octet-stream", "byte_size": len(data),
	})
	if res.StatusCode >= 400 {
		return res
	}
	var started uploadStarted
	if err := json.NewDecoder(res.Body).Decode(&started); err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	if put := h.putObject(started, filename, data); put.StatusCode >= 400 {
		return put
	}
	return h.postRaw("/api/attachments/"+started.Attachment.ID+"/complete", token, nil)
}

func (h *harness) completeStatus(attachmentID, token string) int {
	h.t.Helper()
	return h.status(http.MethodPost, "/api/attachments/"+attachmentID+"/complete", token, nil)
}

func (h *harness) sendWith(convID, token, attachmentID string) int {
	h.t.Helper()
	return h.status(http.MethodPost, "/api/conversations/"+convID+"/messages", token,
		map[string]any{"client_msg_id": id.New(), "body": "첨부", "attachment_id": attachmentID})
}

// download 는 발급받은 주소로 내려받은 내용이다.
func (h *harness) download(attachmentID, token string) string {
	h.t.Helper()
	link, _ := h.get("/api/attachments/"+attachmentID+"/url", token)["url"].(string)
	if link == "" {
		h.t.Fatal("내려받기 주소가 없다")
	}
	// 발급받은 주소는 그 자체가 허가다. 로그인 토큰을 싣지 않는다.
	res := h.roundTrip(http.MethodGet, link, "", nil, nil)
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("내려받기: %d %s", res.StatusCode, got)
	}
	return string(got)
}

// pendingAttachment 는 아직 메시지에 붙지 않은 업로드 하나를 집어 온다.
func (h *harness) pendingAttachment(convID string) string {
	h.t.Helper()
	var attachmentID string
	err := h.store.Pool().QueryRow(context.Background(),
		"SELECT id::text FROM attachments WHERE conversation_id = $1 AND message_id IS NULL LIMIT 1",
		convID).Scan(&attachmentID)
	if err != nil {
		h.t.Fatalf("보내지 않은 첨부를 못 찾았다: %v", err)
	}
	return attachmentID
}

func (h *harness) workspaceOf(convID string) string {
	h.t.Helper()
	var ws string
	if err := h.store.Pool().QueryRow(context.Background(),
		"SELECT workspace_id FROM conversations WHERE id = $1", convID).Scan(&ws); err != nil {
		h.t.Fatal(err)
	}
	return ws
}

// finalFiles 는 로컬 저장소에서 이 첨부의 확정 자리(ws/<워크스페이스>/<대화>/<첨부>/<임대 token>)에 남은 파일들이다.
func (h *harness) finalFiles(convID, attachmentID string) []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.files.Root(), "ws", h.workspaceOf(convID), convID, attachmentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

type timedStatus struct {
	status  int
	code    string
	elapsed time.Duration
	err     error
}

// timedCall 은 고루틴에서 부를 수 있는 요청이다. 실패를 t.Fatal 로 끊지 않고 돌려준다.
func timedCall(ctx context.Context, method, url, token string) timedStatus {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return timedStatus{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return timedStatus{err: err, elapsed: time.Since(start)}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return timedStatus{status: res.StatusCode, code: errorCodeOf(body), elapsed: time.Since(start)}
}

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// ---- WebSocket ----

type frame struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id"`
	LastSeq        int64  `json:"last_seq"`
	State          string `json:"state"`
	Visibility     string `json:"visibility"`
	Attachment     *struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		ByteSize    int64  `json:"byte_size"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
	} `json:"attachment"`
	Seq          int64           `json:"seq"`
	ClientMsgID  string          `json:"client_msg_id"`
	SenderKind   string          `json:"sender_kind"`
	Body         string          `json:"body"`
	SenderName   string          `json:"sender_name"`
	ReaderKind   string          `json:"reader_kind"`
	ReaderID     string          `json:"reader_id"`
	Conversation json.RawMessage `json:"conversation"`
	Reads        []struct {
		ReaderKind  string `json:"reader_kind"`
		ReaderID    string `json:"reader_id"`
		LastReadSeq int64  `json:"last_read_seq"`
	} `json:"reads"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
}

// dial 은 브라우저(chat.js)와 같은 길로 붙는다: 세션 토큰으로 일회용 티켓을 받고 /ws?ticket= 으로 붙는다(D46).
func (h *harness) dial(ctx context.Context, token string) *client {
	h.t.Helper()
	ws, _, err := websocket.Dial(ctx, h.wsURL()+"?ticket="+h.wsTicket(token), nil)
	if err != nil {
		h.t.Fatalf("WebSocket 접속 실패: %v", err)
	}
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: h.t, ws: ws}
}

// dialToken 은 옛 길(/ws?token=, DEF-91)로 붙는다. 그 길이 아직 받아지는지 보는 테스트만 쓴다.
func (h *harness) dialToken(ctx context.Context, token string) *client {
	h.t.Helper()
	ws, _, err := websocket.Dial(ctx, h.wsURL()+"?token="+token, nil)
	if err != nil {
		h.t.Fatalf("WebSocket 접속 실패: %v", err)
	}
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: h.t, ws: ws}
}

func (h *harness) wsURL() string { return "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws" }

// wsTicket 은 세션 토큰으로 WebSocket 티켓을 받는다.
func (h *harness) wsTicket(token string) string {
	h.t.Helper()
	out := h.post("/api/ws-ticket", token, nil)
	ticket, _ := out["ticket"].(string)
	if ticket == "" {
		h.t.Fatalf("티켓을 받지 못했다: %+v", out)
	}
	return ticket
}

func (c *client) send(ctx context.Context, v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("프레임 전송 실패: %v", err)
	}
}

func (c *client) subscribe(ctx context.Context, convID string, afterSeq int64) frame {
	c.t.Helper()
	c.send(ctx, map[string]any{"type": "subscribe", "conversation_id": convID, "after_seq": afterSeq})
	return c.await(ctx, func(f frame) bool { return f.Type == "ready" || f.Type == "error" })
}

// read 는 프레임 하나를 읽는다.
func (c *client) read(ctx context.Context) frame {
	c.t.Helper()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("프레임 수신 실패: %v", err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		c.t.Fatalf("프레임 해석 실패: %v (%s)", err, data)
	}
	return f
}

// await 는 조건에 맞는 프레임이 올 때까지 읽는다.
func (c *client) await(ctx context.Context, pred func(frame) bool) frame {
	c.t.Helper()
	for {
		f := c.read(ctx)
		if pred(f) {
			return f
		}
	}
}

// collectSeqs 는 메시지 프레임의 seq 를 want 개만큼 모은다.
func (c *client) collectSeqs(ctx context.Context, want int) []int64 {
	c.t.Helper()
	var got []int64
	for len(got) < want {
		f := c.read(ctx)
		if f.Type == "message" {
			got = append(got, f.Seq)
		}
	}
	return got
}

func testCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
