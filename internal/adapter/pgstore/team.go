package pgstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 상담원 팀 관리. 역할·활성·초대·비밀번호.

func (s *Store) ListTeam(ctx context.Context, wsID string) ([]core.TeamMember, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, email, name, role, active, created_at FROM agents
		  WHERE workspace_id = $1 AND approved_at IS NOT NULL ORDER BY created_at`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.TeamMember, 0, 8)
	for rows.Next() {
		var m core.TeamMember
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Active, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentRole 은 권한 판정에 쓴다. 비활성 상담원은 역할이 있어도 아무것도 못 한다.
func (s *Store) AgentRole(ctx context.Context, agentID string) (core.Role, bool, error) {
	var role core.Role
	var active bool
	err := s.pool.QueryRow(ctx, "SELECT role, active FROM agents WHERE id = $1", agentID).Scan(&role, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, core.ErrNotFound
	}
	return role, active, err
}

// guardLastAdmin 은 같은 트랜잭션 안에서 락을 잡고 센다. 대상이 활성 관리자이고 그가 마지막이면 거절한다.
func guardLastAdmin(ctx context.Context, tx pgx.Tx, wsID, agentID string) error {
	// 락은 워크스페이스 단위다. 다른 테넌트의 역할 변경과 줄을 서지 않는다.
	if err := lockWorkspaceStaff(ctx, tx, wsID); err != nil {
		return err
	}
	var isAdmin bool
	var admins int
	err := tx.QueryRow(ctx, `
		SELECT (role = 'ADMIN' AND active),
		       (SELECT count(*) FROM agents WHERE workspace_id = $2 AND role = 'ADMIN' AND active)
		  FROM agents WHERE id = $1 AND workspace_id = $2`, agentID, wsID).Scan(&isAdmin, &admins)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	if isAdmin && admins <= 1 {
		return core.ErrLastAdmin
	}
	return nil
}

// SetAgentRole 은 강등일 때 마지막 관리자 보호를 같은 트랜잭션에서 건다.
// 승인 대기 계정은 없는 것으로 본다. 역할은 승인할 때 정한다.
func (s *Store) SetAgentRole(ctx context.Context, wsID, agentID string, role core.Role) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if role != core.RoleAdmin {
			if err := guardLastAdmin(ctx, tx, wsID, agentID); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx,
			"UPDATE agents SET role = $2 WHERE id = $1 AND workspace_id = $3 AND approved_at IS NOT NULL", agentID, role, wsID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrNotFound
		}
		return nil
	})
}

// SetAgentActive 는 비활성화하면서 세션도 끊는다. 내보낸 사람이 토큰으로 계속 들어오면 안 된다.
// 되살릴 때도 상담원 구성 줄에 선다. 세는 한도는 없지만(D21) 강등·비활성화와 같은 줄에서 구성을 바꾼다.
func (s *Store) SetAgentActive(ctx context.Context, wsID, agentID string, active bool) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if !active {
			if err := guardLastAdmin(ctx, tx, wsID, agentID); err != nil {
				return err
			}
		} else if err := lockWorkspaceStaff(ctx, tx, wsID); err != nil {
			return err
		}
		// 승인 대기 계정은 여기서 깨우지 않는다. 깨우면 승인·감사 로그를 건너뛴 계정이 생긴다.
		tag, err := tx.Exec(ctx,
			"UPDATE agents SET active = $2 WHERE id = $1 AND workspace_id = $3 AND approved_at IS NOT NULL", agentID, active, wsID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrNotFound
		}
		if !active {
			_, err = tx.Exec(ctx, "DELETE FROM sessions WHERE agent_id = $1", agentID)
		}
		return err
	})
}

// EnsureAdmin 은 활성 관리자가 한 명도 없으면 가장 오래된 활성 상담원을 승격한다.
// 마이그레이션이 "첫 상담원을 관리자로" 올리지만 새 설치에선 그 시점에 상담원이 없다.
// 관리자가 0명인 설치는 아무도 팀을 못 들이므로, 시드 뒤에 반드시 한 번 부른다.
// 돌려주는 것은 승격한 상담원의 id 다. 기동 로그가 쓰고, 로그에는 이메일을 싣지 않는다.
func (s *Store) EnsureAdmin(ctx context.Context, wsID string) (promoted string, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE agents SET role = 'ADMIN'
		 WHERE id = (SELECT id FROM agents WHERE workspace_id = $1 AND active ORDER BY created_at LIMIT 1)
		   AND NOT EXISTS (SELECT 1 FROM agents WHERE workspace_id = $1 AND role = 'ADMIN' AND active)
		RETURNING id`, wsID).Scan(&promoted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // 이미 관리자가 있다
	}
	return promoted, err
}

// CountActiveAdmins 는 조회용이다. 마지막 관리자 보호는 guardLastAdmin 이 트랜잭션 안에서 직접 센다.
func (s *Store) CountActiveAdmins(ctx context.Context, wsID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM agents WHERE workspace_id = $1 AND role = 'ADMIN' AND active", wsID).Scan(&n)
	return n, err
}

