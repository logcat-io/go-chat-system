package pgstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 마이그레이션은 서버 기동이 아니라 배포의 앞 단계(`server migrate`)가 돌린다(D49). 서버는 스키마를 읽기만 한다(PendingMigrations).
// 러너가 지키는 것:
//   - 잠금을 오래 기다리지 않는다. 트랜잭션 파일마다 lock_timeout(기본 3초)을 걸고, 못 잡으면(55P03) 물러났다가 다시 한다. ALTER 가 잠금 앞에서
//     기다리는 동안 그 뒤의 SELECT 까지 줄을 서기 때문이다 — 시한이 없던 때 단순 SELECT 가 11.9초 멈췄다(docs/perf/deploy-db.md).
//     트랜잭션 없는 파일(CONCURRENTLY)에는 lock_timeout 을 걸지 않는다(0). CONCURRENTLY 가 기다리는 것은 오래된 트랜잭션이 끝나기이고,
//     그동안 그 표의 읽기·쓰기는 줄을 서지 않는다. 시한을 걸면 무해한 대기가 INVALID 인덱스를 남기는 실패가 된다(감사 31, DEF-192).
//   - 문장이 끝없이 돌지 않는다. 트랜잭션 파일에는 statement_timeout(기본 60초)을 건다. 트랜잭션 없는 파일의 상한은 배포 단계의 시한
//     (ECS 태스크·GitHub Actions job)이다.
//   - 동시에 둘이 돌지 않는다. 세션 자문 잠금을 pg_try_advisory_lock 으로 잡고, 한도(기본 60초) 안에 못 잡으면 실패한다.
//   - 머리에 `-- migrate: no-transaction` 이 있는 파일은 트랜잭션 없이 문장 하나씩 돈다. CREATE INDEX CONCURRENTLY 는 이렇게만 된다.

//go:embed all:migrations
var migrationFS embed.FS

// migrationLockKey 는 마이그레이션 자문 잠금의 키다("chat").
const migrationLockKey = int64(0x6368_6174)

// NoTransactionDirective 가 파일 머리의 주석 줄에 있으면 그 파일은 트랜잭션 없이 문장 하나씩 돈다.
const NoTransactionDirective = "-- migrate: no-transaction"

// 러너 기본값이다. 설정(CHAT_MIGRATE_*)이 바꾼다. 근거는 design.md D49.
const (
	// DefaultMigrateLockTimeout 은 DDL 이 잠금을 기다리는 한도다. 그동안 그 표의 읽기·쓰기가 DDL 뒤에 줄을 선다.
	// WebSocket 저장 경로의 읽기 시한(5초)보다 짧게 둬, 줄 선 저장이 시한에 걸리기 전에 DDL 이 물러난다.
	DefaultMigrateLockTimeout = 3 * time.Second
	// DefaultMigrateStatementTimeout 은 트랜잭션 파일의 문장 하나의 한도다. 규칙(D49)대로 짧은 DDL 만 두면 밀리초 단위다.
	DefaultMigrateStatementTimeout = time.Minute
	// DefaultMigrateRetries 는 잠금 실패(55P03) 뒤 다시 하는 횟수다. 첫 시도와 합쳐 6번이다. 설정(CHAT_MIGRATE_RETRIES)의 기본값이다.
	DefaultMigrateRetries = 5
	// DefaultMigrateAdvisoryWait 는 다른 migrate 가 자문 잠금을 쥐고 있을 때 기다리는 한도다.
	DefaultMigrateAdvisoryWait = time.Minute
	// defaultMigrateBackoff 는 첫 재시도 전 대기다. 두 배씩 늘고 maxMigrateBackoff 에서 멈춘다. ±20% 흔든다.
	defaultMigrateBackoff = time.Second
	maxMigrateBackoff     = 10 * time.Second
	advisoryPoll          = 500 * time.Millisecond
)

// lockNotAvailable 은 lock_timeout 이 지나 잠금을 못 잡은 오류다.
const lockNotAvailable = "55P03"

