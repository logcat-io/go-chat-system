package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 기동 실패 로그는 운영자가 런북(docs/deploy.md 20.1)으로 원인을 좁히는 유일한 단서다. 원문은 싣지 않으니(D30) 사건 이름·단계 이름·
// 변수 이름이 계약이다. 여기서는 테스트 바이너리를 서버로 다시 띄워(main 그대로) 표준 출력·오류에 나온 줄을 본다.

// TestMain 은 CHAT_TEST_RUN_MAIN=1 이면 테스트 대신 서버의 main 을 돈다. 기동 실패 테스트가 이 바이너리를 자식 프로세스로 띄운다.
func TestMain(m *testing.M) {
	if os.Getenv("CHAT_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serverEnv 는 자식 서버의 환경이다. 바깥의 CHAT_*·OTEL_*·AWS_* 는 지우고 준 값만 쓴다.
// DB 주소는 언제나 준다 — 비우면 기본값이 개발 DB(chat)라 테스트가 그 DB 를 건드린다.
func serverEnv(t *testing.T, dbURL string, kv ...string) []string {
	t.Helper()
	if dbURL == "" {
		t.Fatal("자식 서버의 DB 주소가 비었다")
	}
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CHAT_") && !strings.HasPrefix(e, "OTEL_") && !strings.HasPrefix(e, "AWS_") {
			env = append(env, e)
		}
	}
	env = append(env, "CHAT_TEST_RUN_MAIN=1", "CHAT_DATABASE_URL="+dbURL, "CHAT_ADDR=127.0.0.1:0",
		"CHAT_UPLOAD_DIR="+t.TempDir(), "CHAT_SEED_DEMO=false")
	return append(env, kv...)
}

// closedPort 는 아무도 듣지 않는 로컬 주소다.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// runServer 는 자식 서버(args 가 있으면 그 명령)를 띄워 끝날 때까지 기다리고 표준 출력·오류를 합쳐 돌려준다.
func runServer(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("자식 서버가 60초 안에 끝나지 않았다:\n%s", out.String())
	}
	return out.String(), cmd.ProcessState.ExitCode()
}

// jsonLine 은 out 에서 msg 사건의 JSON 줄을 찾는다.
func jsonLine(t *testing.T, out, msg string) map[string]any {
	t.Helper()
	for _, raw := range strings.Split(out, "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == msg {
			return l
		}
	}
	t.Fatalf("%s 줄이 없다:\n%s", msg, out)
	return nil
}

// onlyKeys 는 줄의 속성이 time·level·msg 와 want 뿐인지 본다. 값을 싣는 키가 하나라도 더 붙으면 실패한다.
func onlyKeys(t *testing.T, l map[string]any, want ...string) {
	t.Helper()
	allowed := map[string]bool{"time": true, "level": true, "msg": true}
	for _, k := range want {
		allowed[k] = true
		if _, ok := l[k]; !ok {
			t.Errorf("%v 에 %s 가 없다", l, k)
		}
	}
	for k := range l {
		if !allowed[k] {
			t.Errorf("%v 에 %s 가 더 실렸다", l, k)
		}
	}
}

// 설정이 규칙을 어기면 DB 에 닿기 전에 config.invalid 한 줄로 끝난다. 그 줄은 변수 이름과 규칙만 싣고 값은 싣지 않는다 —
// 값이 비밀번호나 접속 문자열일 수 있다. CHAT_STORAGE 도 여기서 걸린다(전에는 DB 를 연 뒤 조립에서 server.exit stage=assemble 로 나왔다).
func TestStartup_설정오류는config_invalid에변수이름과규칙만싣고값은싣지않는다(t *testing.T) {
	// 로거를 만들기 전이라 표준 slog 의 글자 꼴이다: "… ERROR config.invalid key=… rule=…"
	line := regexp.MustCompile(`ERROR config\.invalid key=(\S+) rule=("[^"]*"|\S+)\n`)
	for _, tc := range []struct{ key, value string }{
		{"CHAT_SEED_AGENT_PASSWORD", "pwLeak7"},
		{"CHAT_LOG_FORMAT", "jsonx-Leak-4f1c"},
		{"CHAT_STORAGE", "s3-Leak-bucket"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			out, code := runServer(t, serverEnv(t, "postgres://chat:dbpass-Leak@"+closedPort(t)+"/nodb?sslmode=disable", tc.key+"="+tc.value))
			if code != 1 {
				t.Errorf("종료 코드 %d, 1 이어야 한다", code)
			}
			m := line.FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("config.invalid 줄이 key·rule 만 싣지 않는다:\n%s", out)
			}
			if m[1] != tc.key {
				t.Errorf("key=%s, %s 여야 한다", m[1], tc.key)
			}
			for _, leak := range []string{tc.value, "Leak"} {
				if strings.Contains(out, leak) {
					t.Errorf("기동 로그에 값 %q 가 있다:\n%s", leak, out)
				}
			}
			if strings.Contains(out, "server.exit") || strings.Contains(out, "DB 접속") {
				t.Errorf("설정 오류인데 DB 쪽으로 갔다:\n%s", out)
			}
		})
	}
}

