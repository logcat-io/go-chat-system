package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 첨부 완료 임대의 주인. 임대는 시간으로 끝나므로 멈췄던 옛 주인이 임대가 끝난 뒤 깨어날 수 있다.
// 옛 주인이 늦게 옮기거나 지우거나 비우는 것이 이어받은 새 주인의 것을 깨지 않아야 한다. 임대마다 확정 자리가 다르고
// (확정 키 끝의 token), 도장·해제는 token 이 맞을 때만 걸린다. 요청이 취소되거나 시한을 넘겨도 임대가 남지 않고,
// 옮긴 뒤에 끊긴 요청의 파일은 사라지지 않는다.

// hookFiles 는 저장소 호출 뒤(삭제는 앞)에 끼어드는 래퍼다. 이름은 "stat-tmp#1", "stat-final#2", "promote#1", "remove-final#1" 처럼
// 호출 종류와 몇 번째인지다. 삭제 훅이 있으면 훅이 끝난 뒤 요청 ctx 와 상관없이 지운다 — 요청은 이미 저장소로 떠났고
// 저장소가 늦게 적용하는 모습이다.
type hookFiles struct {
	app.FileStore
	mu    sync.Mutex
	n     map[string]int
	hooks map[string]func(ctx context.Context) error
}

func newHookFiles(inner app.FileStore) *hookFiles {
	return &hookFiles{FileStore: inner, n: map[string]int{}, hooks: map[string]func(context.Context) error{}}
}

