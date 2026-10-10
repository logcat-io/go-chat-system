package pgstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 마이그레이션 러너(D49). 리포의 파일로는 잠금 대기·트랜잭션 없는 파일·INVALID 인덱스를 만들 수 없어 가짜 파일(fstest)로 돌린다.
// 러너는 리포 파일과 같은 함수다(MigrateFrom 은 읽는 곳만 바꾼다).

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// events 는 msg 사건의 JSON 줄들이다.
func (l *lockedBuf) events(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, raw := range strings.Split(l.b.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func jsonLog() (*slog.Logger, *lockedBuf) {
	buf := &lockedBuf{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func connect(t *testing.T, dbURL string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func recorded(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// 감사 실측 3(docs/perf/deploy-db.md)의 재현이다. 다른 트랜잭션이 conversations 를 읽는 채로 쥐고 있으면 ALTER 는 잠금을 못 잡는다.
// 시한이 없던 러너는 ALTER 가 끝없이 기다렸고 그 뒤의 단순 SELECT 가 11.9초 줄을 섰다. 이제 ALTER 는 3초(기본 lock_timeout)만 기다리고
// 물러났다가 다시 하며, 다 실패하면 파일을 기록하지 않고 실패한다. 그동안 다른 세션의 읽기는 한 번에 3초 넘게 막히지 않는다.
func TestMigrate_잠금을3초안에못잡으면물러났다가다시하고끝내실패하며그동안다른세션의읽기는3초넘게막히지않는다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	// 시한이 없던 러너는 여기서 끝나지 않는다. 테스트 전체가 멈추지 않게 20초에서 끊는다.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	holder := connect(t, dbURL)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM conversations"); err != nil {
		t.Fatal(err)
	}

	// 다른 세션이 쉬지 않고 읽는다. 가장 오래 기다린 읽기를 잰다.
	probe := connect(t, dbURL)
	stop := make(chan struct{})
	var worst time.Duration
	var reads int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := probe.Exec(ctx, "SELECT count(*) FROM conversations"); err != nil {
				t.Errorf("읽기 실패(%v 기다렸다): %v", time.Since(start), err)
				return
			}
			worst = max(worst, time.Since(start))
			reads++
			time.Sleep(20 * time.Millisecond)
		}
	}()

	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_add_column.sql": {Data: []byte("ALTER TABLE conversations ADD COLUMN zz_lock_test INT;\n")}}
	start := time.Now()
	err = store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{Retries: 2, RetryBackoff: 200 * time.Millisecond, Log: log})
	elapsed := time.Since(start)
	close(stop)
	<-done

	var mig *pgstore.MigrationError
	if !errors.As(err, &mig) || mig.File != "9001_add_column.sql" || sqlState(err) != "55P03" {
		t.Fatalf("잠금 실패(55P03)로 9001 이 실패해야 한다: %v", err)
	}
	// 첫 시도 + 재시도 2번, 시도마다 3초를 기다린다.
	if elapsed < 9*time.Second || elapsed > 15*time.Second {
		t.Errorf("걸린 시간 %v, 3초 × 3번(9초 남짓)이어야 한다", elapsed)
	}
	if n := len(buf.events("migrate.lock_retry")); n != 2 {
		t.Errorf("migrate.lock_retry %d 줄, 2 줄이어야 한다", n)
	}
	if f := buf.events("migrate.file_failed"); len(f) != 1 || f[0]["attempts"] != float64(3) || f[0]["err_kind"] != "55P03" {
		t.Errorf("migrate.file_failed %v, attempts=3 err_kind=55P03 이어야 한다", f)
	}
	if worst > 3500*time.Millisecond {
		t.Errorf("다른 세션의 읽기가 %v 막혔다. lock_timeout(3초) 안이어야 한다", worst)
	}
	if reads < 5 {
		t.Errorf("읽기가 %d 번만 돌았다 — 마이그레이션 동안 읽기가 거의 멈췄다", reads)
	}
	c := connect(t, dbURL)
	if recorded(t, c, "9001_add_column.sql") {
		t.Error("실패한 파일이 schema_migrations 에 기록됐다")
	}
	var exists bool
	if err := c.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'conversations' AND column_name = 'zz_lock_test')").Scan(&exists); err != nil || exists {
		t.Errorf("실패한 파일의 칸이 남았다: %v %v", exists, err)
	}
}

