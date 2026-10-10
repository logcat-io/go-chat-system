// Package filestore 는 첨부를 로컬 디스크에 두는 어댑터다. 개발과 서버 한 대짜리 설치에 쓴다.
//
// 객체 저장소와 같은 계약(app.FileStore)을 채운다: 서명한 주소를 발급하고, 브라우저가 그 주소로 직접 올리고 받는다.
// 그래서 이 어댑터는 자기 주소(/files/…)를 받는 HTTP 핸들러를 같이 가진다. 허가는 프로세스가 뜰 때 만든 키로 봉인한다(AES-256-GCM).
// 봉인이라 주소를 가진 사람도 허가 안의 파일 이름·저장소 키를 읽지 못한다(D46) — 서명만 하던 때는 base64 를 풀면 보였다.
// 서버를 두 대로 늘리면 이 어댑터가 아니라 S3 어댑터를 쓴다.
package filestore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

type Store struct {
	root string
	aead cipher.AEAD
	now  func() time.Time
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{root: abs, aead: aead, now: time.Now}, nil
}

func (s *Store) Root() string { return s.root }

// ticket 은 주소에 실리는 허가다. 무엇을(Op·Key) 언제까지(Exp) 어떤 조건으로 할 수 있는지 담고 봉인한다.
type ticket struct {
	Op     string `json:"op"`
	Key    string `json:"key"`
	Type   string `json:"type,omitempty"`
	Name   string `json:"name,omitempty"`
	Max    int64  `json:"max,omitempty"`
	Inline bool   `json:"inline,omitempty"`
	Exp    int64  `json:"exp"`
}

// seal 은 허가를 봉인한다. 모양은 base64url(nonce‖암호문) + "." + base64url(인증 태그)다. 태그가 맞지 않으면 열리지 않으니 서명을 겸한다.
// 파일 이름과 저장소 키는 암호문 안에 있어 주소·접근 로그·Referer 로 새지 않는다(D46).
func (s *Store) seal(t ticket) string {
	body, _ := json.Marshal(t)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("filestore: crypto/rand 실패: " + err.Error())
	}
	sealed := s.aead.Seal(nil, nonce, body, nil)
	cut := len(sealed) - s.aead.Overhead()
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed[:cut]...)) + "." + base64.RawURLEncoding.EncodeToString(sealed[cut:])
}

// open 은 봉인을 열고 종류(op)와 기한을 본다. 고친 허가·다른 프로세스의 허가·기한이 지난 허가는 false 다.
func (s *Store) open(token, op string) (ticket, bool) {
	var t ticket
	body64, tag64, ok := strings.Cut(token, ".")
	if !ok {
		return t, false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(body64)
	tag, err2 := base64.RawURLEncoding.DecodeString(tag64)
	ns := s.aead.NonceSize()
	if err1 != nil || err2 != nil || len(body) < ns || len(tag) != s.aead.Overhead() {
		return t, false
	}
	plain, err := s.aead.Open(nil, body[:ns], append(body[ns:len(body):len(body)], tag...), nil)
	if err != nil || json.Unmarshal(plain, &t) != nil {
		return t, false
	}
	return t, t.Op == op && s.now().Unix() <= t.Exp
}

// PresignUpload 는 /files/upload 로 보내는 POST 양식을 만든다. 허가에 신청한 크기가 들어가고 핸들러가 그보다 큰 본문을 끊는다.
func (s *Store) PresignUpload(key, contentType string, size int64, ttl time.Duration) (core.UploadForm, error) {
	if _, err := s.resolve(key); err != nil {
		return core.UploadForm{}, err
	}
	exp := s.now().Add(ttl)
	return core.UploadForm{
		Method:    core.UploadPost,
		URL:       "/files/upload",
		Fields:    map[string]string{"token": s.seal(ticket{Op: "put", Key: key, Type: contentType, Max: size, Exp: exp.Unix()})},
		ExpiresAt: exp,
	}, nil
}

func (s *Store) PresignDownload(key, filename, contentType string, inline bool, ttl time.Duration) (string, error) {
	if _, err := s.resolve(key); err != nil {
		return "", err
	}
	t := ticket{Op: "get", Key: key, Name: filename, Type: contentType, Inline: inline, Exp: s.now().Add(ttl).Unix()}
	return "/files/get?token=" + url.QueryEscape(s.seal(t)), nil
}

// Stat·Promote·Remove 는 ctx 가 이미 끝났으면 일하지 않는다. 디스크 연산은 도중에 끊을 수 없지만, 시한이 지난 요청이
// 그 뒤에 파일을 옮기거나 지우기 시작하지는 않게 한다. 앱은 작업 시한을 임대보다 짧게 두고 이것에 기댄다.
func (s *Store) Stat(ctx context.Context, key string) (core.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return core.ObjectInfo{}, err
	}
	full, err := s.resolve(key)
	if err != nil {
		return core.ObjectInfo{}, err
	}
	f, err := os.Open(full) //nolint:gosec // G304: full 은 resolve 가 저장 루트 안으로 묶은 경로다.
	if errors.Is(err, os.ErrNotExist) {
		return core.ObjectInfo{}, core.ErrNotFound
	}
	if err != nil {
		return core.ObjectInfo{}, err
	}
	defer func() { _ = f.Close() }() // 읽기만 한 파일이다. 닫기 실패로 할 일이 없다.
	st, err := f.Stat()
	if err != nil {
		return core.ObjectInfo{}, err
	}
	head := make([]byte, core.ObjectHeadBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return core.ObjectInfo{}, err
	}
	return core.ObjectInfo{Size: st.Size(), Head: head[:n]}, nil
}

