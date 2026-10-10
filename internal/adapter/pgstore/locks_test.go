package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 자문 잠금은 키 숫자가 같아야 같은 줄에 선다. 줄을 서야 하는 경로들이 실제로 같은 잠금을 기다리는지,
// 테스트가 그 잠금을 먼저 쥐고 확인한다. 키 숫자는 정책이라 여기서 리터럴로 고정한다.
func TestAdvisoryLocks_같은줄에선다(t *testing.T) {
	type result struct {
		what string
		err  error
	}
	// hold 는 lockSQL 로 잠금을 쥔 채 paths 를 띄우고, 잠금을 놓기 전에는 아무것도 끝나지 않았는지 본다.
	hold := func(t *testing.T, begin func(context.Context) (pgx.Tx, error), lockSQL string, lockArg any, paths map[string]func() error) {
		t.Helper()
		ctx := context.Background()
		tx, err := begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, lockSQL, lockArg); err != nil {
			t.Fatal(err)
		}
		results := make(chan result, len(paths))
		for what, run := range paths {
			go func() { results <- result{what, run()} }()
		}
		time.Sleep(300 * time.Millisecond)
		if early := len(results); early != 0 {
			t.Errorf("%d개가 잠금을 기다리지 않고 끝났다", early)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for range paths {
			select {
			case r := <-results:
				if r.err != nil {
					t.Errorf("%s: 잠금이 풀린 뒤에는 끝나야 한다: %v", r.what, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("잠금이 풀린 뒤에도 끝나지 않았다")
			}
		}
	}

	t.Run("회원 잠금을 쥔 동안 상담 열기·다시 열기·가리기·되살리기가 기다린다", func(t *testing.T) {
		store := testsupport.Store(t)
		ctx := context.Background()
		agent := testsupport.Agent(t, store)
		// 셋 다 같은 고객의 상담이다. 열린 상담(open)이 끝까지 남아 있어 상담 열기는 순서와 무관하게 새 대화를 만들지 않는다.
		customerID, open := testsupport.Conversation(t, store)
		closed, err := store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetConversationState(ctx, closed.ID, core.StateClosed, nil, nil); err != nil {
			t.Fatal(err)
		}
		toDelete, err := store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		toRestore, err := store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteConversation(ctx, toRestore.ID, agent.ID, "되살릴 상담"); err != nil {
			t.Fatal(err)
		}

		hold(t, store.Pool().Begin, "SELECT pg_advisory_xact_lock(7302, hashtext($1::text))", customerID, map[string]func() error{
			"상담 열기": func() error {
				_, _, err := store.OpenMemberConversation(ctx, core.DefaultWorkspaceID, customerID)
				return err
			},
			"다시 열기": func() error {
				_, err := store.SetConversationState(ctx, closed.ID, core.StateOpen, nil, nil)
				return err
			},
			"가리기":  func() error { return store.DeleteConversation(ctx, toDelete.ID, agent.ID, "가릴 상담") },
			"되살리기": func() error { return store.RestoreConversation(ctx, toRestore.ID) },
		})
		if _, err := store.Conversation(ctx, open.ID); err != nil {
			t.Errorf("열린 상담은 그대로다: %v", err)
		}
	})

	t.Run("대화의 첨부 잠금을 쥔 동안 첨부 신청이 기다린다", func(t *testing.T) {
		store := testsupport.Store(t)
		ctx := context.Background()
		customerID, conv := testsupport.Conversation(t, store)
		attID := id.New()
		hold(t, store.Pool().Begin, "SELECT pg_advisory_xact_lock(7303, hashtext($1::text))", conv.ID, map[string]func() error{
			"첨부 신청": func() error {
				_, err := store.CreateAttachment(ctx, core.NewAttachment{ID: attID, ConversationID: conv.ID, UploaderKind: "CUSTOMER",
					UploaderID: customerID, Filename: "a.txt", ContentType: "text/plain", ByteSize: 5, StoragePath: "tmp/" + attID}, 10)
				return err
			},
		})
	})

	t.Run("워크스페이스 인원 잠금을 쥔 동안 관리자 강등과 재활성화가 기다린다", func(t *testing.T) {
		store := testsupport.Store(t)
		ctx := context.Background()
		staff := testsupport.Agent(t, store)
		// 강등은 마지막 관리자를 남기는 동안만 된다. 빈 DB 에서도 다른 활성 관리자가 있도록 하나를 더 세운다.
		keeper := testsupport.Agent(t, store)
		promoted := testsupport.Agent(t, store)
		for _, a := range []core.Agent{keeper, promoted} {
			if err := store.SetAgentRole(ctx, core.DefaultWorkspaceID, a.ID, core.RoleAdmin); err != nil {
				t.Fatal(err)
			}
		}
		hold(t, store.Pool().Begin, "SELECT pg_advisory_xact_lock(7301, hashtext($1))", core.DefaultWorkspaceID, map[string]func() error{
			"관리자 강등": func() error {
				return store.SetAgentRole(ctx, core.DefaultWorkspaceID, promoted.ID, core.RoleStaff)
			},
			"재활성화": func() error {
				return store.SetAgentActive(ctx, core.DefaultWorkspaceID, staff.ID, true)
			},
		})
	})
}
