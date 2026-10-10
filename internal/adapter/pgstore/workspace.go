package pgstore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// ── 태그 ────────────────────────────────────────────────────────────

func (s *Store) ListTags(ctx context.Context, wsID string) ([]core.Tag, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, name FROM tags WHERE workspace_id = $1 ORDER BY name", wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Tag, 0, 16)
	for rows.Next() {
		var t core.Tag
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EnsureTag 는 이름으로 태그를 찾고 없으면 만든다. 대소문자는 구분하지 않는다.
func (s *Store) EnsureTag(ctx context.Context, wsID, name string) (core.Tag, error) {
	name = strings.TrimSpace(name)
	t, err := s.tagByName(ctx, wsID, name)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return core.Tag{}, err
	}
	t = core.Tag{ID: id.New(), Name: name}
	_, err = s.pool.Exec(ctx, "INSERT INTO tags (id, workspace_id, name) VALUES ($1, $2, $3)", t.ID, wsID, t.Name)
	if isUniqueViolation(err, "") {
		return s.tagByName(ctx, wsID, name)
	}
	return t, err
}

func (s *Store) tagByName(ctx context.Context, wsID, name string) (core.Tag, error) {
	var t core.Tag
	err := s.pool.QueryRow(ctx,
		"SELECT id, name FROM tags WHERE workspace_id = $1 AND lower(name) = lower($2)", wsID, name).Scan(&t.ID, &t.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Tag{}, core.ErrNotFound
	}
	return t, err
}

func (s *Store) ConversationTags(ctx context.Context, convID string) ([]core.Tag, error) {
	return conversationTags(ctx, s.pool, convID)
}

func conversationTags(ctx context.Context, q querier, convID string) ([]core.Tag, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.name FROM conversation_tags ct
		  JOIN tags t ON t.id = ct.tag_id
		 WHERE ct.conversation_id = $1 ORDER BY t.name`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Tag, 0, 4)
	for rows.Next() {
		var t core.Tag
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddConversationTag 는 대화에 태그를 붙인다. 삭제된 대화에는 붙이지 않고 ErrNotFound 다.
// 대화 행을 먼저 바꿔 행 잠금을 잡는다. 그 뒤로는 삭제가 끼어들지 못하고, 앞서 커밋된 삭제는 UPDATE 가 다시 보고 걸러 낸다.
func (s *Store) AddConversationTag(ctx context.Context, convID, tagID string) error {
	var notice string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if notice, err = touchConversation(ctx, tx, convID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversation_tags (conversation_id, tag_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, convID, tagID); err != nil {
			return classify(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.notify(ctx, notice)
	return nil
}

func (s *Store) RemoveConversationTag(ctx context.Context, convID, tagID string) error {
	var notice string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 태그가 이 대화의 워크스페이스 것이 아니면 없는 것으로 본다. 붙어 있지 않은 자기 태그는 조용히 지나간다.
		var sameWorkspace bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM tags t JOIN conversations c ON c.workspace_id = t.workspace_id
			                WHERE t.id = $2 AND c.id = $1)`, convID, tagID).Scan(&sameWorkspace); err != nil {
			return err
		}
		if !sameWorkspace {
			return core.ErrNotFound
		}
		if _, err := tx.Exec(ctx,
			"DELETE FROM conversation_tags WHERE conversation_id = $1 AND tag_id = $2", convID, tagID); err != nil {
			return err
		}
		var err error
		notice, err = touchConversation(ctx, tx, convID)
		return err
	})
	if err != nil {
		return err
	}
	s.notify(ctx, notice)
	return nil
}

// touchConversation 은 메시지 없이 대화가 바뀌었을 때 updated_at 을 올리고, 커밋 뒤 보낼 알림을 만든다.
// updated_at 을 올려야 구독자와 안전망이 "메타가 바뀌었다"를 알아챈다. 삭제된 대화는 ErrNotFound 다.
func touchConversation(ctx context.Context, tx pgx.Tx, convID string) (string, error) {
	var seq, publicSeq int64
	var wsID string
	if err := tx.QueryRow(ctx,
		"UPDATE conversations SET updated_at = clock_timestamp() WHERE id = $1 AND deleted_at IS NULL RETURNING last_seq, last_public_seq, workspace_id",
		convID).Scan(&seq, &publicSeq, &wsID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", core.ErrNotFound
		}
		return "", err
	}
	return changedNotice(convID, wsID, seq, publicSeq, true)
}

