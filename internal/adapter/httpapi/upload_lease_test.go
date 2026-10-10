package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/filestore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 첨부 완료의 임대. 완료 요청 하나가 첨부를 맡고, 맡지 못한 요청은 기다리지 않고 409 busy 로 바로 끝난다.
// 커넥션을 쥔 채 다른 요청을 기다리는 잠금은 동시 요청이 풀 크기에 닿으면 잠금 안의 작업이 쓸 커넥션이 없어 서버가 멈췄다.

// slowFiles 는 Stat 마다 잠깐 쉬는 저장소다. 저장소 왕복이 걸리는 동안 완료 요청이 겹치게 만든다.
type slowFiles struct {
	*filestore.Store
	delay time.Duration
}

func (f slowFiles) Stat(ctx context.Context, key string) (core.ObjectInfo, error) {
	time.Sleep(f.delay)
	return f.Store.Stat(ctx, key)
}

func (h *harness) leaseOf(attachmentID string) (until *time.Time) {
	h.t.Helper()
	if err := h.store.Pool().QueryRow(context.Background(),
		"SELECT completing_until FROM attachments WHERE id = $1", attachmentID).Scan(&until); err != nil {
		h.t.Fatal(err)
	}
	return until
}

func TestE2E_첨부완료의임대(t *testing.T) {
	t.Run("풀 크기보다 많은 동시 완료가 기다리지 않고 모두 끝나고 그동안 다른 요청이 막히지 않는다", func(t *testing.T) {
		const poolSize, sameN, diffN = 4, 30, 30
		h := newHarnessOn(t, testsupport.StoreWithPool(t, poolSize), time.Second, false)
		h.svc.UseFiles(slowFiles{Store: h.files, delay: 20 * time.Millisecond}, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})

		type target struct {
			id, token, body string
		}
		// 같은 첨부 하나와, 서로 다른 첨부 30개. 대화 하나의 미전송 첨부 상한이 10 이라 대화를 나눈다.
		sameCust, sameConv := h.customerSession()
		same := h.startUpload(sameConv, sameCust, "same.txt", "text/plain", 5)
		h.putObject(same, "same.txt", []byte("SAME0"))
		var diff []target
		for len(diff) < diffN {
			cust, conv := h.customerSession()
			for i := 0; i < 10 && len(diff) < diffN; i++ {
				body := fmt.Sprintf("D%04d", len(diff))
				started := h.startUpload(conv, cust, "d.txt", "text/plain", len(body))
				h.putObject(started, "d.txt", []byte(body))
				diff = append(diff, target{started.Attachment.ID, cust, body})
			}
		}
		other, _ := h.customerSession()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make([]timedStatus, sameN+diffN)
		var wg sync.WaitGroup
		for i := range results {
			id, token := same.Attachment.ID, sameCust
			if i >= sameN {
				id, token = diff[i-sameN].id, diff[i-sameN].token
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = timedCall(ctx, http.MethodPost, h.srv.URL+"/api/attachments/"+id+"/complete", token)
			}()
		}
		meDone := make(chan timedStatus, 1)
		go func() {
			time.Sleep(10 * time.Millisecond) // 완료 요청들이 풀을 쥐기 시작한 뒤
			meDone <- timedCall(ctx, http.MethodGet, h.srv.URL+"/api/me", other)
		}()
		wg.Wait()
		me := <-meDone

		if me.err != nil || me.status != http.StatusOK || me.elapsed > 2*time.Second {
			t.Errorf("동시 완료 중의 다른 조회가 막혔다: %d %v %s", me.status, me.err, me.elapsed)
		}
		sameCreated := 0
		for i, r := range results {
			if r.err != nil || r.elapsed > 2*time.Second {
				t.Errorf("%d번째 완료가 2초 안에 끝나지 않았다: %v %s", i, r.err, r.elapsed)
				continue
			}
			switch {
			case r.status == http.StatusCreated:
				if i < sameN {
					sameCreated++
				}
			case r.status == http.StatusConflict && r.code == "busy" && i < sameN:
			default:
				t.Errorf("%d번째 완료: %d %s (같은 첨부는 201 또는 409 busy, 다른 첨부는 201)", i, r.status, r.code)
			}
		}
		if sameCreated == 0 {
			t.Error("같은 첨부의 완료가 하나도 성공하지 않았다")
		}
		if got := h.download(same.Attachment.ID, sameCust); got != "SAME0" {
			t.Errorf("같은 첨부의 내용: %q", got)
		}
		for _, d := range diff {
			if got := h.download(d.id, d.token); got != d.body {
				t.Errorf("%s 의 내용이 올린 것과 다르다: %q, 올린 것 %q", d.id, got, d.body)
			}
		}
	})

	t.Run("처리 중인 첨부의 다른 완료 요청은 기다리지 않고 409 busy 다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		gate := newGatedFiles(h.files)
		h.svc.UseFiles(gate, app.UploadLimits{MaxBytes: 10 << 20, MaxPending: 10})
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "busy.txt", "text/plain", 5)
		h.putObject(started, "busy.txt", []byte("BUSY0"))

		first := make(chan int, 1)
		go func() { first <- h.completeStatus(started.Attachment.ID, cust) }()
		<-gate.entered // 첫 요청이 임대를 쥐고 저장소 확인에서 멈췄다

		second := timedCall(context.Background(), http.MethodPost, h.srv.URL+"/api/attachments/"+started.Attachment.ID+"/complete", cust)
		close(gate.release)
		if second.status != http.StatusConflict || second.code != "busy" {
			t.Errorf("처리 중인 첨부의 완료는 409 busy: %d %s", second.status, second.code)
		}
		if second.elapsed > time.Second {
			t.Errorf("busy 는 기다리지 않고 바로 답한다: %s", second.elapsed)
		}
		if code := <-first; code != http.StatusCreated {
			t.Fatalf("임대를 쥔 요청은 끝까지 간다: %d", code)
		}
		if got := h.download(started.Attachment.ID, cust); got != "BUSY0" {
			t.Errorf("내용: %q", got)
		}
	})

	t.Run("처리 중이던 요청이 실패하면 임대가 풀려 다음 완료가 바로 된다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "late.txt", "text/plain", 5)
		// 올리기 전에 완료를 부르면 확인이 실패한다.
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusBadRequest {
			t.Fatalf("올리기 전 완료는 400: %d", code)
		}
		if until := h.leaseOf(started.Attachment.ID); until != nil {
			t.Errorf("실패한 완료가 임대를 남겼다: %v", until)
		}
		h.putObject(started, "late.txt", []byte("LATE0"))
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusCreated {
			t.Fatalf("임대가 풀렸으면 다음 완료는 바로 201: %d", code)
		}
		if got := h.download(started.Attachment.ID, cust); got != "LATE0" {
			t.Errorf("내용: %q", got)
		}
	})

	t.Run("임대가 끝나면 다른 요청이 이어받는다", func(t *testing.T) {
		h := newHarness(t, time.Second, false)
		cust, conv := h.customerSession()
		started := h.startUpload(conv, cust, "dead.txt", "text/plain", 5)
		h.putObject(started, "dead.txt", []byte("DEAD0"))
		// 처리하던 프로세스가 죽어 임대만 남은 모습이다.
		setLease := func(sql string) {
			if _, err := h.store.Pool().Exec(context.Background(), sql, started.Attachment.ID); err != nil {
				t.Fatal(err)
			}
		}
		setLease("UPDATE attachments SET completing_until = clock_timestamp() + interval '1 hour' WHERE id = $1")
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusConflict {
			t.Fatalf("임대가 살아 있으면 409: %d", code)
		}
		setLease("UPDATE attachments SET completing_until = clock_timestamp() - interval '1 second' WHERE id = $1")
		if code := h.completeStatus(started.Attachment.ID, cust); code != http.StatusCreated {
			t.Fatalf("끝난 임대는 다음 요청이 이어받는다: %d", code)
		}
		if got := h.download(started.Attachment.ID, cust); got != "DEAD0" {
			t.Errorf("내용: %q", got)
		}
		if until := h.leaseOf(started.Attachment.ID); until != nil {
			t.Errorf("도장을 찍으면 임대가 비어야 한다: %v", until)
		}
	})
}