// server.exit 의 stage 는 런북의 이름 그대로다(db_open·migrate·storage). 줄은 명령·단계·종류만 싣는다 — 접속 문자열의 비밀번호도,
// 저장소 키도 나오지 않는다. 정책에 묶인 이름이라 리터럴로 고정한다. DB 에 붙는 단계(db_open·migrate)는 이제 `server migrate` 의 것이다(D49).
func TestStartup_기동실패는server_exit에런북의단계이름과종류만싣고마이그레이션이면실패한파일이름을더한다(t *testing.T) {
	t.Run("db_open", func(t *testing.T) {
		// migrate 는 DB 가 내려가 있으면 20번까지 다시 붙는다. 첫 재시도 줄을 본 뒤 종료 신호를 보내면 그 단계에서 끝난다.
		cmd := exec.Command(os.Args[0], "migrate")
		cmd.Env = serverEnv(t, "postgres://chat:dbpass-Leak@"+closedPort(t)+"/nodb?sslmode=disable&connect_timeout=1", "CHAT_LOG_FORMAT=json")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var all strings.Builder
		sc := bufio.NewScanner(stdout)
		signaled := false
		deadline := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
		defer deadline.Stop()
		for sc.Scan() {
			all.WriteString(sc.Text() + "\n")
			if !signaled && strings.Contains(sc.Text(), "db.connect_retry") {
				var l map[string]any
				if json.Unmarshal(sc.Bytes(), &l) == nil && l["err_kind"] != "conn_refused" {
					t.Errorf("닫힌 포트의 재시도 종류가 %v 다", l["err_kind"])
				}
				_ = cmd.Process.Signal(syscall.SIGINT)
				signaled = true
			}
		}
		_ = cmd.Wait()
		out := all.String() + errOut.String()
		if !signaled {
			t.Fatalf("db.connect_retry 줄이 없다:\n%s", out)
		}
		exit := jsonLine(t, out, "server.exit")
		onlyKeys(t, exit, "command", "stage", "err_kind")
		if exit["command"] != "migrate" || exit["stage"] != "db_open" || exit["level"] != "ERROR" {
			t.Errorf("server.exit %v, command=migrate stage=db_open 이어야 한다", exit)
		}
		if strings.Contains(out, "Leak") || strings.Contains(out, "nodb") {
			t.Errorf("기동 로그에 접속 정보가 있다:\n%s", out)
		}
	})

	t.Run("db_open 형식", func(t *testing.T) {
		// 서버는 DB 에 붙어 보지 않고 뜬다. 접속 문자열의 형식 오류만 기동을 막는다 — 설정 오류는 기다려도 낫지 않는다.
		out, code := runServer(t, serverEnv(t, "postgres://chat:dbpass-Leak@[::1/nodb", "CHAT_LOG_FORMAT=json"))
		if code != 1 {
			t.Errorf("종료 코드 %d", code)
		}
		exit := jsonLine(t, out, "server.exit")
		onlyKeys(t, exit, "command", "stage", "err_kind")
		if exit["command"] != "serve" || exit["stage"] != "db_open" {
			t.Errorf("server.exit %v, command=serve stage=db_open 이어야 한다", exit)
		}
		if strings.Contains(out, "Leak") || strings.Contains(out, "nodb") {
			t.Errorf("기동 로그에 접속 정보가 있다:\n%s", out)
		}
	})

	t.Run("migrate", func(t *testing.T) {
		// 첫 마이그레이션을 적용하지 않은 것처럼 지우면 그 SQL 이 이미 있는 테이블에 부딪혀 실패한다(42P07).
		_, dbURL := testsupport.Isolated(t)
		testsupport.Exec(t, dbURL, "DELETE FROM schema_migrations WHERE name = '0001_init.sql'")
		out, code := runServer(t, serverEnv(t, dbURL, "CHAT_LOG_FORMAT=json"), "migrate")
		if code != 1 {
			t.Errorf("종료 코드 %d", code)
		}
		exit := jsonLine(t, out, "server.exit")
		// 실패한 파일 이름도 싣는다(DEF-85). 리포에 고정된 이름이라 접속 정보가 아니다(D30·D47).
		onlyKeys(t, exit, "command", "stage", "err_kind", "migration")
		if exit["stage"] != "migrate" || exit["err_kind"] != "42P07" || exit["migration"] != "0001_init.sql" {
			t.Errorf("server.exit %v, stage=migrate err_kind=42P07(SQLSTATE) migration=0001_init.sql 이어야 한다", exit)
		}
	})

	t.Run("storage", func(t *testing.T) {
		// 옛 정적 키가 하나만 남은 설정(런북 20.1 의 원인 후보 3)
		_, dbURL := testsupport.Isolated(t)
		out, code := runServer(t, serverEnv(t, dbURL, "CHAT_LOG_FORMAT=json", "CHAT_STORAGE=s3",
			"CHAT_S3_ENDPOINT=http://"+closedPort(t), "CHAT_S3_BUCKET=bucket-Leak", "CHAT_S3_ACCESS_KEY=AKIA-Leak-only"))
		if code != 1 {
			t.Errorf("종료 코드 %d", code)
		}
		exit := jsonLine(t, out, "server.exit")
		onlyKeys(t, exit, "command", "stage", "err_kind")
		if exit["stage"] != "storage" {
			t.Errorf("server.exit %v, stage=storage 여야 한다", exit)
		}
		if strings.Contains(out, "Leak") {
			t.Errorf("기동 로그에 저장소 키·버킷이 있다:\n%s", out)
		}
	})
}