// 잠금을 쥔 트랜잭션이 끝나면 다음 시도가 잡는다. 재시도는 실패를 늦추는 것이 아니라 짧은 트랜잭션 사이로 끼어드는 길이다.
func TestMigrate_잠금을쥔트랜잭션이재시도사이에끝나면다음시도가적용한다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	holder := connect(t, dbURL)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM conversations"); err != nil {
		t.Fatal(err)
	}
	// 첫 시도(1초 시한)가 실패한 뒤에 놓는다.
	release := time.AfterFunc(1500*time.Millisecond, func() { _ = tx.Rollback(ctx) })
	defer release.Stop()

	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_add_column.sql": {Data: []byte("ALTER TABLE conversations ADD COLUMN zz_retry_test INT;\n")}}
	if err := store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{LockTimeout: time.Second, Retries: 3, RetryBackoff: time.Second, Log: log}); err != nil {
		t.Fatalf("재시도로 적용돼야 한다: %v", err)
	}
	applied := buf.events("migrate.file_applied")
	if len(applied) != 1 || applied[0]["migration"] != "9001_add_column.sql" || applied[0]["attempts"].(float64) < 2 {
		t.Errorf("migrate.file_applied %v, 두 번째 시도 이후여야 한다", applied)
	}
	if _, ok := applied[0]["ms"]; !ok {
		t.Errorf("소요 시간(ms)이 없다: %v", applied[0])
	}
	if s := buf.events("migrate.file_started"); len(s) != 1 || s[0]["mode"] != "tx" {
		t.Errorf("migrate.file_started %v", s)
	}
	if !recorded(t, connect(t, dbURL), "9001_add_column.sql") {
		t.Error("적용한 파일이 기록되지 않았다")
	}
}

// 재시도 0 은 값이다(CHAT_MIGRATE_RETRIES=0). 잠금을 한 번 못 잡으면 다시 하지 않고 실패한다. 전에는 0 이 기본값 5 가 되어
// 첫 시도와 합쳐 6번을 기다렸다(감사 31 변이 M28 이 살았다).
func TestMigrate_재시도가0이면잠금을한번못잡자마자다시하지않고실패한다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	holder := connect(t, dbURL)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM conversations"); err != nil {
		t.Fatal(err)
	}
	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_add_column.sql": {Data: []byte("ALTER TABLE conversations ADD COLUMN zz_noretry INT;\n")}}
	start := time.Now()
	err = store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{LockTimeout: 500 * time.Millisecond, Retries: 0, RetryBackoff: 100 * time.Millisecond, Log: log})
	if sqlState(err) != "55P03" {
		t.Fatalf("잠금 실패(55P03)여야 한다: %v", err)
	}
	if n := len(buf.events("migrate.lock_retry")); n != 0 {
		t.Errorf("재시도 0 인데 %d 번 다시 했다", n)
	}
	if f := buf.events("migrate.file_failed"); len(f) != 1 || f[0]["attempts"] != float64(1) {
		t.Errorf("migrate.file_failed %v, attempts=1 이어야 한다", f)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("걸린 시간 %v — 한 번(0.5초)만 기다려야 한다", d)
	}
}

