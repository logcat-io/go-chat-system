package pgstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 관리 화면 셋(D36)의 저장. 포트는 app 의 ports_manage.go 다.

// RenameAgent 는 승인된 상담원의 이름을 바꾸고 바꾸기 전 이름을 돌려준다. 행 잠금 아래 옛 이름을 읽어 같은 문장에서 바꾼다.
// 메시지의 발신자 이름은 저장하지 않고 읽을 때 agents 에서 붙이므로(messageColumns) 이미 보낸 메시지도 새 이름으로 읽힌다.
func (s *Store) RenameAgent(ctx context.Context, wsID, agentID, name string) (string, error) {
	var previous string
	err := s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT id, name FROM agents
			 WHERE id = $1 AND workspace_id = $2 AND approved_at IS NOT NULL
			   FOR UPDATE
		)
		UPDATE agents a SET name = $3 FROM old WHERE a.id = old.id
		RETURNING old.name`, agentID, wsID, name).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", core.ErrNotFound
	}
	return previous, err
}

// UpdateSnippet 은 빠른 답변 하나를 고친다. 단축어 겹침은 만들 때와 같은 unique 인덱스(워크스페이스, 소문자 단축어)가 막는다.
func (s *Store) UpdateSnippet(ctx context.Context, wsID, snippetID, shortcut, title, body string) (core.Snippet, error) {
	var sn core.Snippet
	err := s.pool.QueryRow(ctx, `
		UPDATE snippets SET shortcut = $3, title = $4, body = $5
		 WHERE id = $1 AND workspace_id = $2
		RETURNING id, shortcut, title, body`, snippetID, wsID, shortcut, title, body,
	).Scan(&sn.ID, &sn.Shortcut, &sn.Title, &sn.Body)
	if isUniqueViolation(err, "snippets_shortcut_idx") {
		return core.Snippet{}, core.ErrDuplicate
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Snippet{}, core.ErrNotFound
	}
	return sn, err
}

// WidgetFAQ 는 저장된 목록이다. 행이 없으면 core.ErrSettingMissing — 기본 목록을 쓰라는 뜻이다.
func (s *Store) WidgetFAQ(ctx context.Context, wsID string) ([]core.FAQItem, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT items FROM widget_faq WHERE workspace_id = $1", wsID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrSettingMissing
	}
	if err != nil {
		return nil, err
	}
	items := []core.FAQItem{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// PutWidgetFAQ 는 목록을 통째로 바꾼다. 한 문장이라 반쯤 바뀐 목록이 보이지 않는다.
func (s *Store) PutWidgetFAQ(ctx context.Context, wsID string, items []core.FAQItem, updatedBy string) error {
	if items == nil {
		items = []core.FAQItem{}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO widget_faq (workspace_id, items, updated_by)
		VALUES ($1, $2, nullif($3::text, '')::uuid)
		ON CONFLICT (workspace_id) DO UPDATE
		   SET items = excluded.items, updated_at = now(), updated_by = excluded.updated_by`,
		wsID, raw, updatedBy)
	return err
}
