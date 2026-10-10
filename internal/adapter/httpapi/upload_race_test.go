package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/filestore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 첨부 완료의 경합. 양식은 10분 동안 살아 있어서 완료를 확인하는 도중에도 같은 양식으로 다시 올릴 수 있다.
// 완료는 임대를 잡은 요청 하나만 하고(나머지는 409 busy), 옮긴 뒤 확정 파일을 다시 본다. 확인이 끝난 첨부의 내용은 그 뒤로 바뀌지 않는다.

// gatedFiles 는 첫 Stat 이 끝난 뒤 멈춰 서는 저장소다. 판정과 옮기기 사이에 다른 일이 끼어드는 창을 연다.
// Promote 는 S3 처럼 확정 자리를 덮어쓴다. 로컬 어댑터는 덮어쓰기를 거절하지만 S3 어댑터는 그렇지 않으므로,
// 덮어쓰는 저장소에서도 임대와 재확인만으로 지켜지는지 본다.
type gatedFiles struct {
	*filestore.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newGatedFiles(fs *filestore.Store) *gatedFiles {
	return &gatedFiles{Store: fs, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedFiles) Stat(ctx context.Context, key string) (core.ObjectInfo, error) {
	info, err := g.Store.Stat(ctx, key)
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		<-g.release
	}
	return info, err
}

func (g *gatedFiles) Promote(ctx context.Context, from, to string) error {
	if _, err := g.Store.Stat(ctx, from); errors.Is(err, core.ErrNotFound) {
		return core.ErrNotFound
	}
	if err := g.Store.Remove(ctx, to); err != nil {
		return err
	}
	return g.Store.Promote(ctx, from, to)
}

func TestE2E_첨부완료의경합(t *testing.T) {
	t.Run("완료 요청 여럿과 재업로드를 겹쳐도 확인된 뒤의 내용은 바뀌지 않는다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		gate := newGatedFiles(h.files)
		h.svc.UseFiles(gate, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "a.txt", "text/plain", 5)
		attID := started.Attachment.ID
		if code := h.putObject(started, "a.txt", []byte("AAAAA")).StatusCode; code >= 300 {
			t.Fatalf("저장소 업로드: %d", code)
		}

		slow := make(chan int, 1)
		go func() { slow <- h.completeStatus(attID, cust) }() // 판정 뒤에 멈춘다
		<-gate.entered
		fast := make(chan int, 1)
		go func() { fast <- h.completeStatus(attID, cust) }()

		// 임대가 없으면 뒤에 온 요청이 앞질러 끝낸다. 그러면 바로 보내고 받은 내용을 기억해 둔다.
		sent, fastDone := "", false
		select {
		case code := <-fast:
			fastDone = true
			if code == http.StatusCreated {
				if s := h.sendWith(conv, cust, attID); s != http.StatusCreated {
					t.Fatalf("전송: %d", s)
				}
				sent = h.download(attID, cust)
			}
		case <-time.After(500 * time.Millisecond):
		}
		if code := h.putObject(started, "a.txt", []byte("BBBBB")).StatusCode; code >= 300 {
			t.Fatalf("다시 올리기: %d", code)
		}
		close(gate.release)
		<-slow
		if !fastDone {
			<-fast
		}

		var confirmed bool
		if err := h.store.Pool().QueryRow(context.Background(),
			"SELECT uploaded_at IS NOT NULL FROM attachments WHERE id = $1", attID).Scan(&confirmed); err != nil {
			t.Fatal(err)
		}
		if sent != "" {
			if got := h.download(attID, cust); got != sent {
				t.Fatalf("확인·전송이 끝난 첨부의 내용이 바뀌었다: %q → %q", sent, got)
			}
		}
		if !confirmed {
			// 겹친 요청이 모두 거절됐다. 다시 올리고 완료하면 그때 확인된다.
			if code := h.putObject(started, "a.txt", []byte("CCCCC")).StatusCode; code >= 300 {
				t.Fatalf("다시 올리기: %d", code)
			}
			if code := h.completeStatus(attID, cust); code != http.StatusCreated {
				t.Fatalf("다시 완료: %d", code)
			}
		}
		before := h.download(attID, cust)
		h.putObject(started, "a.txt", []byte("DDDDD"))
		h.completeStatus(attID, cust)
		if got := h.download(attID, cust); got != before {
			t.Errorf("확인된 뒤 다시 올리고 완료하자 내용이 바뀌었다: %q → %q", before, got)
		}
	})

	t.Run("판정과 옮기기 사이에 내용이 바뀌면 완료가 거절되고 확정 파일이 남지 않는다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		gate := newGatedFiles(h.files)
		h.svc.UseFiles(gate, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "b.txt", "text/plain", 5)
		attID := started.Attachment.ID
		h.putObject(started, "b.txt", []byte("AAAAA"))

		done := make(chan int, 1)
		go func() { done <- h.completeStatus(attID, cust) }()
		<-gate.entered
		h.putObject(started, "b.txt", []byte("BBBBB")) // 판정은 AAAAA 로 끝났다
		close(gate.release)

		if code := <-done; code != http.StatusBadRequest {
			t.Fatalf("판정한 것과 옮긴 것이 다르면 400: %d", code)
		}
		if left := h.finalFiles(conv, attID); len(left) != 0 {
			t.Errorf("거절한 확정 파일이 남았다: %v", left)
		}
		var confirmed bool
		_ = h.store.Pool().QueryRow(context.Background(),
			"SELECT uploaded_at IS NOT NULL FROM attachments WHERE id = $1", attID).Scan(&confirmed)
		if confirmed {
			t.Error("거절한 첨부가 확인됨으로 남았다")
		}
		if code := h.sendWith(conv, cust, attID); code < 400 {
			t.Errorf("확인되지 않은 첨부가 메시지에 붙었다: %d", code)
		}
	})

	t.Run("완료 응답의 created_at 이 채워져 있다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "c.txt", "text/plain", 5)
		h.putObject(started, "c.txt", []byte("CCCCC"))
		for i := 0; i < 2; i++ { // 처음 확인과, 이미 확인된 첨부의 재완료
			att, _ := h.post("/api/attachments/"+started.Attachment.ID+"/complete", cust, nil)["attachment"].(map[string]any)
			raw, _ := att["created_at"].(string)
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil || at.Year() < 2000 || time.Since(at) > time.Hour {
				t.Errorf("%d번째 완료의 created_at: %q", i+1, raw)
			}
		}
	})
}