// MigrateOptions 는 러너의 시한과 재시도다. 시한(Duration)은 0 이면 기본값을 쓴다. Retries 는 0 이 값이다(다시 하지 않는다).
type MigrateOptions struct {
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	// Retries 는 잠금 실패(55P03) 뒤 다시 하는 횟수다. 0 이면 첫 시도로 끝낸다(CHAT_MIGRATE_RETRIES=0). 음수는 0 으로 본다.
	// 기본값(5)은 부르는 쪽이 넘긴다 — Migrate 와 설정이 DefaultMigrateRetries 를 쓴다.
	Retries      int
	AdvisoryWait time.Duration
	// RetryBackoff 는 첫 재시도 전 대기다. 테스트가 줄인다.
	RetryBackoff time.Duration
	// Log 가 nil 이면 남기지 않는다.
	Log *slog.Logger
}

func (o MigrateOptions) normalized() MigrateOptions {
	if o.LockTimeout <= 0 {
		o.LockTimeout = DefaultMigrateLockTimeout
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = DefaultMigrateStatementTimeout
	}
	o.Retries = max(o.Retries, 0)
	if o.AdvisoryWait <= 0 {
		o.AdvisoryWait = DefaultMigrateAdvisoryWait
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = defaultMigrateBackoff
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

// MigrationError 는 마이그레이션 파일 하나가 실패한 것이다. 로그(server.exit)가 File 을 싣는다(DEF-85) — 파일 이름은
// 리포에 고정된 이름(migrations/*.sql)이라 접속 정보나 입력이 아니다(D30). 원인은 Err 의 SQLSTATE(err_kind)로 본다.
type MigrationError struct {
	File string
	Err  error
}

func (e *MigrationError) Error() string {
	return "pgstore: 마이그레이션 " + e.File + " 을 적용하지 못했다(그 파일만 되돌렸고 앞의 파일은 커밋됐다): " + e.Err.Error()
}

func (e *MigrationError) Unwrap() error { return e.Err }

// ErrMigrationLockBusy 는 다른 migrate 가 자문 잠금을 한도보다 오래 쥐고 있었다는 뜻이다.
var ErrMigrationLockBusy = errors.New("pgstore: 다른 마이그레이션이 잠금을 쥐고 있어 기다리다 한도를 넘었다")

// ErrInvalidIndex 는 트랜잭션 없는 파일이 실패하고 INVALID 인덱스가 남았다는 뜻이다. 그 인덱스를 DROP INDEX CONCURRENTLY 로 지운 뒤 다시 돌린다.
var ErrInvalidIndex = errors.New("pgstore: CREATE INDEX CONCURRENTLY 가 실패해 INVALID 인덱스가 남았다")

// Migrate 는 리포의 마이그레이션을 기본 시한·기본 재시도로 적용한다. 테스트가 부른다. 서버 기동은 부르지 않는다(D49).
func (s *Store) Migrate(ctx context.Context) error {
	return s.MigrateWith(ctx, MigrateOptions{Retries: DefaultMigrateRetries})
}

// MigrateWith 는 migrations/*.sql 을 이름 순서로 한 번씩 적용한다.
func (s *Store) MigrateWith(ctx context.Context, o MigrateOptions) error {
	return migrate(ctx, s.pool, migrationFS, "migrations", o.normalized())
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string, o MigrateOptions) error {
	started := time.Now()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	c := conn.Conn().PgConn()

	if err := acquireAdvisory(ctx, conn, o); err != nil {
		return err
	}
	defer func() {
		// ctx 가 취소돼도 잠금과 세션 설정은 되돌린다. 이 연결은 풀로 돌아간다(테스트가 같은 풀을 쓴다).
		bg := context.WithoutCancel(ctx)
		_, _ = conn.Exec(bg, "SELECT pg_advisory_unlock($1)", migrationLockKey)
		_, _ = conn.Exec(bg, "RESET lock_timeout")
		_, _ = conn.Exec(bg, "RESET statement_timeout")
	}()
	// 세션 전체에 lock_timeout 을 건다. 트랜잭션 파일은 SET LOCAL 로 한 번 더 걸고, 트랜잭션 없는 파일은 그동안만 0 으로 푼다.
	if _, err := conn.Exec(ctx, "SELECT set_config('lock_timeout', $1, false), set_config('statement_timeout', $2, false)",
		pgDuration(o.LockTimeout), pgDuration(o.StatementTimeout)); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("pgstore: schema_migrations 생성 실패: %w", err)
	}

	names, err := sqlNames(fsys, dir)
	if err != nil {
		return err
	}
	applied, err := appliedNames(ctx, conn)
	if err != nil {
		return err
	}
	var pending []string
	for _, n := range names {
		if !applied[n] {
			pending = append(pending, n)
		}
	}
	o.Log.Info("migrate.started", "pending", len(pending), "applied", len(names)-len(pending))
	for _, name := range pending {
		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return err
		}
		if err := applyFile(ctx, c, name, string(body), o); err != nil {
			return err
		}
	}
	o.Log.Info("migrate.done", "applied_now", len(pending), "ms", time.Since(started).Milliseconds())
	return nil
}