// CREATE INDEX CONCURRENTLY 는 시작 전에 연 트랜잭션이 끝나기를 기다린다 — 다른 표만 읽는 긴 트랜잭션도 기다린다. 그동안 그 표의
// 읽기·쓰기는 줄을 서지 않는다. 그래서 트랜잭션 없는 파일에는 lock_timeout 을 걸지 않는다. 전에는 3초 lock_timeout 이 이 대기에 걸려
// 55P03 으로 실패하고 INVALID 인덱스를 남겼다(감사 31 실측: 시한이 없으면 9.5초 뒤 VALID, 그동안 SELECT 최대 60ms·UPDATE 36ms).
// 다른 표를 읽는 20초 REPEATABLE READ 트랜잭션을 연 채로 돌려도 파일은 성공하고 인덱스는 VALID 이며, 그동안 같은 표의 SELECT·UPDATE 는
// 한 번에 1초 넘게 막히지 않는다.
func TestMigrate_트랜잭션없는파일은오래된트랜잭션을시한없이기다려INVALID를남기지않고그동안같은표의읽기쓰기는1초넘게막히지않는다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 다른 표(agents)를 읽고 스냅샷을 쥔 채 20초 있다가 끝난다.
	const hold = 20 * time.Second
	holder := connect(t, dbURL)
	tx, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM agents"); err != nil {
		t.Fatal(err)
	}
	held := time.Now()
	release := time.AfterFunc(hold, func() { _ = tx.Commit(context.Background()) })
	defer release.Stop()

	// 같은 표(conversations)를 쉬지 않고 읽고 고친다(행이 없어도 UPDATE 는 쓰기 잠금을 잡는다). 가장 오래 기다린 한 번을 잰다.
	probe := connect(t, dbURL)
	stop := make(chan struct{})
	var worstRead, worstWrite time.Duration
	var rounds int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := time.Now()
			if _, err := probe.Exec(ctx, "SELECT count(*) FROM conversations"); err != nil {
				t.Errorf("읽기 실패: %v", err)
				return
			}
			worstRead = max(worstRead, time.Since(s))
			s = time.Now()
			if _, err := probe.Exec(ctx, "UPDATE conversations SET updated_at = updated_at WHERE id = '00000000-0000-0000-0000-000000000000'"); err != nil {
				t.Errorf("쓰기 실패: %v", err)
				return
			}
			worstWrite = max(worstWrite, time.Since(s))
			rounds++
			time.Sleep(20 * time.Millisecond)
		}
	}()

	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_index.sql": {Data: []byte("-- migrate: no-transaction\nCREATE INDEX CONCURRENTLY IF NOT EXISTS zz_conv_cic_idx ON conversations (updated_at);\n")}}
	err = store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{Log: log})
	took := time.Since(held)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("오래된 트랜잭션을 기다리다 실패했다(%v 뒤): %v\n%v", took, err, buf.events("migrate.invalid_indexes"))
	}
	if took < hold-time.Second {
		t.Errorf("%v 만에 끝났다 — 오래된 트랜잭션(%v)을 기다리지 않았다. 재현이 틀렸다", took, hold)
	}
	c := connect(t, dbURL)
	var valid bool
	if err := c.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i JOIN pg_class x ON x.oid = i.indexrelid WHERE x.relname = 'zz_conv_cic_idx'`).Scan(&valid); err != nil || !valid {
		t.Errorf("인덱스가 VALID 가 아니다: valid=%v err=%v", valid, err)
	}
	if !recorded(t, c, "9001_index.sql") {
		t.Error("적용한 파일이 기록되지 않았다")
	}
	if n := len(buf.events("migrate.invalid_indexes")); n != 0 {
		t.Errorf("migrate.invalid_indexes %d 줄", n)
	}
	if worstRead > time.Second || worstWrite > time.Second {
		t.Errorf("기다리는 동안 같은 표의 읽기 %v·쓰기 %v 가 막혔다. 1초 안이어야 한다", worstRead, worstWrite)
	}
	if rounds < 50 {
		t.Errorf("읽기·쓰기가 %d 번만 돌았다 — 기다리는 동안 거의 멈췄다", rounds)
	}
	t.Logf("CIC %v, 읽기 최대 %v, 쓰기 최대 %v, %d 번", took, worstRead, worstWrite, rounds)
}

// 트랜잭션 파일의 문장에는 statement_timeout 이 걸린다. 시한 초과(57014)는 잠금 실패가 아니라 다시 하지 않는다.
func TestMigrate_트랜잭션파일의문장은statement_timeout을넘기면다시하지않고실패한다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_slow.sql": {Data: []byte("SELECT pg_sleep(3);\n")}}
	err := store.MigrateFrom(context.Background(), fsys, "m", pgstore.MigrateOptions{StatementTimeout: 300 * time.Millisecond, Log: log})
	if sqlState(err) != "57014" {
		t.Fatalf("statement_timeout(57014)이어야 한다: %v", err)
	}
	if n := len(buf.events("migrate.lock_retry")); n != 0 {
		t.Errorf("시한 초과를 %d 번 다시 했다", n)
	}
	if recorded(t, connect(t, dbURL), "9001_slow.sql") {
		t.Error("실패한 파일이 기록됐다")
	}
}

// 두 migrate 가 겹치면 늦은 쪽은 자문 잠금을 한도까지만 기다린다. 전에는 pg_advisory_lock 이 시한 없이 기다렸다.
func TestMigrate_다른migrate가자문잠금을쥐고있으면한도까지만기다리고실패한다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	other := connect(t, dbURL)
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x6368_6174)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := store.MigrateFrom(ctx, fstest.MapFS{}, ".", pgstore.MigrateOptions{AdvisoryWait: time.Second})
	if !errors.Is(err, pgstore.ErrMigrationLockBusy) {
		t.Fatalf("ErrMigrationLockBusy 여야 한다: %v", err)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("기다린 시간 %v, 한도(1초) 남짓이어야 한다", d)
	}
}

// 머리에 `-- migrate: no-transaction` 이 있는 파일은 트랜잭션 없이 문장 하나씩 돈다. CREATE INDEX CONCURRENTLY 는 이렇게만 된다
// (여러 문장을 한 번에 보내면 Postgres 가 암묵 트랜잭션으로 묶는다). 표시가 없는 파일의 CONCURRENTLY 는 25001 로 실패하고 기록되지 않는다.
func TestMigrate_트랜잭션없는파일은문장하나씩돌아CONCURRENTLY인덱스를만들고표시가없으면실패한다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	log, buf := jsonLog()
	fsys := fstest.MapFS{"m/9001_index.sql": {Data: []byte(`-- 표의 읽기·쓰기를 막지 않고 인덱스를 만든다.