func (f *hookFiles) on(name string, hook func(ctx context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[name] = hook
}

func (f *hookFiles) hook(op, key string) func(context.Context) error {
	kind := "final"
	if strings.HasPrefix(key, "tmp/") {
		kind = "tmp"
	}
	name := op + "-" + kind
	if strings.HasPrefix(op, "promote") {
		name = op
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[name]++
	return f.hooks[fmt.Sprintf("%s#%d", name, f.n[name])]
}

func (f *hookFiles) Stat(ctx context.Context, key string) (core.ObjectInfo, error) {
	info, err := f.FileStore.Stat(ctx, key)
	if h := f.hook("stat", key); h != nil {
		if herr := h(ctx); herr != nil {
			return core.ObjectInfo{}, herr
		}
	}
	return info, err
}

// Promote 의 "promote-start#n" 은 옮기기 직전, "promote#n" 은 옮긴 직후다.
func (f *hookFiles) Promote(ctx context.Context, from, to string) error {
	if h := f.hook("promote-start", from); h != nil {
		if herr := h(ctx); herr != nil {
			return herr
		}
	}
	err := f.FileStore.Promote(ctx, from, to)
	if h := f.hook("promote", from); h != nil {
		if herr := h(ctx); herr != nil {
			return herr
		}
	}
	return err
}

func (f *hookFiles) Remove(ctx context.Context, key string) error {
	if h := f.hook("remove", key); h != nil {
		_ = h(ctx)
		return f.FileStore.Remove(context.Background(), key)
	}
	return f.FileStore.Remove(ctx, key)
}

// gate 는 훅 안에서 멈춰 서는 자리다. 테스트가 entered 를 보고 일을 끼워 넣은 뒤 open 으로 풀어 준다.
type gate struct {
	entered, release chan struct{}
	once             sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open) // 테스트가 먼저 실패해도 멈춘 요청이 풀린다
	return g
}

func (g *gate) wait(context.Context) error {
	close(g.entered)
	<-g.release
	return nil
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// hookStore 는 앱이 쓰는 DB 저장소를 감싼다. 완료 요청의 요청 ctx 를 잡고, 임대를 잡은 뒤와 도장에 끼어든다.
type hookStore struct {
	*pgstore.Store
	mu         sync.Mutex
	reqCtx     context.Context
	afterClaim func()
	complete   func(stamp func() (core.Attachment, bool, error)) (core.Attachment, bool, error)
	releaseErr error // 있으면 임대 비우기가 이 오류로 실패한다
	failRead   bool  // 참이면 다음 AttachmentFile 하나가 실패한다
	claimErr   error // 있으면 다음 임대 잡기가 실제로 잡은 뒤 이 오류를 돌려준다(커밋됐는데 응답을 잃은 모습)
}

// AttachmentFile 은 완료 요청이 맨 처음 부르는 저장소 호출이다. 그 ctx 가 요청 ctx 다.
func (s *hookStore) AttachmentFile(ctx context.Context, attachmentID string) (core.AttachmentRef, error) {
	s.mu.Lock()
	if s.reqCtx == nil {
		s.reqCtx = ctx
	}
	fail := s.failRead
	s.failRead = false
	s.mu.Unlock()
	if fail {
		return core.AttachmentRef{}, errors.New("DB 읽기 오류(주입)")
	}
	return s.Store.AttachmentFile(ctx, attachmentID)
}

func (s *hookStore) ReleaseAttachment(ctx context.Context, attachmentID, token string) (bool, error) {
	s.mu.Lock()
	err := s.releaseErr
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	return s.Store.ReleaseAttachment(ctx, attachmentID, token)
}

func (s *hookStore) ClaimAttachment(ctx context.Context, attachmentID, token string, lease time.Duration) (core.AttachmentRef, bool, error) {
	ref, claimed, err := s.Store.ClaimAttachment(ctx, attachmentID, token, lease)
	s.mu.Lock()
	after := s.afterClaim
	s.afterClaim = nil
	claimErr := s.claimErr
	s.claimErr = nil
	s.mu.Unlock()
	if after != nil {
		after()
	}
	if claimErr != nil {
		return core.AttachmentRef{}, false, claimErr
	}
	return ref, claimed, err
}

func (s *hookStore) CompleteAttachment(ctx context.Context, attachmentID, token, storagePath, contentType string, width, height int) (core.Attachment, bool, error) {
	stamp := func() (core.Attachment, bool, error) {
		return s.Store.CompleteAttachment(ctx, attachmentID, token, storagePath, contentType, width, height)
	}
	s.mu.Lock()
	hook := s.complete
	s.complete = nil
	s.mu.Unlock()
	if hook != nil {
		return hook(stamp)
	}
	return stamp()
}

// requestDone 은 잡아 둔 요청 ctx 가 끝날 때까지 기다린다. 서버가 클라이언트의 끊음을 알아챈 시점이다.
func (s *hookStore) requestDone(t *testing.T) {
	s.mu.Lock()
	req := s.reqCtx
	s.mu.Unlock()
	if req == nil {
		t.Error("요청 ctx 를 잡지 못했다")
		return
	}
	select {
	case <-req.Done():
	case <-time.After(5 * time.Second):
		t.Error("클라이언트가 끊었는데 서버의 요청 ctx 가 끝나지 않았다")
	}
}

// ownerCase 는 첨부 하나를 신청하고 AAAAA 를 올려 둔 상태다.
type ownerCase struct {
	t     *testing.T
	h     *harness
	files *hookFiles
	inner app.FileStore
	cust  string
	conv  string
	st    uploadStarted
	att   string
}

// ownerModes 는 로컬 디스크와 MinIO 다. S3 는 확정 자리를 덮어쓰고 ctx 를 지킨다 — 로컬과 성질이 달라 둘 다 본다.
var ownerModes = []string{"local", "s3"}

func newOwnerCase(t *testing.T, h *harness, mode string, limits app.UploadLimits) *ownerCase {
	t.Helper()
	var inner app.FileStore = h.files
	if mode == "s3" {
		endpoint := os.Getenv("CHAT_TEST_S3_ENDPOINT")
		if endpoint == "" {
			endpoint = "http://127.0.0.1:59100"
		}
		s3, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: "chat-test", AccessKey: "chat", SecretKey: "chatchat123", PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s3.EnsureBucket(ctx); err != nil {
			t.Skipf("MinIO 에 닿지 않는다 (%v). `docker compose up -d minio` 뒤에 다시 돌린다", err)
		}
		inner = s3
	}
	files := newHookFiles(inner)
	if limits.MaxBytes == 0 {
		limits.MaxBytes, limits.MaxPending = 10<<20, 10
	}
	h.svc.UseFiles(files, limits)
	cust, conv := h.customerSession()
	st := h.startUpload(conv, cust, "a.txt", "text/plain", 5)
	c := &ownerCase{t: t, h: h, files: files, inner: inner, cust: cust, conv: conv, st: st, att: st.Attachment.ID}
	c.put("AAAAA")
	return c
}

func (c *ownerCase) put(body string) {
	c.t.Helper()
	if code := c.h.putObject(c.st, "a.txt", []byte(body)).StatusCode; code >= 300 {
		c.t.Fatalf("저장소에 %s 올리기: %d", body, code)
	}
}

func (c *ownerCase) complete() timedStatus {
	return timedCall(context.Background(), http.MethodPost, c.h.srv.URL+"/api/attachments/"+c.att+"/complete", c.cust)
}

// completeMessage 는 완료의 상태·코드와 사용자에게 보일 문구다.
func (c *ownerCase) completeMessage() (int, string, string) {
	c.t.Helper()
	status, body := c.h.call(http.MethodPost, "/api/attachments/"+c.att+"/complete", c.cust, nil)
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return status, errorCodeOf(body), msg
}

// reapply 는 같은 대화에 첨부를 새로 신청하고 그 첨부로 바꿔 든다. 올리기는 하지 않는다.
func (c *ownerCase) reapply() {
	c.t.Helper()
	c.st = c.h.startUpload(c.conv, c.cust, "a.txt", "text/plain", 5)
	c.att = c.st.Attachment.ID
}

func (c *ownerCase) completeAsync() chan timedStatus {
	done := make(chan timedStatus, 1)
	go func() { done <- c.complete() }()
	return done
}

// expireLease 는 임대만 끝난 것으로 만든다. 처리하던 요청은 그대로 멈춰 있다.
func (c *ownerCase) expireLease() {
	c.t.Helper()
	if _, err := c.h.store.Pool().Exec(context.Background(),
		"UPDATE attachments SET completing_until = clock_timestamp() - interval '1 second' WHERE id = $1 AND completing_until IS NOT NULL",
		c.att); err != nil {
		c.t.Fatal(err)
	}
}

// fetch 는 내려받은 내용이다. 받을 수 없으면 그 까닭을 꺾쇠로 감싸 돌려준다.
func (c *ownerCase) fetch() string {
	c.t.Helper()
	status, body := c.h.call(http.MethodGet, "/api/attachments/"+c.att+"/url", c.cust, nil)
	link, _ := body["url"].(string)
	if link == "" {
		return fmt.Sprintf("<주소 %d>", status)
	}
	if strings.HasPrefix(link, "/") {
		link = c.h.srv.URL + link
	}
	res, err := http.Get(link)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return fmt.Sprintf("<내려받기 %d>", res.StatusCode)
	}
	return string(got)
}

func (c *ownerCase) row() (uploaded bool, path string, lease *time.Time) {
	c.t.Helper()
	if err := c.h.store.Pool().QueryRow(context.Background(),
		"SELECT uploaded_at IS NOT NULL, storage_path, completing_until FROM attachments WHERE id = $1", c.att).Scan(&uploaded, &path, &lease); err != nil {
		c.t.Fatal(err)
	}
	return uploaded, path, lease
}

// exists 는 저장소에 키가 있는지다.
func (c *ownerCase) exists(key string) bool {
	c.t.Helper()
	_, err := c.inner.Stat(context.Background(), key)
	if errors.Is(err, core.ErrNotFound) {
		return false
	}
	if err != nil {
		c.t.Fatal(err)
	}
	return true
}

// confirmedAndSent 는 지금 올라 있는 내용으로 완료하고 메시지에 붙여 보낸다. 내려받은 내용을 돌려준다.
func (c *ownerCase) confirmedAndSent(want string) string {
	c.t.Helper()
	if r := c.complete(); r.status != http.StatusCreated {
		c.t.Fatalf("새 주인의 완료: %d %s %v", r.status, r.code, r.err)
	}
	if code := c.h.sendWith(c.conv, c.cust, c.att); code != http.StatusCreated {
		c.t.Fatalf("전송: %d", code)
	}
	got := c.fetch()
	if got != want {
		c.t.Fatalf("새 주인이 확인한 내용: %q, 올린 것 %q", got, want)
	}
	return got
}

// eventually 는 cond 가 참이 될 때까지 잠깐씩 기다린다. 서버는 클라이언트가 끊은 뒤에도 요청을 마저 처리한다.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("5초 안에 %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestE2E_첨부완료임대의주인(t *testing.T) {
	for _, mode := range ownerModes {
		t.Run(mode, func(t *testing.T) {
			t.Run("임대가 끝난 옛 주인이 판정 뒤 늦게 깨어나 도장까지 가도 새 주인이 확인·전송한 첨부의 내용과 파일은 그대로다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{})
				g := newGate(t)
				c.files.on("stat-tmp#1", g.wait)
				old := c.completeAsync()
				<-g.entered // 옛 주인이 AAAAA 를 판정하고 멈췄다

				c.expireLease()
				c.put("BBBBB")
				sent := c.confirmedAndSent("BBBBB")
				_, path, _ := c.row()
				// 옛 주인이 판정한 것과 같은 내용을 다시 올려 둔다. 옛 주인은 옮기고 재확인을 지나 도장까지 간다.
				c.put("AAAAA")
				g.open()
				r := <-old
				if r.status != http.StatusCreated {
					t.Errorf("이미 확인된 첨부라 옛 주인도 그 첨부를 받는다: %d %s", r.status, r.code)
				}

				if got := c.fetch(); got != sent {
					t.Errorf("전송된 첨부의 내용이 바뀌었다: %q → %q", sent, got)
				}
				if _, after, _ := c.row(); after != path {
					t.Errorf("확인된 첨부의 확정 키가 바뀌었다: %s → %s", path, after)
				}
				if left := c.finalKeysExcept(path); len(left) != 0 {
					t.Errorf("옛 주인이 옮긴 파일이 남았다: %v", left)
				}
			})

			t.Run("임대가 끝난 옛 주인이 판정 뒤 늦게 깨어나 실패해도 새 주인이 확인·전송한 첨부의 내용과 파일은 그대로다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{})
				g := newGate(t)
				c.files.on("stat-tmp#1", g.wait)
				old := c.completeAsync()
				<-g.entered

				c.expireLease()
				c.put("BBBBB")
				sent := c.confirmedAndSent("BBBBB")
				// 다른 내용을 다시 올려 둔다. 옛 주인은 옮긴 뒤 재확인에서 거절된다.
				c.put("CCCCC")
				g.open()
				if r := <-old; r.status != http.StatusBadRequest {
					t.Errorf("판정한 것과 옮긴 것이 다르면 옛 주인은 400: %d %s", r.status, r.code)
				}
				if got := c.fetch(); got != sent {
					t.Errorf("전송된 첨부의 내용이 바뀌었다: %q → %q", sent, got)
				}
			})

			t.Run("옛 주인이 깨어날 때 새 주인이 도장 직전이어도 옛 주인의 도장은 걸리지 않고 새 주인이 확인한 내용을 내려받는다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{})
				oldGate, freshGate := newGate(t), newGate(t)
				c.files.on("stat-tmp#1", oldGate.wait)     // 옛 주인: 판정 뒤
				c.files.on("stat-final#1", freshGate.wait) // 새 주인: 확정 재확인 뒤, 도장 앞
				old := c.completeAsync()
				<-oldGate.entered

				c.expireLease()
				c.put("BBBBB")
				fresh := c.completeAsync()
				select {
				case <-freshGate.entered:
				case r := <-fresh:
					t.Fatalf("새 주인이 도장 앞까지 가지 못했다: %d %s", r.status, r.code)
				}
				c.put("AAAAA")
				oldGate.open()
				if r := <-old; r.status != http.StatusConflict || r.code != "busy" {
					t.Errorf("임대를 잃은 옛 주인은 409 busy: %d %s", r.status, r.code)
				}
				freshGate.open()
				if r := <-fresh; r.status != http.StatusCreated {
					t.Fatalf("새 주인의 완료: %d %s", r.status, r.code)
				}
				if got := c.fetch(); got != "BBBBB" {
					t.Errorf("새 주인이 BBBBB 를 확인하고 201 을 받았는데 내려받기는 %q", got)
				}
				_, path, _ := c.row()
				if left := c.finalKeysExcept(path); len(left) != 0 {
					t.Errorf("옛 주인이 옮긴 파일이 남았다: %v", left)
				}
			})

			t.Run("임대가 끝난 옛 주인의 정리 삭제가 늦게 닿아도 새 주인이 확인·전송한 파일을 지우지 않는다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{})
				removing := newGate(t)
				// 옛 주인: 옮긴 뒤 재확인이 실패하고, 자기 확정 파일 삭제가 저장소에서 늦어진다.
				c.files.on("stat-final#1", func(context.Context) error { return errors.New("저장소 오류(주입)") })
				c.files.on("remove-final#1", removing.wait)
				old := c.completeAsync()
				<-removing.entered

				c.expireLease()
				c.put("BBBBB")
				sent := c.confirmedAndSent("BBBBB")
				removing.open()
				if r := <-old; r.status == http.StatusCreated {
					t.Errorf("재확인이 실패한 옛 주인이 201 을 받았다")
				}
				if got := c.fetch(); got != sent {
					t.Errorf("확인·전송된 첨부를 내려받을 수 없다: %q", got)
				}
			})

			t.Run("옮기기 직전에 클라이언트가 끊어도 첨부는 확인되고 올린 파일이 남는다", func(t *testing.T) {
				// S3 의 옮기기는 복사 뒤 원본 삭제다. 끊긴 요청에 끌려가면 원본은 지워졌는데 확정 파일은 정리돼 업로드가 사라진다.
				hs := &hookStore{}
				h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
				c := newOwnerCase(t, h, mode, app.UploadLimits{})
				reqCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.files.on("promote-start#1", func(context.Context) error {
					cancel()
					hs.requestDone(t)
					return nil
				})
				if r := timedCall(reqCtx, http.MethodPost, h.srv.URL+"/api/attachments/"+c.att+"/complete", c.cust); r.err == nil {
					t.Fatalf("클라이언트가 끊었는데 응답을 받았다: %d", r.status)
				}
				eventually(t, "옮기는 중에 끊긴 완료가 첨부를 확인하지 않았다", func() bool { up, _, _ := c.row(); return up })
				if got := c.fetch(); got != "AAAAA" {
					t.Errorf("옮기는 중에 끊긴 완료의 파일: %q", got)
				}
			})

			t.Run("옮긴 뒤에 클라이언트가 끊어도 첨부는 확인되고 올린 파일이 남는다", func(t *testing.T) {
				hs := &hookStore{}
				h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
				c := newOwnerCase(t, h, mode, app.UploadLimits{})
				reqCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.files.on("promote#1", func(context.Context) error {
					cancel()
					hs.requestDone(t)
					return nil
				})
				r := timedCall(reqCtx, http.MethodPost, h.srv.URL+"/api/attachments/"+c.att+"/complete", c.cust)
				if r.err == nil {
					t.Fatalf("클라이언트가 끊었는데 응답을 받았다: %d", r.status)
				}
				eventually(t, "끊긴 완료가 첨부를 확인하지 않았다", func() bool { up, _, _ := c.row(); return up })
				if _, _, lease := c.row(); lease != nil {
					t.Errorf("확인된 첨부에 임대가 남았다: %v", lease)
				}
				if got := c.fetch(); got != "AAAAA" {
					t.Errorf("끊긴 완료가 옮긴 파일: %q", got)
				}
			})
		})
	}
}