// acquireAdvisory 는 자문 잠금을 기다리지 않고 잡아 본다. 다른 migrate 가 쥐고 있으면 한도까지 짧게 쉬며 다시 묻는다.
// pg_advisory_lock 으로 기다리면 시한이 없다 — 앞의 migrate 가 멈추면 이 배포도 함께 멈춘다.
func acquireAdvisory(ctx context.Context, conn *pgxpool.Conn, o MigrateOptions) error {
	deadline := time.Now().Add(o.AdvisoryWait)
	logged := false
	for {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&ok); err != nil {
			return fmt.Errorf("pgstore: 마이그레이션 잠금 확인 실패: %w", err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrMigrationLockBusy
		}
		if !logged {
			o.Log.Info("migrate.lock_wait", "wait_ms", o.AdvisoryWait.Milliseconds())
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(advisoryPoll):
		}
	}
}

func appliedNames(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		applied[n] = true
	}
	return applied, rows.Err()
}

// applyFile 은 파일 하나를 적용한다. 잠금을 못 잡으면 물러났다가 다시 한다.
func applyFile(ctx context.Context, c *pgconn.PgConn, name, body string, o MigrateOptions) error {
	noTx := HasNoTransactionDirective(body)
	mode := "tx"
	if noTx {
		mode = "no_tx"
	}
	o.Log.Info("migrate.file_started", "migration", name, "mode", mode)
	started := time.Now()
	var attempts int
	var err error
	if noTx {
		attempts, err = applyNoTx(ctx, c, name, body, o)
	} else {
		attempts, err = applyTx(ctx, c, name, body, o)
	}
	if err != nil {
		o.Log.Warn("migrate.file_failed", "migration", name, "attempts", attempts, "ms", time.Since(started).Milliseconds(),
			"err_kind", logctx.ErrKind(err))
		var mig *MigrationError
		if errors.As(err, &mig) {
			return err
		}
		return &MigrationError{File: name, Err: err}
	}
	o.Log.Info("migrate.file_applied", "migration", name, "attempts", attempts, "ms", time.Since(started).Milliseconds())
	return nil
}

// applyTx 는 파일 전체를 트랜잭션 하나로 돌리고 같은 트랜잭션에서 기록한다. 55P03 이면 롤백하고 다시 한다.
func applyTx(ctx context.Context, c *pgconn.PgConn, name, body string, o MigrateOptions) (int, error) {
	for attempt := 1; ; attempt++ {
		err := execTx(ctx, c, name, body, o)
		if err == nil {
			return attempt, nil
		}
		if !isLockTimeout(err) || attempt > o.Retries {
			return attempt, err
		}
		if err := backoff(ctx, name, attempt, err, o); err != nil {
			return attempt, err
		}
	}
}

func execTx(ctx context.Context, c *pgconn.PgConn, name, body string, o MigrateOptions) error {
	if err := c.Exec(ctx, "BEGIN").Close(); err != nil {
		return err
	}
	rollback := func() { _ = c.Exec(context.WithoutCancel(ctx), "ROLLBACK").Close() }
	setLocal := fmt.Sprintf("SET LOCAL lock_timeout = '%s'; SET LOCAL statement_timeout = '%s'",
		pgDuration(o.LockTimeout), pgDuration(o.StatementTimeout))
	if _, err := c.Exec(ctx, setLocal).ReadAll(); err != nil {
		rollback()
		return err
	}
	if _, err := c.Exec(ctx, body).ReadAll(); err != nil {
		rollback()
		return err
	}
	if err := c.ExecParams(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", [][]byte{[]byte(name)}, nil, nil, nil).Read().Err; err != nil {
		rollback()
		return err
	}
	_, err := c.Exec(ctx, "COMMIT").ReadAll()
	return err
}