// ErrPromoteExists 는 확정 위치에 이미 파일이 있어 옮기지 않았다는 뜻이다. 앱이 저장소 종류를 모르고도 가르도록 core 의 값을 쓴다.
var ErrPromoteExists = core.ErrObjectExists

// Promote 는 임시 파일을 확정 위치로 옮긴다. 확정 위치에 이미 파일이 있으면 덮어쓰지 않고 ErrPromoteExists 다.
// 확인을 마친 파일은 바뀌면 안 된다. 이름 바꾸기(rename)는 있던 파일을 조용히 덮으므로 쓰지 않는다.
//
// 하드 링크를 만든 뒤 원본 이름을 지운다. 같은 디스크 안이라 바이트를 다시 쓰지 않고, 링크는 대상이 있으면 실패한다.
// 링크를 못 거는 파일시스템이면 대상을 O_EXCL 로 만들어 복사한다.
func (s *Store) Promote(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := s.resolve(from)
	if err != nil {
		return err
	}
	dst, err := s.resolve(to)
	if err != nil {
		return err
	}
	if err := s.place(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// place 는 src 를 dst 에 링크(안 되면 복사)한다. 만든 상위 디렉터리를 다른 요청의 Remove 가 빈 디렉터리로 보고 지울 수 있다.
// 그러면 링크가 "없다" 로 실패하는데, 원본이 그대로면 디렉터리를 다시 만들고 다시 건다. 원본이 없을 때만 ErrNotFound 다.
func (s *Store) place(src, dst string) error {
	const attempts = 3
	for i := 0; ; i++ {
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			return core.ErrNotFound
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		err := os.Link(src, dst)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, os.ErrExist):
			return ErrPromoteExists
		case errors.Is(err, os.ErrNotExist):
			if i+1 < attempts {
				continue
			}
			return core.ErrNotFound
		}
		err = copyExclusive(src, dst)
		if errors.Is(err, os.ErrNotExist) && i+1 < attempts {
			continue
		}
		return err
	}
}

// copyExclusive 는 링크를 못 걸 때의 길이다. 대상이 있으면 만들지 않는다. 복사가 끊기면 반쪽 대상을 지운다.
func copyExclusive(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: src·dst 는 place 를 부르는 쪽이 resolve 로 저장 루트 안에 묶었다.
	if errors.Is(err, os.ErrNotExist) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	// 읽기 쪽이다. 쓰기 쪽(out)의 닫기는 아래에서 본다.
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // G304: 위와 같다.
	if errors.Is(err, os.ErrExist) {
		return ErrPromoteExists
	}
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// tempPrefix 는 올리기 양식이 쓰는 임시 위치다. app 이 정하는 키(tmp/<첨부 id>)와 같은 접두사다.
const tempPrefix = "tmp"

// SweepTemp 는 olderThan 보다 오래된 임시 파일을 지우고 지운 수를 돌려준다.
// 확인한 뒤 같은 양식으로 다시 올린 파일은 행이 없어 고아 정리에 걸리지 않는다. 그런 것을 여기서 치운다.
// S3 는 버킷 수명 주기 규칙(tmp/ 접두사 1일)이 같은 일을 한다.
func (s *Store) SweepTemp(olderThan time.Duration) (int, error) {
	dir := filepath.Join(s.root, tempPrefix)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-olderThan)
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Store) Remove(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.HasPrefix(key, finalPrefix+"/") {
		s.pruneEmpty(filepath.Dir(full))
	}
	return nil
}

