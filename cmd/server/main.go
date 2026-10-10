// go-chat-system 서버. 실행 파일 하나에 API·WebSocket·정적 자산이 다 들어 있다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/adapter/filestore"
	"github.com/logcat-io/go-chat-system/internal/adapter/httpapi"
	"github.com/logcat-io/go-chat-system/internal/adapter/ollama"
	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/adapter/realtime"
	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/config"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// 포트가 구현과 어긋나면 여기서 컴파일이 막힌다. pgstore 는 app 을 import 하지 않으므로 단언은 조립부에 둔다.
var (
	_ app.Store            = (*pgstore.Store)(nil)
	_ app.MaintenanceStore = (*pgstore.Store)(nil)
	_ app.ManageStore      = (*pgstore.Store)(nil)
	_ app.FileStore        = (*filestore.Store)(nil)
	_ app.TempSweeper      = (*filestore.Store)(nil)
	_ app.FileStore        = (*s3store.Store)(nil)
)

func main() {
	cmd, ok := command(os.Args[1:])
	if !ok {
		// 인자 값은 싣지 않는다. 쓰는 법만 알린다.
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		// 값은 싣지 않는다 — 접속 문자열·비밀번호가 값이다(D30). 어느 변수가 어떤 규칙을 어겼는지만 남긴다.
		var bad *config.Error
		if errors.As(err, &bad) {
			slog.Error("config.invalid", "key", bad.Key, "rule", bad.Rule)
		} else {
			slog.Error("config.invalid", "err_kind", logctx.ErrKind(err))
		}
		os.Exit(1)
	}
	log := newLogger(cfg)
	// app 계층은 로거를 들고 다니지 않고 slog.XxxContext 기본 로거를 쓴다. 같은 핸들러를 보게 맞춘다.
	slog.SetDefault(log)

	switch cmd {
	case cmdMigrate:
		err = runMigrate(cfg, log)
	case cmdBootstrap:
		err = runBootstrap(cfg, log)
	default:
		err = run(cfg, log)
	}
	if err != nil {
		// 오류 원문은 싣지 않는다(D30). DB 접속 실패 문구에는 user=·database=·호스트:포트가 들어간다. 어느 명령의 어느 단계에서 어떤 종류로 실패했는지만 남긴다.
		stage := "serve"
		var st *stageError
		if errors.As(err, &st) {
			stage = st.stage
		}
		var bad *config.Error
		var mig *pgstore.MigrationError
		switch {
		case errors.As(err, &bad):
			log.Error("server.exit", "command", cmd, "stage", stage, "key", bad.Key, "rule", bad.Rule)
		case errors.As(err, &mig):
			// 파일 이름은 리포에 고정된 이름이라 싣는다(DEF-85). 원인은 err_kind(SQLSTATE)다.
			log.Error("server.exit", "command", cmd, "stage", stage, "migration", mig.File, "err_kind", logctx.ErrKind(err))
		default:
			log.Error("server.exit", "command", cmd, "stage", stage, "err_kind", logctx.ErrKind(err))
		}
		os.Exit(1)
	}
}

// 실행 파일 하나가 세 명령을 한다(D49). 같은 이미지(/app/server)를 인자로 가른다.
//
//	server            서버. DB 에 쓰지 않는다 — 스키마가 모자라면 /readyz 만 503 이다. DB 가 없어도 뜬다
//	server migrate    마이그레이션 + 보정. 배포의 앞 단계(ECS 1회성 태스크)가 돌린다
//	server bootstrap  보정만: 시드(CHAT_SEED_DEMO=true 일 때), 관리자 0명 보정, 비어 있는 회원 서명 비밀 채우기
const (
	cmdServe     = "serve"
	cmdMigrate   = "migrate"
	cmdBootstrap = "bootstrap"
)

const usage = "쓰는 법: server [serve|migrate|bootstrap]"

// command 는 인자에서 명령을 고른다. 없으면 서버다. 모르는 명령이나 인자가 더 있으면 false 다.
func command(args []string) (string, bool) {
	if len(args) == 0 {
		return cmdServe, true
	}
	if len(args) > 1 {
		return "", false
	}
	switch args[0] {
	case cmdServe, cmdMigrate, cmdBootstrap:
		return args[0], true
	}
	return "", false
}

