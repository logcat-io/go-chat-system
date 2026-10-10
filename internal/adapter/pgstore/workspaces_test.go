package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func TestWorkspace_OriginAllowed(t *testing.T) {
	open := core.Workspace{}
	if !open.OriginAllowed("https://anything.example") {
		t.Error("허용 목록이 비어 있으면 전부 받는다")
	}
	locked := core.Workspace{AllowedOrigins: []string{"https://shop.example.com"}}
	if !locked.OriginAllowed("https://Shop.Example.com/") || locked.OriginAllowed("https://evil.example") {
		t.Error("목록이 있으면 대소문자·꼬리 슬래시만 너그럽고 나머지는 거절한다")
	}
}

func TestWorkspace_슬러그는유일하다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	slug := "ws-" + id.New()[28:]
	if _, _, err := store.CreateWorkspaceWithAdmin(ctx, slug, "첫째", "a@x.com", "A", "pw-12345678"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateWorkspaceWithAdmin(ctx, slug, "둘째", "b@x.com", "B", "pw-12345678"); !errors.Is(err, core.ErrSlugTaken) {
		t.Errorf("같은 슬러그는 ErrSlugTaken 이어야 한다: %v", err)
	}
}

// 가입은 워크스페이스와 첫 관리자가 한 트랜잭션이다. 관리자가 못 생기면 워크스페이스도 남지 않는다.
func TestCreateWorkspaceWithAdmin_관리자가실패하면워크스페이스도남지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	slug := "ws-" + id.New()[28:]
	// NUL 바이트는 Postgres 가 text 에 못 넣는다. agents INSERT 만 실패하게 만드는 가장 작은 방법이다.
	_, _, err := store.CreateWorkspaceWithAdmin(ctx, slug, "실패 상점", "bad\x00@x.com", "A", "pw-12345678")
	if err == nil {
		t.Fatal("실패해야 한다")
	}
	var left int
	if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM workspaces WHERE slug = $1", slug).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("관리자 없는 워크스페이스가 남았다: %d", left)
	}
}

// 회원 서명 비밀은 비어 있을 때 한 번만 채운다. 관리자가 돌린 비밀을 재시작이 되돌리면 안 된다.
func TestFillEmptyMemberSecrets_비어있을때만채운다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	ctx := context.Background()
	if n, err := store.FillEmptyMemberSecrets(ctx, "from-env"); err != nil || n != 1 {
		t.Fatalf("새 설치의 기본 워크스페이스 하나를 채워야 한다: n=%d err=%v", n, err)
	}
	ws, _ := store.Workspace(ctx, core.DefaultWorkspaceID)
	if ws.MemberSecret != "from-env" {
		t.Fatalf("비어 있으면 환경변수 값을 쓴다: %q", ws.MemberSecret)
	}
	rotated, err := store.RotateMemberSecret(ctx, core.DefaultWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := store.FillEmptyMemberSecrets(ctx, "from-env"); n != 0 {
		t.Errorf("이미 값이 있으면 건드리지 않는다: n=%d", n)
	}
	ws, _ = store.Workspace(ctx, core.DefaultWorkspaceID)
	if ws.MemberSecret != rotated {
		t.Errorf("돌린 비밀이 환경변수로 되돌아갔다: %q", ws.MemberSecret)
	}
}

// 운영은 태스크 둘 이상이 같이 뜬다(ECS). 둘 다 빈 비밀을 보고 각자 채우면 뒤에 쓴 쪽이 앞을 덮는다 —
// 앞의 값으로 이미 서명한 회원 토큰이 있으면 그 순간 거절되고, 사이에 관리자가 돌린 비밀도 덮인다.
func TestFillEmptyMemberSecrets_여러서버가동시에떠도회원서명비밀은한번만채워진다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	other, _, err := store.CreateWorkspaceWithAdmin(ctx, "ws-"+id.New()[24:], "둘째", "b@x.com", "B", "pw-12345678")
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Exec(t, dbURL, "UPDATE workspaces SET member_secret = '' WHERE id = $1", other.ID)

	const servers = 8
	filled := make([]int64, servers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := store.FillEmptyMemberSecrets(ctx, fmt.Sprintf("env-%d", i))
			if err != nil {
				t.Error(err)
			}
			filled[i] = n
		}()
	}
	close(start)
	wg.Wait()

	var total int64
	for _, n := range filled {
		total += n
	}
	if total != 2 {
		t.Errorf("빈 비밀 둘은 합쳐서 정확히 두 번 채워져야 한다: 서버별 %v", filled)
	}
	def, _ := store.Workspace(ctx, core.DefaultWorkspaceID)
	if def.MemberSecret == "" || !strings.HasPrefix(def.MemberSecret, "env-") {
		t.Fatalf("기본 워크스페이스 비밀: %q", def.MemberSecret)
	}
	// 기본 워크스페이스를 채웠다고 말한 서버가 하나뿐이고 남은 값이 그 서버의 값이다.
	var claimed []int
	for i, n := range filled {
		if n > 0 && def.MemberSecret == fmt.Sprintf("env-%d", i) {
			claimed = append(claimed, i)
		}
	}
	if len(claimed) != 1 {
		t.Errorf("남은 값 %q 를 쓴 서버가 채웠다고 보고하지 않았다: 서버별 %v", def.MemberSecret, filled)
	}
	if ws, _ := store.Workspace(ctx, other.ID); ws.MemberSecret == "" {
		t.Error("둘째 워크스페이스가 비어 있다")
	}
}

func TestAnonymizeCustomer_다른고객은건드리지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	victimID, victimConv := testsupport.Conversation(t, store)
	otherID, otherConv := testsupport.Conversation(t, store)
	for _, c := range []struct{ conv, cust string }{{victimConv.ID, victimID}, {otherConv.ID, otherID}} {
		if _, err := store.AppendMessage(ctx, core.AppendParams{
			ConversationID: c.conv, ClientMsgID: id.New(), SenderKind: core.SenderCustomer,
			SenderCustomer: c.cust, Visibility: core.Public, Body: "개인정보 010",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AnonymizeCustomer(ctx, core.DefaultWorkspaceID, victimID); err != nil {
		t.Fatal(err)
	}
	msgs, _ := store.ListMessages(ctx, otherConv.ID, 0, 10, true)
	if len(msgs) != 1 || msgs[0].Body != "개인정보 010" {
		t.Errorf("다른 고객의 메시지가 바뀌었다: %+v", msgs)
	}
	if _, err := store.AnonymizeCustomer(ctx, "00000000-0000-0000-0000-000000000099", otherID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("다른 워크스페이스에서 지우려 하면 ErrNotFound: %v", err)
	}
}
