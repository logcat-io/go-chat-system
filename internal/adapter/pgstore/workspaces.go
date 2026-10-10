package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 워크스페이스 = 테넌트 (docs/design.md D19). 설치형이라 설치 하나에 워크스페이스 하나다(D35) — 입구는 전부
// 기본 워크스페이스로 간다. 그래도 테넌트 소유 행은 전부 workspace_id 를 들고, 조회는 그 안에서만 한다.
// 여러 테넌트를 다시 받게 되면 입구만 다시 만들면 된다.

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 실패: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// newWidgetKey 는 workspaces.widget_key 칸을 채운다. 위젯 키는 2026-10-05 에 걷어냈고(D35) 아무것도 이 값을 읽지 않는다.
// 마이그레이션은 추가만 하므로 칸(NOT NULL UNIQUE)이 남아 있어 새 행에도 겹치지 않는 값을 넣는다.
func newWidgetKey() string { return "wk_" + randomHex(18) }

// NewMemberSecret 은 호스트 서버가 회원 토큰을 서명하는 비밀이다.
func NewMemberSecret() string { return randomHex(32) }

const workspaceColumns = "id, slug, name, member_secret, allowed_origins, created_at"

func scanWorkspace(row pgx.Row) (core.Workspace, error) {
	var w core.Workspace
	err := row.Scan(&w.ID, &w.Slug, &w.Name, &w.MemberSecret, &w.AllowedOrigins, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Workspace{}, core.ErrNotFound
	}
	if w.AllowedOrigins == nil {
		w.AllowedOrigins = []string{}
	}
	return w, err
}

// CreateWorkspaceWithAdmin 은 워크스페이스와 첫 관리자를 같은 트랜잭션에 만든다 —
// 관리자 없는 워크스페이스가 남으면 아무도 거기 못 들어간다. 회사 셀프 가입(POST /api/signup)을 걷어낸 뒤로(D35)
// 운영 경로는 이 함수를 부르지 않는다. 테스트가 두 번째 테넌트를 만들어 격리(I11)를 볼 때 쓴다.
func (s *Store) CreateWorkspaceWithAdmin(ctx context.Context, slug, name, email, agentName, password string) (core.Workspace, core.Agent, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return core.Workspace{}, core.Agent{}, err
	}
	w := core.Workspace{ID: id.New(), Slug: slug, Name: name, MemberSecret: NewMemberSecret()}
	a := core.Agent{ID: id.New(), WorkspaceID: w.ID, Email: strings.ToLower(email), Name: agentName, Role: core.RoleAdmin}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspaces (id, slug, name, widget_key, member_secret)
			VALUES ($1, $2, $3, $4, $5)`, w.ID, w.Slug, w.Name, newWidgetKey(), w.MemberSecret); err != nil {
			if isUniqueViolation(err, "workspaces_slug_key") {
				return core.ErrSlugTaken
			}
			return classify(err)
		}
		_, err := tx.Exec(ctx,
			"INSERT INTO agents (id, workspace_id, email, name, password_hash, role, approved_at) VALUES ($1, $2, $3, $4, $5, 'ADMIN', now())",
			a.ID, a.WorkspaceID, a.Email, a.Name, hash)
		return classify(err)
	})
	if err != nil {
		return core.Workspace{}, core.Agent{}, err
	}
	ws, err := s.Workspace(ctx, w.ID)
	return ws, a, err
}

func (s *Store) Workspace(ctx context.Context, wsID string) (core.Workspace, error) {
	return scanWorkspace(s.pool.QueryRow(ctx, "SELECT "+workspaceColumns+" FROM workspaces WHERE id = $1", wsID))
}

func (s *Store) UpdateWorkspace(ctx context.Context, wsID, name string, allowedOrigins []string) (core.Workspace, error) {
	if allowedOrigins == nil {
		allowedOrigins = []string{}
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE workspaces SET name = $2, allowed_origins = $3, updated_at = now() WHERE id = $1",
		wsID, name, allowedOrigins)
	if err != nil {
		return core.Workspace{}, classify(err)
	}
	if tag.RowsAffected() == 0 {
		return core.Workspace{}, core.ErrNotFound
	}
	return s.Workspace(ctx, wsID)
}

// RotateMemberSecret 은 새 비밀을 돌려준다. 옛 비밀로 서명한 회원 토큰은 그 순간부터 전부 거절된다.
// 같은 트랜잭션에서 그 워크스페이스 회원 고객의 세션을 모두 지운다(D46). 비밀을 새로 받는 까닭은 대개 옛 비밀이 샜다는 것이고,
// 그러면 샌 비밀로 위조한 회원 토큰으로 이미 열어 둔 세션도 죽어야 한다. 회원은 다음 페이지에서 새 토큰으로 다시 들어온다.
// 익명 고객과 상담원의 세션은 건드리지 않는다.
func (s *Store) RotateMemberSecret(ctx context.Context, wsID string) (string, error) {
	secret := NewMemberSecret()
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE workspaces SET member_secret = $2, updated_at = now() WHERE id = $1", wsID, secret)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrNotFound
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM sessions s USING customers c
			 WHERE s.customer_id = c.id AND c.workspace_id = $1 AND c.external_id IS NOT NULL`, wsID)
		return err
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// SetMemberSecret 은 운영자가 환경변수로 준 비밀을 기본 워크스페이스에 싣는 용도다.
func (s *Store) SetMemberSecret(ctx context.Context, wsID, secret string) error {
	_, err := s.pool.Exec(ctx, "UPDATE workspaces SET member_secret = $2, updated_at = now() WHERE id = $1", wsID, secret)
	return err
}