// finalPrefix 는 확정 파일의 접두사다. app 이 정하는 키(ws/<워크스페이스>/<대화>/<첨부 id>/<token>)와 같다.
const finalPrefix = "ws"

// pruneEmpty 는 dir 에서 위로 올라가며 빈 디렉터리를 지운다. 저장 루트는 지우지 않는다.
// 확정 키마다 첨부 디렉터리가 생기므로 파일만 지우면 빈 디렉터리가 쌓인다. 비지 않은 디렉터리에서 멈춘다(os.Remove 가 실패한다).
// 임시 자리(tmp/)는 올리기가 MkdirAll 뒤 그 안에 파일을 만들기 때문에 건드리지 않는다.
func (s *Store) pruneEmpty(dir string) {
	for {
		rel, err := filepath.Rel(s.root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// resolve 는 키를 디스크 경로로 바꾼다. 저장 루트를 벗어나면 거절한다.
// 절대 경로도 거절한다. Join 이 루트 아래로 붙여 주기는 하지만, 키는 언제나 루트 기준 상대 경로라 그런 키는 잘못 만든 것이다.
func (s *Store) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("filestore: 저장소 키가 비어 있다")
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || filepath.IsAbs(key) {
		return "", errors.New("filestore: 저장소 키가 절대 경로라 받지 않았다")
	}
	full := filepath.Join(s.root, filepath.FromSlash(key))
	if rel, err := filepath.Rel(s.root, full); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", errors.New("filestore: 저장소 키가 저장 루트 밖을 가리켜 받지 않았다")
	}
	return full, nil
}

// Handler 는 발급한 주소를 받는다. 권한은 서명한 허가가 전부다 — 세션 토큰을 보지 않는다(객체 저장소도 그렇다).
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files/upload", s.upload)
	mux.HandleFunc("GET /files/get", s.download)
	return mux
}

// upload 는 양식의 token 을 확인하고 file 을 저장한다. 허가의 한도를 넘으면 받지 않는다.
// file 은 마지막 필드여야 한다. S3 의 양식 업로드와 같은 규칙이다.
func (s *Store) upload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid", "bad_form", "파일 올리기 요청의 형식이 올바르지 않습니다(multipart 양식이어야 합니다)")
		return
	}
	var t ticket
	allowed := false
	for {
		part, err := reader.NextPart()
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid", "no_file", "올린 파일이 없습니다(file 필드가 있어야 합니다)")
			return
		}
		switch part.FormName() {
		case "token":
			raw, _ := io.ReadAll(io.LimitReader(part, 8<<10))
			t, allowed = s.open(string(raw), "put")
		case "file":
			if !allowed {
				fail(w, http.StatusForbidden, "forbidden", "bad_file_token", "파일 주소가 만료되었거나 올바르지 않습니다. 화면에서 파일을 다시 열거나 다시 올려 주세요")
				return
			}
			switch err := s.save(t, part); {
			case errors.Is(err, errTooLarge):
				fail(w, http.StatusRequestEntityTooLarge, "too_large", "", "파일이 처음 알려 준 크기보다 큽니다. 파일을 다시 올려 주세요")
			case errors.Is(err, errEmpty):
				fail(w, http.StatusBadRequest, "invalid", "empty_file", "빈 파일은 올릴 수 없습니다")
			case err != nil:
				// 원인 줄이다. 키·경로는 싣지 않는다(D30) — 디스크가 찼는지 권한인지는 err_kind 와 서버의 디스크 상태로 본다.
				// 올리는 사람이 끊은 것(canceled)은 서버 잘못이 아니라 WARN 이다.
				level, kind := slog.LevelError, errKind(err)
				if kind == "canceled" {
					level = slog.LevelWarn
				}
				slog.Log(r.Context(), level, "files.save_failed", "err_kind", kind)
				// http.request(500)의 reason 도 이 종류다. 알람은 http.request 로 센다(files.save_failed 는 원인 줄, deploy.md 15.3).
				fail(w, http.StatusInternalServerError, "internal", kind, "서버 오류로 파일을 저장하지 못했습니다. 잠시 후 다시 올려 주세요")
			default:
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		_ = part.Close()
	}
}

