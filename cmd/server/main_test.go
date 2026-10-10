package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/filestore"
	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 보류가 풀리는 것은 사람이 아니라 주기 작업이 하는 상태 변화다. 그래도 conversation_id 로 거른 상담의 생애에 나와야 한다.
func TestWakeSnoozed_보류해제는conversation_state_changed로남는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	_, conv := testsupport.Conversation(t, store)
	if _, err := store.Pool().Exec(ctx,
		"UPDATE conversations SET state = 'SNOOZED', snoozed_until = now() - interval '1 minute' WHERE id = $1", conv.ID); err != nil {
		t.Fatal(err)
	}
	buf := captureDefaultLog(t)

	app.NewMaintenance(app.New(store).Commands, store, nil).WakeSnoozed(ctx)

	var found map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "conversation.state_changed" && l["conversation_id"] == conv.ID {
			found = l
		}
	}
	if found == nil {
		t.Fatalf("conversation.state_changed 가 없다:\n%s", buf.String())
	}
	if found["from"] != "SNOOZED" || found["to"] != "OPEN" || found["actor_kind"] != "system" || found["trace_id"] == nil {
		t.Errorf("내용이 틀렸다: %v", found)
	}
	if st, err := store.Conversation(ctx, conv.ID); err != nil || st.State != "OPEN" {
		t.Errorf("보류가 풀리지 않았다: %v %v", st.State, err)
	}
}

// 고아 정리 한 주기는 메시지에 붙지 못한 오래된 첨부(확인 전·확인 뒤 미전송)의 행과 저장소 파일을 지우고,
// 메시지에 붙은 첨부는 남긴다. 로컬 저장소면 오래된 임시 파일도 같은 주기(같은 trace)에 치운다.
func TestCleanupUploads_한주기가고아첨부의행과파일을지우고보낸첨부는남긴다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	files, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(store)
	svc.UseFiles(files, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
	custID, conv := testsupport.Conversation(t, store)
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}

	onDisk := func(key string) string { return filepath.Join(files.Root(), filepath.FromSlash(key)) }
	storagePath := func(attID string) string {
		t.Helper()
		ref, err := store.AttachmentFile(ctx, attID)
		if err != nil {
			t.Fatal(err)
		}
		return ref.StoragePath
	}
	// upload 는 첨부를 시작하고 임시 자리에 내용을 둔다. 브라우저가 양식으로 올린 것과 같은 상태다.
	upload := func(name string) string {
		t.Helper()
		att, _, err := svc.Commands.StartUpload(ctx, actor, conv.ID, app.UploadRequest{Filename: name, ContentType: "text/plain", ByteSize: 5})
		if err != nil {
			t.Fatal(err)
		}
		path := onDisk(storagePath(att.ID))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		return att.ID
	}
	complete := func(attID string) {
		t.Helper()
		if _, err := svc.Commands.CompleteUpload(ctx, actor, attID); err != nil {
			t.Fatal(err)
		}
	}

	pending := upload("pending.txt")
	confirmed := upload("confirmed.txt")
	complete(confirmed)
	sent := upload("sent.txt")
	complete(sent)
	if _, err := svc.Commands.Send(ctx, actor, app.SendParams{ConversationID: conv.ID, ClientMsgID: "cleanup-sent", AttachmentID: sent}); err != nil {
		t.Fatal(err)
	}
	pendingPath, confirmedPath, sentPath := onDisk(storagePath(pending)), onDisk(storagePath(confirmed)), onDisk(storagePath(sent))
	// 확인한 뒤 같은 양식으로 다시 올린 임시 파일처럼 행이 없는 오래된 임시 파일.
	stray := filepath.Join(filepath.Dir(pendingPath), "stray")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stray, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx,
		"UPDATE attachments SET created_at = now() - interval '7 hours' WHERE conversation_id = $1", conv.ID); err != nil {
		t.Fatal(err)
	}
	buf := captureDefaultLog(t)

	app.NewMaintenance(svc.Commands, store, files).CleanupUploads(ctx)

	for name, attID := range map[string]string{"확인 전": pending, "미전송": confirmed} {
		if _, err := store.AttachmentFile(ctx, attID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s 첨부의 행이 남았다: %v", name, err)
		}
	}
	if _, err := store.AttachmentFile(ctx, sent); err != nil {
		t.Errorf("보낸 첨부의 행이 지워졌다: %v", err)
	}
	for name, path := range map[string]string{"확인 전(임시)": pendingPath, "미전송(확정)": confirmedPath, "행 없는 임시": stray} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 파일이 남았다: %v", name, err)
		}
	}
	if _, err := os.Stat(sentPath); err != nil {
		t.Errorf("보낸 첨부의 파일이 지워졌다: %v", err)
	}

	lines := map[string]map[string]any{}
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "maintenance.done" {
			task, _ := l["task"].(string)
			lines[task] = l
		}
	}
	reaped, swept := lines["orphan_uploads"], lines["temp_uploads"]
	if reaped == nil || swept == nil {
		t.Fatalf("정리 로그가 다 나오지 않았다:\n%s", buf.String())
	}
	if reaped["level"] != "INFO" || reaped["rows"] != float64(2) || swept["level"] != "INFO" || swept["files"] != float64(1) {
		t.Errorf("정리 로그 내용이 틀렸다: %v / %v", reaped, swept)
	}
	if reaped["trace_id"] == nil || reaped["trace_id"] != swept["trace_id"] {
		t.Errorf("정리 한 주기는 trace 하나다: %v / %v", reaped["trace_id"], swept["trace_id"])
	}
}

