package app

import (
	"context"
	"errors"
	"fmt"
)

// SchemaStore 는 이 코드가 기대하는 스키마가 DB 에 다 있는지 알려 준다(D49). 읽기만 한다 — 서버는 스키마를 바꾸지 않는다.
// 마이그레이션은 배포의 앞 단계(`server migrate`)가 돌린다.
type SchemaStore interface {
	// PendingMigrations 는 이 코드에 있는데 DB 에 적용되지 않은 마이그레이션 이름이다. DB 에만 있는 것(더 새 코드)은 세지 않는다.
	PendingMigrations(ctx context.Context) ([]string, error)
}

// ErrSchemaBehind 는 DB 스키마가 이 코드보다 뒤에 있다는 뜻이다. /readyz 가 503 schema_behind 로 답한다.
var ErrSchemaBehind = errors.New("app: DB 스키마가 이 코드보다 뒤에 있다(server migrate 가 아직 돌지 않았다)")

// UseSchema 는 스키마 확인 저장소를 꽂는다. 꽂지 않으면 준비 확인은 DB 핑만 본다(테스트 하네스).
func (a *App) UseSchema(store SchemaStore) { a.Queries.schema = store }

// PendingMigrations 는 적용되지 않은 마이그레이션 이름이다. 스키마 저장소가 없으면 없다.
func (s *Queries) PendingMigrations(ctx context.Context) ([]string, error) {
	if s.schema == nil {
		return nil, nil
	}
	return s.schema.PendingMigrations(ctx)
}

// Ready 는 저장소가 살아 있고 스키마가 이 코드를 따라왔는지다. 스키마를 확인하는 질의 하나가 DB 핑을 겸한다.
func (s *Queries) Ready(ctx context.Context) error {
	if s.schema == nil {
		return s.read.Ping(ctx)
	}
	pending, err := s.schema.PendingMigrations(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: %d개", ErrSchemaBehind, len(pending))
	}
	return nil
}