// stageError 는 기동이 어느 단계에서 실패했는지 붙인다. 로그는 원문 대신 단계와 종류를 싣는다(server.exit).
type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// atStage 는 err 에 단계를 붙인다. 이미 붙어 있으면(안쪽 단계가 더 자세하다) 그대로 둔다.
func atStage(stage string, err error) error {
	var st *stageError
	if err == nil || errors.As(err, &st) {
		return err
	}
	return &stageError{stage: stage, err: err}
}

// newLogger 는 표준 출력 핸들러를 logctx 로 감싼다. ctx 에 얹힌 trace_id·conversation_id 가 모든 줄에 붙는다.
func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var inner slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if cfg.LogFormat == "json" {
		inner = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(logctx.NewHandler(inner))
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 관측(D33)은 조립보다 먼저 띄운다. HTTP·S3 계측이 만들 때의 프로바이더를 쓴다.
	// 엔드포인트가 비면 아무것도 내보내지 않는다. 내보내기는 비동기라 컬렉터가 죽어 있어도 기동이 막히지 않는다.
	tel, err := telemetry.Setup(ctx, log)
	if err != nil {
		return atStage("telemetry", err)
	}
	// 종료 순서(D32)의 맨 끝이다. 아래 defer 들(DB 닫기)보다 늦게, serve 의 HTTP 정리가 끝난 뒤에 돈다.
	defer flushTelemetry(tel, log)

	// 풀만 만들고 붙어 보지 않는다(D49). DB 가 재시작 중이어도 서버는 뜨고 /healthz 는 200, /readyz 는 503 이다.
	// 기동은 DB 에 쓰지 않는다 — 마이그레이션·시드·보정은 배포의 앞 단계 `server migrate`(보정만은 `server bootstrap`)가 한다.
	// 여기서 실패하는 것은 접속 문자열의 형식 오류뿐이다.
	store, err := pgstore.OpenLazy(ctx, cfg.DatabaseURL, int32(cfg.DBMaxConns)) //nolint:gosec // G115: config.Load 가 int32 범위를 넘는 값을 거절한다.
	if err != nil {
		return atStage("db_open", err)
	}
	defer store.Close()

	a, err := assemble(ctx, cfg, store, log)
	if err != nil {
		return atStage("assemble", err)
	}

	// 리슨은 곧바로 끝난다. 종료 신호는 serve 가 받아 순서대로 정리하므로(D32) 여기서는 취소를 잇지 않는다 — 전과 같은 동작이다.
	ln, err := (&net.ListenConfig{}).Listen(context.WithoutCancel(ctx), "tcp", cfg.Addr)
	if err != nil {
		return atStage("listen", err)
	}
	return atStage("serve", serve(ctx, ln, store, a, cfg, log))
}

// assembly 는 운영 조립의 결과다. run 은 이것으로 서버를 띄우고 주기 작업을 건다.
// 테스트(main_test.go)도 같은 함수로 서버를 만들어, 조립 줄이 빠지면 테스트가 잡는다.
type assembly struct {
	handler http.Handler
	hub     *realtime.Hub
	rt      *realtime.Server
	// drain 은 /readyz 를 503 으로 바꾼다. 종료의 첫 단계다.
	drain func()
	// jobs 는 주기 작업이다. 이 순서대로 건다.
	jobs []periodicJob
	// schema 는 적용되지 않은 마이그레이션을 읽는다(D49). startBackground 가 주기적으로 남긴다.
	schema func(context.Context) ([]string, error)
}

// telemetryFlushTimeout 은 종료할 때 남은 스팬·지표를 내보내는 시한이다. 컬렉터가 없으면 여기까지 기다리고 버린다.
const telemetryFlushTimeout = 5 * time.Second

// flushTelemetry 는 남은 스팬·지표를 시한 안에서 내보낸다. 실패해도 종료를 막지 않는다.
func flushTelemetry(tel *telemetry.Telemetry, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		log.Warn("shutdown.telemetry_flushed", "err_kind", logctx.ErrKind(err))
		return
	}
	log.Info("shutdown.telemetry_flushed")
}

