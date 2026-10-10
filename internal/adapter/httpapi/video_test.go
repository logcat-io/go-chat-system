package httpapi_test

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/config"
)

// 영상 첨부(D37). 한도는 100MB, 종류는 내용으로 판정하고, 재생하는 영상은 화면에서 바로 튼다.
// 로컬 저장소 모드의 끝에서 끝이다. S3 는 s3store 의 테스트가 MinIO 로 본다.

func TestE2E_영상첨부(t *testing.T) {
	t.Setenv("CHAT_MAX_UPLOAD_MB", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := buildHarness(t, harnessConfig{catchup: time.Second, uploadMax: cfg.MaxUploadBytes})
	tn := h.newTenant("영상 상점")
	cust, convID := h.widgetCustomer(tn)

	t.Run("설정을 건드리지 않은 서버는 100MB 신청을 받고 그보다 1바이트 큰 신청은 413 too_large 로 100MB 까지라고 답한다", func(t *testing.T) {
		status, body := h.call(http.MethodPost, "/api/conversations/"+convID+"/attachments", cust, map[string]any{
			"filename": "큰영상.mp4", "content_type": "video/mp4", "byte_size": 100<<20 + 1,
		})
		if status != http.StatusRequestEntityTooLarge || errorCodeOf(body) != "too_large" {
			t.Fatalf("413 too_large 여야 한다: %d %v", status, body)
		}
		if msg := errorMessageOf(body); msg != "파일은 100MB 까지 올릴 수 있습니다" {
			t.Errorf("문구: %q", msg)
		}
		h.startUpload(convID, cust, "딱100MB.mp4", "video/mp4", 100<<20) // 201 이 아니면 Fatal
	})

	t.Run("100MB 영상은 로컬 저장소로 한 번에 올라가 완료되고 video/mp4 로 판정된다", func(t *testing.T) {
		const size = 100 << 20
		started := h.startUpload(convID, cust, "꽉찬.mp4", "video/mp4", size)
		if code := h.streamObject(started, "꽉찬.mp4", mp4Head("isom"), size); code >= 300 {
			t.Fatalf("저장소 업로드: %d", code)
		}
		att := h.post("/api/attachments/"+started.Attachment.ID+"/complete", cust, nil)["attachment"].(map[string]any)
		if att["content_type"] != "video/mp4" || int64(att["byte_size"].(float64)) != size {
			t.Errorf("완료된 첨부: %v", att)
		}
	})

	t.Run("영상은 브라우저가 말한 종류가 아니라 내용으로 판정한다 — mp4·mov·webm", func(t *testing.T) {
		for _, tc := range []struct {
			name, declared, want string
			data                 []byte
		}{
			{"클립.mp4", "application/octet-stream", "video/mp4", sample(mp4Head("mp42"), 4096)},
			{"아이폰.MOV", "", "video/quicktime", sample(movHead(), 4096)},
			{"화면녹화.webm", "video/mp4", "video/webm", sample(webmEBML(), 4096)},
		} {
			att := h.uploadDeclared(convID, cust, tc.name, tc.declared, tc.data)
			if att["content_type"] != tc.want {
				t.Errorf("%s: %s 여야 한다, %v", tc.name, tc.want, att["content_type"])
			}
		}
	})

	t.Run("영상이라고 말해도 내용이 영상이 아니면 영상으로 판정하지 않고 내려받기로 준다", func(t *testing.T) {
		att := h.uploadDeclared(convID, cust, "가짜.mp4", "video/mp4", []byte("<script>alert(1)</script> 영상이 아니다"))
		if ct, _ := att["content_type"].(string); strings.HasPrefix(ct, "video/") {
			t.Fatalf("영상이 아닌데 영상으로 판정했다: %v", ct)
		}
		h.sendWith(convID, cust, att["id"].(string))
		res := h.fetchSigned(att["id"].(string), tn.token, nil)
		res.Body.Close()
		if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("영상이 아니면 내려받기여야 한다: %v", res.Header.Get("Content-Disposition"))
		}
	})

	// 내려받기 주소 하나를 여러 단언이 같이 쓴다.
	data := sample(movHead(), 64<<10)
	mov := h.uploadDeclared(convID, cust, "상담요청.mov", "video/quicktime", data)
	movID := mov["id"].(string)
	if code := h.sendWith(convID, cust, movID); code >= 300 {
		t.Fatalf("전송: %d", code)
	}

	t.Run("재생하는 영상의 내려받기 주소는 inline 이고 판정한 종류와 nosniff 로 내려간다", func(t *testing.T) {
		res := h.fetchSigned(movID, tn.token, nil)
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
			t.Fatalf("내용이 그대로 내려와야 한다: %d, %d 바이트", res.StatusCode, len(got))
		}
		if ct := res.Header.Get("Content-Type"); ct != "video/quicktime" {
			t.Errorf("판정한 종류여야 한다: %s", ct)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
			t.Errorf("화면에서 바로 재생하도록 inline 이어야 한다: %s", cd)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("브라우저가 종류를 추측하지 않게 막아야 한다")
		}
		// 주소를 새 탭에서 열어도 그 영상만 튼다. 다른 것은 여전히 막는다.
		if csp := res.Header.Get("Content-Security-Policy"); csp != "default-src 'none'; img-src 'self'; media-src 'self'" {
			t.Errorf("CSP: %q", csp)
		}
	})

	t.Run("로컬 저장소의 영상 주소는 Range 요청에 206 과 요청한 구간만 준다", func(t *testing.T) {
		res := h.fetchSigned(movID, tn.token, map[string]string{"Range": "bytes=1000-1999"})
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusPartialContent {
			t.Fatalf("206 이어야 한다: %d", res.StatusCode)
		}
		if cr := res.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 1000-1999/%d", len(data)) {
			t.Errorf("Content-Range: %s", cr)
		}
		if !bytes.Equal(got, data[1000:2000]) {
			t.Errorf("요청한 구간이 아니다: %d 바이트", len(got))
		}
		if res.Header.Get("Content-Type") != "video/quicktime" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("구간 응답도 판정한 종류와 nosniff 여야 한다: %v", res.Header)
		}
	})

	t.Run("재생하지 않는 영상(mkv)은 내려받기로 준다", func(t *testing.T) {
		att := h.uploadDeclared(convID, cust, "녹화.mkv", "video/webm", sample(mkvEBML(), 4096))
		if att["content_type"] != "video/x-matroska" {
			t.Fatalf("mkv 는 webm 이 아니다: %v", att["content_type"])
		}
		h.sendWith(convID, cust, att["id"].(string))
		res := h.fetchSigned(att["id"].(string), tn.token, nil)
		res.Body.Close()
		if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("재생하지 않는 영상은 내려받기여야 한다: %v", res.Header.Get("Content-Disposition"))
		}
	})
}

