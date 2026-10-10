package pgstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 관리자가 0명인 설치는 아무도 팀을 못 들인다. 새 설치가 그 상태로 시작하면 안 된다.
func TestEnsureAdmin(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()

	t.Run("관리자가 있으면 아무도 승격하지 않는다", func(t *testing.T) {
		a := testsupport.Agent(t, store)
		if err := store.SetAgentRole(ctx, core.DefaultWorkspaceID, a.ID, core.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		promoted, err := store.EnsureAdmin(ctx, core.DefaultWorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if promoted != "" {
			t.Errorf("이미 관리자가 있는데 %s 를 승격했다", promoted)
		}
	})

	t.Run("활성 관리자가 없으면 가장 오래된 활성 상담원을 승격한다", func(t *testing.T) {
		oldest := testsupport.Agent(t, store)
		second := testsupport.Agent(t, store)
		// 저장소는 마지막 관리자 강등을 거절하므로 "관리자 0명" 은 SQL 로 만든다. 새 설치가 이 상태다.
		testsupport.Exec(t, dbURL, "UPDATE agents SET role = 'STAFF', active = (id = $1 OR id = $2)", oldest.ID, second.ID)
		promoted, err := store.EnsureAdmin(ctx, core.DefaultWorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if promoted == "" {
			t.Fatal("아무도 승격되지 않았다")
		}
		role, _, err := store.AgentRole(ctx, oldest.ID)
		if err != nil {
			t.Fatal(err)
		}
		if role != core.RoleAdmin {
			t.Errorf("가장 오래된 활성 상담원이 관리자여야 한다: promoted=%s", promoted)
		}
	})
}

// 두 관리자가 서로를 동시에 강등하면 "세고 나서 바꾸기" 로는 둘 다 통과해 관리자가 0명이 된다.
func TestSetAgentRole_마지막관리자보호는동시요청에도지켜진다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	a := testsupport.Agent(t, store)
	b := testsupport.Agent(t, store)

	for round := 0; round < 20; round++ {
		for _, id := range []string{a.ID, b.ID} {
			if err := store.SetAgentRole(ctx, core.DefaultWorkspaceID, id, core.RoleAdmin); err != nil {
				t.Fatal(err)
			}
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, id := range []string{a.ID, b.ID} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = store.SetAgentRole(ctx, core.DefaultWorkspaceID, id, core.RoleStaff)
			}()
		}
		wg.Wait()

		n, err := store.CountActiveAdmins(ctx, core.DefaultWorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if n < 1 {
			t.Fatalf("%d번째: 활성 관리자가 0명이 됐다", round)
		}
		refused := 0
		for _, e := range errs {
			if errors.Is(e, core.ErrLastAdmin) {
				refused++
			} else if e != nil {
				t.Fatalf("예상 밖 오류: %v", e)
			}
		}
		if refused != 1 {
			t.Fatalf("%d번째: 하나는 되고 하나는 거절돼야 한다. 거절 %d건, 관리자 %d명", round, refused, n)
		}
	}

	t.Run("비활성화도 같은 보호를 받는다", func(t *testing.T) {
		// a 만 관리자로 남긴다.
		if err := store.SetAgentRole(ctx, core.DefaultWorkspaceID, a.ID, core.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if err := store.SetAgentRole(ctx, core.DefaultWorkspaceID, b.ID, core.RoleStaff); err != nil {
			t.Fatal(err)
		}
		if err := store.SetAgentActive(ctx, core.DefaultWorkspaceID, a.ID, false); !errors.Is(err, core.ErrLastAdmin) {
			t.Errorf("마지막 관리자 비활성화는 ErrLastAdmin 이어야 한다: %v", err)
		}
		if err := store.SetAgentActive(ctx, core.DefaultWorkspaceID, b.ID, false); err != nil {
			t.Errorf("관리자가 아닌 사람은 비활성화된다: %v", err)
		}
	})
}