// applyNoTx 는 문장 하나씩 자동 커밋으로 돈다. 린트가 이 파일에 CONCURRENTLY 문장(CREATE·DROP·REINDEX INDEX)만 두게 한다.
// CONCURRENTLY 는 오래 걸려도 표의 읽기·쓰기를 막지 않으므로 statement_timeout 도 lock_timeout 도 걸지 않는다(둘 다 0).
// 이 문장이 기다리는 것은 둘이다. 표의 SHARE UPDATE EXCLUSIVE 잠금(일반 읽기·쓰기의 잠금과 겹치지 않아 그 뒤에 아무도 줄을 서지
// 않는다)과, 시작 전에 연 트랜잭션들이 끝나기(가상 트랜잭션 잠금 — 다른 표를 읽는 긴 트랜잭션도 기다린다). 전에는 lock_timeout(3초)이
// 이 두 번째 대기에도 걸려, 아무도 막지 않는 대기가 3초 만에 55P03 으로 실패하고 INVALID 인덱스를 남겼다(감사 31, DEF-192).
// 대기의 상한은 배포 단계의 시한이다. 기다리는 동안 남는 것은 pg_stat_activity 의 이 세션(wait_event_type=Lock)뿐이다.
//
// 실패한 CONCURRENTLY 는 INVALID 인덱스를 남긴다. 그 상태로 다시 돌리면 IF NOT EXISTS 가 깨진 인덱스를 있는 것으로 보고 넘어간다.
// 그래서 실패할 때마다 INVALID 인덱스를 찾고, 있으면 다시 하지 않고 멈춘다(이름을 남긴다). 지우고 다시 돌리는 것은 사람이 한다.
func applyNoTx(ctx context.Context, c *pgconn.PgConn, name, body string, o MigrateOptions) (int, error) {
	stmts, err := SplitStatements(body)
	if err != nil {
		return 0, err
	}
	if _, err := c.Exec(ctx, "SET statement_timeout = 0; SET lock_timeout = 0").ReadAll(); err != nil {
		return 0, err
	}
	defer func() {
		_, _ = c.Exec(context.WithoutCancel(ctx), "SELECT set_config('statement_timeout', '"+pgDuration(o.StatementTimeout)+"', false), "+
			"set_config('lock_timeout', '"+pgDuration(o.LockTimeout)+"', false)").ReadAll()
	}()
	before, err := invalidIndexes(ctx, c)
	if err != nil {
		return 0, err
	}
	if len(before) > 0 {
		o.Log.Warn("migrate.invalid_indexes", "migration", name, "indexes", before)
		return 0, ErrInvalidIndex
	}
	total := 0
	for _, stmt := range stmts {
		for attempt := 1; ; attempt++ {
			total++
			_, err := c.Exec(ctx, stmt).ReadAll()
			if err == nil {
				break
			}
			left, ierr := invalidIndexes(context.WithoutCancel(ctx), c)
			if ierr == nil && len(left) > 0 {
				o.Log.Warn("migrate.invalid_indexes", "migration", name, "indexes", left)
				return total, fmt.Errorf("%w: %w", ErrInvalidIndex, err)
			}
			if !isLockTimeout(err) || attempt > o.Retries {
				return total, err
			}
			if err := backoff(ctx, name, attempt, err, o); err != nil {
				return total, err
			}
		}
	}
	if err := c.ExecParams(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", [][]byte{[]byte(name)}, nil, nil, nil).Read().Err; err != nil {
		return total, err
	}
	return total, nil
}

// invalidIndexes 는 public 스키마의 INVALID 인덱스 이름이다. 인덱스 이름은 리포의 마이그레이션이 정한 고정 문자열이라 로그에 싣는다(D30).
func invalidIndexes(ctx context.Context, c *pgconn.PgConn) ([]string, error) {
	res := c.ExecParams(ctx, `SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE NOT i.indisvalid AND n.nspname = 'public' ORDER BY 1`, nil, nil, nil, nil)
	var names []string
	for res.NextRow() {
		names = append(names, string(res.Values()[0]))
	}
	_, err := res.Close()
	return names, err
}

