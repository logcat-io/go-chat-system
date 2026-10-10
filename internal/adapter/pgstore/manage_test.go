package pgstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 관리 화면 셋(D36)의 저장. 모든 쓰기는 workspace_id 안에서만 걸린다(불변식 I11).

func newWorkspace(t *testing.T, store *pgstore.Store) (core.Workspace, core.Agent) {
	t.Helper()
	ws, admin, err := store.CreateWorkspaceWithAdmin(context.Background(), "ws-"+id.New()[28:], "관리 화면 상점",
		"owner-"+id.New()[28:]+"@x.com", "대표", "pw-12345678")
	if err != nil {
		t.Fatal(err)
	}
	return ws, admin
}

func TestWidgetFAQ_위젯FAQ는워크스페이스마다따로저장된다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	a, adminA := newWorkspace(t, store)
	b, _ := newWorkspace(t, store)
	items := []core.FAQItem{{Question: "둘째로 보일 것", Answer: "B"}, {Question: "첫째", Answer: "A"}}
	if err := store.PutWidgetFAQ(ctx, a.ID, items, adminA.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.WidgetFAQ(ctx, a.ID)
	if err != nil || !slices.Equal(got, items) {
		t.Errorf("A 는 저장한 순서 그대로다: %v %v", got, err)
	}
	if _, err := store.WidgetFAQ(ctx, b.ID); !errors.Is(err, core.ErrSettingMissing) {
		t.Errorf("B 는 저장한 적이 없다(ErrSettingMissing): %v", err)
	}
	if err := store.PutWidgetFAQ(ctx, b.ID, []core.FAQItem{}, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := store.WidgetFAQ(ctx, b.ID); err != nil || got == nil || len(got) != 0 {
		t.Errorf("B 의 빈 목록은 저장된 빈 목록이다: %v %v", got, err)
	}
	if got, _ := store.WidgetFAQ(ctx, a.ID); !slices.Equal(got, items) {
		t.Errorf("B 의 저장이 A 를 건드리면 안 된다: %v", got)
	}
}

func TestManage_다른워크스페이스의상담원이름과빠른답변은바꿀수없다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	a, adminA := newWorkspace(t, store)
	b, _ := newWorkspace(t, store)

	if _, err := store.RenameAgent(ctx, b.ID, adminA.ID, "남이 바꾼 이름"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("다른 워크스페이스의 상담원은 ErrNotFound: %v", err)
	}
	if got, _ := store.AgentByID(ctx, adminA.ID); got.Name != "대표" {
		t.Errorf("이름이 바뀌면 안 된다: %q", got.Name)
	}
	if prev, err := store.RenameAgent(ctx, a.ID, adminA.ID, "새 대표"); err != nil || prev != "대표" {
		t.Errorf("같은 워크스페이스에서는 바뀌고 옛 이름을 돌려준다: %q %v", prev, err)
	}

	snA, err := store.CreateSnippet(ctx, a.ID, "hello", "인사", "안녕하세요")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSnippet(ctx, b.ID, snA.ID, "x", "x", "x"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("다른 워크스페이스의 빠른 답변은 ErrNotFound: %v", err)
	}
	// 단축어 겹침도 워크스페이스 안에서만 센다.
	snB, err := store.CreateSnippet(ctx, b.ID, "other", "다른", "다른 답")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSnippet(ctx, b.ID, snB.ID, "HELLO", "인사", "B 의 인사"); err != nil {
		t.Errorf("다른 워크스페이스의 단축어와는 겹치지 않는다: %v", err)
	}
	list, _ := store.ListSnippets(ctx, a.ID)
	if len(list) != 1 || list[0].Body != "안녕하세요" {
		t.Errorf("A 의 빠른 답변은 그대로다: %v", list)
	}
}
