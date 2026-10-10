// Package config 는 환경변수에서 설정을 읽는다.
package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Error 는 설정 오류다. 어느 변수가 어떤 규칙을 어겼는지만 담고 값은 담지 않는다 — 접속 문자열·비밀번호가 값이다(D30).
// 기동 로그(config.invalid·server.exit)가 Key 와 Rule 을 그대로 싣는다. Rule 은 변수 이름 없이 혼자 읽히는 문장이다
// ("text 또는 json 이어야 한다"). 조사로 시작하는 조각("은 text 또는 json 이다")은 로그에서 key 와 떨어지면 뜻이 끊긴다.
type Error struct {
	Key, Rule string
}

func (e *Error) Error() string { return "config: " + e.Key + ": " + e.Rule }

type Config struct {
	DatabaseURL string
	Addr        string
	// CatchupInterval 은 NOTIFY 를 놓쳤을 때 안전망이 바뀐 대화를 DB 에서 모아 구독자를 깨우는 주기다.
	// 유실 방지의 안전망이라 0 으로 둘 수 없다 (docs/design.md D4).
	CatchupInterval time.Duration
	// SessionTTL 은 상담원 세션의 절대 수명이고 회원 고객 세션 수명의 상한이다(회원은 회원 토큰 exp 와 이른 쪽). 익명 고객 세션에는 걸리지 않는다(D46 고침).
	SessionTTL time.Duration
	// SessionIdleCustomer·SessionIdleAgent 는 세션을 이만큼 안 쓰면 끝나는 유휴 한도다(D46). 절대 수명(SessionTTL)은 그대로 산다.
	// 고객은 0 이 기본이고 "유휴 만료 없음" 이다.
	SessionIdleCustomer time.Duration
	SessionIdleAgent    time.Duration
	SeedAgentEmail      string
	SeedAgentPass       string
	SeedAgentName       string
	SeedAgentCount      int
	DBMaxConns          int
	// MemberSecret 은 기본 워크스페이스의 회원 서명 비밀 초기값이다. 비어 있던 비밀을 채울 때 한 번만 쓴다.
	MemberSecret string
	// SeedDemo 가 true 면 `server migrate`·`server bootstrap` 이 시드 상담원·빠른 답변을 만든다. 서버 기동은 시드하지 않는다(D49).
	// 기본은 false 이고 비밀번호 기본값도 없다 — 켰는데 CHAT_SEED_AGENT_PASSWORD 가 비면 bootstrap 이 실패한다.
	SeedDemo bool
	// Migrate 는 `server migrate` 의 시한과 재시도다(D49). 서버 기동은 쓰지 않는다.
	Migrate Migrate
	// PublicURL 은 초대 링크처럼 바깥에서 여는 주소의 앞부분이다.
	PublicURL string
	// EntryRatePerMinute 는 토큰 없는 입구 하나당 IP 당 분당 한도다. 입구마다 따로 센다.
	EntryRatePerMinute int
	// TrustedProxies 는 앞에 선 역프록시 수다. 0 이면 X-Forwarded-For 를 믿지 않는다.
	// 직접 노출된 서버가 이 헤더를 믿으면 한도를 헤더 한 줄로 우회한다.
	TrustedProxies int
	// OllamaURL 이 비어 있으면 AI 초안이 꺼진다. 예: http://localhost:11434
	OllamaURL   string
	OllamaModel string
	// Storage 는 첨부가 사는 곳이다: local(서버 디스크) 또는 s3(S3 호환 객체 저장소).
	Storage        string
	UploadDir      string
	MaxUploadBytes int64
	S3             S3
	// MaxPendingUploads 는 대화 하나에 "올렸지만 안 보낸" 첨부의 상한이다.
	MaxPendingUploads int
	// LogFormat 은 text 또는 json 이다. 운영은 json 으로 두고 수집기가 표준 출력을 읽는다.
	LogFormat string
	// LogLevel 은 debug·info·warn·error 중 하나다.
	LogLevel slog.Level
	// ShutdownDrain 은 종료 신호를 받고 /readyz 를 503 으로 바꾼 뒤 WebSocket 을 닫기 전까지 기다리는 시간이다.
	// 로드밸런서 헬스 체크가 이 서버를 빼는 시간을 번다. 기본 0 이다.
	ShutdownDrain time.Duration
}