-- migrate: no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS zz_conv_updated_idx ON conversations (updated_at);
-- 세미콜론이 든 주석도 문장을 나누지 않는다; 이렇게
CREATE INDEX CONCURRENTLY IF NOT EXISTS zz_conv_state_idx ON conversations (state) WHERE state <> 'x;y';
`)}}
	if err := store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{Log: log}); err != nil {
		t.Fatalf("트랜잭션 없는 파일이 실패했다: %v", err)
	}
	c := connect(t, dbURL)
	var valid int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_index i JOIN pg_class x ON x.oid = i.indexrelid
		WHERE x.relname IN ('zz_conv_updated_idx', 'zz_conv_state_idx') AND i.indisvalid`).Scan(&valid); err != nil || valid != 2 {
		t.Errorf("유효한 인덱스 %d 개, 2 개여야 한다: %v", valid, err)
	}
	if !recorded(t, c, "9001_index.sql") {
		t.Error("트랜잭션 없는 파일이 기록되지 않았다")
	}
	if s := buf.events("migrate.file_started"); len(s) != 1 || s[0]["mode"] != "no_tx" {
		t.Errorf("migrate.file_started %v, mode=no_tx 여야 한다", s)
	}

	bad := fstest.MapFS{"m/9002_index.sql": {Data: []byte("CREATE INDEX CONCURRENTLY zz_conv_created_idx ON conversations (created_at);\n")}}
	err := store.MigrateFrom(ctx, bad, "m", pgstore.MigrateOptions{})
	if sqlState(err) != "25001" {
		t.Fatalf("트랜잭션 안의 CONCURRENTLY 는 25001 이어야 한다: %v", err)
	}
	if recorded(t, c, "9002_index.sql") {
		t.Error("실패한 파일이 기록됐다")
	}
}

