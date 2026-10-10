package pgstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 설정은 키마다 JSON 한 덩어리다. 값의 모양은 app 패키지(settings.go)가 안다.

func (s *Store) Setting(ctx context.Context, wsID, key string, out any) error {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE workspace_id = $1 AND key = $2", wsID, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrSettingMissing
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (s *Store) PutSetting(ctx context.Context, wsID, key string, value any, updatedBy string) error {
	return s.PutSettings(ctx, wsID, map[string]any{key: value}, updatedBy)
}

// PutSettings 는 여러 키를 한 트랜잭션에 쓴다. 반쯤 저장된 설정이 남지 않는다.
func (s *Store) PutSettings(ctx context.Context, wsID string, values map[string]any, updatedBy string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for key, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO settings (workspace_id, key, value, updated_by)
				VALUES ($4, $1, $2, nullif($3::text, '')::uuid)
				ON CONFLICT (workspace_id, key) DO UPDATE
				   SET value = excluded.value, updated_at = now(), updated_by = excluded.updated_by`,
				key, raw, updatedBy, wsID); err != nil {
				return err
			}
		}
		return nil
	})
}
