package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 배포의 앞 단계가 부르는 명령들이다(D49). 서버와 같은 이미지·같은 설정으로 돌고, 끝나면 프로세스가 끝난다.
// 서버는 이 둘이 하는 일(스키마 바꾸기·행 넣기)을 하지 않는다 — 배포가 DB 를 멈추거나 DB 상태가 배포를 막지 않게 둘을 떼었다.

// commandPoolConns 는 명령이 여는 풀의 크기다. 마이그레이션은 연결 하나로 돈다(자문 잠금이 세션에 묶인다).
const commandPoolConns = 2

// errSchemaBehind 는 bootstrap 이 마이그레이션보다 먼저 불렸다는 뜻이다.
var errSchemaBehind = errors.New("스키마가 이 코드보다 뒤에 있다 — server migrate 를 먼저 돌린다")

// runMigrate 는 배포의 앞 단계 하나다(배포 도구가 명령 하나로 부를 수 있게). 마이그레이션을 적용하고, 이어서
// bootstrap 과 같은 보정(시드는 CHAT_SEED_DEMO=true 일 때만)을 한 뒤 끝난다. 보정은 고칠 것이 없으면 아무 행도 쓰지 않는다(정책 880).
// DB 가 올라오는 중이면 openWithRetry 가 기다린다(약 50초). 잠금은 짧게만 기다리고(CHAT_MIGRATE_LOCK_TIMEOUT) 못 잡으면 물러났다가
// 다시 한다. 다 실패하면 종료 코드 1 이고 배포가 멈춘다 — 돌던 서버는 옛 버전 그대로 서빙한다.
func runMigrate(cfg config.Config, log *slog.Logger) error {
	if err := checkSeedConfig(cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openWithRetry(ctx, cfg.DatabaseURL, commandPoolConns, log)
	if err != nil {
		return atStage("db_open", err)
	}
	defer store.Close()
	if err := store.MigrateWith(ctx, pgstore.MigrateOptions{
		LockTimeout: cfg.Migrate.LockTimeout, StatementTimeout: cfg.Migrate.StatementTimeout,
		Retries: cfg.Migrate.Retries, AdvisoryWait: cfg.Migrate.AdvisoryWait, Log: log,
	}); err != nil {
		return atStage("migrate", err)
	}
	return atStage("seed", seed(ctx, store, cfg, log))
}

// runBootstrap 은 마이그레이션 없이 보정만 한다: 시드(CHAT_SEED_DEMO=true 일 때), 관리자 0명 보정, 비어 있는 회원 서명 비밀 채우기.
// 몇 번을 돌려도 같다 — 있으면 건드리지 않는다. 스키마가 모자라면 아무것도 하지 않고 실패한다.
func runBootstrap(cfg config.Config, log *slog.Logger) error {
	if err := checkSeedConfig(cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openWithRetry(ctx, cfg.DatabaseURL, commandPoolConns, log)
	if err != nil {
		return atStage("db_open", err)
	}
	defer store.Close()
	pending, err := store.PendingMigrations(ctx)
	if err != nil {
		return atStage("seed", err)
	}
	if len(pending) > 0 {
		log.Warn("bootstrap.schema_behind", "pending", len(pending), "first_pending", pending[0])
		return atStage("seed", errSchemaBehind)
	}
	return atStage("seed", seed(ctx, store, cfg, log))
}

// checkSeedConfig 는 시드를 켰는데 비밀번호가 없는 설정을 DB 에 닿기 전에 거절한다. 비밀번호 기본값은 없다(D49).
func checkSeedConfig(cfg config.Config) error {
	if cfg.SeedDemo && cfg.SeedAgentPass == "" {
		return atStage("seed", &config.Error{Key: "CHAT_SEED_AGENT_PASSWORD", Rule: "CHAT_SEED_DEMO=true 면 비어 있으면 안 된다"})
	}
	return nil
}

// 스키마 확인 주기다. 모자라는 동안은 자주 보고, 따라온 뒤로는 드물게 본다(복원으로 뒤로 갈 수도 있다).
// 모자라는 동안의 WARN 은 1분에 한 줄이다.
const (
	schemaRecheck      = 10 * time.Second
	schemaRecheckReady = 5 * time.Minute
	schemaWarnEvery    = time.Minute
	schemaCheckTimeout = 5 * time.Second
)

// watchSchema 는 DB 스키마가 이 코드를 따라왔는지 주기적으로 읽어 남긴다(D49). 쓰지 않는다. /readyz 는 요청마다 따로 확인한다.
//
//	schema.ready         INFO. 이 코드가 기대하는 마이그레이션이 다 있다
//	schema.behind        WARN. pending 개가 모자란다. first_pending 은 첫 파일 이름(리포 고정 문자열, D30). server migrate 가 안 돌았다
//	schema.check_failed  WARN. 확인 질의가 실패했다(DB 가 없다 등). err_kind 만 싣는다
func watchSchema(check func(context.Context) ([]string, error), log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		state := ""
		var warned time.Time
		for {
			cctx, cancel := context.WithTimeout(ctx, schemaCheckTimeout)
			pending, err := check(cctx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				if state != "failed" {
					log.Warn("schema.check_failed", "err_kind", logctx.ErrKind(err))
				}
				state = "failed"
			case len(pending) > 0:
				if state != "behind" || time.Since(warned) >= schemaWarnEvery {
					log.Warn("schema.behind", "pending", len(pending), "first_pending", pending[0])
					warned = time.Now()
				}
				state = "behind"
			default:
				if state != "ready" {
					log.Info("schema.ready")
				}
				state = "ready"
			}
			wait := schemaRecheck
			if state == "ready" {
				wait = schemaRecheckReady
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}