// 기동 로그도 운영 로그다. 시드가 만든 계정의 이메일과 위젯 키를 싣지 않는다(D30). 새 설치처럼 관리자가 없는 DB 에서
// 승격 로그까지 나오게 돌린다.
func TestSeed_기동로그에는이메일과위젯키가없다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	var buf bytes.Buffer
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	cfg := config.Config{
		SeedDemo: true, SeedAgentEmail: "seed-owner@example.com", SeedAgentName: "시드", SeedAgentPass: "chat1234",
		SeedAgentCount: 3, MemberSecret: "seed-member-secret",
	}

	if err := seed(ctx, store, cfg, log); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "seed.agent_ready") || !strings.Contains(out, "seed.admin_promoted") || !strings.Contains(out, "server.workspace") {
		t.Fatalf("시드 로그가 다 나오지 않았다:\n%s", out)
	}
	// 위젯 키는 걷어냈지만(D35) 칸과 값은 DB 에 남아 있다. 그 값도 로그에 나오면 안 된다.
	var widgetKey string
	if err := store.Pool().QueryRow(ctx, "SELECT widget_key FROM workspaces WHERE id = $1", core.DefaultWorkspaceID).Scan(&widgetKey); err != nil || widgetKey == "" {
		t.Fatalf("위젯 키를 읽지 못했다: %v", err)
	}
	for _, secret := range []string{"@", "seed-owner", "agent2", "agent3", widgetKey, cfg.SeedAgentPass} {
		if strings.Contains(out, secret) {
			t.Errorf("기동 로그에 %q 가 있다:\n%s", secret, out)
		}
	}
}

// captureDefaultLog 는 운영과 같은 모양(logctx 로 감싼 JSON)의 로그를 모으는 기본 로거를 세운다.
// app 의 주기 작업은 slog 기본 로거를 쓴다. 테스트가 끝나면 되돌린다. 이 파일의 테스트는 병렬로 돌리지 않는다.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ---- 운영 조립 ----
// 아래 테스트는 run 이 쓰는 assemble 로 서버를 만든다. httpapi 의 테스트 하네스는 따로 조립하므로, 조립 줄이 빠져도
// 하네스 테스트는 통과한다. 여기서 잡는다.

// assembled 는 assemble 로 만든 서버다. 저장소는 이 테스트만 쓰는 DB 다.
type assembled struct {
	t     *testing.T
	srv   *httptest.Server
	a     assembly
	store *pgstore.Store
	cfg   config.Config
}

