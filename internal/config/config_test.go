package config_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/config"
)

// 시드 상담원 비밀번호는 앱의 비밀번호 검사와 같은 기준이다. 하한은 8글자, 상한은 bcrypt 가 받는 72바이트다.
// 상한을 넘으면 기동 중에 해시가 실패하므로 설정을 읽을 때 거절한다.
func TestLoad_시드비밀번호(t *testing.T) {
	t.Run("시드 상담원 비밀번호가 8글자 미만이거나 72바이트를 넘으면 설정 로드가 오류다", func(t *testing.T) {
		testSeedPassword(t)
	})
}

func testSeedPassword(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		ok             bool
	}{
		{"한글 7글자는 거절한다", strings.Repeat("가", 7), false},
		{"한글 8글자는 받는다", strings.Repeat("가", 8), true},
		{"정확히 72바이트는 받는다", strings.Repeat("a", 72), true},
		{"영문 73자(73바이트)는 거절한다", strings.Repeat("a", 73), false},
		{"멀티바이트가 72바이트 경계에 걸친 73바이트는 거절한다", "a" + strings.Repeat("가", 24), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CHAT_SEED_AGENT_PASSWORD", tc.password)
			_, err := config.Load()
			if tc.ok && err != nil {
				t.Errorf("받아야 한다: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "CHAT_SEED_AGENT_PASSWORD")) {
				t.Errorf("시드 비밀번호 오류여야 한다: %v", err)
			}
		})
	}
}

// 첨부 한도는 휴대폰으로 찍은 1~2분 영상이 들어가는 크기다(D37). 정책에 묶인 숫자라 리터럴로 고정한다.
func TestLoad_첨부한도(t *testing.T) {
	t.Run("첨부 한도 기본값은 100MB 다", func(t *testing.T) {
		t.Setenv("CHAT_MAX_UPLOAD_MB", "")
		c, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.MaxUploadBytes != 100<<20 {
			t.Errorf("기본 한도 %d 바이트, 100MB(%d) 여야 한다", c.MaxUploadBytes, 100<<20)
		}
	})
}

// 첨부 저장소 종류는 local 과 s3 뿐이다. 다른 값은 설정을 읽을 때 거절한다 — DB 를 열고 조립까지 가서야 걸리면 기동 로그가
// config.invalid 가 아니라 server.exit 로 나와 런북(docs/deploy.md 20.1)으로 찾지 못한다. 오류에는 값을 싣지 않는다.
func TestLoad_첨부저장소종류는local과s3뿐이고다른값은설정을읽을때거절한다(t *testing.T) {
	for _, v := range []string{"local", "s3", ""} {
		t.Setenv("CHAT_STORAGE", v)
		if _, err := config.Load(); err != nil {
			t.Errorf("CHAT_STORAGE=%q 를 거절했다: %v", v, err)
		}
	}
	t.Setenv("CHAT_STORAGE", "minio-bucket-x")
	_, err := config.Load()
	var bad *config.Error
	if !errors.As(err, &bad) || bad.Key != "CHAT_STORAGE" {
		t.Fatalf("CHAT_STORAGE 설정 오류여야 한다: %v", err)
	}
	if strings.Contains(err.Error(), "minio-bucket-x") {
		t.Errorf("오류에 값이 들어갔다: %v", err)
	}
}

// S3 올리기 방식은 post(양식 업로드)와 put(서명된 PUT 주소)뿐이다(D42). 기본은 post 라 설정하지 않은 설치는 전과 같다.
// 오타를 기본값으로 삼키면 PUT 만 받는 저장소에서 첫 업로드가 되어서야 실패하므로 설정을 읽을 때 거절한다. 오류에는 값을 싣지 않는다.
func TestLoad_S3올리기방식은post와put뿐이고기본은post이며다른값은설정을읽을때거절한다(t *testing.T) {
	t.Setenv("CHAT_S3_UPLOAD", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.S3.Upload != "post" {
		t.Errorf("기본 올리기 방식 %q, post 여야 한다", c.S3.Upload)
	}
	for v, want := range map[string]string{"post": "post", "put": "put", "PUT": "put"} {
		t.Setenv("CHAT_S3_UPLOAD", v)
		c, err := config.Load()
		if err != nil || c.S3.Upload != want {
			t.Errorf("CHAT_S3_UPLOAD=%q: %q, %v", v, c.S3.Upload, err)
		}
	}
	t.Setenv("CHAT_S3_UPLOAD", "presigned-r2x")
	_, err = config.Load()
	var bad *config.Error
	if !errors.As(err, &bad) || bad.Key != "CHAT_S3_UPLOAD" {
		t.Fatalf("CHAT_S3_UPLOAD 설정 오류여야 한다: %v", err)
	}
	if strings.Contains(err.Error(), "presigned-r2x") {
		t.Errorf("오류에 값이 들어갔다: %v", err)
	}
}

// DB 풀 크기는 pgxpool 에 int32 로 간다. int32 를 넘는 값을 받으면 음수로 돌아 풀이 깨진 채 기동한다. 설정을 읽을 때 거절한다.
func TestLoad_DB커넥션상한이int32범위를넘으면설정을읽을때거절한다(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"2147483647", true},
		{"2147483648", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CHAT_DB_MAX_CONNS", tc.value)
			c, err := config.Load()
			if tc.ok {
				if err != nil || c.DBMaxConns != 2147483647 {
					t.Errorf("받아야 한다: %d, %v", c.DBMaxConns, err)
				}
				return
			}
			var ce *config.Error
			if !errors.As(err, &ce) || ce.Key != "CHAT_DB_MAX_CONNS" {
				t.Fatalf("CHAT_DB_MAX_CONNS 설정 오류여야 한다: %v", err)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("오류에 값을 싣지 않는다: %v", err)
			}
		})
	}
}

