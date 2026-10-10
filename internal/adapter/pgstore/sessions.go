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

func (s *Store) CreateAgent(ctx context.Context, wsID, email, name, password string) (core.Agent, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return core.Agent{}, err
	}
	// 운영자가 직접 만드는 계정(시드·테스트)은 내부 직원이고 이미 승인된 것이다. 가입 입구의 계정은 CreatePendingAgent 가 만든다.
	a := core.Agent{ID: id.New(), WorkspaceID: wsID, Email: strings.ToLower(email), Name: name, Role: core.RoleStaff}
	_, err = s.pool.Exec(ctx,
		"INSERT INTO agents (id, workspace_id, email, name, password_hash, role, approved_at) VALUES ($1, $2, $3, $4, $5, $6, now())",
		a.ID, a.WorkspaceID, a.Email, a.Name, hash, a.Role)
	if isUniqueViolation(err, "") {
		return core.Agent{}, core.ErrDuplicate
	}
	if err != nil {
		return core.Agent{}, classify(err)
	}
	return a, nil
}

// EnsureAgent 는 seed 상담원을 만들거나 이미 있으면 그대로 돌려준다. 비밀번호를 덮어쓰지 않는다.
func (s *Store) EnsureAgent(ctx context.Context, wsID, email, name, password string) (core.Agent, error) {
	a, err := s.AgentByEmail(ctx, wsID, email)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return core.Agent{}, err
	}
	a, err = s.CreateAgent(ctx, wsID, email, name, password)
	if errors.Is(err, core.ErrDuplicate) {
		return s.AgentByEmail(ctx, wsID, email)
	}
	return a, err
}

func (s *Store) AgentByEmail(ctx context.Context, wsID, email string) (core.Agent, error) {
	var a core.Agent
	err := s.pool.QueryRow(ctx,
		"SELECT id, workspace_id, email, name, role FROM agents WHERE workspace_id = $1 AND lower(email) = lower($2)",
		wsID, email,
	).Scan(&a.ID, &a.WorkspaceID, &a.Email, &a.Name, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Agent{}, core.ErrNotFound
	}
	return a, err
}

// AgentByID 는 id 로 활성 상담원을 찾는다. 비밀번호 변경이 현재 비밀번호를 다시 확인할 때 쓴다.
func (s *Store) AgentByID(ctx context.Context, agentID string) (core.Agent, error) {
	var a core.Agent
	err := s.pool.QueryRow(ctx,
		"SELECT id, workspace_id, email, name, role FROM agents WHERE id = $1 AND active", agentID,
	).Scan(&a.ID, &a.WorkspaceID, &a.Email, &a.Name, &a.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Agent{}, core.ErrNotFound
	}
	return a, err
}

// VerifyAgent 는 워크스페이스 안에서 이메일·비밀번호를 확인한다. 실패 이유를 구분해 주지 않는다.
// 계정이 없어도 비밀번호 비교를 한 번 한다. 응답 시간으로 계정이 있는지 갈리지 않게 한다.
func (s *Store) VerifyAgent(ctx context.Context, wsID, email, password string) (core.Agent, error) {
	var a core.Agent
	var hash string
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, email, name, role, password_hash FROM agents
		 WHERE workspace_id = $1 AND lower(email) = lower($2) AND active`, wsID, email,
	).Scan(&a.ID, &a.WorkspaceID, &a.Email, &a.Name, &a.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.SpendPasswordCheck(password)
		return core.Agent{}, core.ErrNotFound
	}
	if err != nil {
		return core.Agent{}, err
	}
	if !auth.CheckPassword(hash, password) {
		return core.Agent{}, core.ErrNotFound
	}
	return a, nil
}

// CreateSession 은 평문 토큰을 돌려준다. 저장은 해시만 한다. 소속은 주체에서 따라온다.
// ttl 은 절대 수명이고 idle 은 유휴 한도다(D46). 0 이면 그 만료가 없다 — 둘 다 0 이면 로그아웃·삭제로만 끝난다(익명 고객 세션, 0019).
// 행의 idle 은 상담원 세션만 판정에 쓴다. 고객 세션의 유휴 한도는 판정 시점의 설정(SetCustomerIdle)이다.
func (s *Store) CreateSession(ctx context.Context, kind, agentID, customerID string, ttl, idle time.Duration) (string, error) {
	plain, hash := auth.NewToken()
	interval := func(d time.Duration) *string {
		if d <= 0 {
			return nil
		}
		v := d.String()
		return &v
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, subject_kind, agent_id, customer_id, workspace_id, expires_at, idle_for)
		SELECT $1, $2, nullif($3::text, '')::uuid, nullif($4::text, '')::uuid, ws.id, now() + $5::interval, $6::interval
		  FROM (SELECT workspace_id AS id FROM agents    WHERE id = nullif($3::text, '')::uuid
		        UNION ALL
		        SELECT workspace_id        FROM customers WHERE id = nullif($4::text, '')::uuid) ws
		 LIMIT 1`,
		hash, kind, agentID, customerID, interval(ttl), interval(idle))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", core.ErrNotFound
	}
	return plain, nil
}