// periodicJob 은 주기 작업 하나다. 한 번 부르는 것이 한 주기다.
type periodicJob struct {
	name string
	// key 는 스팬 이름에 쓰는 영문 이름이다.
	key    string
	period time.Duration
	run    func(context.Context)
}

// assemble 은 이미 연 DB 위에 나머지를 조립한다: 첨부 저장소를 열고, 유스케이스에 저장소와 한도를 꽂고,
// 실시간 서버와 HTTP 핸들러를 만들고, 주기 작업 목록을 정한다. 고루틴은 띄우지 않는다.
func assemble(ctx context.Context, cfg config.Config, store *pgstore.Store, log *slog.Logger) (assembly, error) {
	files, err := openFiles(ctx, cfg, log)
	if err != nil {
		return assembly{}, err
	}
	log.Info("server.db_pool", "max", cfg.DBMaxConns)

	svc := app.New(store)
	// 첨부의 규칙은 app 에 있다. 저장소와 한도를 여기서 한 번 꽂는다.
	svc.UseFiles(files.store, app.UploadLimits{MaxBytes: cfg.MaxUploadBytes, MaxPending: cfg.MaxPendingUploads})
	// 관리 화면(상담원 이름·빠른 답변 고치기·위젯 FAQ)의 저장소다(D36).
	svc.UseManage(store)
	// 상담 지표와 만족도 조사의 저장소다(D45).
	svc.UseMetrics(store)
	// 세션의 유휴 한도(D46). 절대 수명(CHAT_SESSION_TTL)은 발급하는 쪽이 넘긴다.
	svc.SetSessionPolicy(app.SessionPolicy{CustomerIdle: cfg.SessionIdleCustomer, AgentIdle: cfg.SessionIdleAgent})
	// 스키마가 이 코드를 따라왔는지는 읽어서만 안다(D49). 모자라면 /readyz 가 503 schema_behind 다.
	svc.UseSchema(store)
	if cfg.OllamaURL != "" {
		// 초안은 제안일 뿐이다. 모델이 뭐라고 하든 고객에게 가는 건 상담원이 누른 뒤다.
		svc.Queries.SetDraftProvider(ollama.New(cfg.OllamaURL, cfg.OllamaModel))
		// 주소 전체는 싣지 않는다 — 사용자 정보(user:pass@)가 들어갈 수 있다. 어느 호스트인지만 남긴다.
		log.Info("server.draft_enabled", "ollama_host", hostOf(cfg.OllamaURL), "model", cfg.OllamaModel)
	}
	hub := realtime.NewHub()
	rt := realtime.NewServer(svc, hub, cfg.CatchupInterval, log)

	api := httpapi.New(svc, rt, files.local, cfg, log)
	maint := app.NewMaintenance(svc.Commands, store, files.sweeper)
	return assembly{
		handler: api.Handler(),
		hub:     hub,
		rt:      rt,
		drain:   api.Drain,
		schema:  svc.Queries.PendingMigrations,
		jobs: []periodicJob{
			{name: "만료 세션 정리", key: "cleanup_sessions", period: time.Hour, run: maint.CleanupSessions},
			{name: "고아 업로드 정리", key: "cleanup_uploads", period: time.Hour, run: maint.CleanupUploads},
			{name: "보류 해제", key: "wake_snoozed", period: time.Minute, run: maint.WakeSnoozed},
		},
	}, nil
}

// fileStorage 는 열린 첨부 저장소와, 로컬 디스크일 때만 있는 것들이다.
type fileStorage struct {
	store app.FileStore
	// local 은 로컬 디스크 저장소가 발급한 주소를 받는 핸들러다. 객체 저장소면 nil 이다.
	local http.Handler
	// sweeper 는 행 없는 오래된 임시 파일을 지우는 능력이다. 로컬 디스크 저장소만 가진다.
	// S3 는 버킷 수명 주기 규칙(tmp/ 접두사 1일)에 맡기므로 nil 이다.
	sweeper app.TempSweeper
}