// CONCURRENTLY 가 실패하면 INVALID 인덱스가 남는다. 그 채로 다시 돌리면 IF NOT EXISTS 가 깨진 인덱스를 있는 것으로 보고 넘어가므로,
// 러너는 INVALID 인덱스가 있으면 파일을 기록하지 않고 멈춘다(이름을 남긴다). 지우고 다시 돌리면 적용된다.
func TestMigrate_CONCURRENTLY가실패해INVALID인덱스가남으면기록하지않고멈추며지운뒤다시돌리면적용된다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	c := connect(t, dbURL)
	if _, err := c.Exec(ctx, "CREATE TABLE zz_dup (v INT); INSERT INTO zz_dup VALUES (1), (1)"); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"m/9001_unique.sql": {Data: []byte("-- migrate: no-transaction\nCREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS zz_dup_v ON zz_dup (v);\n")}}

	log, buf := jsonLog()
	err := store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{Log: log})
	if !errors.Is(err, pgstore.ErrInvalidIndex) || sqlState(err) != "23505" {
		t.Fatalf("중복으로 실패하고 INVALID 인덱스를 알려야 한다: %v", err)
	}
	inv := buf.events("migrate.invalid_indexes")
	if len(inv) != 1 || !strings.Contains(jsonString(inv[0]["indexes"]), `"zz_dup_v"`) {
		t.Errorf("migrate.invalid_indexes %v 에 zz_dup_v 가 없다", inv)
	}
	if recorded(t, c, "9001_unique.sql") {
		t.Fatal("실패한 파일이 기록됐다")
	}

	// 다시 돌려도 IF NOT EXISTS 로 넘어가지 않고 멈춘다.
	if err := store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{}); !errors.Is(err, pgstore.ErrInvalidIndex) {
		t.Fatalf("INVALID 인덱스가 남은 채 다시 돌렸는데 멈추지 않았다: %v", err)
	}
	if recorded(t, c, "9001_unique.sql") {
		t.Fatal("깨진 인덱스로 파일이 기록됐다")
	}

	if _, err := c.Exec(ctx, "DROP INDEX CONCURRENTLY zz_dup_v"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "DELETE FROM zz_dup WHERE ctid = (SELECT max(ctid) FROM zz_dup)"); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateFrom(ctx, fsys, "m", pgstore.MigrateOptions{}); err != nil {
		t.Fatalf("지운 뒤 다시 돌렸는데 실패했다: %v", err)
	}
	if !recorded(t, c, "9001_unique.sql") {
		t.Error("적용한 파일이 기록되지 않았다")
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 서버는 스키마를 읽기만 한다(D49). 모자란 것은 리포에 있는데 DB 에 기록되지 않은 파일이다. DB 에만 있는 이름(더 새 코드가 적용한 것)은
// 세지 않는다 — 옛 코드가 새 스키마 위에서 돈다(추가만 하는 규칙). schema_migrations 가 없는 빈 DB 는 전부 모자란다.
func TestPendingMigrations_모자란파일만세고DB에만있는이름은세지않으며빈DB는전부다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()
	names, err := pgstore.MigrationNames()
	if err != nil || len(names) < 20 {
		t.Fatalf("리포 마이그레이션 %d 개: %v", len(names), err)
	}
	last := names[len(names)-1]
	if p, err := store.PendingMigrations(ctx); err != nil || len(p) != 0 {
		t.Fatalf("다 적용된 DB 의 모자란 파일 %v: %v", p, err)
	}
	testsupport.Exec(t, dbURL, "DELETE FROM schema_migrations WHERE name = $1", last)
	testsupport.Exec(t, dbURL, "INSERT INTO schema_migrations (name) VALUES ('9999_future.sql')")
	if p, err := store.PendingMigrations(ctx); err != nil || len(p) != 1 || p[0] != last {
		t.Fatalf("모자란 파일 %v, [%s] 여야 한다: %v", p, last, err)
	}
	testsupport.Exec(t, dbURL, "DROP TABLE schema_migrations")
	if p, err := store.PendingMigrations(ctx); err != nil || len(p) != len(names) {
		t.Fatalf("빈 DB 의 모자란 파일 %d 개, %d 개여야 한다: %v", len(p), len(names), err)
	}
}