// Migrate 는 마이그레이션 러너의 시한과 재시도다(D49). 근거는 pgstore 의 Default* 상수에 있다.
type Migrate struct {
	// LockTimeout 은 DDL 이 잠금을 기다리는 한도다. 그동안 그 표의 다른 질의가 DDL 뒤에 줄을 선다.
	LockTimeout time.Duration
	// StatementTimeout 은 트랜잭션 파일의 문장 하나의 한도다. 트랜잭션 없는 파일(CONCURRENTLY)에는 걸지 않는다.
	StatementTimeout time.Duration
	// Retries 는 잠금을 못 잡았을 때 다시 하는 횟수다. 0 이면 다시 하지 않는다(첫 시도로 끝). 음수·정수가 아닌 값은 설정 오류다.
	Retries int
	// AdvisoryWait 는 다른 migrate 가 끝나기를 기다리는 한도다.
	AdvisoryWait time.Duration
}

// S3 는 객체 저장소 접속 정보다. MinIO 도 같은 값으로 붙는다.
type S3 struct {
	Endpoint string
	// PublicEndpoint 는 브라우저가 쓰는 주소다. 서버가 컨테이너 안에서 다른 이름으로 저장소를 부를 때만 따로 준다.
	PublicEndpoint string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	PathStyle      bool
	// Upload 는 브라우저가 올리는 방식이다: post(양식 업로드, 기본) 또는 put(서명된 PUT 주소, 양식 업로드가 없는 S3 호환 저장소). D42.
	Upload string
	// CreateBucket 이 true 면 뜰 때 버킷을 만든다. 로컬 개발용이다.
	CreateBucket bool
}