// finalKeysExcept 는 이 첨부의 확정 자리 중 keep 이 아닌 키다. 로컬은 디렉터리를 읽고, S3 는 알 수 있는 키가 없어 비교하지 않는다.
func (c *ownerCase) finalKeysExcept(keep string) []string {
	c.t.Helper()
	if _, ok := c.inner.(*s3store.Store); ok {
		// S3 는 목록 조회를 어댑터에 두지 않았다. 내용 단언(내려받기)으로 대신한다.
		return nil
	}
	var left []string
	for _, name := range c.h.finalFiles(c.conv, c.att) {
		key := "ws/" + c.h.workspaceOf(c.conv) + "/" + c.conv + "/" + c.att + "/" + name
		if key != keep {
			left = append(left, key)
		}
	}
	return left
}

func TestE2E_첨부완료의취소와시한(t *testing.T) {
	t.Run("임대를 잡은 직후 클라이언트가 끊으면 임대를 비우고 다음 완료가 바로 된다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		reqCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.mu.Lock()
		hs.afterClaim = func() {
			cancel()
			hs.requestDone(t)
		}
		hs.mu.Unlock()
		if r := timedCall(reqCtx, http.MethodPost, h.srv.URL+"/api/attachments/"+c.att+"/complete", c.cust); r.err == nil {
			t.Fatalf("클라이언트가 끊었는데 응답을 받았다: %d", r.status)
		}
		eventually(t, "끊긴 완료의 임대가 비지 않았다", func() bool { _, _, lease := c.row(); return lease == nil })
		if up, _, _ := c.row(); up {
			t.Error("임대를 잡은 직후 끊긴 완료가 첨부를 확인했다")
		}
		if r := c.complete(); r.status != http.StatusCreated || r.elapsed > time.Second {
			t.Fatalf("다음 완료는 바로 201: %d %s %s", r.status, r.code, r.elapsed)
		}
		if got := c.fetch(); got != "AAAAA" {
			t.Errorf("내용: %q", got)
		}
	})

	t.Run("판정 중에 클라이언트가 끊으면 옮기지 않고 임대를 비운다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		reqCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.files.on("stat-tmp#1", func(ctx context.Context) error {
			cancel()
			select { // 판정은 요청 취소를 따른다. 서버가 끊음을 알아챌 때까지 기다린다.
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Error("판정 중의 ctx 가 클라이언트의 끊음을 따르지 않는다")
			}
			return nil
		})
		if r := timedCall(reqCtx, http.MethodPost, h.srv.URL+"/api/attachments/"+c.att+"/complete", c.cust); r.err == nil {
			t.Fatalf("클라이언트가 끊었는데 응답을 받았다: %d", r.status)
		}
		eventually(t, "끊긴 완료의 임대가 비지 않았다", func() bool { _, _, lease := c.row(); return lease == nil })
		if up, _, _ := c.row(); up {
			t.Error("판정 중에 끊긴 완료가 첨부를 확인했다")
		}
		if !c.exists("tmp/" + c.att) {
			t.Error("판정 중에 끊긴 완료가 임시 파일을 옮겼다")
		}
		if r := c.complete(); r.status != http.StatusCreated {
			t.Fatalf("다음 완료는 바로 201: %d %s", r.status, r.code)
		}
	})

	t.Run("저장소가 작업 시한을 넘기면 503 storage_timeout 이고 임대가 남지 않는다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		c := newOwnerCase(t, h, "local", app.UploadLimits{Lease: 3 * time.Second, Work: 300 * time.Millisecond})
		c.files.on("stat-final#1", func(ctx context.Context) error {
			<-ctx.Done() // 저장소가 시한 안에 답하지 않는다
			return ctx.Err()
		})
		r := c.complete()
		if r.status != http.StatusServiceUnavailable || r.code != "storage_timeout" {
			t.Fatalf("시한 초과는 503 storage_timeout: %d %s", r.status, r.code)
		}
		if up, _, lease := c.row(); up || lease != nil {
			t.Errorf("시한을 넘긴 완료가 확인됐거나 임대를 남겼다: uploaded=%v lease=%v", up, lease)
		}
		// 시한이 지나 정리 삭제도 저장소에 닿지 못했다. 행이 모르는 확정 파일이 남는 것은 받아들인 한계다(D28).
		// 다시 올리고 완료하면 새 임대의 자리로 확인된다.
		c.put("AAAAA")
		if r := c.complete(); r.status != http.StatusCreated {
			t.Fatalf("다시 완료: %d %s", r.status, r.code)
		}
		if got := c.fetch(); got != "AAAAA" {
			t.Errorf("내용: %q", got)
		}
	})

	t.Run("도장이 오류로 돌아왔는데 실제로는 커밋됐으면 확정 파일을 지우지 않고 확인된 첨부를 준다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.complete = func(stamp func() (core.Attachment, bool, error)) (core.Attachment, bool, error) {
			if _, _, err := stamp(); err != nil {
				t.Errorf("도장: %v", err)
			}
			return core.Attachment{}, false, errors.New("응답을 잃었다(주입)")
		}
		hs.mu.Unlock()
		if r := c.complete(); r.status != http.StatusCreated {
			t.Errorf("커밋된 도장은 201: %d %s", r.status, r.code)
		}
		if got := c.fetch(); got != "AAAAA" {
			t.Errorf("커밋된 도장의 확정 파일이 사라졌다: %q", got)
		}
	})

	t.Run("도장의 결과를 모르고 임대 비우기도 실패하면 확정 파일을 지우지 않는다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.complete = func(stamp func() (core.Attachment, bool, error)) (core.Attachment, bool, error) {
			if _, _, err := stamp(); err != nil {
				t.Errorf("도장: %v", err)
			}
			hs.mu.Lock()
			hs.releaseErr = errors.New("DB 오류(주입)")
			hs.mu.Unlock()
			return core.Attachment{}, false, errors.New("응답을 잃었다(주입)")
		}
		hs.mu.Unlock()
		c.complete()
		hs.mu.Lock()
		hs.releaseErr = nil
		hs.mu.Unlock()
		if got := c.fetch(); got != "AAAAA" {
			t.Errorf("결과를 모르는 채 확정 파일을 지웠다: %q", got)
		}
	})

	t.Run("도장의 결과를 모르고 행도 읽지 못하면 확정 파일을 지우지 않는다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.complete = func(stamp func() (core.Attachment, bool, error)) (core.Attachment, bool, error) {
			if _, _, err := stamp(); err != nil {
				t.Errorf("도장: %v", err)
			}
			hs.mu.Lock()
			hs.failRead = true
			hs.mu.Unlock()
			return core.Attachment{}, false, errors.New("응답을 잃었다(주입)")
		}
		hs.mu.Unlock()
		c.complete()
		if got := c.fetch(); got != "AAAAA" {
			t.Errorf("결과를 모르는 채 확정 파일을 지웠다: %q", got)
		}
	})

	t.Run("도장이 정말 실패하면 이 요청의 확정 파일을 지우고 임대를 비운다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.complete = func(func() (core.Attachment, bool, error)) (core.Attachment, bool, error) {
			return core.Attachment{}, false, errors.New("DB 오류(주입)")
		}
		hs.mu.Unlock()
		if r := c.complete(); r.status < 500 {
			t.Errorf("도장 실패는 서버 오류: %d %s", r.status, r.code)
		}
		if left := h.finalFiles(c.conv, c.att); len(left) != 0 {
			t.Errorf("찍히지 않은 도장의 확정 파일이 남았다: %v", left)
		}
		if up, _, lease := c.row(); up || lease != nil {
			t.Errorf("실패한 도장 뒤: uploaded=%v lease=%v", up, lease)
		}
	})
}