// s3Endpoint 는 MinIO 주소다. 다른 테스트와 같은 변수로 바꾼다.
func s3Endpoint() string {
	if v := os.Getenv("CHAT_TEST_S3_ENDPOINT"); v != "" {
		return v
	}
	return "http://127.0.0.1:59100"
}

// assembleFor 는 storage("local" 또는 "s3") 설정으로 운영 조립을 한다. 로컬 저장소는 임시 디렉토리다.
func assembleFor(t *testing.T, storage string) *assembled {
	t.Helper()
	return assembleWith(t, storage, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// assembleWith 는 로거와 설정을 바꿔 조립한다. adjust 가 nil 이 아니면 조립 전에 설정을 고친다.
func assembleWith(t *testing.T, storage string, log *slog.Logger, adjust func(*config.Config)) *assembled {
	t.Helper()
	store, dbURL := testsupport.Isolated(t)
	cfg := config.Config{
		// serve 가 띄우는 알림 리스너가 이 DB 에 붙는다.
		DatabaseURL:     dbURL,
		CatchupInterval: time.Second, SessionTTL: time.Hour,
		MaxUploadBytes: 10 << 20, MaxPendingUploads: 10,
		EntryRatePerMinute: 100000, PublicURL: "http://test.local",
		Storage: storage, UploadDir: t.TempDir(),
		S3: config.S3{Endpoint: s3Endpoint(), Region: "us-east-1", Bucket: "chat-rf3",
			AccessKey: "chat", SecretKey: "chatchat123", PathStyle: true, CreateBucket: true},
	}
	ctx := context.Background()
	if storage == "s3" {
		probe, err := s3store.New(s3store.Config{Endpoint: cfg.S3.Endpoint, Bucket: cfg.S3.Bucket,
			AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := probe.EnsureBucket(wait); err != nil {
			t.Skipf("MinIO 에 닿지 않는다 (%v). `docker compose up -d minio` 뒤에 다시 돌린다", err)
		}
	}
	if adjust != nil {
		adjust(&cfg)
	}
	a, err := assemble(ctx, cfg, store, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.handler)
	t.Cleanup(srv.Close)
	return &assembled{t: t, srv: srv, a: a, store: store, cfg: cfg}
}

// call 은 JSON 요청을 보내고 상태 코드와 풀린 본문을 돌려준다. path 가 / 로 시작하면 이 서버로 보낸다.
func (s *assembled) call(method, path, token string, body any) (int, map[string]any, string) {
	s.t.Helper()
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		payload = bytes.NewReader(b)
	}
	target := path
	if strings.HasPrefix(path, "/") {
		target = s.srv.URL + path
	}
	req, err := http.NewRequest(method, target, payload)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, string(raw)
}

// customer 는 위젯 입구로 고객 세션과 대화를 연다.
func (s *assembled) customer() (token, convID string) {
	s.t.Helper()
	_, sess, raw := s.call(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "조립고객"})
	token, _ = sess["token"].(string)
	_, started, raw2 := s.call(http.MethodPost, "/api/customer/conversations", token, nil)
	conv, _ := started["conversation"].(map[string]any)
	convID, _ = conv["id"].(string)
	if token == "" || convID == "" {
		s.t.Fatalf("고객을 만들지 못했다: %s / %s", raw, raw2)
	}
	return token, convID
}

// startUpload 는 첨부를 신청하고 첨부 id 와 올릴 주소·양식을 돌려준다.
func (s *assembled) startUpload(token, convID string, size int) (attID, url string, fields map[string]any) {
	s.t.Helper()
	code, out, raw := s.call(http.MethodPost, "/api/conversations/"+convID+"/attachments", token,
		map[string]any{"filename": "a.txt", "content_type": "application/octet-stream", "byte_size": size})
	if code != http.StatusCreated {
		s.t.Fatalf("첨부 신청: %d %s", code, raw)
	}
	att, _ := out["attachment"].(map[string]any)
	up, _ := out["upload"].(map[string]any)
	attID, _ = att["id"].(string)
	url, _ = up["url"].(string)
	fields, _ = up["fields"].(map[string]any)
	return attID, url, fields
}

// job 은 이름으로 주기 작업을 찾는다.
func (s *assembled) job(name string) periodicJob {
	s.t.Helper()
	for _, j := range s.a.jobs {
		if j.name == name {
			return j
		}
	}
	s.t.Fatalf("주기 작업 %q 가 없다: %+v", name, s.a.jobs)
	return periodicJob{}
}

// orphan 은 신청만 하고 7시간이 지난 첨부를 하나 만든다.
func (s *assembled) orphan() string {
	s.t.Helper()
	token, convID := s.customer()
	attID, _, _ := s.startUpload(token, convID, 5)
	if _, err := s.store.Pool().Exec(context.Background(),
		"UPDATE attachments SET created_at = now() - interval '7 hours' WHERE id = $1", attID); err != nil {
		s.t.Fatal(err)
	}
	return attID
}

func TestAssemble_운영조립으로띄운서버에서첨부신청올리기완료붙이기내려받기가된다(t *testing.T) {
	s := assembleFor(t, "local")
	token, convID := s.customer()
	attID, url, fields := s.startUpload(token, convID, 5)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v.(string))
	}
	part, err := mw.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = mw.Close()
	if strings.HasPrefix(url, "/") {
		url = s.srv.URL + url
	}
	put, err := http.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	put.Body.Close()
	if put.StatusCode >= 300 {
		t.Fatalf("저장소로 올리기: %d", put.StatusCode)
	}

	if code, _, raw := s.call(http.MethodPost, "/api/attachments/"+attID+"/complete", token, nil); code != http.StatusCreated {
		t.Fatalf("완료: %d %s", code, raw)
	}
	if code, _, raw := s.call(http.MethodPost, "/api/conversations/"+convID+"/messages", token,
		map[string]any{"client_msg_id": "assemble-1", "body": "첨부", "attachment_id": attID}); code >= 300 {
		t.Fatalf("메시지에 붙이기: %d %s", code, raw)
	}
	_, link, raw := s.call(http.MethodGet, "/api/attachments/"+attID+"/url", token, nil)
	got, _ := link["url"].(string)
	if got == "" {
		t.Fatalf("내려받기 주소가 없다: %s", raw)
	}
	// 발급받은 주소는 그 자체가 허가다. 로그인 토큰을 싣지 않는다.
	if code, _, content := s.call(http.MethodGet, got, "", nil); code != http.StatusOK || content != "hello" {
		t.Errorf("내려받기: %d %q", code, content)
	}
}