// ── 팔로워 ──────────────────────────────────────────────────────────

func (s *Store) Followers(ctx context.Context, convID string) ([]core.Agent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.email, a.name FROM conversation_followers f
		  JOIN agents a ON a.id = f.agent_id
		 WHERE f.conversation_id = $1 ORDER BY a.name`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Agent, 0, 4)
	for rows.Next() {
		var a core.Agent
		if err := rows.Scan(&a.ID, &a.Email, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Follow 는 상담원을 팔로워로 단다. 삭제된 대화에는 달지 않고 ErrNotFound 다.
// 대화 행을 FOR SHARE 로 잡는다. 삭제의 UPDATE 와 부딪혀서, 앞서 커밋된 삭제는 다시 보고 걸러 내고 뒤에 온 삭제는 기다리게 한다.
func (s *Store) Follow(ctx context.Context, convID, agentID string) error {
	var live int
	err := s.pool.QueryRow(ctx, `
		WITH c AS (
		  SELECT id FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR SHARE
		), ins AS (
		  INSERT INTO conversation_followers (conversation_id, agent_id)
		  SELECT c.id, $2::text::uuid FROM c ON CONFLICT DO NOTHING
		)
		SELECT count(*) FROM c`, convID, agentID).Scan(&live)
	if err != nil {
		return classify(err)
	}
	if live == 0 {
		return core.ErrNotFound
	}
	return nil
}

func (s *Store) Unfollow(ctx context.Context, convID, agentID string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM conversation_followers WHERE conversation_id = $1 AND agent_id = $2", convID, agentID)
	return err
}

// ── 빠른 답변 ───────────────────────────────────────────────────────

func (s *Store) ListSnippets(ctx context.Context, wsID string) ([]core.Snippet, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, shortcut, title, body FROM snippets WHERE workspace_id = $1 ORDER BY shortcut", wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Snippet, 0, 16)
	for rows.Next() {
		var sn core.Snippet
		if err := rows.Scan(&sn.ID, &sn.Shortcut, &sn.Title, &sn.Body); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) CreateSnippet(ctx context.Context, wsID, shortcut, title, body string) (core.Snippet, error) {
	sn := core.Snippet{ID: id.New(), Shortcut: shortcut, Title: title, Body: body}
	_, err := s.pool.Exec(ctx,
		"INSERT INTO snippets (id, workspace_id, shortcut, title, body) VALUES ($1, $2, $3, $4, $5)",
		sn.ID, wsID, sn.Shortcut, sn.Title, sn.Body)
	if isUniqueViolation(err, "snippets_shortcut_idx") {
		return core.Snippet{}, core.ErrDuplicate
	}
	return sn, err
}

func (s *Store) DeleteSnippet(ctx context.Context, wsID, snippetID string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM snippets WHERE id = $1 AND workspace_id = $2", snippetID, wsID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

// EnsureSnippet 은 seed 용이다. 이미 있으면 그대로 둔다.
func (s *Store) EnsureSnippet(ctx context.Context, wsID, shortcut, title, body string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO snippets (id, workspace_id, shortcut, title, body) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, id.New(), wsID, shortcut, title, body)
	return err
}

// ── 고객 프로필 ─────────────────────────────────────────────────────

func (s *Store) Customer(ctx context.Context, customerID string) (core.Customer, error) {
	var c core.Customer
	var email, phone, externalID *string
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.workspace_id, c.external_id, c.display_name, c.email, c.phone, c.note, c.attributes,
		       c.created_at,
		       (SELECT count(*) FROM conversations v WHERE v.customer_id = c.id)
		  FROM customers c WHERE c.id = $1`, customerID,
	).Scan(&c.ID, &c.WorkspaceID, &externalID, &c.DisplayName, &email, &phone, &c.Note, &c.Attributes, &c.CreatedAt, &c.Conversations)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Customer{}, core.ErrNotFound
	}
	if err != nil {
		return core.Customer{}, err
	}
	c.Email, c.Phone, c.ExternalID = deref(email), deref(phone), deref(externalID)
	if c.Attributes == nil {
		c.Attributes = map[string]any{}
	}
	return c, nil
}