// 옮긴 뒤 실패한 완료. 임시 파일은 확정 자리로 갔고 행은 그 자리를 모른다. 같은 첨부의 완료를 다시 불러도 되지 않는다 —
// 다음 완료는 400 "파일을 찾지 못했다. 다시 올려라" 이고 사용자는 다시 올린다(D28 남는 한계).
func TestE2E_옮긴뒤실패한첨부(t *testing.T) {
	for _, mode := range ownerModes {
		t.Run(mode, func(t *testing.T) {
			t.Run("옮긴 뒤 시한을 넘겨 실패한 첨부는 다음 완료가 400 이고 다시 올리면 된다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{Lease: 3 * time.Second, Work: 300 * time.Millisecond})
				c.files.on("stat-final#1", func(ctx context.Context) error {
					<-ctx.Done() // 옮긴 뒤 재확인에서 저장소가 시한 안에 답하지 않는다
					return ctx.Err()
				})
				status, code, msg := c.completeMessage()
				if status != http.StatusServiceUnavailable || code != "storage_timeout" || msg != "파일 서버가 제때 응답하지 않아 첨부를 마치지 못했습니다. 파일을 다시 올려 주세요" {
					t.Fatalf("옮긴 뒤 시한 초과: %d %s %q", status, code, msg)
				}
				status, code, msg = c.completeMessage()
				if status != http.StatusBadRequest || code != "invalid" || msg != "파일을 찾지 못했습니다. 다시 올려 주세요" {
					t.Errorf("옮긴 뒤 실패한 첨부의 다음 완료: %d %s %q", status, code, msg)
				}
				if up, _, lease := c.row(); up || lease != nil {
					t.Errorf("실패한 첨부: uploaded=%v lease=%v", up, lease)
				}
				c.reapply()
				c.put("AAAAA")
				if r := c.complete(); r.status != http.StatusCreated {
					t.Fatalf("다시 신청하고 올린 첨부의 완료: %d %s", r.status, r.code)
				}
				if got := c.fetch(); got != "AAAAA" {
					t.Errorf("다시 올린 첨부의 내용: %q", got)
				}
			})

			t.Run("임대가 끝난 옛 주인이 임시 파일을 옮겨 가면 새 주인과 그 뒤의 완료는 400 이다", func(t *testing.T) {
				c := newOwnerCase(t, newHarness(t, time.Second, false), mode, app.UploadLimits{})
				oldGate, freshGate := newGate(t), newGate(t)
				c.files.on("promote-start#1", oldGate.wait)   // 옛 주인: 판정 뒤, 옮기기 앞
				c.files.on("promote-start#2", freshGate.wait) // 새 주인: 판정 뒤, 옮기기 앞
				old := c.completeAsync()
				<-oldGate.entered

				c.expireLease()
				fresh := c.completeAsync()
				select {
				case <-freshGate.entered:
				case r := <-fresh:
					t.Fatalf("새 주인이 옮기기 앞까지 가지 못했다: %d %s", r.status, r.code)
				}
				oldGate.open()
				if r := <-old; r.status == http.StatusCreated {
					t.Errorf("임대를 잃은 옛 주인이 201 을 받았다")
				}
				freshGate.open()
				if r := <-fresh; r.status != http.StatusBadRequest || r.code != "invalid" {
					t.Errorf("옮기려는 임시 파일이 없으면 400: %d %s", r.status, r.code)
				}
				status, code, msg := c.completeMessage()
				if status != http.StatusBadRequest || code != "invalid" || msg != "파일을 찾지 못했습니다. 다시 올려 주세요" {
					t.Errorf("임시 파일이 없는 첨부의 완료: %d %s %q", status, code, msg)
				}
			})
		})
	}
}