func TestAssemble_로컬저장소모드에만파일주소핸들러가있다(t *testing.T) {
	t.Run("로컬 저장소면 /files/upload·/files/get 을 저장소 핸들러가 받는다", func(t *testing.T) {
		s := assembleFor(t, "local")
		if code, _, raw := s.call(http.MethodPost, "/files/upload", "", nil); code != http.StatusBadRequest || !strings.Contains(raw, "multipart 양식이어야 합니다") {
			t.Errorf("POST /files/upload: %d %q", code, raw)
		}
		if code, _, raw := s.call(http.MethodGet, "/files/get", "", nil); code != http.StatusForbidden || !strings.Contains(raw, "파일 주소가 만료되었거나 올바르지 않습니다") {
			t.Errorf("GET /files/get: %d %q", code, raw)
		}
	})
	t.Run("S3 면 두 주소가 없다", func(t *testing.T) {
		s := assembleFor(t, "s3")
		if code, _, raw := s.call(http.MethodPost, "/files/upload", "", nil); code != http.StatusMethodNotAllowed {
			t.Errorf("POST /files/upload: %d %q", code, raw)
		}
		if code, _, raw := s.call(http.MethodGet, "/files/get", "", nil); code != http.StatusNotFound {
			t.Errorf("GET /files/get: %d %q", code, raw)
		}
	})
}

