package pgstore

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// AppendInTx 는 테스트가 쥔 트랜잭션 안에서 메시지 한 건을 넣는다. 커밋은 테스트가 한다.
// 두 저장이 같은 대화에서 겹치는 순간(한쪽이 행을 쥔 채 다른 쪽이 기다린다)을 정확히 만들 때 쓴다(DEF-103). 테스트 빌드에만 있다.
func AppendInTx(ctx context.Context, tx pgx.Tx, p core.AppendParams) (core.AppendResult, error) {
	if p.Visibility == "" {
		p.Visibility = core.Public // AppendMessage 와 같다
	}
	res, _, err := appendOnce(ctx, tx, p)
	return res, err
}

// CloseByCustomerInTx 는 테스트가 쥔 트랜잭션 안에서 고객 종료 한 번을 한다(D48). 커밋은 테스트가 한다.
// 종료가 대화 행을 쥔 채 커밋하지 않은 동안 상담원 답·고객 글이 그 행을 기다리게 만들 때 쓴다. 테스트 빌드에만 있다.
func CloseByCustomerInTx(ctx context.Context, tx pgx.Tx, convID, customerID string) (core.Conversation, error) {
	conv, _, err := changeOnce(ctx, tx, convID, nil, closeByCustomer(ctx, customerID), nil)
	return conv, err
}

// MigrateFrom 은 리포 대신 fsys 의 dir 에 있는 .sql 파일로 러너를 돌린다(D49). 잠금 대기·트랜잭션 없는 파일·INVALID 인덱스처럼
// 리포의 파일로는 만들 수 없는 상황을 가짜 파일로 만든다. 테스트 빌드에만 있다.
func (s *Store) MigrateFrom(ctx context.Context, fsys fs.FS, dir string, o MigrateOptions) error {
	return migrate(ctx, s.pool, fsys, dir, o.normalized())
}