// openFiles 는 설정에 맞는 첨부 저장소를 연다. 어느 쪽이든 브라우저가 쓰는 절차는 같다.
func openFiles(ctx context.Context, cfg config.Config, log *slog.Logger) (fileStorage, error) {
	switch cfg.Storage {
	case "s3":
		store, err := s3store.New(s3store.Config{
			Endpoint: cfg.S3.Endpoint, PublicEndpoint: cfg.S3.PublicEndpoint, Region: cfg.S3.Region,
			Bucket: cfg.S3.Bucket, AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, PathStyle: cfg.S3.PathStyle,
			Upload: cfg.S3.Upload, Logger: log,
		})
		if err != nil {
			return fileStorage{}, atStage("storage", err)
		}
		if cfg.S3.CreateBucket {
			wait, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := store.EnsureBucket(wait); err != nil {
				return fileStorage{}, atStage("storage", err)
			}
		}
		// credentials 는 static(정적 키) 또는 container(태스크 역할)다. 키 값은 싣지 않는다.
		log.Info("server.file_store", "kind", "s3", "bucket", cfg.S3.Bucket, "origin", store.Origin(), "max", cfg.MaxUploadBytes,
			"upload", store.UploadMode(), "credentials", store.CredentialSource())
		return fileStorage{store: store}, nil
	case "local", "":
		store, err := filestore.New(cfg.UploadDir)
		if err != nil {
			return fileStorage{}, atStage("storage", err)
		}
		log.Info("server.file_store", "kind", "local", "dir", store.Root(), "max", cfg.MaxUploadBytes)
		return fileStorage{store: store, local: store.Handler(), sweeper: store}, nil
	default:
		// config.Load 가 먼저 거른다. 여기 닿으면 설정을 거치지 않고 조립한 것이다.
		return fileStorage{}, atStage("storage", &config.Error{Key: "CHAT_STORAGE", Rule: "local 또는 s3 여야 한다"})
	}
}

// seed 는 `server migrate` 뒤 단계와 `server bootstrap` 의 본체다(D49). 서버 기동은 부르지 않는다.
// 시드를 켜면(CHAT_SEED_DEMO=true) 빈 설치에서도 바로 눌러 볼 수 있게 기본 계정과 빠른 답변을 넣는다.
// 시드와 상관없이 관리자 0명 보정과 비어 있는 회원 서명 비밀 채우기를 한다. 있으면 건드리지 않는다.
func seed(ctx context.Context, store *pgstore.Store, cfg config.Config, log *slog.Logger) error {
	ws := core.DefaultWorkspaceID
	// 시드 계정의 비밀번호는 CHAT_SEED_AGENT_PASSWORD 다(기본값 없음). 공개 설치는 첫 관리자를 만든 뒤 CHAT_SEED_DEMO=false 로 끈다(docs/deploy.md 17.4). 회사 셀프 가입은 없다(D35).
	if cfg.SeedDemo {
		agent, err := store.EnsureAgent(ctx, ws, cfg.SeedAgentEmail, cfg.SeedAgentName, cfg.SeedAgentPass)
		if err != nil {
			return err
		}
		// 로그에는 id 만 싣는다. 이메일은 개인정보다(D30).
		log.Info("seed.agent_ready", "agent_id", agent.ID)

		// 배정·이관을 눌러 보려면 상담원이 여럿 있어야 한다. 팀 하나 분량을 기본으로 둔다.
		for i := 2; i <= cfg.SeedAgentCount; i++ {
			email := fmt.Sprintf("agent%d@example.com", i)
			seeded, err := store.EnsureAgent(ctx, ws, email, fmt.Sprintf("상담원%d", i), cfg.SeedAgentPass)
			if err != nil {
				log.Warn("seed.agent_failed", "seq", i, "err_kind", logctx.ErrKind(err))
				continue
			}
			log.Info("seed.agent_ready", "agent_id", seeded.ID)
		}
		seedSnippets(ctx, store, log)
	} else {
		log.Info("seed.skipped", "reason", "CHAT_SEED_DEMO=false")
	}
	// 관리자가 0명인 설치는 아무도 팀을 못 들인다. 시드가 끝난 뒤 반드시 확인한다.
	if promoted, err := store.EnsureAdmin(ctx, ws); err != nil {
		return err
	} else if promoted != "" {
		log.Info("seed.admin_promoted", "agent_id", promoted)
	}
	// 회원 서명 비밀. 마이그레이션은 안전한 난수를 못 만들어 여기서 채운다 — 비어 있을 때 한 번만.
	if n, err := store.FillEmptyMemberSecrets(ctx, cfg.MemberSecret); err != nil {
		return err
	} else if n > 0 {
		log.Info("seed.member_secret_filled", "workspaces", n)
	}
	if def, err := store.Workspace(ctx, ws); err == nil {
		// 위젯 키(걷어냈지만 DB 칸에 값이 남아 있다, D35)는 싣지 않는다. 로그 저장소에서 모아 볼 일이 아니다.
		log.Info("server.workspace", "slug", def.Slug)
	}
	return nil
}