// ChangePassword 는 비밀번호를 바꾸고 다른 세션을 전부 끊는다. 지금 세션만 남긴다.
func (s *Store) ChangePassword(ctx context.Context, agentID, password, keepToken string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE agents SET password_hash = $2 WHERE id = $1", agentID, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrNotFound
		}
		_, err = tx.Exec(ctx,
			"DELETE FROM sessions WHERE agent_id = $1 AND token_hash <> $2",
			agentID, auth.HashToken(keepToken))
		return err
	})
}

// ── 초대 ───────────────────────────────────────────────────────────

// CreateInvite 는 초대 토큰을 만든다. 같은 이메일의 미사용 초대는 새 것으로 바뀐다.
func (s *Store) CreateInvite(ctx context.Context, wsID, email, name string, role core.Role, invitedBy string, ttl time.Duration) (core.Invite, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	plain, hash := auth.NewToken()
	inv := core.Invite{WorkspaceID: wsID, Email: email, Name: name, Role: role, Token: plain}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"DELETE FROM agent_invites WHERE workspace_id = $2 AND lower(email) = $1 AND used_at IS NULL", email, wsID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO agent_invites (token_hash, workspace_id, email, name, role, invited_by, expires_at)
			VALUES ($1, $7, $2, $3, $4, nullif($5::text, '')::uuid, now() + $6::interval)
			RETURNING expires_at, created_at`,
			hash, email, name, role, invitedBy, ttl.String(), wsID,
		).Scan(&inv.ExpiresAt, &inv.CreatedAt)
	})
	if err != nil {
		return core.Invite{}, classify(err)
	}
	return inv, nil
}

func (s *Store) ListInvites(ctx context.Context, wsID string) ([]core.Invite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT email, name, role, expires_at, created_at FROM agent_invites
		 WHERE workspace_id = $1 AND used_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Invite, 0, 4)
	for rows.Next() {
		var inv core.Invite
		if err := rows.Scan(&inv.Email, &inv.Name, &inv.Role, &inv.ExpiresAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) RevokeInvite(ctx context.Context, wsID, email string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM agent_invites WHERE workspace_id = $2 AND lower(email) = lower($1) AND used_at IS NULL", email, wsID)
	return err
}

// InviteByToken 은 초대 수락 화면이 먼저 부른다. 만료·사용된 초대는 없는 것으로 본다.
func (s *Store) InviteByToken(ctx context.Context, token string) (core.Invite, error) {
	var inv core.Invite
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, email, name, role, expires_at, created_at FROM agent_invites
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()`,
		auth.HashToken(token),
	).Scan(&inv.WorkspaceID, &inv.Email, &inv.Name, &inv.Role, &inv.ExpiresAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Invite{}, core.ErrNotFound
	}
	return inv, err
}

// AcceptInvite 는 초대를 소비하고 계정을 만든다. 같은 트랜잭션이라 토큰을 두 번 쓸 수 없다.
// 상담원 구성 줄(lockWorkspaceStaff)에 서서 만든다. 상담원 수 한도는 없다(D21).
//
// 같은 워크스페이스·같은 이메일의 승인 대기 행은 지우고 초대의 역할로 새 계정을 만든다. 가입 입구는 이메일 소유를
// 확인하지 않으므로 대기 행은 남이 만든 것일 수 있다. 초대 링크는 그 이메일로 받은 사람만 연다. 승인된 계정(활성·비활성)이
// 있으면 지금처럼 ErrDuplicate 다. replacedPending 은 대기 행을 대체했는지다 — 감사 로그가 그 사실을 남긴다.
func (s *Store) AcceptInvite(ctx context.Context, token, password string) (agent core.Agent, replacedPending bool, err error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return core.Agent{}, false, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var role core.Role
		err := tx.QueryRow(ctx, `
			UPDATE agent_invites SET used_at = now()
			 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING workspace_id, email, name, role`, auth.HashToken(token),
		).Scan(&agent.WorkspaceID, &agent.Email, &agent.Name, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := lockWorkspaceStaff(ctx, tx, agent.WorkspaceID); err != nil {
			return err
		}
		deleted, err := tx.Exec(ctx,
			"DELETE FROM agents WHERE workspace_id = $1 AND lower(email) = lower($2) AND approved_at IS NULL",
			agent.WorkspaceID, agent.Email)
		if err != nil {
			return err
		}
		replacedPending = deleted.RowsAffected() > 0
		agent.ID = id.New()
		agent.Role = role
		_, err = tx.Exec(ctx,
			"INSERT INTO agents (id, workspace_id, email, name, password_hash, role, approved_at) VALUES ($1, $2, $3, $4, $5, $6, now())",
			agent.ID, agent.WorkspaceID, agent.Email, agent.Name, hash, role)
		if isUniqueViolation(err, "") {
			return core.ErrDuplicate
		}
		return err
	})
	if err != nil {
		return core.Agent{}, false, err
	}
	return agent, replacedPending, nil
}

