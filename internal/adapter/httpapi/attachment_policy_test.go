package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 첨부의 접근 정책. 파일은 대화에 붙지만 대화 권한만으로 끝나지 않는다 — 올린 사람, 확인 여부, 메시지의 상태가 같이 걸린다.

// storagePath 는 첨부 행이 가리키는 저장소 키다. 없으면 빈 문자열이다.
func (h *harness) storagePath(attachmentID string) string {
	h.t.Helper()
	var path string
	err := h.store.Pool().QueryRow(context.Background(),
		"SELECT storage_path FROM attachments WHERE id = $1", attachmentID).Scan(&path)
	if err != nil {
		return ""
	}
	return path
}

func (h *harness) completed(convID, token, filename string, data []byte) string {
	h.t.Helper()
	att, _ := h.upload(convID, token, filename, data)["attachment"].(map[string]any)
	attID, _ := att["id"].(string)
	if attID == "" {
		h.t.Fatal("첨부 id 가 없다")
	}
	return attID
}

func TestE2E_확인한파일은바꿔치기할수없다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("바꿔치기 상점")
	cust, convID := h.widgetCustomer(tn)
	ctx := context.Background()

	t.Run("확인한 뒤 같은 양식으로 다른 내용을 다시 올려도 내려받는 내용은 확인 때의 것이다", func(t *testing.T) {
		// 크기가 같아야 저장소 한도(신청한 크기)를 통과한다. 바꿔치기는 같은 크기로 한다.
		original, forged := []byte("refund receipt: 10,000 won"), []byte("refund receipt: 99,999 won")
		started := h.startUpload(convID, cust, "receipt.txt", "text/plain", len(original))
		if code := h.putObject(started, "receipt.txt", original).StatusCode; code >= 300 {
			t.Fatalf("저장소 업로드: %d", code)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusCreated {
			t.Fatalf("완료: %d", code)
		}
		// 양식은 10분 동안 살아 있다. 같은 양식으로 다시 올리는 것 자체는 막지 않는다 — 임시 자리에만 쓴다.
		if code := h.putObject(started, "receipt.txt", forged).StatusCode; code >= 300 {
			t.Fatalf("다시 올리기: %d", code)
		}
		if code := h.sendWith(convID, cust, started.Attachment.ID); code >= 300 {
			t.Fatalf("전송: %d", code)
		}
		if got := h.download(started.Attachment.ID, tn.token); got != string(original) {
			t.Errorf("확인한 뒤 내용이 바뀌었다: %q", got)
		}
		// 완료를 다시 불러도 다시 올린 내용으로 바뀌지 않는다.
		h.completeStatus(started.Attachment.ID, cust)
		if got := h.download(started.Attachment.ID, tn.token); got != string(original) {
			t.Errorf("완료를 다시 부르니 내용이 바뀌었다: %q", got)
		}
	})

	t.Run("크기가 다르면 완료가 거절되고 임시 파일이 남지 않는다", func(t *testing.T) {
		started := h.startUpload(convID, cust, "short.txt", "text/plain", 100)
		h.putObject(started, "short.txt", []byte("짧다"))
		path := h.storagePath(started.Attachment.ID)
		if _, err := h.files.Stat(ctx, path); err != nil {
			t.Fatalf("올린 직후에는 임시 파일이 있어야 한다: %v", err)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Fatalf("크기가 다르면 거절: %d", code)
		}
		if _, err := h.files.Stat(ctx, path); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("거절한 임시 파일이 남았다: %v", err)
		}
	})
}

