package s3store_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 실제 저장소(MinIO)에 대고 확인한다. 서명은 저장소가 받아 줘야 맞는 것이라 흉내로는 검증이 안 된다.
// docker compose 의 minio 가 떠 있어야 한다. 없으면 건너뛴다.
func open(t *testing.T) *s3store.Store {
	t.Helper()
	endpoint := os.Getenv("CHAT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:59100"
	}
	store, err := s3store.New(s3store.Config{
		Endpoint: endpoint, Bucket: "chat-test", AccessKey: "chat", SecretKey: "chatchat123", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.EnsureBucket(ctx); err != nil {
		t.Skipf("MinIO 에 닿지 않는다 (%v). `docker compose up -d minio` 뒤에 다시 돌린다", err)
	}
	return store
}

// post 는 브라우저가 하듯이 양식 필드를 싣고 마지막에 파일을 붙여 올린다.
func post(t *testing.T, form core.UploadForm, mutate func(map[string]string), data []byte) int {
	t.Helper()
	fields := map[string]string{}
	for k, v := range form.Fields {
		fields[k] = v
	}
	if mutate != nil {
		mutate(fields)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	part, _ := mw.CreateFormFile("file", "upload.bin")
	_, _ = part.Write(data)
	_ = mw.Close()
	res, err := http.Post(form.URL, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestS3_올리고확인하고받고지운다(t *testing.T) {
	store := open(t)
	ctx := context.Background()
	key := "ws/test/" + time.Now().Format("150405.000000000") + "/영수증 사본.txt"
	data := []byte("환불 영수증 내용")

	form, err := store.PresignUpload(key, "text/plain", int64(len(data)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("올리기 전에는 없는 파일이다", func(t *testing.T) {
		if _, err := store.Stat(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})

	t.Run("신청한 크기를 넘는 파일은 저장소가 거절한다", func(t *testing.T) {
		if code := post(t, form, nil, append(data, []byte(" 그리고 더")...)); code < 400 {
			t.Errorf("한도를 넘는 업로드가 받아졌다: %d", code)
		}
	})

	t.Run("양식의 키나 종류를 바꾸면 저장소가 거절한다", func(t *testing.T) {
		if code := post(t, form, func(f map[string]string) { f["key"] = "ws/other/훔친자리.txt" }, data); code < 400 {
			t.Errorf("다른 키로 올라갔다: %d", code)
		}
		if code := post(t, form, func(f map[string]string) { f["Content-Type"] = "text/html" }, data); code < 400 {
			t.Errorf("다른 종류로 올라갔다: %d", code)
		}
		if _, err := store.Stat(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("거절된 업로드가 남았다: %v", err)
		}
	})

	t.Run("발급한 양식 그대로 올리면 받아지고, 크기와 앞부분이 확인된다", func(t *testing.T) {
		if code := post(t, form, nil, data); code >= 300 {
			t.Fatalf("업로드: %d", code)
		}
		info, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(len(data)) || string(info.Head) != string(data) {
			t.Errorf("크기 %d, 앞부분 %q", info.Size, info.Head)
		}
	})

	t.Run("내려받기 주소는 서버가 정한 종류와 이름으로 내려준다", func(t *testing.T) {
		link, err := store.PresignDownload(key, "영수증 사본.txt", "text/plain; charset=utf-8", false, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(got) != string(data) {
			t.Fatalf("내용: %d %q", res.StatusCode, got)
		}
		if ct := res.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("종류: %q", ct)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "filename*=UTF-8''") {
			t.Errorf("내려받기 헤더: %q", cd)
		}
	})

	t.Run("서명을 고친 주소는 받지 못한다", func(t *testing.T) {
		link, _ := store.PresignDownload(key, "a.txt", "text/plain", false, time.Minute)
		res, err := http.Get(strings.Replace(link, "X-Amz-Expires=60", "X-Amz-Expires=6000", 1))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("고친 주소: %d", res.StatusCode)
		}
	})

	t.Run("큰 파일은 앞부분만 읽고 전체 크기를 안다", func(t *testing.T) {
		big := bytes.Repeat([]byte("0123456789abcdef"), 8<<10) // 128KB
		bigKey := key + ".big"
		f, _ := store.PresignUpload(bigKey, "application/octet-stream", int64(len(big)), time.Minute)
		if code := post(t, f, nil, big); code >= 300 {
			t.Fatalf("업로드: %d", code)
		}
		info, err := store.Stat(ctx, bigKey)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(len(big)) || len(info.Head) != core.ObjectHeadBytes {
			t.Errorf("크기 %d (원래 %d), 앞부분 %d바이트", info.Size, len(big), len(info.Head))
		}
		_ = store.Remove(ctx, bigKey)
	})

	t.Run("지우면 없는 파일이고, 없는 파일을 지워도 오류가 아니다", func(t *testing.T) {
		if err := store.Remove(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Stat(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
		if err := store.Remove(ctx, key); err != nil {
			t.Errorf("두 번째 삭제: %v", err)
		}
	})
}

func TestS3_확인한파일은임시자리에서확정자리로옮긴다(t *testing.T) {
	store := open(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	tmp, final := "tmp/"+stamp, "ws/test/promote/"+stamp
	original, forged := []byte("refund receipt: 10,000 won"), []byte("refund receipt: 99,999 won")
	t.Cleanup(func() { _ = store.Remove(ctx, final); _ = store.Remove(ctx, tmp) })

	form, err := store.PresignUpload(tmp, "text/plain", int64(len(original)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, form, nil, original); code >= 300 {
		t.Fatalf("업로드: %d", code)
	}

	t.Run("옮기면 확정 자리에 같은 내용이 있고 임시 자리는 비어 있다", func(t *testing.T) {
		if err := store.Promote(ctx, tmp, final); err != nil {
			t.Fatal(err)
		}
		info, err := store.Stat(ctx, final)
		if err != nil || string(info.Head) != string(original) {
			t.Fatalf("확정 자리: %q %v", info.Head, err)
		}
		if _, err := store.Stat(ctx, tmp); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("임시 자리가 남았다: %v", err)
		}
	})

	t.Run("옮긴 뒤 같은 양식으로 다른 내용을 올려도 확정 파일은 그대로다", func(t *testing.T) {
		if code := post(t, form, nil, forged); code >= 300 {
			t.Fatalf("다시 올리기: %d", code)
		}
		link, _ := store.PresignDownload(final, "receipt.txt", "text/plain", false, time.Minute)
		res, err := http.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if string(got) != string(original) {
			t.Errorf("확정 파일이 바뀌었다: %q", got)
		}
	})

	t.Run("원본이 없으면 ErrNotFound 다", func(t *testing.T) {
		if err := store.Promote(ctx, "tmp/없는-"+stamp, final+".none"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("ErrNotFound 여야 한다: %v", err)
		}
	})

	t.Run("내려받기 헤더의 filename= 에는 ASCII 만 들어가고 원래 이름은 filename*= 로 간다", func(t *testing.T) {
		link, _ := store.PresignDownload(final, "영수증\x7f\"사본.txt", "text/plain", false, time.Minute)
		res, err := http.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		cd := res.Header.Get("Content-Disposition")
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

// 저장소 오류 문자열은 로그·스팬으로 옮겨 갈 수 있다(전에는 "요청 처리 실패"(지금 http.failed) 로그의 err 로 찍혔다). 서명된 주소(자격 증명 id·서명·버킷·키)가 실리면 남는다.
// MinIO 없이 돈다 — 닿지 않는 주소와 거절만 하는 가짜 저장소를 쓴다.
func TestS3_오류에는서명된주소가없다(t *testing.T) {
	const bucket, access = "leak-bucket", "LEAKACCESSKEY"
	key, final := "tmp/leak-attachment", "ws/leak-ws/leak-conv/leak-att/leak-token"
	forbidden := []string{"X-Amz-", "Signature", "/" + bucket + "/", access, "leak-attachment", "leak-token"}
	clean := func(t *testing.T, what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: 오류가 나야 한다", what)
		}
		for _, f := range forbidden {
			if strings.Contains(err.Error(), f) {
				t.Errorf("%s 오류에 %q 가 있다: %s", what, f, err)
			}
		}
	}
	newStore := func(t *testing.T, endpoint string) *s3store.Store {
		t.Helper()
		store, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: bucket, AccessKey: access, SecretKey: "secret", PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("닿지 않는 저장소에 대한 Stat·Remove·Promote 의 오류에는 서명된 주소가 없다", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close() // 닫힌 포트라 연결이 바로 거절된다
		store := newStore(t, "http://"+addr)
		ctx := context.Background()
		_, err = store.Stat(ctx, key)
		clean(t, "Stat", err)
		clean(t, "Remove", store.Remove(ctx, final))
		clean(t, "Promote", store.Promote(ctx, key, final))
	})

	t.Run("저장소가 거절한 응답의 오류에는 오류 본문의 버킷·키가 없다", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Key>`+final+`</Key><BucketName>`+bucket+
				`</BucketName><Resource>/`+bucket+`/`+final+`</Resource></Error>`)
		}))
		defer srv.Close()
		store := newStore(t, srv.URL)
		ctx := context.Background()
		_, err := store.Stat(ctx, key)
		clean(t, "Stat", err)
		if err != nil && !strings.Contains(err.Error(), "AccessDenied") {
			t.Errorf("오류 코드는 남아야 한다: %s", err)
		}
		clean(t, "Remove", store.Remove(ctx, final))
		clean(t, "Promote", store.Promote(ctx, key, final))
	})

	t.Run("주소를 뺀 오류도 시한 초과를 가를 수 있다", func(t *testing.T) {
		store := newStore(t, "http://127.0.0.1:1")
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := store.Stat(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("시한 초과가 감싸져 있지 않다: %v", err)
		}
	})
}

// 영상 첨부(D37)의 저장소 쪽. 한도 100MB 를 양식이 그대로 받는지, 영상 주소가 화면에서 재생되도록 inline·판정한 종류로
// 내려가고 탐색(Range)에 206 으로 답하는지 MinIO 로 본다. inline 으로 줄지는 앱이 정한다(로컬 모드 테스트가 본다).
func TestS3_영상(t *testing.T) {
	store := open(t)
	ctx := context.Background()
	base := "tmp/video-" + time.Now().Format("150405.000000000")

	t.Run("100MB 양식은 100MB 파일을 받고 그보다 1바이트 큰 파일은 거절한다", func(t *testing.T) {
		const size = 100 << 20
		key := base + "-100mb"
		form, err := store.PresignUpload(key, "video/mp4", size, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if code := postStream(t, form, size+1); code < 400 {
			t.Fatalf("한도를 넘는 업로드가 받아졌다: %d", code)
		}
		if code := postStream(t, form, size); code >= 300 {
			t.Fatalf("100MB 업로드: %d", code)
		}
		info, err := store.Stat(ctx, key)
		if err != nil || info.Size != size {
			t.Errorf("크기 %d, %v", info.Size, err)
		}
		_ = store.Remove(ctx, key)
	})

	t.Run("S3 의 영상 주소는 inline 과 판정한 종류로 내려가고 Range 요청에 206 과 그 구간만 준다", func(t *testing.T) {
		key := base + "-mov"
		data := make([]byte, 64<<10)
		copy(data, "\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  ")
		for i := 20; i < len(data); i++ {
			data[i] = byte(i * 7)
		}
		form, _ := store.PresignUpload(key, "application/octet-stream", int64(len(data)), time.Minute)
		if code := post(t, form, nil, data); code >= 300 {
			t.Fatalf("업로드: %d", code)
		}
		link, err := store.PresignDownload(key, "상담요청.mov", "video/quicktime", true, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, link, nil)
		req.Header.Set("Range", "bytes=1000-1999")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusPartialContent || !bytes.Equal(got, data[1000:2000]) {
			t.Fatalf("206 과 요청한 구간이어야 한다: %d, %d 바이트", res.StatusCode, len(got))
		}
		if ct := res.Header.Get("Content-Type"); ct != "video/quicktime" {
			t.Errorf("올릴 때 말한 종류가 아니라 판정한 종류여야 한다: %q", ct)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
			t.Errorf("inline 이어야 한다: %q", cd)
		}
		_ = store.Remove(ctx, key)
	})
}

// postStream 은 mp4 머리 뒤를 0 으로 채운 size 바이트 파일을 흘려 보내며 올린다. 메모리에 다 올리지 않는다.
// 브라우저처럼 Content-Length 를 싣는다. S3 의 양식 업로드는 길이 없는(chunked) 본문을 받지 않는다.
func postStream(t *testing.T, form core.UploadForm, size int64) int {
	t.Helper()
	head := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomavc1")
	var prefix bytes.Buffer
	mw := multipart.NewWriter(&prefix)
	for k, v := range form.Fields {
		_ = mw.WriteField(k, v)
	}
	if _, err := mw.CreateFormFile("file", "upload.mp4"); err != nil {
		t.Fatal(err)
	}
	suffix := "\r\n--" + mw.Boundary() + "--\r\n"
	body := io.MultiReader(&prefix, bytes.NewReader(head), io.LimitReader(zeroReader{}, size-int64(len(head))), strings.NewReader(suffix))
	req, err := http.NewRequest(http.MethodPost, form.URL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(prefix.Len()) + size + int64(len(suffix))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// 저장소가 한도를 넘는 본문을 다 읽기 전에 거절하고 연결을 닫으면 여기로 온다. 거절이다.
		t.Logf("올리기가 끊겼다(거절): %v", err)
		return http.StatusRequestEntityTooLarge
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode >= 300 {
		_, code, _ := strings.Cut(string(raw), "<Code>")
		code, _, _ = strings.Cut(code, "<")
		t.Logf("저장소 답 %d %s", res.StatusCode, code)
	}
	return res.StatusCode
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// openPut 은 서명된 PUT 주소로 올리는 저장소다(D42). 양식 업로드(POST policy)가 없는 저장소는 이 방식만 쓸 수 있다.
func openPut(t *testing.T) *s3store.Store {
	t.Helper()
	open(t) // MinIO 가 없으면 건너뛰고, 버킷을 만든다
	endpoint := os.Getenv("CHAT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:59100"
	}
	store, err := s3store.New(s3store.Config{
		Endpoint: endpoint, Bucket: "chat-test", AccessKey: "chat", SecretKey: "chatchat123", PathStyle: true, Upload: s3store.UploadModePut,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// put 은 브라우저가 하듯이 서명된 주소에 발급받은 헤더를 그대로 싣고 파일을 본문으로 보낸다. Content-Length 는 본문이 정한다.
func put(t *testing.T, form core.UploadForm, url string, mutate func(http.Header), data []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range form.Headers {
		req.Header.Set(k, v)
	}
	if mutate != nil {
		mutate(req.Header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestS3_PUT방식은서명된주소에신청한종류와크기로만올라가고확인옮기기받기는POST방식과같다(t *testing.T) {
	store := openPut(t)
	ctx := context.Background()
	key := "tmp/put-" + time.Now().Format("150405.000000000")
	data := []byte("PUT 으로 올린 영수증")

	form, err := store.PresignUpload(key, "text/plain", int64(len(data)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Remove(ctx, key) })

	t.Run("양식은 PUT 이고 실을 헤더는 Content-Type 하나다. Content-Length 는 브라우저가 본문으로 정한다", func(t *testing.T) {
		if form.Method != http.MethodPut || len(form.Fields) != 0 {
			t.Errorf("방식 %q, 필드 %v", form.Method, form.Fields)
		}
		if len(form.Headers) != 1 || form.Headers["Content-Type"] != "text/plain" {
			t.Errorf("헤더: %v", form.Headers)
		}
		if !strings.Contains(form.URL, "X-Amz-SignedHeaders=content-length%3Bcontent-type%3Bhost") {
			t.Errorf("Content-Length·Content-Type 이 서명 헤더여야 한다: %s", form.URL)
		}
		if !form.ExpiresAt.After(time.Now()) {
			t.Errorf("만료 시각: %v", form.ExpiresAt)
		}
	})

	t.Run("서명한 종류와 다른 Content-Type 으로 올리면 저장소가 거절한다", func(t *testing.T) {
		if code := put(t, form, form.URL, func(h http.Header) { h.Set("Content-Type", "text/html") }, data); code != http.StatusForbidden {
			t.Errorf("다른 종류로 올라갔다: %d", code)
		}
	})

	t.Run("신청한 크기와 다른 파일은 서명이 맞지 않아 저장소가 거절한다", func(t *testing.T) {
		if code := put(t, form, form.URL, nil, append(data, []byte(" 그리고 더")...)); code != http.StatusForbidden {
			t.Errorf("큰 파일이 올라갔다: %d", code)
		}
		if code := put(t, form, form.URL, nil, data[:3]); code != http.StatusForbidden {
			t.Errorf("작은 파일이 올라갔다: %d", code)
		}
	})

	t.Run("주소의 키를 바꾸면 저장소가 거절한다", func(t *testing.T) {
		other := strings.Replace(form.URL, "/tmp/put-", "/ws/stolen-", 1)
		if code := put(t, form, other, nil, data); code != http.StatusForbidden {
			t.Errorf("다른 키로 올라갔다: %d", code)
		}
		if _, err := store.Stat(ctx, key); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("거절된 업로드가 남았다: %v", err)
		}
	})

	t.Run("발급한 주소와 헤더 그대로 올리면 받아지고, 확인·옮기기·내려받기가 된다", func(t *testing.T) {
		if code := put(t, form, form.URL, nil, data); code != http.StatusOK {
			t.Fatalf("업로드: %d", code)
		}
		info, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(len(data)) || string(info.Head) != string(data) {
			t.Errorf("크기 %d, 앞부분 %q", info.Size, info.Head)
		}
		final := "ws/put/" + time.Now().Format("150405.000000000")
		if err := store.Promote(ctx, key, final); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Remove(ctx, final) })
		link, err := store.PresignDownload(final, "영수증.txt", "text/plain; charset=utf-8", false, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(got) != string(data) {
			t.Errorf("내려받기: %d %q", res.StatusCode, got)
		}
	})
}

func TestS3_올리기방식은post와put뿐이고비우면post다(t *testing.T) {
	base := s3store.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	for mode, want := range map[string]string{"": "post", "post": "post", "put": "put"} {
		cfg := base
		cfg.Upload = mode
		store, err := s3store.New(cfg)
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		if store.UploadMode() != want {
			t.Errorf("%q: %q", mode, store.UploadMode())
		}
		form, err := store.PresignUpload("tmp/x", "text/plain", 5, time.Minute)
		if err != nil || form.Method != strings.ToUpper(want) {
			t.Errorf("%q: 양식 방식 %q, %v", mode, form.Method, err)
		}
	}
	cfg := base
	cfg.Upload = "multipart"
	if _, err := s3store.New(cfg); err == nil {
		t.Error("모르는 올리기 방식을 받았다")
	}
}

// D47(감사 23 N11): 저장소가 거절한 응답의 고정 코드는 s3_<HTTP 상태>다. 로그의 err_kind 가 이 값이라 권한(403)과 저장소 장애(5xx)를
// 로그로 가른다. 저장소가 정한 글자(<Code>AccessDenied)는 코드에 넣지 않는다. 가짜 저장소로 상태를 골라 답한다.
func TestS3_저장소가거절한응답의고정코드는s3_상태다(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>m</Message><Key>k</Key></Error>`)
	}))
	t.Cleanup(srv.Close)
	store, err := s3store.New(s3store.Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := map[string]func() error{
		"확인": func() error { _, err := store.Stat(ctx, "tmp/a"); return err },
		"복사": func() error { return store.Promote(ctx, "tmp/a", "att/a") },
		"삭제": func() error { return store.Remove(ctx, "att/a") },
	}
	for _, st := range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		status.Store(int32(st))
		for what, call := range calls {
			err := call()
			want := "s3_" + strconv.Itoa(st)
			if got := core.ErrCode(err); got != want {
				t.Errorf("%s 가 %d 를 받았는데 코드가 %q 다. %q 여야 한다(err=%v)", what, st, got, want, err)
			}
			if got := logctx.ErrKind(err); got != want {
				t.Errorf("%s 의 err_kind 가 %q 다. %q 여야 한다", what, got, want)
			}
		}
	}
}
