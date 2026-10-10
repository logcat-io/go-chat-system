package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 0013 은 옛 AGENT 를 STAFF 로 옮기고, 이미 있던 계정을 승인된 것으로 본다.
// 격리 DB 를 0012 까지의 모양으로 되돌린 뒤 옛 값을 넣고 0013 을 다시 돌린다. 운영 DB 가 겪을 길과 같다.
func TestMigration0013_기존AGENT계정은STAFF로옮겨지고승인된것으로본다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()

	// 0013 이 만든 것을 걷어 낸다. 지운 열에 걸린 부분 색인은 열과 함께 사라지므로 0012 때의 색인을 다시 만든다.
	testsupport.Exec(t, dbURL, `
		DROP INDEX agents_pending_idx;
		ALTER TABLE agents DROP COLUMN approved_at;
		ALTER TABLE conversations DROP COLUMN deleted_at, DROP COLUMN deleted_by, DROP COLUMN delete_reason;
		CREATE INDEX conversations_workspace_inbox_idx ON conversations (workspace_id, last_message_at DESC NULLS LAST, created_at DESC);
		ALTER TABLE agents DROP CONSTRAINT agents_role_check;
		ALTER TABLE agent_invites DROP CONSTRAINT agent_invites_role_check;
		ALTER TABLE agents ALTER COLUMN role SET DEFAULT 'AGENT';
		DELETE FROM schema_migrations WHERE name = '0013_roles.sql'`)
	old := "00000000-0000-0000-0000-0000000000a1"
	testsupport.Exec(t, dbURL, `
		INSERT INTO agents (id, workspace_id, email, name, password_hash, role, created_at)
		VALUES ($1, $2, 'old-agent@example.com', '옛 상담원', 'x', 'AGENT', '2026-01-02T03:04:05Z')`, old, core.DefaultWorkspaceID)
	testsupport.Exec(t, dbURL, `
		INSERT INTO agent_invites (token_hash, workspace_id, email, name, role, expires_at)
		VALUES ('\x01', $1, 'old-invite@example.com', '옛 초대', 'AGENT', now() + interval '1 day')`, core.DefaultWorkspaceID)
	testsupport.Exec(t, dbURL, `
		ALTER TABLE agents ADD CONSTRAINT agents_role_check CHECK (role IN ('ADMIN', 'AGENT'));
		ALTER TABLE agent_invites ADD CONSTRAINT agent_invites_role_check CHECK (role IN ('ADMIN', 'AGENT'))`)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("0013 이 0012 상태 위에서 돌아야 한다: %v", err)
	}

	var role string
	var approved, created time.Time
	if err := store.Pool().QueryRow(ctx,
		"SELECT role, approved_at, created_at FROM agents WHERE id = $1", old).Scan(&role, &approved, &created); err != nil {
		t.Fatal(err)
	}
	if role != "STAFF" {
		t.Errorf("AGENT 는 STAFF 가 돼야 한다: %s", role)
	}
	if !approved.Equal(created) {
		t.Errorf("기존 계정은 created_at 으로 승인된 것으로 본다: approved=%v created=%v", approved, created)
	}
	var inviteRole string
	if err := store.Pool().QueryRow(ctx,
		"SELECT role FROM agent_invites WHERE email = 'old-invite@example.com'").Scan(&inviteRole); err != nil {
		t.Fatal(err)
	}
	if inviteRole != "STAFF" {
		t.Errorf("초대의 AGENT 도 STAFF 가 돼야 한다: %s", inviteRole)
	}
	var leftover int
	if err := store.Pool().QueryRow(ctx,
		"SELECT (SELECT count(*) FROM agents WHERE role = 'AGENT') + (SELECT count(*) FROM agent_invites WHERE role = 'AGENT')").Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Errorf("AGENT 가 남았다: %d", leftover)
	}
	if _, err := store.Pool().Exec(ctx, "UPDATE agents SET role = 'AGENT' WHERE id = $1", old); err == nil {
		t.Error("이제 AGENT 는 CHECK 가 거절해야 한다")
	}
	a := testsupport.Agent(t, store)
	if r, active, err := store.AgentRole(ctx, a.ID); err != nil || r != core.RoleStaff || !active {
		t.Errorf("시드·운영자 계정은 활성 STAFF 로 만들어진다: %s %v %v", r, active, err)
	}
}