// SetCustomerIdle 은 고객 세션(익명·회원)의 유휴 한도를 꽂는다. 0 이하면 없다. 판정·정리가 그때그때 이 값을 읽는다 —
// 켜면 이미 있는 고객 세션 모두에 걸린다(행에 적힌 한도를 보지 않는다, 감사 21). 앱의 SetSessionPolicy 가 부른다.
func (s *Store) SetCustomerIdle(d time.Duration) { s.customerIdle.Store(int64(max(d, 0))) }

// customerIdleParam 은 SQL 에 넘길 고객 유휴 한도다. 없으면 NULL 이다.
func (s *Store) customerIdleParam() *string {
	d := time.Duration(s.customerIdle.Load())
	if d <= 0 {
		return nil
	}
	v := d.String()
	return &v
}

// SubjectByToken 은 토큰으로 주체를 찾는다. 만료·유휴 만료된 세션은 없는 것으로 본다.
func (s *Store) SubjectByToken(ctx context.Context, token string) (core.Subject, error) {
	return s.subjectByHash(ctx, auth.HashToken(token))
}

// sessionIdleSQL 은 그 세션 행에 걸리는 유휴 한도다. 고객 세션은 판정 시점의 설정($n), 상담원 세션은 행의 idle_for 다. 비면 유휴 만료가 없다.
func sessionIdleSQL(alias, param string) string {
	return "CASE " + alias + ".subject_kind WHEN 'CUSTOMER' THEN " + param + "::interval ELSE " + alias + ".idle_for END"
}

// subjectByHash 는 세션 해시로 주체를 찾는다. 토큰·티켓·세션 참조 세 길이 여기 하나로 모인다.
// 살아 있는 세션은 셋 다 맞아야 한다: 절대 수명 안, 유휴 한도 안(D46), 상담원이면 활성. 수명·한도가 비어 있으면 그 만료는 없다(익명 고객, 0019).
// 고객 세션의 유휴 한도는 행이 아니라 지금 설정이다(sessionIdleSQL) — 설정을 켜면 그 전에 연 세션에도 걸린다(감사 21).
func (s *Store) subjectByHash(ctx context.Context, hash []byte) (core.Subject, error) {
	var sub core.Subject
	var agentID, customerID, name, role *string
	err := s.pool.QueryRow(ctx, `
		SELECT s.subject_kind, s.workspace_id, s.agent_id, s.customer_id,
		       COALESCE(a.name, c.display_name), a.role, c.external_id IS NOT NULL, s.last_used_at
		  FROM sessions s
		  LEFT JOIN agents    a ON a.id = s.agent_id
		  LEFT JOIN customers c ON c.id = s.customer_id
		 WHERE s.token_hash = $1 AND (s.expires_at IS NULL OR s.expires_at > now())
		   AND ((`+sessionIdleSQL("s", "$2")+`) IS NULL OR s.last_used_at + (`+sessionIdleSQL("s", "$2")+`) > now())
		   AND (s.agent_id IS NULL OR a.active)`, hash, s.customerIdleParam(),
	).Scan(&sub.Kind, &sub.WorkspaceID, &agentID, &customerID, &name, &role, &sub.Member, &sub.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Subject{}, core.ErrNotFound
	}
	if err != nil {
		return core.Subject{}, err
	}
	sub.Session = sessionRef(hash)
	if agentID != nil {
		sub.AgentID = *agentID
	}
	if customerID != nil {
		sub.CustomerID = *customerID
	}
	if name != nil {
		sub.Name = *name
	}
	if role != nil {
		sub.Role = core.Role(*role)
	}
	return sub, nil
}

// DeleteExpiredSessions 는 절대 수명이나 유휴 한도가 지난 세션과 만료된 WebSocket 티켓을 치운다. 주기 실행용이다.
// 수명·한도가 비어 있는 세션(익명 고객, 0019)은 비교가 NULL 이라 지우지 않는다. 유휴 한도는 판정과 같다(sessionIdleSQL).
// 지운 세션 수를 돌려준다. 세션을 지우면 트리거가 그 세션들을 알린다(0018) — 아직 열린 연결이 있으면 곧바로 닫힌다.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM sessions s
		 WHERE s.expires_at <= now() OR s.last_used_at + (`+sessionIdleSQL("s", "$1")+`) <= now()`, s.customerIdleParam())
	if err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM ws_tickets WHERE expires_at <= now()"); err != nil {
		return tag.RowsAffected(), err
	}
	return tag.RowsAffected(), nil
}