// 주기 작업은 셋이다. 주기는 정책이라 리터럴로 고정한다. 이름마다 그 일을 하는지도 본다(바꿔 꽂으면 깨진다).
func TestAssemble_주기작업은셋이고세션정리1시간고아업로드정리1시간보류해제1분이다(t *testing.T) {
	s := assembleFor(t, "local")
	want := []struct {
		name   string
		period time.Duration
	}{
		{"만료 세션 정리", 60 * time.Minute},
		{"고아 업로드 정리", 60 * time.Minute},
		{"보류 해제", 1 * time.Minute},
	}
	if len(s.a.jobs) != len(want) {
		t.Fatalf("주기 작업이 %d개다: %+v", len(s.a.jobs), s.a.jobs)
	}
	for i, w := range want {
		if j := s.a.jobs[i]; j.name != w.name || j.period != w.period || j.run == nil {
			t.Errorf("%d번째: %q %v, %q %v 여야 한다", i, j.name, j.period, w.name, w.period)
		}
	}

	ctx := context.Background()
	t.Run("만료 세션 정리는 만료 세션을 지운다", func(t *testing.T) {
		agent := testsupport.Agent(t, s.store)
		token, err := s.store.CreateSession(ctx, "AGENT", agent.ID, "", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		hash := auth.HashToken(token)
		s.exec("UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE token_hash = $1", hash)
		s.job("만료 세션 정리").run(ctx)
		if n := s.count("SELECT count(*) FROM sessions WHERE token_hash = $1", hash); n != 0 {
			t.Errorf("만료 세션이 남았다: %d", n)
		}
	})
	t.Run("고아 업로드 정리는 오래된 미전송 첨부를 지운다", func(t *testing.T) {
		attID := s.orphan()
		s.job("고아 업로드 정리").run(ctx)
		if n := s.count("SELECT count(*) FROM attachments WHERE id = $1", attID); n != 0 {
			t.Errorf("고아 첨부가 남았다: %d", n)
		}
	})
	t.Run("보류 해제는 시각이 지난 보류를 연다", func(t *testing.T) {
		_, conv := testsupport.Conversation(t, s.store)
		s.exec("UPDATE conversations SET state = 'SNOOZED', snoozed_until = now() - interval '1 minute' WHERE id = $1", conv.ID)
		s.job("보류 해제").run(ctx)
		if st, err := s.store.Conversation(ctx, conv.ID); err != nil || st.State != core.StateOpen {
			t.Errorf("보류가 풀리지 않았다: %v %v", st.State, err)
		}
	})
}

func TestAssemble_고아업로드정리는로컬저장소에서오래된임시파일도지우고S3에서는임시파일정리없이돈다(t *testing.T) {
	ctx := context.Background()
	t.Run("로컬 저장소", func(t *testing.T) {
		s := assembleFor(t, "local")
		stray := filepath.Join(s.cfg.UploadDir, "tmp", "stray")
		if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(stray, old, old); err != nil {
			t.Fatal(err)
		}
		s.job("고아 업로드 정리").run(ctx)
		if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("행 없는 오래된 임시 파일이 남았다: %v", err)
		}
	})
	t.Run("S3", func(t *testing.T) {
		s := assembleFor(t, "s3")
		attID := s.orphan()
		buf := captureDefaultLog(t)
		s.job("고아 업로드 정리").run(ctx)
		if n := s.count("SELECT count(*) FROM attachments WHERE id = $1", attID); n != 0 {
			t.Errorf("고아 첨부가 남았다: %d", n)
		}
		if out := buf.String(); !strings.Contains(out, "orphan_uploads") || strings.Contains(out, "temp_uploads") {
			t.Errorf("S3 는 고아 정리만 하고 임시 파일 정리는 하지 않는다:\n%s", out)
		}
	})
}

func (s *assembled) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.store.Pool().Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatal(err)
	}
}