// fail 은 평문 오류 응답을 쓰고, 감싼 쪽(httpapi 의 http.request)이 있으면 오류 code 와 고정 사유를 적는다(D47, 감사 23).
// 전에는 /files/* 의 실패가 http.request 에 code·reason 없이 상태만 남아, 500 이 디스크가 찬 것인지 권한인지 그 한 줄로 몰랐다.
// 감싼 쪽은 NoteError 를 가진 ResponseWriter 로 찾는다(Unwrap 사슬). 없으면(이 저장소만 띄운 테스트) 응답만 쓴다.
func fail(w http.ResponseWriter, status int, code, reason, msg string) {
	for cur := w; cur != nil; {
		if n, ok := cur.(interface{ NoteError(code, reason string) }); ok {
			n.NoteError(code, reason)
			break
		}
		u, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	http.Error(w, msg, status)
}

var (
	errTooLarge = errors.New("filestore: 올라온 파일이 허가한 크기보다 크다")
	errEmpty    = errors.New("filestore: 올라온 파일이 비어 있다")
)

// save 는 임시 파일에 받아 놓고 다 받은 뒤에 제자리로 옮긴다. 받다 끊기면 반쪽 파일이 남지 않는다.
func (s *Store) save(t ticket, r io.Reader) error {
	full, err := s.resolve(t.Key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return err
	}
	// 이름을 옮긴 뒤에는 지울 것이 없어 실패한다. 실패한 길에서만 뜻이 있으니 결과를 보지 않는다.
	defer func() { _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(r, t.Max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > t.Max:
		return errTooLarge
	case n == 0:
		return errEmpty
	}
	return os.Rename(tmp.Name(), full)
}

func (s *Store) download(w http.ResponseWriter, r *http.Request) {
	t, ok := s.open(r.URL.Query().Get("token"), "get")
	if !ok {
		fail(w, http.StatusForbidden, "forbidden", "bad_file_token", "파일 주소가 만료되었거나 올바르지 않습니다. 화면에서 파일을 다시 열거나 다시 올려 주세요")
		return
	}
	full, err := s.resolve(t.Key)
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "file_missing", "파일을 찾을 수 없습니다. 이미 지워졌을 수 있습니다")
		return
	}
	f, err := os.Open(full) //nolint:gosec // G304: full 은 resolve 가 저장 루트 안으로 묶은 경로다.
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "file_missing", "파일을 찾을 수 없습니다. 이미 지워졌을 수 있습니다")
		return
	}
	defer func() { _ = f.Close() }() // 읽기만 한 파일이다.
	st, err := f.Stat()
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", "file_missing", "파일을 찾을 수 없습니다. 이미 지워졌을 수 있습니다")
		return
	}
	// 브라우저가 내용을 추측해 실행하지 않도록 nosniff 와 CSP 를 같이 건다. 우리 출처에서 내려가는 파일이라 필요하다.
	// img-src·media-src 는 주소를 새 탭에서 바로 열 때 브라우저가 그 그림·영상을 자기 문서 안에 띄우게 하는 만큼만 연다.
	// 탐색(seek)은 Range 요청이다. ServeContent 가 206 으로 답한다(D37).
	name := core.SafeFilename(t.Name)
	w.Header().Set("Content-Type", t.Type)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Disposition", core.ContentDisposition(t.Inline, t.Name))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// errKind 는 디스크 쓰기 실패의 종류다(로그의 err_kind). 원문에는 저장 경로가 들어 있어 싣지 않는다(D30).
// 이 어댑터는 logctx 를 모른다(archtest) — 디스크에서 나는 종류만 가른다.
func errKind(err error) string {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "no_space"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, context.Canceled), errors.Is(err, io.ErrUnexpectedEOF):
		// 올리는 사람이 도중에 끊었다.
		return "canceled"
	}
	return "error"
}