func TestE2E_첨부완료의정리(t *testing.T) {
	t.Run("도장의 결과를 모르고 임대를 비우지 못했는데 행이 이 요청의 확정 키를 가리키지 않으면 확정 파일을 지운다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.complete = func(func() (core.Attachment, bool, error)) (core.Attachment, bool, error) {
			// 도장은 걸리지 않았고, 그 사이 다른 요청이 임대를 이어받았다. 이 token 으로는 비울 수 없다.
			if _, err := c.h.store.Pool().Exec(context.Background(),
				"UPDATE attachments SET completing_token = 'other-owner' WHERE id = $1", c.att); err != nil {
				t.Error(err)
			}
			return core.Attachment{}, false, errors.New("응답을 잃었다(주입)")
		}
		hs.mu.Unlock()
		if r := c.complete(); r.status == http.StatusCreated {
			t.Errorf("도장이 걸리지 않은 완료가 201 을 받았다")
		}
		if up, _, _ := c.row(); up {
			t.Fatal("도장이 걸리지 않았는데 확인됐다")
		}
		if left := h.finalFiles(c.conv, c.att); len(left) != 0 {
			t.Errorf("아무도 가리키지 않는 확정 파일이 남았다: %v", left)
		}
	})

	t.Run("임대를 잡는 호출이 오류로 돌아와도 임대를 비우고 다음 완료가 바로 된다", func(t *testing.T) {
		hs := &hookStore{}
		h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { hs.Store = s; return hs })
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		hs.mu.Lock()
		hs.claimErr = errors.New("응답을 잃었다(주입)")
		hs.mu.Unlock()
		if r := c.complete(); r.status < 500 {
			t.Errorf("임대 잡기 오류는 서버 오류: %d %s", r.status, r.code)
		}
		if _, _, lease := c.row(); lease != nil {
			t.Errorf("커밋된 임대가 남았다: %v", lease)
		}
		if r := c.complete(); r.status != http.StatusCreated {
			t.Fatalf("다음 완료는 바로 201: %d %s", r.status, r.code)
		}
	})
}

