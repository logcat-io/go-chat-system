package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// anyDeletion 은 어떤 대화에도 삭제 기록이 있다고 답하는 저장소다. 판정이 저장소의 거름에 기대지 않고 app 에 있는지 본다.
type anyDeletion struct{ *pgstore.Store }

func (anyDeletion) ConversationDeletion(context.Context, string) (core.Deletion, error) {
	return core.Deletion{DeletedAt: time.Now(), DeletedByName: "가짜", Reason: "가짜"}, nil
}

// 삭제하지 않은 상담은 삭제 상담 보기로 열리지 않는다. 저장소가 삭제 기록을 돌려주더라도 app 이 대화의 deleted_at 으로 거절한다.
// HTTP 로는 저장소의 거름(deleted_at IS NOT NULL)에 같이 막혀 이 판정만 따로 보이지 않는다(TestE2E_삭제한상담보기).
func TestDeletedConversation_삭제하지않은상담은저장소가삭제기록을돌려줘도열리지않는다(t *testing.T) {
	store := testsupport.Store(t)
	svc := app.New(anyDeletion{store})
	_, conv := testsupport.Conversation(t, store)
	admin := app.Actor{Kind: app.ActorAgent, AgentID: testsupport.Agent(t, store).ID, Role: core.RoleAdmin, WorkspaceID: conv.WorkspaceID}

	_, _, msgs, err := svc.Queries.DeletedConversation(context.Background(), admin, conv.ID, 0, 0)
	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("산 상담이 삭제 보기로 열렸다: err=%v, 메시지 %d개", err, len(msgs))
	}
}