// UpsertMember 는 회원 id 로 고객을 찾고, 없으면 만든다. 이름·연락처는 호스트 값으로 맞춘다.
// 호스트가 진실이다. 고객이 위젯에서 고친 이름은 다음 로그인 때 호스트 값으로 돌아간다.
func (s *Store) UpsertMember(ctx context.Context, wsID string, m core.MemberProfile) (string, error) {
	var customerID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO customers (id, workspace_id, external_id, display_name, email, phone, last_seen_at)
		VALUES ($1, $6, $2, $3, nullif($4::text, ''), nullif($5::text, ''), now())
		ON CONFLICT (workspace_id, external_id) WHERE external_id IS NOT NULL DO UPDATE
		   SET display_name = excluded.display_name,
		       email        = COALESCE(excluded.email, customers.email),
		       phone        = COALESCE(excluded.phone, customers.phone),
		       last_seen_at = now()
		RETURNING id`,
		id.New(), m.ExternalID, m.DisplayName, m.Email, m.Phone, wsID,
	).Scan(&customerID)
	return customerID, classify(err)
}

// UpdateCustomer 는 빈 값을 건너뛰지 않는다. 호출자가 보낸 그대로 덮어쓴다.
func (s *Store) UpdateCustomer(ctx context.Context, customerID string, p core.CustomerProfile) (core.Customer, error) {
	attrs := p.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE customers
		   SET display_name = $2,
		       email      = nullif($3::text, ''),
		       phone      = nullif($4::text, ''),
		       note       = $5,
		       attributes = $6
		 WHERE id = $1`, customerID, p.DisplayName, p.Email, p.Phone, p.Note, attrs)
	if err != nil {
		return core.Customer{}, err
	}
	if tag.RowsAffected() == 0 {
		return core.Customer{}, core.ErrNotFound
	}
	return s.Customer(ctx, customerID)
}

func (s *Store) TouchCustomer(ctx context.Context, customerID string) error {
	_, err := s.pool.Exec(ctx, "UPDATE customers SET last_seen_at = now() WHERE id = $1", customerID)
	return err
}

// ── 검색 ────────────────────────────────────────────────────────────

// SearchMessages 는 본문 부분 일치로 찾는다. 한국어 형태소 분석은 쓰지 않는다.
// pg_trgm 인덱스가 붙어 있어 짧은 질의에서도 쓸 만하다.
func (s *Store) SearchMessages(ctx context.Context, wsID, query string, internal bool, limit int) ([]core.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []core.SearchHit{}, nil
	}
	limit = pageLimit(limit, core.SearchPageDefault, core.SearchPageMax)
	filter := ""
	if !internal {
		filter = " AND m.visibility = 'PUBLIC'"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.conversation_id, cu.display_name, m.seq, m.body,
		       COALESCE(a.name, c.display_name, CASE m.sender_kind WHEN 'AI' THEN 'AI 상담원' ELSE '시스템' END),
		       m.created_at
		  FROM messages m
		  JOIN conversations v ON v.id = m.conversation_id
		  JOIN customers cu ON cu.id = v.customer_id
		  LEFT JOIN agents a ON a.id = m.sender_agent_id
		  LEFT JOIN customers c ON c.id = m.sender_customer
		 WHERE v.workspace_id = $3 AND v.deleted_at IS NULL AND m.kind = 'MESSAGE' AND m.deleted_at IS NULL
		   AND m.body ILIKE '%' || $1 || '%'`+filter+`
		 ORDER BY m.created_at DESC
		 LIMIT $2`, query, limit, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.SearchHit, 0, 16)
	for rows.Next() {
		var h core.SearchHit
		if err := rows.Scan(&h.ConversationID, &h.CustomerName, &h.Seq, &h.Body, &h.SenderName, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ── 상담원 목록 ─────────────────────────────────────────────────────

func (s *Store) ListAgents(ctx context.Context, wsID string) ([]core.Agent, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, email, name FROM agents WHERE workspace_id = $1 AND active ORDER BY name", wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Agent, 0, 8)
	for rows.Next() {
		var a core.Agent
		if err := rows.Scan(&a.ID, &a.Email, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