// DeleteSession 은 로그아웃이다. 브라우저에서 토큰을 지우는 것만으로는 서버에 720시간 산다.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", auth.HashToken(token))
	return err
}

// ── 직접 가입 ──────────────────────────────────────────────────────

// CreatePendingAgent 는 가입 요청을 승인 대기 계정으로 남긴다. 만들었으면 true 다.
// 이미 있는 이메일이거나 대기가 maxPending 건에 닿았으면 아무것도 하지 않고 false 를 준다 — 호출자는 둘을 구분하지 않고
// 같은 답을 한다. 세는 것과 만드는 것이 같은 락 안이라 동시 가입이 상한을 넘지 못한다.
func (s *Store) CreatePendingAgent(ctx context.Context, p core.PendingSignup, maxPending int) (bool, error) {
	hash, err := auth.HashPassword(p.Password)
	if err != nil {
		return false, err
	}
	created := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceStaff(ctx, tx, p.WorkspaceID); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM agents WHERE workspace_id = $1 AND approved_at IS NULL", p.WorkspaceID).Scan(&pending); err != nil {
			return err
		}
		if pending >= maxPending {
			return nil
		}
		// 같은 이메일이 이미 있으면 유일 색인이 막는다. 기존 계정(활성·비활성·대기)은 이름도 비밀번호도 바뀌지 않는다.
		tag, err := tx.Exec(ctx, `
			INSERT INTO agents (id, workspace_id, email, name, password_hash, role, active, approved_at)
			VALUES ($1, $2, lower($3), $4, $5, 'PART_TIME', false, NULL)
			ON CONFLICT DO NOTHING`, id.New(), p.WorkspaceID, strings.TrimSpace(p.Email), p.Name, hash)
		if err != nil {
			return classify(err)
		}
		created = tag.RowsAffected() == 1
		return nil
	})
	return created, err
}

// ListPendingAgents 는 승인 대기 목록이다. 오래 기다린 사람이 위에 온다.
func (s *Store) ListPendingAgents(ctx context.Context, wsID string) ([]core.TeamMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, name, role, active, created_at FROM agents
		 WHERE workspace_id = $1 AND approved_at IS NULL ORDER BY created_at`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.TeamMember, 0, 4)
	for rows.Next() {
		var m core.TeamMember
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Active, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ApproveAgent 는 대기 계정을 활성으로 바꾸고 역할을 정한다. 대기 중이 아니거나 다른 워크스페이스면 ErrNotFound.
// 초대 수락·재활성화와 같은 상담원 구성 줄에 선다. 상담원 수 한도는 없다(D21).
func (s *Store) ApproveAgent(ctx context.Context, wsID, agentID string, role core.Role) (core.TeamMember, error) {
	var m core.TeamMember
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceStaff(ctx, tx, wsID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM agents WHERE id = $1 AND workspace_id = $2 AND approved_at IS NULL)",
			agentID, wsID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return core.ErrNotFound
		}
		return tx.QueryRow(ctx, `
			UPDATE agents SET active = true, approved_at = now(), role = $3
			 WHERE id = $1 AND workspace_id = $2 AND approved_at IS NULL
			RETURNING id, email, name, role, active, created_at`, agentID, wsID, role,
		).Scan(&m.ID, &m.Email, &m.Name, &m.Role, &m.Active, &m.CreatedAt)
	})
	if err != nil {
		return core.TeamMember{}, err
	}
	return m, nil
}

// RejectAgent 는 대기 계정을 지운다. 승인된 계정은 지우지 않는다 — 내보내기는 비활성화다.
// 돌려주는 이메일은 감사 로그용이다. 행이 사라진 뒤에는 누구였는지 알 길이 없다.
func (s *Store) RejectAgent(ctx context.Context, wsID, agentID string) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `
		DELETE FROM agents WHERE id = $1 AND workspace_id = $2 AND approved_at IS NULL
		RETURNING email`, agentID, wsID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", core.ErrNotFound
	}
	return email, err
}

// PendingAgentMatches 는 비밀번호가 맞는 승인 대기 계정이 있는지다. wsID 가 비면 모든 워크스페이스에서 찾는다.
// 로그인이 "틀렸다" 와 "승인을 기다린다" 를 가를 때만 쓴다. 비밀번호가 맞아야 true 라 계정 존재가 새지 않는다.
// 대기 계정이 없어도 비밀번호 비교를 한 번 한다. 응답 시간으로 "가입 신청한 이메일인가" 가 갈리지 않게 한다.
func (s *Store) PendingAgentMatches(ctx context.Context, wsID, email, password string) (bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT password_hash FROM agents
		 WHERE lower(email) = lower($1) AND approved_at IS NULL
		   AND ($2::text = '' OR workspace_id = nullif($2::text, '')::uuid)`, email, wsID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return false, err
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(hashes) == 0 {
		auth.SpendPasswordCheck(password)
	}
	for _, h := range hashes {
		if auth.CheckPassword(h, password) {
			return true, nil
		}
	}
	return false, nil
}