func TestE2E_첨부접근정책(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("첨부 정책 상점")
	ctx := context.Background()

	t.Run("한글 100자 이름도 업로드 신청이 되고 저장된 이름은 올바른 UTF-8 이다", func(t *testing.T) {
		_, convID := h.widgetCustomer(tn)
		name := strings.Repeat("가", 100) + ".txt"
		started := h.startUpload(convID, tn.token, name, "text/plain", 4)
		var stored string
		if err := h.store.Pool().QueryRow(ctx, "SELECT filename FROM attachments WHERE id = $1", started.Attachment.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(stored) || len(stored) > 200 || !strings.HasPrefix(stored, "가가가") {
			t.Errorf("저장된 이름: %d바이트, 올바른 UTF-8 %v", len(stored), utf8.ValidString(stored))
		}
	})

	t.Run("보내지 않은 첨부 상한은 동시에 20건을 신청해도 넘지 않는다", func(t *testing.T) {
		_, convID := h.widgetCustomer(tn)
		var wg sync.WaitGroup
		codes := make([]int, 20)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = h.status(http.MethodPost, "/api/conversations/"+convID+"/attachments", tn.token,
					map[string]any{"filename": "a.txt", "content_type": "text/plain", "byte_size": 4})
			}(i)
		}
		wg.Wait()
		var pending int
		if err := h.store.Pool().QueryRow(ctx,
			"SELECT count(*) FROM attachments WHERE conversation_id = $1 AND message_id IS NULL", convID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending > 10 {
			t.Errorf("미전송 첨부가 상한(10)을 넘었다: %d건, 응답 %v", pending, codes)
		}
		for _, c := range codes {
			if c != http.StatusCreated && c != http.StatusTooManyRequests {
				t.Errorf("201 또는 429 여야 한다: %v", codes)
				break
			}
		}
	})

	t.Run("상담원이 올리고 보내지 않은 첨부를 고객이 자기 메시지에 붙일 수 없다", func(t *testing.T) {
		cust, convID := h.widgetCustomer(tn)
		attID := h.completed(convID, tn.token, "internal.txt", []byte("상담원 자료"))
		if code := h.sendWith(convID, cust, attID); code < 400 {
			t.Errorf("고객이 상담원의 첨부를 붙였다: %d", code)
		}
		if code := h.sendWith(convID, tn.token, attID); code >= 300 {
			t.Errorf("올린 상담원은 붙인다: %d", code)
		}
	})

	t.Run("다른 대화의 첨부 id 로는 메시지에 붙일 수 없다", func(t *testing.T) {
		_, convA := h.widgetCustomer(tn)
		_, convB := h.widgetCustomer(tn)
		attID := h.completed(convA, tn.token, "a.txt", []byte("A 대화 자료"))
		if code := h.sendWith(convB, tn.token, attID); code < 400 {
			t.Errorf("다른 대화의 첨부가 붙었다: %d", code)
		}
	})

	t.Run("삭제된 메시지의 첨부는 내려받기 주소가 발급되지 않는다", func(t *testing.T) {
		cust, convID := h.widgetCustomer(tn)
		attID := h.completed(convID, cust, "gone.txt", []byte("지울 파일"))
		out := h.post("/api/conversations/"+convID+"/messages", cust,
			map[string]any{"client_msg_id": id.New(), "body": "첨부", "attachment_id": attID})
		msg, _ := out["message"].(map[string]any)
		msgID, _ := msg["id"].(string)
		if code := h.status(http.MethodDelete, "/api/conversations/"+convID+"/messages/"+msgID, cust, nil); code >= 300 {
			t.Fatalf("삭제: %d", code)
		}
		for who, token := range map[string]string{"고객": cust, "상담원": tn.token} {
			if code := h.status(http.MethodGet, "/api/attachments/"+attID+"/url", token, nil); code != http.StatusNotFound {
				t.Errorf("%s 에게 지운 메시지의 첨부 주소가 발급됐다: %d", who, code)
			}
		}
	})

	t.Run("고객을 삭제하면 그 고객의 첨부 파일과 행이 사라진다", func(t *testing.T) {
		cust, convID := h.widgetCustomer(tn)
		sent := h.completed(convID, cust, "sent.txt", []byte("보낸 파일"))
		h.sendWith(convID, cust, sent)
		unsent := h.completed(convID, cust, "unsent.txt", []byte("안 보낸 파일"))
		paths := []string{h.storagePath(sent), h.storagePath(unsent)}
		for _, p := range paths {
			if _, err := h.files.Stat(ctx, p); err != nil {
				t.Fatalf("삭제 전에는 파일이 있어야 한다: %v", err)
			}
		}
		customerID, _ := h.get("/api/conversations/"+convID, tn.token)["customer_id"].(string)
		if code := h.status(http.MethodDelete, "/api/customers/"+customerID, tn.token, nil); code != http.StatusNoContent {
			t.Fatalf("고객 삭제: %d", code)
		}
		var rows int
		if err := h.store.Pool().QueryRow(ctx, "SELECT count(*) FROM attachments WHERE id = ANY($1)", []string{sent, unsent}).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Errorf("첨부 행이 %d건 남았다", rows)
		}
		for _, p := range paths {
			if _, err := h.files.Stat(ctx, p); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("첨부 파일이 남았다(%s): %v", p, err)
			}
		}
	})

	t.Run("지난 상담의 첨부는 회원이 완료할 수도 내려받을 수도 없다", func(t *testing.T) {
		cust := h.memberSession(tn)
		oldID, _ := h.startConversation(cust)["id"].(string)
		sent := h.completed(oldID, cust, "old.txt", []byte("지난 상담 파일"))
		h.sendWith(oldID, cust, sent)
		data := []byte("완료 못 한 파일")
		started := h.startUpload(oldID, cust, "late.txt", "text/plain", len(data))
		h.putObject(started, "late.txt", data)

		h.post("/api/agent/conversations/"+oldID+"/state", tn.token, map[string]any{"state": "CLOSED"})
		if newID, _ := h.startConversation(cust)["id"].(string); newID == "" || newID == oldID {
			t.Fatalf("새 상담이 열려야 한다: %s", newID)
		}
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusNotFound {
			t.Errorf("지난 상담의 첨부를 회원이 완료: %d", code)
		}
		if code := h.status(http.MethodGet, "/api/attachments/"+sent+"/url", cust, nil); code != http.StatusNotFound {
			t.Errorf("지난 상담의 첨부 주소가 회원에게 발급됐다: %d", code)
		}
		if code := h.status(http.MethodGet, "/api/attachments/"+sent+"/url", tn.token, nil); code != http.StatusOK {
			t.Errorf("상담원은 받는다: %d", code)
		}
	})
}