func TestE2E_저장소오류로그(t *testing.T) {
	t.Run("완료가 저장소 전송 오류로 실패해도 로그에 서명된 주소가 없다", func(t *testing.T) {
		h, logs := newCapturedHarness(t, time.Second, false)
		c := newOwnerCase(t, h, "local", app.UploadLimits{})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close() // 닫힌 포트라 저장소 요청이 전송 단계에서 실패한다
		down, err := s3store.New(s3store.Config{Endpoint: "http://" + addr, Bucket: "chat-test", AccessKey: "chat", SecretKey: "chatchat123", PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		c.files.on("stat-tmp#1", func(ctx context.Context) error {
			_, err := down.Stat(ctx, "tmp/"+c.att)
			return err
		})
		if r := c.complete(); r.status != http.StatusInternalServerError {
			t.Fatalf("저장소 전송 오류는 500: %d %s", r.status, r.code)
		}
		all := logs.String()
		if !strings.Contains(all, "http.failed") {
			t.Fatalf("http.failed 로그가 없다:\n%s", all)
		}
		for _, f := range []string{"X-Amz-", "Signature", "/chat-test/"} {
			if strings.Contains(all, f) {
				t.Errorf("로그에 %q 가 있다:\n%s", f, all)
			}
		}
	})
}