// FillEmptyMemberSecrets 는 비어 있는 비밀을 채운다. 마이그레이션은 안전한 난수를 못 만들어 부팅이 한다.
// defaultSecret 이 있으면 기본 워크스페이스에만 그 값을 쓴다 — 단, 비어 있을 때 한 번뿐이다.
// 매 부팅마다 덮어쓰면 관리자가 화면에서 돌린 비밀이 재시작 때 되살아난다.
func (s *Store) FillEmptyMemberSecrets(ctx context.Context, defaultSecret string) (int64, error) {
	rows, err := s.pool.Query(ctx, "SELECT id FROM workspaces WHERE member_secret = ''")
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var wsID string
		if err := rows.Scan(&wsID); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, wsID)
	}
	rows.Close()
	// 태스크 여럿이 같이 뜨면 모두 같은 빈 행을 본다. 쓰기는 아직 비어 있을 때만 하고, 실제로 바꾼 행만 센다.
	// 먼저 쓴 쪽이 이기고 뒤에 온 쪽은 아무것도 바꾸지 않는다. 사이에 관리자가 돌린 비밀도 덮지 않는다.
	var filled int64
	for _, wsID := range ids {
		secret := NewMemberSecret()
		if wsID == core.DefaultWorkspaceID && defaultSecret != "" {
			secret = defaultSecret
		}
		tag, err := s.pool.Exec(ctx,
			"UPDATE workspaces SET member_secret = $2, updated_at = now() WHERE id = $1 AND member_secret = ''", wsID, secret)
		if err != nil {
			return 0, err
		}
		filled += tag.RowsAffected()
	}
	return filled, nil
}

// ── 감사 로그 ──────────────────────────────────────────────────────

// Audit 은 실패해도 본 작업을 되돌리지 않는다. 기록이 빠지는 것보다 설정 변경이 막히는 게 더 나쁘다.
func (s *Store) Audit(ctx context.Context, e core.AuditEntry) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	raw, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO audit_log (workspace_id, actor_id, actor_name, action, target, detail)
		VALUES ($1, nullif($2::text, '')::uuid, $3, $4, $5, $6)`,
		e.WorkspaceID, e.ActorID, e.ActorName, e.Action, e.Target, raw)
	return err
}

func (s *Store) AuditLog(ctx context.Context, wsID string, limit int) ([]core.AuditEntry, error) {
	limit = pageLimit(limit, core.AuditPageDefault, core.AuditPageMax)
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(actor_id::text, ''), actor_name, action, target, detail, created_at
		  FROM audit_log WHERE workspace_id = $1
		 ORDER BY id DESC LIMIT $2`, wsID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.AuditEntry, 0, 32)
	for rows.Next() {
		var e core.AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorName, &e.Action, &e.Target, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── 내보내기 ───────────────────────────────────────────────────────

// EachConversation 은 워크스페이스의 대화를 오래된 순으로 하나씩 넘긴다. 전체를 메모리에 올리지 않는다.
func (s *Store) EachConversation(ctx context.Context, wsID string, fn func(core.Conversation) error) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM conversations WHERE workspace_id = $1 ORDER BY created_at, id`, wsID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var convID string
		if err := rows.Scan(&convID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, convID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, convID := range ids {
		conv, err := s.Conversation(ctx, convID)
		if err != nil {
			return err
		}
		if err := fn(conv); err != nil {
			return err
		}
	}
	return nil
}

// ── 고객 삭제(익명화) ──────────────────────────────────────────────

// AnonymizeCustomer 는 개인정보를 지우되 대화 번호는 그대로 둔다 (불변식 I1 — 빈칸을 만들지 않는다).
// 돌려주는 경로는 호출자가 디스크에서 지울 첨부 파일이다.
func (s *Store) AnonymizeCustomer(ctx context.Context, wsID, customerID string) (paths []string, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE customers
			   SET display_name = '삭제된 고객', email = NULL, phone = NULL, note = '',
			       attributes = '{}'::jsonb, external_id = NULL, anonymized_at = now()
			 WHERE id = $1 AND workspace_id = $2`, customerID, wsID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return core.ErrNotFound
		}
		if _, err := tx.Exec(ctx,
			"UPDATE messages SET body = '(삭제된 고객의 메시지)' WHERE sender_customer = $1 AND body <> ''", customerID); err != nil {
			return err
		}
		// 고치기 전 원문도 개인정보다. 이력의 본문까지 지워야 "지웠다" 가 참이다.
		if _, err := tx.Exec(ctx, `
			UPDATE message_revisions r SET body = '(삭제된 고객의 메시지)'
			  FROM messages m WHERE m.id = r.message_id AND m.sender_customer = $1`, customerID); err != nil {
			return err
		}
		// 만족도의 한 줄 의견도 고객이 쓴 글이다. 점수는 남긴다 — 집계가 쓰고 사람을 가리키지 않는다(D45).
		if _, err := tx.Exec(ctx,
			"UPDATE conversation_ratings SET comment = '' WHERE customer_id = $1 AND comment <> ''", customerID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			DELETE FROM attachments WHERE uploader_kind = 'CUSTOMER' AND uploader_id = $1
			RETURNING storage_path`, customerID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			paths = append(paths, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM sessions WHERE customer_id = $1", customerID)
		return err
	})
	return paths, err
}
