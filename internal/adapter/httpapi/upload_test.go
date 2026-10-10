package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 직접 업로드의 정책. 서버는 파일을 받지 않으므로, 올라온 것이 신청한 것과 같은지 확인하기 전에는 아무 데도 쓰지 못하게 한다.

func TestE2E_직접업로드(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("업로드 상점")
	cust, convID := h.widgetCustomer(tn)

	t.Run("올리고 완료한 파일은 메시지에 붙고, 로그인 토큰 없이 발급받은 주소로 내려받는다", func(t *testing.T) {
		data := []byte("영수증 사본")
		started := h.startUpload(convID, cust, "영수증.txt", "text/plain", len(data))
		if code := h.putObject(started, "영수증.txt", data).StatusCode; code >= 300 {
			t.Fatalf("저장소 업로드: %d", code)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusCreated {
			t.Fatalf("완료: %d", code)
		}
		if code := h.sendWith(convID, cust, started.Attachment.ID); code >= 300 {
			t.Fatalf("전송: %d", code)
		}
		link, _ := h.get("/api/attachments/"+started.Attachment.ID+"/url", tn.token)["url"].(string)
		if link == "" {
			t.Fatal("내려받기 주소가 없다")
		}
		res, err := http.Get(h.srv.URL + link)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		got, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || string(got) != string(data) {
			t.Errorf("내용이 그대로 내려와야 한다: %d %q", res.StatusCode, got)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("이미지가 아니면 내려받기로, 내용 추측 없이: %v", res.Header)
		}
	})

	t.Run("완료하지 않은 첨부는 메시지에 붙일 수 없고 내려받을 수도 없다", func(t *testing.T) {
		data := []byte("아직 확인 안 된 파일")
		started := h.startUpload(convID, cust, "pending.txt", "text/plain", len(data))
		h.putObject(started, "pending.txt", data)
		if code := h.sendWith(convID, cust, started.Attachment.ID); code < 400 {
			t.Errorf("확인 전 첨부가 메시지에 붙었다: %d", code)
		}
		if code := h.status(http.MethodGet, "/api/attachments/"+started.Attachment.ID+"/url", cust, nil); code != http.StatusNotFound {
			t.Errorf("확인 전 첨부의 주소가 발급됐다: %d", code)
		}
	})

	t.Run("올리지 않고 완료를 부르면 거절된다", func(t *testing.T) {
		started := h.startUpload(convID, cust, "never.txt", "text/plain", 10)
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Errorf("완료: %d", code)
		}
	})

	t.Run("신청한 것보다 큰 파일은 저장소가 받지 않는다", func(t *testing.T) {
		started := h.startUpload(convID, cust, "small.txt", "text/plain", 10)
		if code := h.putObject(started, "small.txt", []byte(strings.Repeat("가", 100))).StatusCode; code != http.StatusRequestEntityTooLarge {
			t.Errorf("저장소 업로드: %d", code)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Errorf("올라가지 않았으니 완료도 안 된다: %d", code)
		}
	})

	t.Run("신청한 것보다 작은 파일을 올리면 완료가 거절되고 그 파일은 남지 않는다", func(t *testing.T) {
		started := h.startUpload(convID, cust, "short.txt", "text/plain", 100)
		h.putObject(started, "short.txt", []byte("짧다"))
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Fatalf("크기가 다르면 거절: %d", code)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Errorf("파일이 지워졌으니 다시 불러도 거절: %d", code)
		}
	})

	t.Run("올린 본인만 완료할 수 있다", func(t *testing.T) {
		data := []byte("고객이 올린 파일")
		started := h.startUpload(convID, cust, "mine.txt", "text/plain", len(data))
		h.putObject(started, "mine.txt", data)
		if code := h.completeStatus(started.Attachment.ID, tn.token); code != http.StatusForbidden {
			t.Errorf("상담원이 고객의 업로드를 완료: %d", code)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusCreated {
			t.Errorf("본인: %d", code)
		}
	})

	t.Run("완료는 두 번 불러도 같은 첨부를 돌려준다", func(t *testing.T) {
		out := h.upload(convID, cust, "twice.txt", []byte("두 번"))
		att, _ := out["attachment"].(map[string]any)
		again := h.post("/api/attachments/"+att["id"].(string)+"/complete", cust, nil)["attachment"].(map[string]any)
		if again["id"] != att["id"] || again["content_type"] != att["content_type"] {
			t.Errorf("처음 %+v, 다시 %+v", att, again)
		}
	})

	t.Run("브라우저가 말한 종류를 믿지 않는다 — 내용으로 판정한다", func(t *testing.T) {
		data := []byte("<script>alert(1)</script> 이건 그림이 아니다")
		started := h.startUpload(convID, cust, "가짜.png", "image/png", len(data))
		h.putObject(started, "가짜.png", data)
		att, _ := h.post("/api/attachments/"+started.Attachment.ID+"/complete", cust, nil)["attachment"].(map[string]any)
		if ct, _ := att["content_type"].(string); strings.HasPrefix(ct, "image/") {
			t.Errorf("그림이 아닌데 그림으로 판정했다: %v", ct)
		}
	})

	t.Run("서명이 없거나 틀린 주소는 저장소가 받지 않는다", func(t *testing.T) {
		forged := uploadStarted{}
		forged.Upload.URL = "/files/upload"
		forged.Upload.Fields = map[string]string{"token": "eyJvcCI6InB1dCJ9.AAAA"}
		if code := h.putObject(forged, "x.txt", []byte("x")).StatusCode; code != http.StatusForbidden {
			t.Errorf("위조한 허가로 올리기: %d", code)
		}
		res, err := http.Get(h.srv.URL + "/files/get?token=eyJvcCI6ImdldCJ9.AAAA")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("위조한 허가로 받기: %d", res.StatusCode)
		}
	})

	t.Run("다른 고객은 내려받기 주소를 발급받지 못한다", func(t *testing.T) {
		out := h.upload(convID, cust, "private.txt", []byte("내 파일"))
		att, _ := out["attachment"].(map[string]any)
		h.sendWith(convID, cust, att["id"].(string))
		other, _ := h.widgetCustomer(tn)
		if code := h.status(http.MethodGet, "/api/attachments/"+att["id"].(string)+"/url", other, nil); code != http.StatusForbidden {
			t.Errorf("남의 첨부 주소: %d", code)
		}
	})
}