func backoff(ctx context.Context, name string, attempt int, cause error, o MigrateOptions) error {
	wait := o.RetryBackoff << (attempt - 1)
	if wait > maxMigrateBackoff || wait <= 0 {
		wait = maxMigrateBackoff
	}
	wait = time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: 재시도 간격을 흔들 뿐이다.
	o.Log.Warn("migrate.lock_retry", "migration", name, "attempt", attempt, "after_ms", wait.Milliseconds(),
		"lock_timeout_ms", o.LockTimeout.Milliseconds(), "err_kind", logctx.ErrKind(cause))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// pgDuration 은 Postgres 설정 값으로 쓰는 밀리초 문자열이다.
func pgDuration(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

// HasNoTransactionDirective 는 파일 머리(첫 SQL 문장 앞의 주석 줄)에 NoTransactionDirective 가 있는지다.
func HasNoTransactionDirective(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "--") {
			return false
		}
		if t == NoTransactionDirective {
			return true
		}
	}
	return false
}

// MigrationNames 는 리포의 마이그레이션 파일 이름을 순서대로 돌려준다.
func MigrationNames() ([]string, error) { return sqlNames(migrationFS, "migrations") }

// MigrationBody 는 리포의 마이그레이션 파일 내용이다.
func MigrationBody(name string) ([]byte, error) { return migrationFS.ReadFile("migrations/" + name) }

func sqlNames(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// PendingMigrations 는 리포에 있는데 이 DB 에 적용되지 않은 파일 이름이다. 읽기만 한다 — 서버의 /readyz 와 기동 확인이 부른다(D49).
// schema_migrations 가 없으면(빈 DB) 전부다. DB 에만 있는 이름(더 새 코드가 적용한 것)은 세지 않는다 — 옛 코드는 새 스키마 위에서 돈다(추가만 하는 규칙).
func (s *Store) PendingMigrations(ctx context.Context) ([]string, error) {
	names, err := MigrationNames()
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		if isUndefinedTable(err) {
			return names, nil
		}
		return nil, err
	}
	applied := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		if isUndefinedTable(err) {
			return names, nil
		}
		return nil, err
	}
	var pending []string
	for _, n := range names {
		if !applied[n] {
			pending = append(pending, n)
		}
	}
	return pending, nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// SplitStatements 는 SQL 을 문장으로 나눈다. 작은따옴표 문자열, 큰따옴표 이름, 달러 인용, -- 와 /* */ 주석 안의 ; 는 나누지 않는다.
// 주석만 남은 조각은 버린다. 트랜잭션 없는 파일을 문장 하나씩 보내고(여러 문장을 한 번에 보내면 Postgres 가 암묵 트랜잭션으로 묶어
// CONCURRENTLY 가 실패한다), 마이그레이션 린트가 문장 단위로 본다.
func SplitStatements(sql string) ([]string, error) {
	var out []string
	start, i, n := 0, 0, len(sql)
	hasCode := false
	flush := func(end int) {
		if hasCode {
			out = append(out, strings.TrimSpace(sql[start:end]))
		}
		start, hasCode = end+1, false
	}
	for i < n {
		ch := sql[i]
		switch {
		case ch == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
			continue
		case ch == '/' && i+1 < n && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("pgstore: 닫히지 않은 /* 주석")
			}
			i += end + 4
			continue
		case ch == '\'' || ch == '"':
			hasCode = true
			j := i + 1
			for {
				if j >= n {
					return nil, errors.New("pgstore: 닫히지 않은 따옴표")
				}
				if sql[j] == ch {
					if j+1 < n && sql[j+1] == ch { // '' 나 "" 는 따옴표 하나다
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = j + 1
			continue
		case ch == '$':
			if tag, ok := dollarTag(sql[i:]); ok {
				hasCode = true
				end := strings.Index(sql[i+len(tag):], tag)
				if end < 0 {
					return nil, errors.New("pgstore: 닫히지 않은 달러 인용")
				}
				i += len(tag) + end + len(tag)
				continue
			}
		case ch == ';':
			flush(i)
			i++
			continue
		}
		if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
			hasCode = true
		}
		i++
	}
	if start < n {
		flush(n)
	}
	return out, nil
}

// dollarTag 는 s 가 $태그$ 로 시작하면 그 태그를 돌려준다. $1 같은 자리표시자는 태그가 아니다.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (j > 1 && c >= '0' && c <= '9'):
		default:
			return "", false
		}
	}
	return "", false
}