// cancelAfterAnonymize 는 고객 익명화가 커밋된 직후에 한 번 끼어들어, 요청을 보낸 쪽이 연결을 끊고
// 서버가 그 요청의 ctx 를 취소할 때까지 기다린다. 운영 코드에 훅을 넣지 않고 저장소를 바깥에서 감싼다.
type cancelAfterAnonymize struct {
	*pgstore.Store
	committed chan struct{}
	once      sync.Once
	sawCancel atomic.Bool
}

func (c *cancelAfterAnonymize) AnonymizeCustomer(ctx context.Context, wsID, customerID string) ([]string, error) {
	paths, err := c.Store.AnonymizeCustomer(ctx, wsID, customerID)
	c.once.Do(func() { close(c.committed) })
	select {
	case <-ctx.Done():
		c.sawCancel.Store(true)
	case <-time.After(5 * time.Second):
	}
	return paths, err
}

// 익명화가 커밋되면 첨부 파일은 요청이 끊겨도 끝까지 지운다. 파일 삭제가 요청 취소를 따르면 그 고객의 파일이 남는다.
func TestE2E_고객삭제중연결이끊겨도익명화가커밋됐으면그고객의첨부파일을지운다(t *testing.T) {
	wrapped := &cancelAfterAnonymize{committed: make(chan struct{})}
	h := newHarnessWrapped(t, func(s *pgstore.Store) app.Store { wrapped.Store = s; return wrapped })
	tn := h.newTenant("끊긴 삭제 상점")
	cust, convID := h.widgetCustomer(tn)
	ctx := context.Background()

	sent := h.completed(convID, cust, "sent.txt", []byte("보낸 파일"))
	h.sendWith(convID, cust, sent)
	unsent := h.completed(convID, cust, "unsent.txt", []byte("안 보낸 파일"))
	paths := []string{h.storagePath(sent), h.storagePath(unsent)}
	customerID, _ := h.get("/api/conversations/"+convID, tn.token)["customer_id"].(string)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, h.srv.URL+"/api/customers/"+customerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tn.token)
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()
	select {
	case <-wrapped.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("익명화가 불리지 않았다")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("요청이 끊기지 않았다")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		left := 0
		for _, p := range paths {
			if _, err := h.files.Stat(ctx, p); !errors.Is(err, core.ErrNotFound) {
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("요청이 끊긴 뒤 첨부 파일 %d개가 남았다(서버의 요청 취소 관찰: %v)", left, wrapped.sawCancel.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !wrapped.sawCancel.Load() {
		t.Fatal("서버가 요청 ctx 를 취소하지 않았다 — 끊긴 경로를 재현하지 못했다")
	}
}