func (s *assembled) count(sql string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.store.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// ---- 주기 작업의 기준 ----

func TestCleanupSessions_만료된세션은지우고살아있는세션은남긴다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	session := func() []byte {
		t.Helper()
		token, err := store.CreateSession(ctx, "AGENT", agent.ID, "", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		return auth.HashToken(token)
	}
	expired, live := session(), session()
	if _, err := store.Pool().Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1", expired); err != nil {
		t.Fatal(err)
	}

	app.NewMaintenance(app.New(store).Commands, store, nil).CleanupSessions(ctx)

	for name, c := range map[string]struct {
		hash []byte
		want int
	}{"만료 세션": {expired, 0}, "살아 있는 세션": {live, 1}} {
		var n int
		if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_hash = $1", c.hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != c.want {
			t.Errorf("%s: %d행, %d행이어야 한다", name, n, c.want)
		}
	}
}

// 고아의 기준은 6시간이다. 5시간 59분 된 미전송 첨부는 남고 6시간 1분 된 것은 지워진다.
func TestCleanupUploads_고아기준은6시간이다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	files, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(store)
	svc.UseFiles(files, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
	custID, conv := testsupport.Conversation(t, store)
	actor := app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}
	aged := func(age string) string {
		t.Helper()
		att, _, err := svc.Commands.StartUpload(ctx, actor, conv.ID, app.UploadRequest{Filename: "a.txt", ContentType: "text/plain", ByteSize: 5})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Pool().Exec(ctx, "UPDATE attachments SET created_at = now() - $2::interval WHERE id = $1", att.ID, age); err != nil {
			t.Fatal(err)
		}
		return att.ID
	}
	young, old := aged("5 hours 59 minutes"), aged("6 hours 1 minute")

	app.NewMaintenance(svc.Commands, store, files).CleanupUploads(ctx)

	if _, err := store.AttachmentFile(ctx, young); err != nil {
		t.Errorf("5시간 59분 된 첨부가 지워졌다: %v", err)
	}
	if _, err := store.AttachmentFile(ctx, old); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("6시간 1분 된 첨부가 남았다: %v", err)
	}
}

// 행 없는 임시 파일의 기준은 1시간이다. 59분 된 것은 남고 61분 된 것은 지워진다.
func TestCleanupUploads_임시파일기준은1시간이다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	files, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(store)
	svc.UseFiles(files, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
	stray := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(files.Root(), "tmp", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	young, old := stray("young", 59*time.Minute), stray("old", 61*time.Minute)

	app.NewMaintenance(svc.Commands, store, files).CleanupUploads(ctx)

	if _, err := os.Stat(young); err != nil {
		t.Errorf("59분 된 임시 파일이 지워졌다: %v", err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("61분 된 임시 파일이 남았다: %v", err)
	}
}

// ---- 단일 워크스페이스 (D35) ----

// 위젯 키와 CHAT_DEFAULT_WORKSPACE 는 걷어냈다. 옛 배포 설정에는 이 변수가 빈 값으로 남아 있을 수 있다 — 예전에는 "키가 필수다" 라는
// 뜻이었다. 이제는 아무것도 읽지 않으니 남아 있어도 키 없는 위젯은 기본 워크스페이스로 간다. 운영 조립(assemble)을 지난다.
func TestConfig_옛설정CHAT_DEFAULT_WORKSPACE를빈값으로둬도키없는위젯은기본워크스페이스로간다(t *testing.T) {
	t.Setenv("CHAT_DEFAULT_WORKSPACE", "")
	t.Setenv("CHAT_SIGNUP_OPEN", "true")
	// 옛 변수가 남아 있어도 설정 읽기가 실패하지 않는다.
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	s := assembleFor(t, "local")
	if code, _, raw := s.call(http.MethodGet, "/api/widget/settings", "", nil); code != http.StatusOK {
		t.Errorf("키 없는 위젯 설정이 %d 다: %s", code, raw)
	}
	code, out, raw := s.call(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "키없음"})
	if code != http.StatusOK {
		t.Fatalf("키 없는 세션 생성이 %d 다: %s", code, raw)
	}
	cust, _ := out["customer"].(map[string]any)
	if n := s.count("SELECT count(*) FROM customers WHERE id = $1 AND workspace_id = $2", cust["id"], core.DefaultWorkspaceID); n != 1 {
		t.Errorf("고객이 기본 워크스페이스에 없다: %s", raw)
	}
	if code, _, raw := s.call(http.MethodGet, "/widget?key="+url.QueryEscape("wk_옛키"), "", nil); code != http.StatusOK {
		t.Errorf("옛 키를 붙인 위젯 화면이 %d 다: %q", code, raw)
	}
	if code, _, raw := s.call(http.MethodPost, "/api/signup", "", map[string]any{"workspace": "새 회사", "email": "new@example.com", "password": "owner1234"}); code != http.StatusMethodNotAllowed {
		t.Errorf("회사 가입 경로가 살아 있다: %d %q", code, raw)
	}
}

// errCode 는 오류 응답의 code 다.
func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// ---- 배포 (D32·D34) ----

// 컨테이너 헬스체크는 프로세스 생존(/healthz)을 본다. /readyz 를 보면 DB 장애 조치 동안 모든 태스크가 비정상이 되어
// 멀쩡한 태스크가 교체되고, 내려가는 중(readyz 503)인 태스크가 drain 을 마치기 전에 죽는다.
func TestDockerfile_컨테이너헬스체크는readyz가아니라healthz다(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var checks []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "HEALTHCHECK") {
			checks = append(checks, line)
		}
	}
	if len(checks) != 1 {
		t.Fatalf("HEALTHCHECK 가 %d 줄이다", len(checks))
	}
	if !strings.Contains(checks[0], "/healthz") || strings.Contains(checks[0], "/readyz") {
		t.Errorf("HEALTHCHECK 가 /healthz 를 보지 않는다: %s", checks[0])
	}
}