// 세션 유휴 한도(D46 고침). 상담원 12시간이 기본이고 고객은 0(유휴 만료 없음)이 기본이다.
// 상담원은 0 이하를 거절한다 — 상담원 유휴 만료를 끄는 값이 오타로 들어가지 않게. 고객은 0 을 받고 음수만 거절한다.
func TestLoad_세션유휴한도는상담원12시간이기본이고고객은기본으로없다(t *testing.T) {
	t.Setenv("CHAT_SESSION_IDLE_CUSTOMER", "")
	t.Setenv("CHAT_SESSION_IDLE_AGENT", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionIdleCustomer != 0 || c.SessionIdleAgent != 12*time.Hour {
		t.Errorf("기본값 고객 %s 상담원 %s", c.SessionIdleCustomer, c.SessionIdleAgent)
	}
	reject := func(t *testing.T, key string) {
		t.Helper()
		var ce *config.Error
		if _, err := config.Load(); !errors.As(err, &ce) || ce.Key != key {
			t.Errorf("%s=%s 를 받았다: %v", key, os.Getenv(key), err)
		}
	}
	t.Run("상담원 0 은 거절한다", func(t *testing.T) {
		t.Setenv("CHAT_SESSION_IDLE_AGENT", "0")
		reject(t, "CHAT_SESSION_IDLE_AGENT")
	})
	t.Run("상담원 음수는 거절한다", func(t *testing.T) {
		t.Setenv("CHAT_SESSION_IDLE_AGENT", "-1s")
		reject(t, "CHAT_SESSION_IDLE_AGENT")
	})
	t.Run("고객 음수는 거절한다", func(t *testing.T) {
		t.Setenv("CHAT_SESSION_IDLE_CUSTOMER", "-1s")
		reject(t, "CHAT_SESSION_IDLE_CUSTOMER")
	})
	t.Run("고객 0 은 받는다 — 유휴 만료 없음", func(t *testing.T) {
		t.Setenv("CHAT_SESSION_IDLE_CUSTOMER", "0")
		if c, err := config.Load(); err != nil || c.SessionIdleCustomer != 0 {
			t.Errorf("고객 0: %s, %v", c.SessionIdleCustomer, err)
		}
	})
	t.Run("운영자가 고객 유휴 한도를 켤 수 있다", func(t *testing.T) {
		t.Setenv("CHAT_SESSION_IDLE_CUSTOMER", "2160h")
		if c, err := config.Load(); err != nil || c.SessionIdleCustomer != 2160*time.Hour {
			t.Errorf("고객 2160h: %s, %v", c.SessionIdleCustomer, err)
		}
	})
}

// 회원 서명 비밀은 HMAC-SHA256 키다. 32바이트보다 짧으면 회원 토큰 하나로 비밀을 오프라인으로 맞춰 볼 수 있다(DEF-93).
func TestLoad_회원서명비밀은32바이트이상이어야하고비어있으면서버가만든다(t *testing.T) {
	for _, tc := range []struct {
		name, v string
		ok      bool
	}{
		{"비어 있으면 받는다(서버가 만든다)", "", true},
		{"31바이트는 거절한다", strings.Repeat("k", 31), false},
		{"32바이트는 받는다", strings.Repeat("k", 32), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CHAT_MEMBER_SECRET", tc.v)
			_, err := config.Load()
			var ce *config.Error
			if tc.ok && err != nil {
				t.Errorf("받아야 한다: %v", err)
			}
			if !tc.ok && (!errors.As(err, &ce) || ce.Key != "CHAT_MEMBER_SECRET" || strings.Contains(err.Error(), tc.v)) {
				t.Errorf("CHAT_MEMBER_SECRET 오류여야 하고 값을 싣지 않는다: %v", err)
			}
		})
	}
}