// startBackground 는 알림 리스너, 안전망, 주기 작업을 띄운다. 돌려준 stop 이 전부 멈추고 끝나기를 기다린다.
//
// 종료 신호의 ctx 를 받지 않는다. 신호와 함께 멈추면 SIGTERM 순간 LISTEN 이 끊겨, 기다리는 동안(drain)과 WebSocket 을
// 닫는 동안 이 서버에 붙은 연결에 다른 서버가 쓴 메시지가 가지 않는다. 멈추는 것은 종료 순서의 맨 끝이다(D32).
func startBackground(store *pgstore.Store, a assembly, cfg config.Config, log *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// 알림 리스너는 전용 커넥션을 쓴다. 재접속할 때마다 전원을 깨워 놓친 알림을 메운다.
	listener := pgstore.NewListener(cfg.DatabaseURL, log)
	listener.OnChange = a.rt.OnChange
	listener.OnLive = a.rt.Live
	listener.OnResync = func() {
		fault("listener") // 테스트 전용(운영에서 nil). 리스너 고루틴 안에서 패닉을 일으켜 supervise 배선을 본다
		a.rt.Resync()
	}
	// 세션이 지워지면 그 세션의 열린 WebSocket 을 곧바로 닫는다(D46). 서버마다 같은 알림을 받는다.
	listener.OnSessionsEnded = a.rt.SessionsEnded
	wg.Go(func() { supervise(ctx, log, "listener", listener.Run) })

	// 알림을 놓쳤을 때의 안전망. 연결마다 되묻지 않고 바뀐 대화를 한 번에 모아 깨운다.
	watcher := realtime.NewChangeWatcher(faultySource{store}, a.hub, cfg.CatchupInterval, log)
	wg.Go(func() { supervise(ctx, log, "watcher", watcher.Run) })

	// 스키마가 이 코드를 따라왔는지 읽어 남긴다. 쓰지 않는다(D49).
	if a.schema != nil {
		wg.Go(func() { supervise(ctx, log, "schema", watchSchema(a.schema, log)) })
	}

	for _, job := range a.jobs {
		wg.Go(func() { every(ctx, job.period, guarded(job, log)) })
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// faultHook 은 테스트 전용 주입 지점이다(D47, 감사 23 N1·N2). 운영에서는 늘 nil 이다 — package main 의 소문자 변수라 다른 패키지가
// 닿을 수 없고, 운영 코드(run·assemble·startBackground)는 Store 를 부르지 않으며(internal/archtest 의 TestHook_… 가 지킨다)
// 환경 변수·설정으로 켜는 길도 없다.
// 테스트가 startBackground 로 띄운 리스너·안전망 고루틴 안에서 패닉을 일으켜, 조립된 배선(supervise)이 프로세스를 살리는지 본다.
// realtime 의 faultHook·frameHook 과 같은 꼴이다.
var faultHook atomic.Pointer[func(where string)]

func fault(where string) {
	if hook := faultHook.Load(); hook != nil {
		(*hook)(where)
	}
}

// faultySource 는 안전망이 쓰는 저장소다. 시작 시각을 읽을 때 fault 를 지나는 것 말고는 Store 그대로다(운영에서 fault 는 아무것도 안 한다).
type faultySource struct{ *pgstore.Store }

func (f faultySource) Now(ctx context.Context) (time.Time, error) {
	fault("watcher")
	return f.Store.Now(ctx)
}

// guarded 는 주기 작업 한 번을 스팬으로 감싸고 패닉을 되살린다. 한 번의 패닉은 그 한 번만 끝내고 다음 주기는 그대로 돈다(D47).
func guarded(job periodicJob, log *slog.Logger) func(context.Context) {
	once := traced(job)
	return func(ctx context.Context) { telemetry.Guard(ctx, log, "job."+job.key, once) }
}

// supervise 는 끝나지 않아야 하는 백그라운드 루프(알림 리스너·안전망)를 돌린다. 패닉이면 panic.recovered 를 남기고
// 1초 뒤 다시 띄운다(D47). recover 없이 두면 고루틴 하나의 패닉이 프로세스 전체를 끝내고, 다시 띄우지 않으면 그 서버의 실시간 전달이
// 안전망 없이 멈춘다. ctx 가 끝나거나 루프가 정상으로 돌아오면 멈춘다.
func supervise(ctx context.Context, log *slog.Logger, where string, run func(context.Context)) {
	for ctx.Err() == nil {
		if !telemetry.Guard(ctx, log, where, run) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(superviseRestart):
		}
	}
}

// superviseRestart 는 패닉으로 끝난 백그라운드 루프를 다시 띄우기 전의 간격이다. 같은 패닉이 되풀이돼도 로그가 초당 한 줄을 넘지 않는다.
const superviseRestart = time.Second

// hostOf 는 주소의 호스트(포트 포함)다. 읽지 못하면 빈 문자열이다.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// traced 는 주기 작업 한 번을 스팬 하나로 감싼다(D33). 그 안의 SQL 이 이 스팬 아래에 달리고,
// app 이 남기는 로그(logctx.NewTrace)의 trace_id 도 이 스팬의 것이 된다.
func traced(job periodicJob) func(context.Context) {
	return func(ctx context.Context) {
		ctx, span := telemetry.Tracer().Start(ctx, "job "+job.key, trace.WithNewRoot())
		defer span.End()
		job.run(ctx)
	}
}

// every 는 period 마다 once 를 부른다. 첫 호출은 period 가 지난 뒤다. ctx 가 끝나면 멈춘다.
func every(ctx context.Context, period time.Duration, once func(context.Context)) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			once(ctx)
		}
	}
}