// 첨부의 created_at 은 어느 응답에 실려 와도 실제 생성 시각이다. 제로값(0001-01-01)이 나가면 화면이 "2000년 전" 을 그린다.
func TestE2E_첨부의생성시각(t *testing.T) {
	h := newHarness(t, time.Second, false)
	cust, conv := h.customerSession()
	recent := func(t *testing.T, where string, att map[string]any) {
		t.Helper()
		raw, _ := att["created_at"].(string)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || at.Year() < 2000 || time.Since(at) > time.Hour {
			t.Errorf("%s 의 created_at: %q", where, raw)
		}
	}

	started := h.post("/api/conversations/"+conv+"/attachments", cust, map[string]any{
		"filename": "d.txt", "content_type": "text/plain", "byte_size": 5,
	})
	att, _ := started["attachment"].(map[string]any)
	attID, _ := att["id"].(string)

	t.Run("업로드 신청 응답의 첨부 created_at 이 실제 생성 시각이다", func(t *testing.T) {
		recent(t, "신청 응답", att)
	})

	var st uploadStarted
	st.Attachment.ID = attID
	up, _ := started["upload"].(map[string]any)
	st.Upload.URL, _ = up["url"].(string)
	st.Upload.Fields = map[string]string{}
	for k, v := range up["fields"].(map[string]any) {
		st.Upload.Fields[k], _ = v.(string)
	}
	h.putObject(st, "d.txt", []byte("DDDDD"))
	h.post("/api/attachments/"+attID+"/complete", cust, nil)

	t.Run("메시지에 실린 첨부 created_at 이 실제 생성 시각이다", func(t *testing.T) {
		body := map[string]any{"client_msg_id": id.New(), "body": "첨부", "attachment_id": attID}
		for i, where := range []string{"보내기 응답", "같은 client_msg_id 재전송 응답"} {
			msg, _ := h.post("/api/conversations/"+conv+"/messages", cust, body)["message"].(map[string]any)
			a, _ := msg["attachment"].(map[string]any)
			if a == nil {
				t.Fatalf("%d번째 응답에 첨부가 없다: %+v", i+1, msg)
			}
			recent(t, where, a)
		}
		var found bool
		for _, m := range h.messagesOf(conv, cust) {
			if a, _ := m["attachment"].(map[string]any); a != nil && a["id"] == attID {
				found = true
				recent(t, "메시지 목록", a)
			}
		}
		if !found {
			t.Error("목록에 첨부 메시지가 없다")
		}
	})
}