// D47: config.invalid 의 rule 은 변수 이름 없이 혼자 읽히는 문장이다. 전에는 "은 text 또는 json 이다" 처럼 조사로 시작해
// 로그의 key 와 떨어지면 뜻이 끊겼다.
func TestLoad_설정오류의규칙은조사로시작하지않고혼자읽히는문장이다(t *testing.T) {
	cases := map[string]string{
		"CHAT_LOG_FORMAT":       "yaml",
		"CHAT_STORAGE":          "ftp",
		"CHAT_CATCHUP_INTERVAL": "0s",
		"CHAT_SHUTDOWN_DRAIN":   "-1s",
		"CHAT_LOG_LEVEL":        "loud",
	}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			t.Setenv("CHAT_DATABASE_URL", "postgres://x")
			t.Setenv(key, value)
			_, err := config.Load()
			var ce *config.Error
			if !errors.As(err, &ce) || ce.Key != key {
				t.Fatalf("%s=%s 를 거절하지 않았다: %v", key, value, err)
			}
			for _, particle := range []string{"은 ", "는 ", "이 ", "가 "} {
				if strings.HasPrefix(ce.Rule, particle) {
					t.Errorf("rule %q 가 조사로 시작한다", ce.Rule)
				}
			}
			if !strings.HasSuffix(ce.Rule, "한다") && !strings.HasSuffix(ce.Rule, "안 된다") && !strings.HasSuffix(ce.Rule, ")") {
				t.Errorf("rule %q 이 무엇이어야 하는지 말하는 문장이 아니다", ce.Rule)
			}
		})
	}
}

// 서버 기동은 DB 에 쓰지 않는다(D49). 시드는 `server bootstrap` 이 하고, 기본은 끔이며 비밀번호 기본값이 없다 —
// 설정을 빠뜨린 공개 설치에 chat1234 계정이 생기지 않는다.
func TestLoad_시드는기본으로꺼져있고시드비밀번호에기본값이없다(t *testing.T) {
	t.Setenv("CHAT_SEED_DEMO", "")
	t.Setenv("CHAT_SEED_AGENT_PASSWORD", "")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SeedDemo {
		t.Error("CHAT_SEED_DEMO 기본값이 true 다")
	}
	if c.SeedAgentPass != "" {
		t.Error("CHAT_SEED_AGENT_PASSWORD 에 기본값이 있다")
	}
}

// 마이그레이션 러너의 기본 시한은 정책 숫자다(D49): 잠금 대기 3초, 문장 60초, 잠금 실패 뒤 재시도 5번, 다른 migrate 기다리기 60초.
func TestLoad_마이그레이션기본시한은잠금3초문장60초재시도5번자문잠금60초다(t *testing.T) {
	for _, k := range []string{"CHAT_MIGRATE_LOCK_TIMEOUT", "CHAT_MIGRATE_STATEMENT_TIMEOUT", "CHAT_MIGRATE_RETRIES", "CHAT_MIGRATE_ADVISORY_WAIT"} {
		t.Setenv(k, "")
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.Migrate{LockTimeout: 3 * time.Second, StatementTimeout: 60 * time.Second, Retries: 5, AdvisoryWait: 60 * time.Second}
	if c.Migrate != want {
		t.Errorf("기본값 %+v, %+v 여야 한다", c.Migrate, want)
	}
	t.Setenv("CHAT_MIGRATE_LOCK_TIMEOUT", "-1s")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "CHAT_MIGRATE_LOCK_TIMEOUT") {
		t.Errorf("음수 잠금 시한을 받았다: %v", err)
	}
}

// 재시도 횟수의 0 은 값이다 — 잠금을 한 번 못 잡으면 바로 실패한다. 음수와 정수가 아닌 값은 기본값으로 삼키지 않고 설정 오류다.
// 전에는 0 과 음수가 기본값 5 가 되어 "다시 하지 마라" 를 줄 수 없었고, 오타가 조용히 5 가 됐다(감사 31).
func TestLoad_마이그레이션재시도0은재시도없음이고음수나정수가아니면설정오류다(t *testing.T) {
	t.Setenv("CHAT_MIGRATE_RETRIES", "0")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Migrate.Retries != 0 {
		t.Errorf("CHAT_MIGRATE_RETRIES=0 이 %d 가 됐다. 0 이어야 한다", c.Migrate.Retries)
	}
	t.Setenv("CHAT_MIGRATE_RETRIES", "2")
	if c, err := config.Load(); err != nil || c.Migrate.Retries != 2 {
		t.Errorf("CHAT_MIGRATE_RETRIES=2 → %d %v", c.Migrate.Retries, err)
	}
	for _, v := range []string{"-1", "five", "1.5"} {
		t.Setenv("CHAT_MIGRATE_RETRIES", v)
		_, err := config.Load()
		var ce *config.Error
		if !errors.As(err, &ce) || ce.Key != "CHAT_MIGRATE_RETRIES" {
			t.Errorf("CHAT_MIGRATE_RETRIES=%q 를 받았다: %v", v, err)
		}
	}
}