// serve 는 백그라운드 작업을 띄우고 ln 으로 HTTP 서버를 띄운 뒤, 종료 신호(ctx)를 기다려 shutdown 으로 내려간다.
// 백그라운드 작업은 ctx 와 따로 돌고 shutdown 의 맨 끝에서 멈춘다.
func serve(ctx context.Context, ln net.Listener, store *pgstore.Store, a assembly, cfg config.Config, log *slog.Logger) error {
	stopBackground := startBackground(store, a, cfg, log)

	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// WebSocket 이 오래 열려 있어야 하므로 전체 쓰기 타임아웃을 두지 않는다.
		// 프레임 단위 타임아웃은 realtime 이 건다.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server.started", "addr", ln.Addr().String(), "catchup", cfg.CatchupInterval)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		stopBackground()
		return err
	case <-ctx.Done():
	}
	return shutdown(srv, a, cfg.ShutdownDrain, stopBackground, log)
}

// 종료 단계마다 쓰는 시한이다. ECS 의 stopTimeout 은 CHAT_SHUTDOWN_DRAIN + 세 시한(두 시한 20초 + 관측 내보내기 5초)보다 길게 둔다.
const (
	// goAwayTimeout 은 열린 WebSocket 을 1001 로 닫는 데 쓰는 시한이다. 진행 중인 저장이 끝나기를 기다리는 시간이 여기 들어간다.
	goAwayTimeout = 10 * time.Second
	// httpShutdownTimeout 은 남은 HTTP 요청을 마무리하는 시한이다.
	httpShutdownTimeout = 10 * time.Second
)