// uploadDeclared 는 브라우저가 말한 종류(declared)를 실어 세 걸음을 밟고 완료된 첨부를 돌려준다.
func (h *harness) uploadDeclared(convID, token, filename, declared string, data []byte) map[string]any {
	h.t.Helper()
	started := h.startUpload(convID, token, filename, declared, len(data))
	if code := h.putObject(started, filename, data).StatusCode; code >= 300 {
		h.t.Fatalf("저장소 업로드: %d", code)
	}
	att, _ := h.post("/api/attachments/"+started.Attachment.ID+"/complete", token, nil)["attachment"].(map[string]any)
	if att == nil {
		h.t.Fatalf("완료되지 않았다: %s", filename)
	}
	return att
}

// fetchSigned 는 내려받기 주소를 발급받아 로그인 토큰 없이 받는다.
func (h *harness) fetchSigned(attachmentID, token string, header map[string]string) *http.Response {
	h.t.Helper()
	link, _ := h.get("/api/attachments/"+attachmentID+"/url", token)["url"].(string)
	if link == "" {
		h.t.Fatal("내려받기 주소가 없다")
	}
	return h.roundTrip(http.MethodGet, link, "", nil, header)
}

// streamObject 는 head 뒤를 0 으로 채운 size 바이트 파일을 양식으로 올린다. 메모리에 다 올리지 않고 흘려 보낸다.
func (h *harness) streamObject(started uploadStarted, filename string, head []byte, size int64) int {
	h.t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		for k, v := range started.Upload.Fields {
			_ = mw.WriteField(k, v)
		}
		part, err := mw.CreateFormFile("file", filename)
		if err == nil {
			_, err = io.Copy(part, io.MultiReader(bytes.NewReader(head), io.LimitReader(zeros{}, size-int64(len(head)))))
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	res, err := http.Post(h.srv.URL+started.Upload.URL, mw.FormDataContentType(), pr)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// sample 은 head 뒤를 순서 있는 바이트로 채워 n 바이트로 만든다. 구간 응답이 맞는 자리인지 볼 수 있게 0 으로 채우지 않는다.
func sample(head []byte, n int) []byte {
	b := make([]byte, n)
	copy(b, head)
	for i := len(head); i < n; i++ {
		b[i] = byte(i * 7)
	}
	return b
}

// mp4Head 는 mp4 의 첫 상자(ftyp)와 mdat 머리다.
func mp4Head(major string) []byte {
	return append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p'}, []byte(major+"\x00\x00\x02\x00"+major+"avc1\x00\x00\x00\x08mdat")...)
}

// movHead 는 아이폰이 찍은 mov 의 첫 상자다. 주 브랜드가 "qt  " 다.
func movHead() []byte {
	return []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  \x00\x00\x00\x08wide")
}

// webmEBML·mkvEBML 은 Matroska 계열의 EBML 머리다. DocType 만 다르다.
func webmEBML() []byte { return ebml("webm") }
func mkvEBML() []byte  { return ebml("matroska") }

func ebml(docType string) []byte {
	body := append([]byte{0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0x82, 0x80 | byte(len(docType))}, docType...)
	body = append(body, 0x42, 0x87, 0x81, 0x04)
	return append(append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80 | byte(len(body))}, body...), 0x18, 0x53, 0x80, 0x67)
}
