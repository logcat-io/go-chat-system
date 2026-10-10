// Package pgstore 는 Postgres 스키마와 쿼리를 소유한다.
// 상위 패키지는 SQL 을 알지 못하고, 이 패키지는 WebSocket 을 알지 못한다.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// NotifyChannel 은 "대화에 변화가 있다" 신호가 흐르는 Postgres 채널이다.
// 신호에는 메시지 본문이 없다. 받는 쪽이 DB 를 다시 읽는다 (docs/design.md D1).
const NotifyChannel = "chat_changed"

var (
	errRetry                 = errors.New("pgstore: 동시에 같은 행을 고친 트랜잭션과 겹쳐 이 트랜잭션을 처음부터 다시 한다(내부 신호)")
	uniqueViolationSQLState  = "23505"
	foreignKeyViolationState = "23503"
	checkViolationState      = "23514"
)

// classify 는 저장소 오류 중 "입력이 틀린 것" 을 골라낸다.
// 이걸 안 하면 없는 상담원에게 배정하거나 빈 파일을 올렸을 때 500 이 나간다.
func classify(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case foreignKeyViolationState, checkViolationState:
		return fmt.Errorf("%w: %s", core.ErrInvalidReference, pgErr.ConstraintName)
	default:
		return err
	}
}

type Store struct {
	pool *pgxpool.Pool
	// unobserve 는 풀 게이지 등록을 푼다.
	unobserve func()
	// customerIdle 은 고객 세션(익명·회원)의 유휴 한도(나노초)다. 0 이면 없다. 세션 행이 아니라 판정 시점의 이 값으로 본다(D46, 감사 21).
	// 앱이 SetCustomerIdle 로 꽂고(조립 때 한 번, 테스트는 정책을 바꿀 때) 판정마다 읽는다.
	customerIdle atomic.Int64
}

// poolPingTimeout 은 풀에서 꺼낸 연결을 확인하는 Ping 의 시한이고, poolConnectTimeout 은 새 연결 수립의 시한이다(DEF-71).
// Ping 은 빈 문장 한 번 왕복이라 평소 수 밀리초다. 1초를 넘기면 그 연결은 죽었다고 보고 버린다 — WebSocket 경로의 읽기 시한(5초) 안에서
// 죽은 연결 여럿을 걸러 내고 새 연결로 답할 수 있어야 한다.
const (
	poolPingTimeout    = time.Second
	poolConnectTimeout = 5 * time.Second
	// poolLifetimeJitter 는 연결 수명(1시간)에 더하는 흔들림의 최대다.
	poolLifetimeJitter = 10 * time.Minute
)

func Open(ctx context.Context, url string) (*Store, error) {
	return OpenWithPool(ctx, url, 20)
}

// OpenWithPool 은 커넥션 풀 크기를 정해 연다.
// 풀이 작으면 DB 가 멀쩡해도 요청이 풀 앞에서 줄을 선다. 부하 시험에서 제일 먼저 드러나는 한계다.
func OpenWithPool(ctx context.Context, url string, maxConns int32) (*Store, error) {
	s, err := OpenLazy(ctx, url, maxConns)
	if err != nil {
		return nil, err
	}
	if err := s.pool.Ping(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("pgstore: 접속 확인 실패: %w", err)
	}
	return s, nil
}

// OpenLazy 는 DB 에 붙어 보지 않고 풀만 만든다. 연결은 처음 쓸 때 맺는다. 서버 기동이 쓴다 — DB 가 재시작 중이어도 서버는 뜨고
// /readyz 만 503 이다(D49). 접속 문자열이 틀린 것(형식)은 여기서 바로 실패한다.
func OpenLazy(ctx context.Context, url string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 접속 문자열 해석 실패: %w", err)
	}
	if maxConns < 2 {
		maxConns = 2
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	// 같은 때 연 연결이 같은 때 수명을 다해 한꺼번에 다시 붙지 않게 수명을 흔든다. 배포 직후 연 연결들이 1시간 뒤 같이 끊기는 것을 막는다.
	cfg.MaxConnLifetimeJitter = poolLifetimeJitter
	cfg.HealthCheckPeriod = 30 * time.Second
	// 장애 조치 뒤 옛 연결의 패킷이 거절되지 않고 사라지면 그 연결은 조용히 죽는다(DEF-71). 풀은 1초 넘게 쉰 연결을 꺼낼 때
	// Ping 으로 확인하는데 시한이 없으면 그 Ping 이 TCP 재전송이 포기할 때까지 멈춘다. 시한을 넘긴 연결은 버리고 다음 연결을 꺼낸다.
	// 건강 확인 주기(HealthCheckPeriod)는 쉬는 연결의 수명만 보고 Ping 하지 않는다 — 죽은 연결을 걸러 내는 것은 꺼낼 때의 Ping 이다.
	cfg.PingTimeout = poolPingTimeout
	// 새 연결 수립의 시한. 접속 문자열에 connect_timeout 이 있으면 그것을 따른다. 없으면 pgxpool 기본은 2분이다.
	if cfg.ConnConfig.ConnectTimeout <= 0 {
		cfg.ConnConfig.ConnectTimeout = poolConnectTimeout
	}
	// 쿼리마다 스팬을 연다. 부모 스팬이 없거나 내보내기가 꺼져 있으면 아무 일도 하지 않는다(D33).
	cfg.ConnConfig.Tracer = queryTracer{}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 풀 생성 실패: %w", err)
	}
	return &Store{pool: pool, unobserve: observePool(pool, telemetry.MeterProvider())}, nil
}

func (s *Store) Close() {
	if s.unobserve != nil {
		s.unobserve()
	}
	s.pool.Close()
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// inTx 는 커밋·롤백을 한 곳에서 처리한다. fn 이 errRetry 를 돌려주면 호출자가 다시 시도한다.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// fn 이 요청 취소로 실패했어도 롤백 문장은 보낸다. 취소된 ctx 로 롤백하면 실패하고, pgx 는 그 커넥션을 버린다.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != uniqueViolationSQLState {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// pageLimit 은 목록 조회의 개수를 정한다. 0 이하면 def, maxN 을 넘으면 maxN 으로 자른다(core/page.go).
func pageLimit(limit, def, maxN int) int {
	switch {
	case limit <= 0:
		return def
	case limit > maxN:
		return maxN
	}
	return limit
}