// shutdown 은 배포·축소로 태스크가 내려갈 때의 순서다(D32).
//
//  1. /readyz 를 503 으로 — 로드밸런서가 이 서버로 새 연결을 보내지 않게 한다
//  2. drain 만큼 기다린다 — 헬스 체크가 이 서버를 빼는 시간을 번다
//  3. 열린 WebSocket 을 1001 로 닫는다 — 처리 중인 저장은 커밋과 ack 까지 끝낸 뒤에 닫는다
//  4. 남은 HTTP 요청을 정리한다
//
// 그 뒤에 알림 리스너·안전망·주기 작업을 멈춘다(stopBackground). 3단계까지 열린 연결은 다른 서버가 쓴 메시지도 받는다.
//
// 클라이언트는 1001 을 받으면 기다리지 않고 다시 붙어 after_seq 로 이어 받는다. ack 를 못 받은 메시지는 같은
// client_msg_id 로 다시 보내고 멱등으로 처리된다(I3). 로그에는 숫자만 싣는다(D30).
func shutdown(srv *http.Server, a assembly, drain time.Duration, stopBackground func(), log *slog.Logger) error {
	a.drain()
	log.Info("shutdown.not_ready", "drain_ms", drain.Milliseconds())

	if drain > 0 {
		time.Sleep(drain)
		log.Info("shutdown.drained", "drain_ms", drain.Milliseconds())
	}

	goAwayCtx, cancelGoAway := context.WithTimeout(context.Background(), goAwayTimeout)
	closed, left := a.rt.GoAway(goAwayCtx)
	cancelGoAway()
	if left > 0 {
		log.Warn("shutdown.ws_closed", "closed", closed, "left", left)
	} else {
		log.Info("shutdown.ws_closed", "closed", closed, "left", 0)
	}

	// ctx 는 이미 끝났다. 진행 중인 요청을 마무리할 시간은 새 ctx 로 잰다.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if err != nil {
		log.Warn("shutdown.http_closed", "err_kind", logctx.ErrKind(err))
	} else {
		log.Info("shutdown.http_closed")
	}
	stopBackground()
	return err
}

// seedSnippets 는 빈 설치에서도 빠른 답변이 뭔지 보이게 기본값을 넣는다. 있으면 건드리지 않는다.
func seedSnippets(ctx context.Context, store *pgstore.Store, log *slog.Logger) {
	defaults := []struct{ shortcut, title, body string }{
		{"인사", "첫 인사", "안녕하세요, 문의 주셔서 감사합니다. 확인하고 바로 도와드리겠습니다."},
		{"확인중", "확인 중 안내", "문의하신 내용을 확인하고 있습니다. 잠시만 기다려 주세요."},
		{"배송", "배송 지연 안내", "배송이 지연되어 불편을 드려 죄송합니다. 송장번호를 확인해 다시 안내드리겠습니다."},
		{"마무리", "상담 종료 인사", "더 궁금하신 점이 있으면 언제든 말씀해 주세요. 감사합니다."},
	}
	for _, d := range defaults {
		if err := store.EnsureSnippet(ctx, core.DefaultWorkspaceID, d.shortcut, d.title, d.body); err != nil {
			log.Warn("seed.snippet_failed", "shortcut", d.shortcut, "err_kind", logctx.ErrKind(err))
		}
	}
}

// openWithRetry 는 DB 가 올라오는 동안(compose 기동, 배포 중 DB 재시작)을 버틴다. 명령(migrate·bootstrap)만 쓴다 — 서버는 기다리지 않고 뜬다.
func openWithRetry(ctx context.Context, url string, maxConns int32, log *slog.Logger) (*pgstore.Store, error) {
	backoff := 300 * time.Millisecond
	for attempt := 1; ; attempt++ {
		store, err := pgstore.OpenWithPool(ctx, url, maxConns)
		if err == nil {
			return store, nil
		}
		if attempt >= 20 || ctx.Err() != nil {
			return nil, err
		}
		log.Warn("db.connect_retry", "attempt", attempt, "err_kind", logctx.ErrKind(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 3*time.Second)
	}
}
