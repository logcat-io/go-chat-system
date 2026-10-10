package pgstore_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 고아 첨부 청소는 행을 먼저 선점한다. 그 사이에 메시지가 첨부를 붙이는 중이면 그 행은 살아남는다.
func TestReapOrphanAttachments_붙는중인첨부는지우지않는다(t *testing.T) {
	// 청소는 순서 없이 한도만큼만 고른다. 공유 DB 에는 다른 테스트와 지난 실행이 남긴 오래된 고아가 쌓여
	// 이 테스트의 두 행이 한도 밖으로 밀리면 검증할 것이 사라진다. 이 테스트의 행만 있는 DB 에서 본다.
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	type uploaded struct{ ID, Path string }
	upload := func() uploaded {
		t.Helper()
		path := "race/" + id.New()
		a, err := store.CreateAttachment(ctx, core.NewAttachment{
			ID: id.New(), ConversationID: conv.ID, UploaderKind: "CUSTOMER", UploaderID: customerID,
			Filename: "x.png", ContentType: "image/png", ByteSize: 10, StoragePath: path,
		}, 10)
		if err != nil {
			t.Fatal(err)
		}
		return uploaded{ID: a.ID, Path: path}
	}
	linking, orphan := upload(), upload()
	// 둘 다 "오래된 미전송" 으로 보이게 만든다.
	if _, err := store.Pool().Exec(ctx, "UPDATE attachments SET created_at = now() - interval '1 day' WHERE id = ANY($1)",
		[]string{linking.ID, orphan.ID}); err != nil {
		t.Fatal(err)
	}
	// 메시지가 첨부를 붙이는 트랜잭션을 커밋 전에 붙잡아 둔다.
	msg, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "첨부 붙일 메시지"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE attachments SET message_id = $1 WHERE id = $2", msg.Message.ID, linking.ID); err != nil {
		t.Fatal(err)
	}

	watch, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close(ctx)

	done := make(chan []string, 1)
	go func() {
		paths, err := store.ReapOrphanAttachments(ctx, time.Hour, 100)
		if err != nil {
			t.Error(err)
		}
		done <- paths
	}()
	// 청소기가 붙는 중인 행의 잠금 앞에서 기다리는 것을 본 뒤에 커밋한다. 시간으로 짐작하면
	// 청소가 커밋 뒤에 시작돼 경합 없이 통과할 수 있다. 격리 DB 라 잠금을 기다리는 세션은 청소기뿐이다.
	waitForLockWait(t, watch)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	paths := <-done
	for _, p := range paths {
		if p == linking.Path {
			t.Fatal("붙는 중이던 첨부의 파일이 청소 대상에 들어갔다")
		}
	}
	if _, err := store.AttachmentFile(ctx, linking.ID); err != nil {
		t.Errorf("붙은 첨부는 행이 남아야 한다: %v", err)
	}
	if _, err := store.AttachmentFile(ctx, orphan.ID); err == nil {
		t.Error("진짜 고아는 지워져야 한다")
	}
}

