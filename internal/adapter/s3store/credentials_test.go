package s3store_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// 운영(ECS)은 태스크 역할만 쓴다. 장기 키는 없다. 자격 증명은 컨테이너 자격 증명 엔드포인트가 주는 임시 키·비밀·세션 토큰이다.
// 아래는 그 엔드포인트를 흉내 내는 가짜(httptest)와, 진짜 임시 자격 증명을 내주는 MinIO STS 로 확인한다.

type issued struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// fakeCredentials 는 ECS 에이전트의 자격 증명 엔드포인트다. 응답을 차례로 내주고, 마지막 것을 되풀이한다.
// 응답의 status 가 0 이 아니면 그 상태로 실패한다.
type fakeCredentials struct {
	srv   *httptest.Server
	mu    sync.Mutex
	queue []fakeReply
	hits  atomic.Int64
	paths []string
	auths []string
	delay time.Duration
}

type fakeReply struct {
	status int
	creds  issued
}

func newFakeCredentials(t *testing.T, replies ...fakeReply) *fakeCredentials {
	t.Helper()
	f := &fakeCredentials{queue: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		reply := f.queue[0]
		if len(f.queue) > 1 {
			f.queue = f.queue[1:]
		}
		delay := f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		if reply.status != 0 {
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, `{"code":"InternalError","AccessKeyId":"`+reply.creds.AccessKeyID+`"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(reply.creds)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// seen 은 지금까지 받은 요청의 경로와 Authorization 헤더다.
func (f *fakeCredentials) seen() (paths, auths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.auths...)
}

func temp(n string, expiresIn time.Duration) fakeReply {
	return fakeReply{creds: issued{
		AccessKeyID: "ASIATEMPKEY" + n, SecretAccessKey: "temp-secret-" + n, Token: "temp-session-token-" + n,
		Expiration: time.Now().Add(expiresIn).UTC().Format(time.RFC3339),
	}}
}

// useTaskRole 은 컨테이너에 태스크 역할이 붙은 것처럼 환경 변수를 놓는다. 다른 경로의 변수는 지운다.
func useTaskRole(t *testing.T, fullURI, token string) {
	t.Helper()
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", fullURI)
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", token)
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "")
}

// captureLog 는 기본 로거를 JSON 버퍼로 바꾼다. 저장소는 Logger 를 안 받으면 기본 로거에 쓴다.
func captureLog(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		mu.Lock()
		text := buf.String()
		mu.Unlock()
		var out []map[string]any
		sc := bufio.NewScanner(strings.NewReader(text))
		for sc.Scan() {
			var r map[string]any
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				r["_raw"] = sc.Text()
				out = append(out, r)
			}
		}
		return out
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func taskRoleStore(t *testing.T, endpoint string) *s3store.Store {
	t.Helper()
	store, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: "chat-test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func tokenIn(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.Query().Get("X-Amz-Security-Token")
}

func TestS3_태스크역할의임시자격증명이면세션토큰이서명주소와양식업로드에실린다(t *testing.T) {
	creds := newFakeCredentials(t, temp("1", time.Hour))
	useTaskRole(t, creds.srv.URL+"/v1/credentials", "agent-auth-token")

	var seen []url.Values
	var seenMu sync.Mutex
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seen = append(seen, r.URL.Query())
		seenMu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer storage.Close()
	store := taskRoleStore(t, storage.URL)

	t.Run("엔드포인트에는 Authorization 헤더로 토큰을 싣는다", func(t *testing.T) {
		if _, auths := creds.seen(); len(auths) == 0 || auths[0] != "agent-auth-token" {
			t.Errorf("Authorization: %v", auths)
		}
	})

	t.Run("내려받기 주소의 쿼리에 세션 토큰과 임시 키가 있다", func(t *testing.T) {
		link, err := store.PresignDownload("ws/a/b", "a.txt", "text/plain", false, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if got := tokenIn(link); got != "temp-session-token-1" {
			t.Errorf("X-Amz-Security-Token = %q", got)
		}
		u, _ := url.Parse(link)
		if !strings.HasPrefix(u.Query().Get("X-Amz-Credential"), "ASIATEMPKEY1/") {
			t.Errorf("X-Amz-Credential = %q", u.Query().Get("X-Amz-Credential"))
		}
	})

	t.Run("양식 업로드의 필드와 policy 조건에 세션 토큰이 있다", func(t *testing.T) {
		form, err := store.PresignUpload("tmp/x", "text/plain", 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if form.Fields["x-amz-security-token"] != "temp-session-token-1" {
			t.Errorf("필드: %q", form.Fields["x-amz-security-token"])
		}
		if !strings.HasPrefix(form.Fields["x-amz-credential"], "ASIATEMPKEY1/") {
			t.Errorf("x-amz-credential = %q", form.Fields["x-amz-credential"])
		}
		raw, _ := base64.StdEncoding.DecodeString(form.Fields["policy"])
		if !strings.Contains(string(raw), `{"x-amz-security-token":"temp-session-token-1"}`) {
			t.Errorf("policy 에 토큰 조건이 없다: %s", raw)
		}
	})

	t.Run("서버가 저장소에 보내는 확인·삭제·복사 요청에도 세션 토큰이 실린다", func(t *testing.T) {
		ctx := context.Background()
		_, _ = store.Stat(ctx, "tmp/x")
		_ = store.Remove(ctx, "ws/x")
		_ = store.Promote(ctx, "tmp/x", "ws/x")
		seenMu.Lock()
		defer seenMu.Unlock()
		if len(seen) < 3 {
			t.Fatalf("요청 수 %d", len(seen))
		}
		for _, q := range seen {
			if q.Get("X-Amz-Security-Token") != "temp-session-token-1" {
				t.Errorf("토큰이 없는 요청: %v", q)
			}
		}
	})
}

func TestS3_RELATIVE_URI면ECS에이전트주소에서토큰없이받는다(t *testing.T) {
	creds := newFakeCredentials(t, temp("rel", time.Hour))
	restore := s3store.SetContainerHost(creds.srv.URL)
	defer restore()
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/credentials/task-uuid")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://should-not-be-used.invalid/")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "should-not-be-sent")
	store := taskRoleStore(t, "http://127.0.0.1:1")
	link, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if tokenIn(link) != "temp-session-token-rel" {
		t.Errorf("토큰: %q", tokenIn(link))
	}
	if paths, auths := creds.seen(); len(paths) != 1 || paths[0] != "/v2/credentials/task-uuid" || auths[0] != "" {
		t.Errorf("경로 %v, Authorization %v", paths, auths)
	}
}

func TestS3_FULL_URI의토큰은파일에서받을때마다읽는다(t *testing.T) {
	creds := newFakeCredentials(t, temp("1", 4*time.Minute), temp("2", time.Hour))
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("file-token-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	useTaskRole(t, creds.srv.URL+"/creds", "inline-token-ignored")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", file)
	store := taskRoleStore(t, "http://127.0.0.1:1")
	if err := os.WriteFile(file, []byte("file-token-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute); err != nil { // 만료 4분 전이라 다시 받는다
		t.Fatal(err)
	}
	if _, auths := creds.seen(); len(auths) != 2 || auths[0] != "file-token-a" || auths[1] != "file-token-b" {
		t.Errorf("Authorization: %v", auths)
	}
}

func TestS3_임시자격증명은만료15분전에새로받는다(t *testing.T) {
	// 첫 자격 증명은 만료까지 14분 59초 — 15분 창의 바로 안쪽이다(14분 창이면 받지 않아 구분된다). 다음 서명이 새로 받는다. 새 것은 1시간이라 그 뒤로는 받지 않는다.
	// 여유가 올리기 양식의 수명(10분)보다 길어야 양식을 받은 직후 서명한 자격 증명이 만료되는 틈이 없다.
	creds := newFakeCredentials(t, temp("1", 14*time.Minute+59*time.Second), temp("2", time.Hour))
	useTaskRole(t, creds.srv.URL+"/creds", "")
	store := taskRoleStore(t, "http://127.0.0.1:1")
	if creds.hits.Load() != 1 {
		t.Fatalf("기동 때 한 번 받아야 한다: %d", creds.hits.Load())
	}
	for i := range 5 {
		link, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if tokenIn(link) != "temp-session-token-2" {
			t.Errorf("%d번째 서명의 토큰 %q — 새로 받은 것이어야 한다", i, tokenIn(link))
		}
	}
	if creds.hits.Load() != 2 {
		t.Errorf("엔드포인트 호출 %d번, 2번이어야 한다(기동 1 + 갱신 1)", creds.hits.Load())
	}

	t.Run("만료 16분 전이면 아직 받지 않는다", func(t *testing.T) {
		far := newFakeCredentials(t, temp("far", 16*time.Minute), temp("next", time.Hour))
		useTaskRole(t, far.srv.URL+"/creds", "")
		store := taskRoleStore(t, "http://127.0.0.1:1")
		link, _ := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
		if tokenIn(link) != "temp-session-token-far" || far.hits.Load() != 1 {
			t.Errorf("토큰 %q, 호출 %d", tokenIn(link), far.hits.Load())
		}
	})

	t.Run("동시에 여럿이 서명해도 갱신은 한 번이고 나머지는 기다리지 않고 옛 것을 쓴다", func(t *testing.T) {
		slow := newFakeCredentials(t, temp("a", 4*time.Minute), temp("b", time.Hour))
		useTaskRole(t, slow.srv.URL+"/creds", "")
		store := taskRoleStore(t, "http://127.0.0.1:1")
		slow.mu.Lock()
		slow.delay = time.Second
		slow.mu.Unlock()
		var wg sync.WaitGroup
		var old atomic.Int64
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				link, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				switch tokenIn(link) {
				case "temp-session-token-a":
					old.Add(1)
				case "temp-session-token-b":
				default:
					t.Errorf("토큰 %q", tokenIn(link))
				}
			}()
		}
		wg.Wait()
		if slow.hits.Load() != 2 {
			t.Errorf("엔드포인트 호출 %d번, 2번이어야 한다", slow.hits.Load())
		}
		// 받으러 간 하나만 새 것을 기다린다. 나머지는 아직 유효한 옛 것으로 바로 서명한다.
		if old.Load() < 45 {
			t.Errorf("옛 자격 증명으로 바로 서명한 호출 %d/50 — 갱신을 기다렸다", old.Load())
		}
	})

	t.Run("이미 만료됐으면 모두 하나의 갱신을 기다려 새 것을 쓴다", func(t *testing.T) {
		gone := newFakeCredentials(t, temp("x", 2*time.Second), temp("y", time.Hour))
		useTaskRole(t, gone.srv.URL+"/creds", "")
		store := taskRoleStore(t, "http://127.0.0.1:1")
		time.Sleep(2100 * time.Millisecond)
		gone.mu.Lock()
		gone.delay = 300 * time.Millisecond
		gone.mu.Unlock()
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				link, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
				if err != nil || tokenIn(link) != "temp-session-token-y" {
					t.Errorf("토큰 %q, 오류 %v", tokenIn(link), err)
				}
			}()
		}
		wg.Wait()
		if gone.hits.Load() != 2 {
			t.Errorf("엔드포인트 호출 %d번, 2번이어야 한다", gone.hits.Load())
		}
	})
}

func TestS3_자격증명엔드포인트가잠시실패해도유효한자격증명으로계속서명하고경고는한번남긴다(t *testing.T) {
	records := captureLog(t)
	creds := newFakeCredentials(t, temp("1", 4*time.Minute), fakeReply{status: http.StatusInternalServerError})
	useTaskRole(t, creds.srv.URL+"/v1/credentials/secret-path", "agent-auth-token")
	store := taskRoleStore(t, "http://127.0.0.1:1")

	for i := range 3 {
		link, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute)
		if err != nil {
			t.Fatalf("%d번째: %v", i, err)
		}
		if tokenIn(link) != "temp-session-token-1" {
			t.Errorf("%d번째 토큰 %q", i, tokenIn(link))
		}
		if _, err := store.PresignUpload("tmp/k", "text/plain", 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if creds.hits.Load() != 2 {
		t.Errorf("엔드포인트 호출 %d번. 실패한 뒤 30초 안에는 다시 두드리지 않는다", creds.hits.Load())
	}
	var warns []map[string]any
	for _, r := range records() {
		if r["msg"] == "s3.credentials_refresh_failed" {
			warns = append(warns, r)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("경고 %d줄, 한 줄이어야 한다: %v", len(warns), records())
	}
	w := warns[0]
	if w["level"] != "WARN" || w["reason"] != "status" || w["status"] != float64(500) || w["source"] != "container" {
		t.Errorf("경고: %v", w)
	}
	if v, _ := w["valid_for_s"].(float64); v < 200 || v > 240 {
		t.Errorf("valid_for_s = %v, 남은 유효 시간(약 240초)이어야 한다", w["valid_for_s"])
	}

	t.Run("유효한 것이 없으면 서명하지 않고 오류다", func(t *testing.T) {
		gone := newFakeCredentials(t, temp("old", 2*time.Second), fakeReply{status: http.StatusServiceUnavailable})
		useTaskRole(t, gone.srv.URL+"/creds", "")
		store := taskRoleStore(t, "http://127.0.0.1:1")
		time.Sleep(2100 * time.Millisecond)
		if _, err := store.PresignDownload("k", "a", "text/plain", false, time.Minute); err == nil {
			t.Error("만료된 자격 증명으로 서명했다")
		}
		if _, err := store.Stat(context.Background(), "k"); err == nil {
			t.Error("만료된 자격 증명으로 요청했다")
		}
	})
}

func TestS3_기동때자격증명을받지못하면저장소를열지않는다(t *testing.T) {
	t.Run("키도 태스크 역할도 없다", func(t *testing.T) {
		useTaskRole(t, "", "")
		if _, err := s3store.New(s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b"}); err == nil {
			t.Error("자격 증명 없이 열렸다")
		}
	})
	t.Run("키를 하나만 줬다", func(t *testing.T) {
		useTaskRole(t, "", "")
		if _, err := s3store.New(s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKey: "only"}); err == nil {
			t.Error("키 하나로 열렸다")
		}
	})
	t.Run("엔드포인트가 실패한다", func(t *testing.T) {
		creds := newFakeCredentials(t, fakeReply{status: http.StatusInternalServerError, creds: issued{AccessKeyID: "LEAKED-IN-BODY"}})
		useTaskRole(t, creds.srv.URL+"/creds", "agent-auth-token")
		_, err := s3store.New(s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b"})
		if err == nil {
			t.Fatal("자격 증명 없이 열렸다")
		}
		for _, f := range []string{"LEAKED-IN-BODY", "agent-auth-token", "/creds", creds.srv.URL} {
			if strings.Contains(err.Error(), f) {
				t.Errorf("오류에 %q: %v", f, err)
			}
		}
	})
	t.Run("엔드포인트가 이미 만료된 것을 준다", func(t *testing.T) {
		creds := newFakeCredentials(t, temp("dead", -time.Minute))
		useTaskRole(t, creds.srv.URL+"/creds", "")
		if _, err := s3store.New(s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b"}); err == nil {
			t.Error("만료된 자격 증명으로 열렸다")
		}
	})
	t.Run("평문 http FULL_URI 는 루프백·ECS 에이전트 주소만 받는다", func(t *testing.T) {
		useTaskRole(t, "http://credentials.example.com/creds", "agent-auth-token")
		if _, err := s3store.New(s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b"}); err == nil {
			t.Error("바깥 http 주소로 토큰을 보내려 했다")
		}
	})
}

func TestS3_자격증명의키와토큰은로그와스팬에남지않는다(t *testing.T) {
	records := captureLog(t)
	exp := tracetest.NewInMemoryExporter()
	restore := telemetry.Install(telemetry.NewTracerProvider(exp, false), noop.NewMeterProvider())
	t.Cleanup(restore)

	creds := newFakeCredentials(t, temp("1", 4*time.Minute), fakeReply{status: http.StatusInternalServerError})
	useTaskRole(t, creds.srv.URL+"/v1/credentials/secret-path", "agent-auth-token")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer storage.Close()
	store := taskRoleStore(t, storage.URL)
	ctx := context.Background()
	_, _ = store.Stat(ctx, "tmp/k")
	_ = store.Remove(ctx, "ws/k")
	_ = store.Promote(ctx, "tmp/k", "ws/k")

	forbidden := []string{"ASIATEMPKEY1", "temp-secret-1", "temp-session-token-1", "agent-auth-token", "secret-path", "X-Amz-"}
	var text strings.Builder
	for _, r := range records() {
		text.WriteString(r["_raw"].(string) + "\n")
	}
	spans := exp.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("S3 호출 스팬이 모자란다: %d", len(spans))
	}
	for _, s := range spans {
		text.WriteString(spanText(s))
	}
	if !strings.Contains(text.String(), "s3.credentials_refresh_failed") {
		t.Fatal("걸러 낼 대상(갱신 실패 경고)을 만들지 못했다")
	}
	for _, f := range forbidden {
		if strings.Contains(text.String(), f) {
			t.Errorf("로그·스팬에 %q 가 있다:\n%s", f, text.String())
		}
	}
}

// ── 진짜 임시 자격 증명 (MinIO STS) ─────────────────────────────────

// assumeRole 은 MinIO 에 STS AssumeRole 을 불러 세션 토큰이 붙은 임시 자격 증명을 받는다. 서명은 SigV4 헤더 방식이다.
func assumeRole(t *testing.T, endpoint string) issued {
	t.Helper()
	body := "Action=AssumeRole&Version=2011-06-15&DurationSeconds=900"
	u, _ := url.Parse(endpoint)
	now := time.Now().UTC()
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	sum := sha256.Sum256([]byte(body))
	payload := hex.EncodeToString(sum[:])
	signed := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{"POST", "/", "",
		"content-type:application/x-www-form-urlencoded\nhost:" + u.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n",
		signed, payload}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	scope := day + "/us-east-1/sts/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	mac := func(k []byte, d string) []byte { h := hmac.New(sha256.New, k); h.Write([]byte(d)); return h.Sum(nil) }
	key := mac(mac(mac(mac([]byte("AWS4chatchat123"), day), "us-east-1"), "sts"), "aws4_request")
	req, _ := http.NewRequest(http.MethodPost, endpoint+"/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=chat/"+scope+", SignedHeaders="+signed+", Signature="+hex.EncodeToString(mac(key, toSign)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("MinIO 에 닿지 않는다 (%v)", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleResult>Credentials"`
	}
	if res.StatusCode != http.StatusOK || xml.Unmarshal(raw, &out) != nil || out.Credentials.SessionToken == "" {
		t.Fatalf("AssumeRole: %d %s", res.StatusCode, raw)
	}
	exp, _ := time.Parse(time.RFC3339, out.Credentials.Expiration)
	return issued{AccessKeyID: out.Credentials.AccessKeyID, SecretAccessKey: out.Credentials.SecretAccessKey,
		Token: out.Credentials.SessionToken, Expiration: exp.UTC().Format(time.RFC3339)}
}

// 서명이 맞는지는 저장소가 받아 줘야 안다. MinIO 가 내준 진짜 임시 자격 증명으로 올리고·확인하고·옮기고·받고·지운다.
func TestS3_태스크역할의임시자격증명으로서명한요청을저장소가받는다(t *testing.T) {
	endpoint := os.Getenv("CHAT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:59100"
	}
	open(t) // 버킷을 만들고, MinIO 가 없으면 건너뛴다
	issued := assumeRole(t, endpoint)
	creds := newFakeCredentials(t, fakeReply{creds: issued})
	useTaskRole(t, creds.srv.URL+"/creds", "")
	store := taskRoleStore(t, endpoint)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	tmp, final := "tmp/sts-"+stamp, "ws/test/sts/"+stamp
	data := []byte("temporary credentials")
	t.Cleanup(func() { _ = store.Remove(ctx, final); _ = store.Remove(ctx, tmp) })

	form, err := store.PresignUpload(tmp, "text/plain", int64(len(data)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("토큰을 뺀 양식은 저장소가 거절한다", func(t *testing.T) {
		if code := post(t, form, func(f map[string]string) { delete(f, "x-amz-security-token") }, data); code < 400 {
			t.Errorf("토큰 없는 양식이 받아졌다: %d — 이 저장소는 토큰을 확인하지 않아 아래 확인이 의미가 없다", code)
		}
	})
	if code := post(t, form, nil, data); code >= 300 {
		t.Fatalf("업로드: %d", code)
	}
	if info, err := store.Stat(ctx, tmp); err != nil || info.Size != int64(len(data)) {
		t.Fatalf("확인: %+v %v", info, err)
	}
	if err := store.Promote(ctx, tmp, final); err != nil {
		t.Fatal(err)
	}
	link, err := store.PresignDownload(final, "a.txt", "text/plain", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(got) != string(data) {
		t.Fatalf("내려받기: %d %q", res.StatusCode, got)
	}
	t.Run("토큰을 뺀 내려받기 주소는 저장소가 거절한다", func(t *testing.T) {
		u, _ := url.Parse(link)
		q := u.Query()
		q.Del("X-Amz-Security-Token")
		u.RawQuery = q.Encode()
		res, err := http.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("토큰 없는 주소: %d", res.StatusCode)
		}
	})
	if err := store.Remove(ctx, final); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, final); err != core.ErrNotFound { //nolint:errorlint // 감싸지 않은 core.ErrNotFound 그대로를 단언한다
		t.Errorf("지운 뒤: %v", err)
	}
}
