package filestore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
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

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 로컬 저장소의 허가 정책. 이 어댑터는 서명한 허가 말고는 아무것도 보지 않는다 — 세션도, DB 도.
// 그래서 허가를 고치거나 다른 용도로 쓰거나 기한을 넘기면 저장소가 직접 거절해야 한다.
// 서명을 만들어 보려면 비공개 sign 이 필요해 패키지 안에서 시험한다.

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// postForm 은 브라우저처럼 token 필드 뒤에 file 을 붙여 올린다.
func postForm(t *testing.T, s *Store, token string, data []byte) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("token", token)
	part, _ := mw.CreateFormFile("file", "upload.bin")
	_, _ = part.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/files/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func getFile(s *Store, link string) (int, string) {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
	return rec.Code, rec.Body.String()
}

func uploadToken(t *testing.T, s *Store, key string, limit int64) string {
	t.Helper()
	form, err := s.PresignUpload(key, "text/plain", limit, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return form.Fields["token"]
}

// files 는 저장 루트 아래의 모든 파일이다. 받다 만 임시 파일까지 센다.
func files(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(s.root, path)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// reforge 는 허가의 본문(암호문)은 그대로 두고 인증 태그만 바꾼다.
func reforge(token string) string {
	body, _, _ := strings.Cut(token, ".")
	return body + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
}

func TestFilestore_허가(t *testing.T) {
	s := newStore(t)
	data := []byte("영수증")

	t.Run("본문이 올바라도 서명이 틀리면 올리기·받기 모두 거절한다", func(t *testing.T) {
		token := uploadToken(t, s, "tmp/forged", int64(len(data)))
		if code := postForm(t, s, reforge(token), data); code != http.StatusForbidden {
			t.Errorf("서명이 틀린 올리기: %d", code)
		}
		if len(files(t, s)) != 0 {
			t.Errorf("거절한 올리기가 파일을 남겼다: %v", files(t, s))
		}
		// 받기 허가는 서버가 직접 만들어 본문이 유효한 것을 쓴다.
		valid := s.seal(ticket{Op: "get", Key: "tmp/forged", Exp: s.now().Add(time.Minute).Unix()})
		if code, _ := getFile(s, "/files/get?token="+reforge(valid)); code != http.StatusForbidden {
			t.Errorf("서명이 틀린 받기: %d", code)
		}
	})

	t.Run("기한이 지난 허가는 거절한다", func(t *testing.T) {
		token := uploadToken(t, s, "tmp/late", int64(len(data)))
		link, _ := s.PresignDownload("tmp/late", "late.txt", "text/plain", false, time.Minute)
		orig := s.now
		s.now = func() time.Time { return orig().Add(2 * time.Minute) }
		defer func() { s.now = orig }()
		if code := postForm(t, s, token, data); code != http.StatusForbidden {
			t.Errorf("만료된 올리기: %d", code)
		}
		if code, _ := getFile(s, link); code != http.StatusForbidden {
			t.Errorf("만료된 받기: %d", code)
		}
	})

	t.Run("올리기 허가로 받을 수 없고, 받기 허가로 올릴 수 없다", func(t *testing.T) {
		put := uploadToken(t, s, "tmp/swap", int64(len(data)))
		if code := postForm(t, s, put, data); code != http.StatusNoContent {
			t.Fatalf("올리기: %d", code)
		}
		if code, _ := getFile(s, "/files/get?token="+put); code != http.StatusForbidden {
			t.Errorf("올리기 허가로 받았다: %d", code)
		}
		link, _ := s.PresignDownload("tmp/swap", "swap.txt", "text/plain", false, time.Minute)
		get, _ := url.QueryUnescape(strings.TrimPrefix(link, "/files/get?token="))
		if code := postForm(t, s, get, []byte("덮어쓰기")); code != http.StatusForbidden {
			t.Errorf("받기 허가로 올렸다: %d", code)
		}
	})

	// D46: 서명만 하던 허가는 base64 를 풀면 파일 이름("신분증.png")과 저장소 키(워크스페이스·대화·첨부 id)가 그대로 보였다.
	// 주소는 <img src>·접근 로그·Referer 로 돈다. 봉인한 허가에서는 어느 쪽도 읽히지 않는다.
	t.Run("받기·올리기 주소의 허가를 풀어도 파일 이름과 저장소 키가 보이지 않는다", func(t *testing.T) {
		key := "ws/w-secret-ws/c-secret-conv/a-secret-att/lease"
		link, err := s.PresignDownload(key, "신분증.png", "image/png", true, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		form, err := s.PresignUpload("tmp/a-secret-att", "image/png", 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{strings.TrimPrefix(link, "/files/get?token="), form.Fields["token"]} {
			tok, _ := url.QueryUnescape(raw)
			var seen []byte
			for _, part := range strings.Split(tok, ".") {
				b, _ := base64.RawURLEncoding.DecodeString(part)
				seen = append(seen, b...)
			}
			for _, secret := range []string{"신분증", "secret-ws", "secret-conv", "secret-att", "image/png"} {
				if bytes.Contains(seen, []byte(secret)) || strings.Contains(tok, secret) {
					t.Errorf("허가에서 %q 가 읽힌다", secret)
				}
			}
		}
		// 봉인해도 받기는 그대로 된다.
		if err := os.MkdirAll(filepath.Join(s.root, filepath.Dir(key)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.root, key), []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _ := getFile(s, link); code != http.StatusOK {
			t.Errorf("봉인한 받기 허가로 받지 못했다: %d", code)
		}
	})

	t.Run("키가 저장 루트를 벗어나면 허가를 발급하지 않는다", func(t *testing.T) {
		for _, key := range []string{"../escape", "tmp/../../escape", "/etc/passwd", "..", `tmp\..\..\escape`} {
			if _, err := s.PresignUpload(key, "text/plain", 10, time.Minute); err == nil {
				t.Errorf("올리기 허가가 나왔다: %q", key)
			}
			if _, err := s.PresignDownload(key, "x", "text/plain", false, time.Minute); err == nil {
				t.Errorf("받기 허가가 나왔다: %q", key)
			}
		}
	})
}

func TestFilestore_올리기(t *testing.T) {
	t.Run("허가한 크기를 넘으면 413 이고 파일이 남지 않는다", func(t *testing.T) {
		s := newStore(t)
		token := uploadToken(t, s, "tmp/big", 4)
		if code := postForm(t, s, token, []byte("다섯 바이트를 넘는다")); code != http.StatusRequestEntityTooLarge {
			t.Errorf("한도를 넘는 올리기: %d", code)
		}
		if got := files(t, s); len(got) != 0 {
			t.Errorf("파일이 남았다: %v", got)
		}
	})

	t.Run("받다 끊긴 올리기는 임시 파일을 남기지 않는다", func(t *testing.T) {
		s := newStore(t)
		token := uploadToken(t, s, "tmp/cut", 1<<20)
		var head bytes.Buffer
		mw := multipart.NewWriter(&head)
		_ = mw.WriteField("token", token)
		part, _ := mw.CreateFormFile("file", "cut.bin")
		_, _ = part.Write(bytes.Repeat([]byte("x"), 4096))
		// 닫는 경계 없이 연결이 끊긴다.
		body := io.MultiReader(&head, &failingReader{})
		req := httptest.NewRequest(http.MethodPost, "/files/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("끊긴 올리기가 성공했다: %d", rec.Code)
		}
		if got := files(t, s); len(got) != 0 {
			t.Errorf("반쪽 파일이 남았다: %v", got)
		}
	})
}

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, errors.New("연결이 끊겼다") }

func TestFilestore_확정과임시정리(t *testing.T) {
	ctx := context.Background()

	t.Run("옮긴 뒤 같은 허가로 다시 올려도 확정 파일은 그대로다", func(t *testing.T) {
		s := newStore(t)
		token := uploadToken(t, s, "tmp/a1", 8)
		postForm(t, s, token, []byte("original"))
		if err := s.Promote(ctx, "tmp/a1", "ws/w/c/a1"); err != nil {
			t.Fatal(err)
		}
		if code := postForm(t, s, token, []byte("replaced")); code != http.StatusNoContent {
			t.Fatalf("임시 자리에 다시 올리기: %d", code)
		}
		link, _ := s.PresignDownload("ws/w/c/a1", "a1.txt", "text/plain", false, time.Minute)
		if code, got := getFile(s, link); code != http.StatusOK || got != "original" {
			t.Errorf("확정 파일: %d %q", code, got)
		}
	})

	t.Run("끝난 ctx 로는 Stat·Promote·Remove 가 일하지 않고 ctx 오류를 낸다", func(t *testing.T) {
		// 앱은 작업 시한을 임대보다 짧게 두고, 시한이 지난 요청이 그 뒤에 파일을 옮기거나 지우지 않는 데 기댄다.
		s := newStore(t)
		postForm(t, s, uploadToken(t, s, "tmp/c1", 8), []byte("original"))
		done, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Stat(done, "tmp/c1"); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat: %v", err)
		}
		if err := s.Promote(done, "tmp/c1", "ws/w/c/c1"); !errors.Is(err, context.Canceled) {
			t.Errorf("Promote: %v", err)
		}
		if err := s.Remove(done, "tmp/c1"); !errors.Is(err, context.Canceled) {
			t.Errorf("Remove: %v", err)
		}
		if _, err := s.Stat(ctx, "tmp/c1"); err != nil {
			t.Errorf("끝난 ctx 의 호출이 임시 파일을 옮기거나 지웠다: %v", err)
		}
		if _, err := s.Stat(ctx, "ws/w/c/c1"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("끝난 ctx 의 Promote 가 확정 파일을 만들었다: %v", err)
		}
	})

	t.Run("원본이 없으면 Promote 는 ErrNotFound 다", func(t *testing.T) {
		s := newStore(t)
		if err := s.Promote(ctx, "tmp/none", "ws/w/c/none"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})

	t.Run("확정 자리에 이미 파일이 있으면 Promote 는 덮어쓰지 않고 오류다", func(t *testing.T) {
		s := newStore(t)
		token := uploadToken(t, s, "tmp/a2", 8)
		postForm(t, s, token, []byte("original"))
		if err := s.Promote(ctx, "tmp/a2", "ws/w/c/a2"); err != nil {
			t.Fatal(err)
		}
		postForm(t, s, token, []byte("replaced"))
		if err := s.Promote(ctx, "tmp/a2", "ws/w/c/a2"); !errors.Is(err, ErrPromoteExists) {
			t.Errorf("ErrPromoteExists 여야 한다: %v", err)
		}
		link, _ := s.PresignDownload("ws/w/c/a2", "a2.txt", "text/plain", false, time.Minute)
		if code, got := getFile(s, link); code != http.StatusOK || got != "original" {
			t.Errorf("확정 파일이 바뀌었다: %d %q", code, got)
		}
	})

	t.Run("링크를 못 거는 길의 복사도 확정 자리를 덮어쓰지 않는다", func(t *testing.T) {
		s := newStore(t)
		postForm(t, s, uploadToken(t, s, "tmp/a3", 8), []byte("original"))
		postForm(t, s, uploadToken(t, s, "tmp/a4", 8), []byte("replaced"))
		src3, _ := s.resolve("tmp/a3")
		src4, _ := s.resolve("tmp/a4")
		dst, _ := s.resolve("ws/w/c/a3")
		_ = os.MkdirAll(filepath.Dir(dst), 0o750)
		if err := copyExclusive(src3, dst); err != nil {
			t.Fatal(err)
		}
		if err := copyExclusive(src4, dst); !errors.Is(err, ErrPromoteExists) {
			t.Errorf("ErrPromoteExists 여야 한다: %v", err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "original" {
			t.Errorf("확정 파일이 바뀌었다: %q", got)
		}
	})

	t.Run("확정 파일을 지우면 비게 된 상위 디렉터리도 지우고 저장 루트와 다른 파일이 있는 디렉터리는 남긴다", func(t *testing.T) {
		s := newStore(t)
		for _, id := range []string{"p1", "p2"} {
			postForm(t, s, uploadToken(t, s, "tmp/"+id, 8), []byte("original"))
			if err := s.Promote(ctx, "tmp/"+id, "ws/w/c/"+id+"/tok"); err != nil {
				t.Fatal(err)
			}
		}
		exists := func(rel string) bool {
			_, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel)))
			return err == nil
		}
		if err := s.Remove(ctx, "ws/w/c/p1/tok"); err != nil {
			t.Fatal(err)
		}
		if exists("ws/w/c/p1") {
			t.Error("비게 된 첨부 디렉터리가 남았다")
		}
		if !exists("ws/w/c/p2/tok") {
			t.Error("같은 대화의 다른 첨부가 사라졌다")
		}
		if err := s.Remove(ctx, "ws/w/c/p2/tok"); err != nil {
			t.Fatal(err)
		}
		if exists("ws") {
			t.Errorf("비게 된 상위 디렉터리가 남았다: %v", files(t, s))
		}
		if !exists("") || !exists("tmp") {
			t.Error("저장 루트나 임시 디렉터리를 지웠다")
		}
	})

	t.Run("SweepTemp 는 오래된 임시 파일만 지운다", func(t *testing.T) {
		s := newStore(t)
		write := func(key string, age time.Duration) {
			full := filepath.Join(s.root, filepath.FromSlash(key))
			_ = os.MkdirAll(filepath.Dir(full), 0o750)
			if err := os.WriteFile(full, []byte("x"), 0o640); err != nil {
				t.Fatal(err)
			}
			when := time.Now().Add(-age)
			if err := os.Chtimes(full, when, when); err != nil {
				t.Fatal(err)
			}
		}
		write("tmp/old", 2*time.Hour)
		write("tmp/fresh", time.Minute)
		write("ws/w/c/kept", 48*time.Hour) // 확정 파일은 아무리 오래돼도 건드리지 않는다
		n, err := s.SweepTemp(time.Hour)
		if err != nil || n != 1 {
			t.Fatalf("지운 수 %d, %v", n, err)
		}
		got := strings.Join(files(t, s), ",")
		if strings.Contains(got, "old") || !strings.Contains(got, "fresh") || !strings.Contains(got, "kept") {
			t.Errorf("남은 파일: %s", got)
		}
	})
}

func TestFilestore_내려받기헤더(t *testing.T) {
	s := newStore(t)
	postForm(t, s, uploadToken(t, s, "tmp/name", 2), []byte("ok"))

	t.Run("filename= 에는 제어 문자와 ASCII 밖의 글자를 넣지 않고 원래 이름은 filename*= 로 보낸다", func(t *testing.T) {
		link, _ := s.PresignDownload("tmp/name", "영수증\x7f\"사본.txt", "text/plain", false, time.Minute)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
		cd := rec.Header().Get("Content-Disposition")
		ascii, _, _ := strings.Cut(strings.TrimPrefix(cd, `attachment; filename="`), `"`)
		for _, r := range ascii {
			if r < 0x20 || r > 0x7e {
				t.Errorf("filename= 에 ASCII 밖의 글자: %q", cd)
				break
			}
		}
		if !strings.Contains(cd, "filename*=UTF-8''%EC%98%81%EC%88%98%EC%A6%9D") {
			t.Errorf("원래 이름이 filename*= 로 가지 않는다: %q", cd)
		}
	})
}

// 감사 20 A12b: GCM 은 같은 키로 nonce 를 되쓰면 두 암호문의 XOR 로 평문이 새고 인증 키까지 드러난다. 프로세스 키 하나로 허가를
// 수없이 봉인하므로 nonce 는 매번 무작위여야 한다. 같은 허가를 두 번 봉인하면 앞부분(nonce)부터 달라야 하고 둘 다 열려야 한다.
func TestFilestore_같은허가를두번봉인해도nonce가달라암호문이다르다(t *testing.T) {
	s := newStore(t)
	tk := ticket{Op: "get", Key: "ws/a/b/c", Name: "영수증.pdf", Exp: s.now().Add(time.Minute).Unix()}
	seen := map[string]bool{}
	nonces := map[string]bool{}
	size := s.aead.NonceSize()
	for range 50 {
		token := s.seal(tk)
		if seen[token] {
			t.Fatal("같은 허가가 같은 봉인으로 나왔다 — nonce 를 되쓴다")
		}
		seen[token] = true
		raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(token, ".", 2)[0])
		if err != nil || len(raw) < size {
			t.Fatalf("봉인 꼴: %v", err)
		}
		nonces[string(raw[:size])] = true
		if got, ok := s.open(token, "get"); !ok || got.Key != tk.Key {
			t.Fatalf("봉인한 허가가 열리지 않는다: %+v %v", got, ok)
		}
	}
	if len(nonces) != 50 {
		t.Errorf("50번 봉인에 nonce %d가지", len(nonces))
	}
}

// noter 는 httpapi 의 logWriter 처럼 NoteError 를 가진 ResponseWriter 다. 그 사이에 다른 감싸개(Unwrap)가 끼어 있어도 찾아야 한다.
type noter struct {
	http.ResponseWriter
	code, reason string
}

func (n *noter) NoteError(code, reason string) { n.code, n.reason = code, reason }

type passthrough struct{ http.ResponseWriter }

func (p passthrough) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// D47(감사 23 N9): 디스크에 쓰지 못하면 500 이고 원인 줄 files.save_failed(ERROR, err_kind)를 남기며, 감싼 쪽(http.request)에도
// code=internal reason=<err_kind> 를 적는다. 전에는 http.request 에 reason 이 없어 500 의 원인을 그 한 줄로 몰랐다.
// 원인 줄과 http.request 가 같은 실패라 알람은 http.request 로만 센다(OtherErrorCount 에서 files.save_failed 를 뺀다, deploy.md 15.3).
func TestFilestore_디스크에쓰지못하면500이고files_save_failed와http_request의reason에err_kind를남긴다(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 는 권한을 무시한다")
	}
	s := newStore(t)
	token := uploadToken(t, s, "tmp/denied/a", 1<<20)
	// 저장 자리의 부모를 읽기 전용으로 만든다. 임시 파일을 만들지 못한다(permission).
	denied := filepath.Join(s.Root(), "tmp", "denied")
	if err := os.MkdirAll(denied, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o750) })

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("token", token)
	part, _ := mw.CreateFormFile("file", "secret-name.bin")
	_, _ = part.Write([]byte("내용"))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/files/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	n := &noter{ResponseWriter: rec}
	s.Handler().ServeHTTP(passthrough{n}, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("저장 실패가 %d 다. 500 이어야 한다", rec.Code)
	}
	if n.code != "internal" || n.reason != "permission" {
		t.Errorf("감싼 쪽에 적은 것이 code=%q reason=%q 다. internal·permission 이어야 한다", n.code, n.reason)
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"files.save_failed"`) || !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, `"err_kind":"permission"`) {
		t.Errorf("files.save_failed ERROR err_kind=permission 한 줄이 없다:\n%s", out)
	}
	if strings.Contains(out, "secret-name") || strings.Contains(out, "denied") {
		t.Errorf("파일 이름이나 저장 경로가 로그에 실렸다:\n%s", out)
	}
}