// 완료는 임대를 쥔 요청 하나가 한다. 임대마다 token 이 있고 도장·해제는 그 token 일 때만 걸린다.
// 임대가 끝나 다른 요청이 이어받은 뒤 옛 요청이 늦게 도장을 찍거나 임대를 비워도 새 요청의 것은 그대로다.
func TestAttachmentLease(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	newAttachment := func() string {
		t.Helper()
		attID := id.New()
		if _, err := store.CreateAttachment(ctx, core.NewAttachment{
			ID: attID, ConversationID: conv.ID, UploaderKind: "CUSTOMER", UploaderID: customerID,
			Filename: "a.txt", ContentType: "text/plain", ByteSize: 5, StoragePath: "tmp/" + attID,
		}, 1000); err != nil {
			t.Fatal(err)
		}
		return attID
	}
	expire := func(attID string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx,
			"UPDATE attachments SET completing_until = clock_timestamp() - interval '1 second' WHERE id = $1", attID); err != nil {
			t.Fatal(err)
		}
	}
	lease := func(attID string) (until *time.Time, token *string) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx,
			"SELECT completing_until, completing_token FROM attachments WHERE id = $1", attID).Scan(&until, &token); err != nil {
			t.Fatal(err)
		}
		return until, token
	}

	t.Run("임대가 살아 있으면 다른 요청은 맡지 못하고, 비우거나 끝나면 맡는다", func(t *testing.T) {
		attID := newAttachment()
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "t1", time.Minute); err != nil || !claimed {
			t.Fatalf("처음 맡기: %v %v", claimed, err)
		}
		if ref, claimed, err := store.ClaimAttachment(ctx, attID, "t2", time.Minute); err != nil || claimed || ref.Uploaded {
			t.Fatalf("임대가 살아 있으면 맡지 못한다: %v %v %+v", claimed, err, ref)
		}
		if released, err := store.ReleaseAttachment(ctx, attID, "t1"); err != nil || !released {
			t.Fatalf("주인의 해제는 임대를 비운다: %v %v", released, err)
		}
		if until, token := lease(attID); until != nil || token != nil {
			t.Fatalf("비운 임대가 남았다: %v %v", until, token)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "t3", 50*time.Millisecond); err != nil || !claimed {
			t.Fatalf("비운 임대는 바로 맡는다: %v %v", claimed, err)
		}
		time.Sleep(100 * time.Millisecond)
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "t4", time.Minute); err != nil || !claimed {
			t.Fatalf("끝난 임대는 다음 요청이 맡는다: %v %v", claimed, err)
		}
	})

	t.Run("임대가 끝난 옛 주인의 해제는 이어받은 새 주인의 임대를 지우지 않는다", func(t *testing.T) {
		attID := newAttachment()
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "old", time.Minute); err != nil || !claimed {
			t.Fatalf("옛 주인: %v %v", claimed, err)
		}
		expire(attID)
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "new", time.Minute); err != nil || !claimed {
			t.Fatalf("새 주인: %v %v", claimed, err)
		}
		if released, err := store.ReleaseAttachment(ctx, attID, "old"); err != nil || released {
			t.Fatalf("옛 주인의 해제는 아무것도 비우지 않는다: %v %v", released, err)
		}
		if until, token := lease(attID); until == nil || token == nil || *token != "new" {
			t.Fatalf("새 주인의 임대가 지워졌다: %v %v", until, token)
		}
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "third", time.Minute); err != nil || claimed {
			t.Errorf("새 주인의 임대가 살아 있으면 셋째는 맡지 못한다: %v %v", claimed, err)
		}
	})

	t.Run("임대가 끝난 옛 주인의 도장은 걸리지 않고 ErrLeaseLost 이며 새 주인의 도장은 걸린다", func(t *testing.T) {
		attID := newAttachment()
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "old", time.Minute); err != nil || !claimed {
			t.Fatalf("옛 주인: %v %v", claimed, err)
		}
		expire(attID)
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "new", time.Minute); err != nil || !claimed {
			t.Fatalf("새 주인: %v %v", claimed, err)
		}
		if _, stamped, err := store.CompleteAttachment(ctx, attID, "old", "ws/old/"+attID, "text/plain", 0, 0); !errors.Is(err, core.ErrLeaseLost) || stamped {
			t.Fatalf("옛 주인의 도장: %v %v", stamped, err)
		}
		if ref, err := store.AttachmentFile(ctx, attID); err != nil || ref.Uploaded || ref.StoragePath != "tmp/"+attID {
			t.Fatalf("옛 주인의 도장이 행을 바꿨다: %+v %v", ref, err)
		}
		if _, stamped, err := store.CompleteAttachment(ctx, attID, "new", "ws/new/"+attID, "text/plain", 0, 0); err != nil || !stamped {
			t.Fatalf("새 주인의 도장: %v %v", stamped, err)
		}
		if until, token := lease(attID); until != nil || token != nil {
			t.Errorf("도장을 찍으면 임대가 비어야 한다: %v %v", until, token)
		}
	})

	t.Run("확인된 첨부는 맡지 않고 확인된 행을 준다", func(t *testing.T) {
		attID := newAttachment()
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "t", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		if _, _, err := store.CompleteAttachment(ctx, attID, "t", "ws/a/"+attID, "text/plain", 0, 0); err != nil {
			t.Fatal(err)
		}
		ref, claimed, err := store.ClaimAttachment(ctx, attID, "u", time.Minute)
		if err != nil || claimed || !ref.Uploaded || ref.StoragePath != "ws/a/"+attID {
			t.Errorf("확인된 첨부: %v %v %+v", claimed, err, ref)
		}
		if _, _, err := store.ClaimAttachment(ctx, id.New(), "u", time.Minute); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("없는 첨부는 ErrNotFound: %v", err)
		}
	})

	t.Run("임대가 끝난 뒤 늦게 온 도장은 다른 요청이 확인한 첨부를 바꾸지 않는다", func(t *testing.T) {
		attID := newAttachment()
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "late", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		expire(attID)
		if _, claimed, err := store.ClaimAttachment(ctx, attID, "first", time.Minute); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		first, stamped, err := store.CompleteAttachment(ctx, attID, "first", "ws/first/"+attID, "image/png", 10, 20)
		if err != nil || !stamped {
			t.Fatal(stamped, err)
		}
		var uploadedAt time.Time
		if err := store.Pool().QueryRow(ctx, "SELECT uploaded_at FROM attachments WHERE id = $1", attID).Scan(&uploadedAt); err != nil {
			t.Fatal(err)
		}
		late, stamped, err := store.CompleteAttachment(ctx, attID, "late", "ws/late/"+attID, "text/plain", 0, 0)
		if err != nil || stamped {
			t.Fatalf("확인된 첨부의 늦은 도장은 그 첨부를 stamped=false 로 돌려준다: %v %v", stamped, err)
		}
		if late != first {
			t.Errorf("늦은 도장이 돌려준 첨부가 다르다: %+v → %+v", first, late)
		}
		ref, err := store.AttachmentFile(ctx, attID)
		if err != nil {
			t.Fatal(err)
		}
		var after time.Time
		if err := store.Pool().QueryRow(ctx, "SELECT uploaded_at FROM attachments WHERE id = $1", attID).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if ref.StoragePath != "ws/first/"+attID || ref.ContentType != "image/png" || ref.Width != 10 || !after.Equal(uploadedAt) {
			t.Errorf("확인된 첨부가 바뀌었다: %+v uploaded_at %v → %v", ref, uploadedAt, after)
		}
	})

	t.Run("임대를 잡는 문장이 호출자의 취소와 겹쳐도 오류를 받았는데 임대가 남는 일이 없다", func(t *testing.T) {
		// 취소가 문장이 커밋된 뒤 응답 앞에 닿으면, 떼어 내지 않은 호출자는 오류를 받고 임대는 남는다.
		// 0~0.9ms 뒤에 끝나는 ctx 로 수백 번 겹쳐 본다.
		const rounds = 300
		errWithLease, claimedN := 0, 0
		for i := 0; i < rounds; i++ {
			attID := newAttachment()
			short, cancel := context.WithTimeout(ctx, time.Duration(rand.IntN(900))*time.Microsecond)
			_, claimed, err := store.ClaimAttachment(short, attID, "t", 30*time.Second)
			cancel()
			until, _ := lease(attID)
			switch {
			case err != nil && until != nil:
				errWithLease++
			case claimed:
				claimedN++
			}
			if _, err := store.Pool().Exec(ctx, "DELETE FROM attachments WHERE id = $1", attID); err != nil {
				t.Fatal(err)
			}
		}
		if errWithLease != 0 {
			t.Errorf("%d번 중 %d번 오류를 받았는데 임대가 남았다", rounds, errWithLease)
		}
		if claimedN == 0 {
			t.Errorf("한 번도 맡지 못했다")
		}
	})
}