func Load() (Config, error) {
	retries, retriesOK := envCount("CHAT_MIGRATE_RETRIES", 5)
	c := Config{
		DatabaseURL:     env("CHAT_DATABASE_URL", "postgres://chat:chat@127.0.0.1:56432/chat?sslmode=disable"),
		Addr:            env("CHAT_ADDR", ":8090"),
		CatchupInterval: envDuration("CHAT_CATCHUP_INTERVAL", 5*time.Second),
		SessionTTL:      envDuration("CHAT_SESSION_TTL", 720*time.Hour),
		// 고객 0(없음, D46 고침): 교환·반품처럼 브랜드사 처리가 길어져 몇 주~몇 달 뒤 돌아오는 고객도 같은 브라우저면 상담이 이어져야 한다.
		// 운영자가 원하면 켤 수 있다(익명·회원 고객 세션 모두에 걸린다). 익명 고객 세션은 절대 수명도 없다.
		// 상담원 12시간: 한 근무(데스크를 열어 두면 WebSocket 재확인이 사용 시각을 올린다)를 덮고, 퇴근 뒤 버려진 브라우저의 세션은 다음 날 아침 전에 죽는다.
		SessionIdleCustomer: envDuration("CHAT_SESSION_IDLE_CUSTOMER", 0),
		SessionIdleAgent:    envDuration("CHAT_SESSION_IDLE_AGENT", 12*time.Hour),
		SeedAgentEmail:      env("CHAT_SEED_AGENT_EMAIL", "agent@example.com"),
		SeedAgentPass:       os.Getenv("CHAT_SEED_AGENT_PASSWORD"),
		SeedAgentName:       env("CHAT_SEED_AGENT_NAME", "상담원"),
		SeedAgentCount:      envInt("CHAT_SEED_AGENT_COUNT", 3),
		SeedDemo:            envBool("CHAT_SEED_DEMO", false),
		Migrate: Migrate{
			LockTimeout:      envDuration("CHAT_MIGRATE_LOCK_TIMEOUT", 3*time.Second),
			StatementTimeout: envDuration("CHAT_MIGRATE_STATEMENT_TIMEOUT", time.Minute),
			Retries:          retries,
			AdvisoryWait:     envDuration("CHAT_MIGRATE_ADVISORY_WAIT", time.Minute),
		},
		DBMaxConns:         envInt("CHAT_DB_MAX_CONNS", 40),
		MemberSecret:       os.Getenv("CHAT_MEMBER_SECRET"),
		PublicURL:          strings.TrimRight(env("CHAT_PUBLIC_URL", "http://localhost:8090"), "/"),
		EntryRatePerMinute: envInt("CHAT_ENTRY_RATE_PER_MINUTE", 30),
		TrustedProxies:     envInt("CHAT_TRUSTED_PROXIES", 0),
		OllamaURL:          os.Getenv("CHAT_OLLAMA_URL"),
		OllamaModel:        env("CHAT_OLLAMA_MODEL", "qwen2.5:7b"),
		Storage:            env("CHAT_STORAGE", "local"),
		UploadDir:          env("CHAT_UPLOAD_DIR", "./data/uploads"),
		S3: S3{
			Endpoint:       env("CHAT_S3_ENDPOINT", ""),
			PublicEndpoint: env("CHAT_S3_PUBLIC_ENDPOINT", ""),
			Region:         env("CHAT_S3_REGION", "us-east-1"),
			Bucket:         env("CHAT_S3_BUCKET", ""),
			AccessKey:      env("CHAT_S3_ACCESS_KEY", ""),
			SecretKey:      env("CHAT_S3_SECRET_KEY", ""),
			PathStyle:      envBool("CHAT_S3_PATH_STYLE", true),
			Upload:         strings.ToLower(env("CHAT_S3_UPLOAD", "post")),
			CreateBucket:   envBool("CHAT_S3_CREATE_BUCKET", false),
		},
		MaxUploadBytes:    int64(envInt("CHAT_MAX_UPLOAD_MB", 100)) << 20,
		MaxPendingUploads: envInt("CHAT_MAX_PENDING_UPLOADS", 10),
		LogFormat:         strings.ToLower(env("CHAT_LOG_FORMAT", "text")),
		ShutdownDrain:     envDuration("CHAT_SHUTDOWN_DRAIN", 0),
	}
	if c.DatabaseURL == "" {
		return c, &Error{Key: "CHAT_DATABASE_URL", Rule: "비어 있으면 안 된다"}
	}
	if c.CatchupInterval <= 0 {
		return c, &Error{Key: "CHAT_CATCHUP_INTERVAL", Rule: "0 보다 커야 한다"}
	}
	// 하한은 글자 수, 상한은 바이트 수다. 앱의 비밀번호 검사와 같은 기준이다. 비어 있으면 시드를 켠 bootstrap 만 거절한다.
	if c.SeedAgentPass != "" && utf8.RuneCountInString(c.SeedAgentPass) < 8 {
		return c, &Error{Key: "CHAT_SEED_AGENT_PASSWORD", Rule: "8자 이상이어야 한다"}
	}
	// bcrypt 의 상한(auth.MaxPasswordBytes)이다. 넘으면 시드 계정의 해시가 기동 중에 실패한다.
	if len(c.SeedAgentPass) > 72 {
		return c, &Error{Key: "CHAT_SEED_AGENT_PASSWORD", Rule: "72바이트를 넘으면 안 된다"}
	}
	// 고객은 0 이 "유휴 만료 없음" 이라 받는다. 상담원은 0 이하를 거절한다 — 상담원 유휴 만료를 끄는 값이 오타로 들어가지 않게.
	if c.SessionIdleCustomer < 0 {
		return c, &Error{Key: "CHAT_SESSION_IDLE_CUSTOMER", Rule: "0 이상이어야 한다(0 이면 유휴 만료가 없다)"}
	}
	if c.SessionIdleAgent <= 0 {
		return c, &Error{Key: "CHAT_SESSION_IDLE_AGENT", Rule: "0 보다 커야 한다"}
	}
	// 회원 서명 비밀은 HMAC-SHA256 키다. 짧으면 회원 토큰 하나를 들고 오프라인으로 비밀을 맞춰 볼 수 있다(RFC 8725 3.5, DEF-93).
	// 해시 출력과 같은 32바이트를 하한으로 둔다. 비어 있으면 서버가 32바이트 난수(16진수 64자)를 만든다.
	if c.MemberSecret != "" && len(c.MemberSecret) < MinMemberSecretBytes {
		return c, &Error{Key: "CHAT_MEMBER_SECRET", Rule: "32바이트 이상이어야 한다"}
	}
	if c.Migrate.LockTimeout <= 0 {
		return c, &Error{Key: "CHAT_MIGRATE_LOCK_TIMEOUT", Rule: "0 보다 커야 한다"}
	}
	if c.Migrate.StatementTimeout <= 0 {
		return c, &Error{Key: "CHAT_MIGRATE_STATEMENT_TIMEOUT", Rule: "0 보다 커야 한다"}
	}
	// 0 은 값이다(재시도 없음). 전에는 0 과 음수를 기본값 5 로 삼켜 "다시 하지 마라" 를 줄 방법이 없었다(감사 31).
	if !retriesOK {
		return c, &Error{Key: "CHAT_MIGRATE_RETRIES", Rule: "0 이상의 정수여야 한다(0 이면 다시 하지 않는다)"}
	}
	if c.Migrate.AdvisoryWait <= 0 {
		return c, &Error{Key: "CHAT_MIGRATE_ADVISORY_WAIT", Rule: "0 보다 커야 한다"}
	}
	if c.ShutdownDrain < 0 {
		return c, &Error{Key: "CHAT_SHUTDOWN_DRAIN", Rule: "0 이상이어야 한다"}
	}
	if c.MaxUploadBytes <= 0 {
		return c, &Error{Key: "CHAT_MAX_UPLOAD_MB", Rule: "0 보다 커야 한다"}
	}
	// 풀 크기는 pgxpool 에 int32 로 넘긴다. 넘치면 음수로 돌아 풀 설정이 깨진 채 기동한다(golangci-lint gosec G115 가 찾았다).
	if c.DBMaxConns > math.MaxInt32 {
		return c, &Error{Key: "CHAT_DB_MAX_CONNS", Rule: "2147483647 을 넘으면 안 된다"}
	}
	if c.MaxPendingUploads <= 0 {
		return c, &Error{Key: "CHAT_MAX_PENDING_UPLOADS", Rule: "0 보다 커야 한다"}
	}
	// 오타 난 로그 설정을 기본값으로 삼키면 운영에서 json 인 줄 알았던 로그가 text 로 쌓인다. 시작을 거부한다.
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return c, &Error{Key: "CHAT_LOG_FORMAT", Rule: "text 또는 json 이어야 한다"}
	}
	// 저장소 종류도 설정을 읽을 때 거른다. 전에는 DB 를 열고 마이그레이션을 마친 뒤 조립에서야 걸려 server.exit stage=assemble 로
	// 나왔다 — 런북(docs/deploy.md 20.1)은 설정 오류를 config.invalid 로 찾는다(감사 16).
	if c.Storage != "local" && c.Storage != "s3" {
		return c, &Error{Key: "CHAT_STORAGE", Rule: "local 또는 s3 여야 한다"}
	}
	// 올리기 방식의 오타를 기본값(post)으로 삼키면 PUT 만 받는 저장소에서 첫 업로드가 되어서야 실패한다. 저장소 종류와 같이 설정을 읽을 때 거른다.
	if c.S3.Upload != "post" && c.S3.Upload != "put" {
		return c, &Error{Key: "CHAT_S3_UPLOAD", Rule: "post 또는 put 이어야 한다"}
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("CHAT_LOG_LEVEL", "info"))); err != nil {
		return c, &Error{Key: "CHAT_LOG_LEVEL", Rule: "debug·info·warn·error 중 하나여야 한다"}
	}
	return c, nil
}

// MinMemberSecretBytes 는 CHAT_MEMBER_SECRET 의 하한이다(DEF-93).
const MinMemberSecretBytes = 32

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

// envCount 는 0 을 값으로 받는 정수 설정이다. 비어 있으면 def, 0 이상의 정수면 그 값이고, 그 밖(음수·정수 아님)은 ok=false 다.
func envCount(key string, def int) (int, bool) {
	v := os.Getenv(key)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	fmt.Fprintf(os.Stderr, "config: %s 를 해석하지 못해 기본값 %s 을 쓴다\n", key, def)
	return def
}