// ---- 올리기 방식(D42) ----

// assemblePut 은 S3 저장소를 서명된 PUT 주소 방식(CHAT_S3_UPLOAD=put)으로 조립한다. 양식 업로드가 없는 S3 호환 저장소가 쓰는 방식이다.
func assemblePut(t *testing.T) *assembled {
	t.Helper()
	return assembleWith(t, "s3", slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) { c.S3.Upload = "put" })
}

// startPut 은 첨부를 신청하고 받은 올리기 양식(방식·주소·헤더)을 돌려준다.
func (s *assembled) startPut(token, convID string, size int) (attID string, form core.UploadForm) {
	s.t.Helper()
	code, _, raw := s.call(http.MethodPost, "/api/conversations/"+convID+"/attachments", token,
		map[string]any{"filename": "a.txt", "content_type": "text/plain", "byte_size": size})
	if code != http.StatusCreated {
		s.t.Fatalf("첨부 신청: %d %s", code, raw)
	}
	var out struct {
		Attachment struct {
			ID string `json:"id"`
		} `json:"attachment"`
		Upload core.UploadForm `json:"upload"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		s.t.Fatal(err)
	}
	return out.Attachment.ID, out.Upload
}

// putFile 은 브라우저가 하듯이 받은 헤더를 그대로 싣고 파일을 본문으로 PUT 한다.
func putFile(t *testing.T, form core.UploadForm, data []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, form.URL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range form.Headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestAssemble_PUT방식으로조립한서버에서첨부신청올리기완료붙이기내려받기가된다(t *testing.T) {
	s := assemblePut(t)
	token, convID := s.customer()
	data := []byte("hello put")
	attID, form := s.startPut(token, convID, len(data))
	if form.Method != http.MethodPut || len(form.Fields) != 0 || form.Headers["Content-Type"] != "text/plain" {
		t.Fatalf("PUT 양식이 아니다: %+v", form)
	}
	if code := putFile(t, form, data); code != http.StatusOK {
		t.Fatalf("저장소로 올리기: %d", code)
	}
	if code, _, raw := s.call(http.MethodPost, "/api/attachments/"+attID+"/complete", token, nil); code != http.StatusCreated {
		t.Fatalf("완료: %d %s", code, raw)
	}
	if code, _, raw := s.call(http.MethodPost, "/api/conversations/"+convID+"/messages", token,
		map[string]any{"client_msg_id": "assemble-put-1", "body": "첨부", "attachment_id": attID}); code >= 300 {
		t.Fatalf("메시지에 붙이기: %d %s", code, raw)
	}
	_, link, raw := s.call(http.MethodGet, "/api/attachments/"+attID+"/url", token, nil)
	got, _ := link["url"].(string)
	if got == "" {
		t.Fatalf("내려받기 주소가 없다: %s", raw)
	}
	if code, _, content := s.call(http.MethodGet, got, "", nil); code != http.StatusOK || content != string(data) {
		t.Errorf("내려받기: %d %q", code, content)
	}
}

// 저장소가 서명된 크기를 지키지 못하는 경우(문서에 적지 않은 저장소가 있다)를 흉내 낸다: 같은 임시 키에 더 큰 크기로 따로 서명해 한도보다 큰 파일을 둔다.
// 앱은 저장소를 믿지 않는다. 완료가 크기를 확인해 거절하고 그 객체를 지운다.
func TestAssemble_PUT방식에서저장소가크기를지키지못해한도보다큰파일이올라와도완료가거절하고객체를지운다(t *testing.T) {
	s := assemblePut(t)
	token, convID := s.customer()
	attID, _ := s.startPut(token, convID, 5)
	var key string
	if err := s.store.Pool().QueryRow(context.Background(), "SELECT storage_path FROM attachments WHERE id = $1", attID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	loose, err := s3store.New(s3store.Config{Endpoint: s.cfg.S3.Endpoint, Bucket: s.cfg.S3.Bucket,
		AccessKey: s.cfg.S3.AccessKey, SecretKey: s.cfg.S3.SecretKey, PathStyle: true, Upload: s3store.UploadModePut})
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), int(s.cfg.MaxUploadBytes)+1)
	form, err := loose.PresignUpload(key, "text/plain", int64(len(big)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code := putFile(t, form, big); code != http.StatusOK {
		t.Fatalf("한도보다 큰 파일 두기: %d", code)
	}

	code, out, raw := s.call(http.MethodPost, "/api/attachments/"+attID+"/complete", token, nil)
	if code != http.StatusBadRequest || errCode(out) != "invalid" {
		t.Fatalf("완료가 거절해야 한다: %d %s", code, raw)
	}
	if _, err := loose.Stat(context.Background(), key); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("한도보다 큰 객체가 남았다: %v", err)
	}
	if n := s.count("SELECT count(*) FROM attachments WHERE id = $1 AND uploaded_at IS NOT NULL", attID); n != 0 {
		t.Errorf("확인된 첨부가 됐다")
	}
}

// D30(감사 23 N8): 초안 모델을 켜면 기동 로그 server.draft_enabled 에 어느 호스트인지만 싣는다. 주소에는 사용자 정보(user:pass@)와
// 경로·쿼리(토큰)가 들어갈 수 있다.
func TestAssemble_초안모델을켜면기동로그에호스트만싣고주소의사용자정보와경로는싣지않는다(t *testing.T) {
	var buf syncBuf
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	assembleWith(t, "local", log, func(cfg *config.Config) {
		cfg.OllamaURL = "http://ollama-user:ollama-secret@ollama.internal:11434/base?token=query-secret"
		cfg.OllamaModel = "qwen"
	})
	lines := linesOf(buf.String(), "server.draft_enabled")
	if len(lines) != 1 || lines[0]["ollama_host"] != "ollama.internal:11434" || lines[0]["model"] != "qwen" {
		t.Fatalf("server.draft_enabled 는 ollama_host=ollama.internal:11434 model=qwen 한 줄이어야 한다: %v", lines)
	}
	for _, secret := range []string{"ollama-user", "ollama-secret", "query-secret", "/base"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("기동 로그에 주소의 %q 가 실렸다", secret)
		}
	}
}
