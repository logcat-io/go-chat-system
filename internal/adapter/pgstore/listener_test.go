package pgstore_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// listenerLog 는 리스너 하나를 띄우고 그 로그를 JSON 레코드로 읽게 한다.
type listenerLog struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func runListener(t *testing.T, dbURL string) listenerLog {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	l := pgstore.NewListener(dbURL, slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return listenerLog{buf: &buf, mu: &mu}
}

func (l listenerLog) records() []map[string]any {
	l.mu.Lock()
	text := l.buf.String()
	l.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		var r map[string]any
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// waitFor 는 msg 사건이 n 개 쌓일 때까지 기다리고 그 사건들을 돌려준다.
func (l listenerLog) waitFor(t *testing.T, msg string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got []map[string]any
		for _, r := range l.records() {
			if r["msg"] == msg {
				got = append(got, r)
			}
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 사건이 %d 개 나오지 않았다. 로그:\n%s", msg, n, l.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l listenerLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// 알람(로그 지표 필터)은 사건 이름을 센다. 문장이 바뀌어도 필터가 조용히 0 이 되지 않게 이름이 계약이다(D30).
func TestListener_사건이름(t *testing.T) {
	t.Run("리스너가 끊기면 listener.reconnect 를 WARN 으로 남기고 다시 붙으면 listener.connected 를 남긴다", func(t *testing.T) {
		t.Run("닿지 않는 DB 면 시도마다 attempt 가 늘고 대기가 두 배가 된다", func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			log := runListener(t, "postgres://nobody:secret-pw@"+addr+"/chat?sslmode=disable&connect_timeout=1")
			got := log.waitFor(t, "listener.reconnect", 3)
			for i, r := range got[:3] {
				if r["level"] != "WARN" {
					t.Errorf("수준: %v", r["level"])
				}
				if r["attempt"] != float64(i+1) {
					t.Errorf("%d번째 attempt = %v", i, r["attempt"])
				}
				if want := float64(int(200) << i); r["after_ms"] != want {
					t.Errorf("%d번째 after_ms = %v, want %v", i, r["after_ms"], want)
				}
			}
			if strings.Contains(log.text(), "secret-pw") {
				t.Error("로그에 DB 비밀번호가 있다")
			}
			// 접속 실패 문구(failed to connect to `user=… database=…`)를 싣지 않는다. 종류만 남긴다(D30)
			for _, leak := range []string{"user=", "database=", "nobody", addr} {
				if strings.Contains(log.text(), leak) {
					t.Errorf("로그에 접속 정보 %q 가 있다:\n%s", leak, log.text())
				}
			}
			for _, r := range got[:3] {
				// 닫힌 포트다. 연결 수준 종류(감사 15)로 "DB 가 내려갔다" 를 가른다
				if _, ok := r["err"]; ok || r["err_kind"] != "conn_refused" {
					t.Errorf("오류는 종류(err_kind=conn_refused)만 실어야 한다: %v", r)
				}
			}
		})

		t.Run("연결을 끊으면 reconnect 뒤 connected 가 나오고 attempt 가 이어진다", func(t *testing.T) {
			u, err := url.Parse(testsupport.URL())
			if err != nil {
				t.Fatal(err)
			}
			app := "listener-test-" + strings.ReplaceAll(id.New(), "-", "")[20:]
			q := u.Query()
			q.Set("application_name", app)
			u.RawQuery = q.Encode()

			log := runListener(t, u.String())
			first := log.waitFor(t, "listener.connected", 1)[0]
			if first["level"] != "INFO" || first["attempt"] != float64(0) {
				t.Fatalf("첫 연결: %v", first)
			}

			ctx := context.Background()
			admin, err := pgx.Connect(ctx, testsupport.URL())
			if err != nil {
				t.Skipf("Postgres 에 붙을 수 없어 건너뛴다: %v", err)
			}
			defer admin.Close(ctx)
			kill := func() {
				t.Helper()
				var killed int
				if err := admin.QueryRow(ctx,
					"SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1) k", app).Scan(&killed); err != nil || killed != 1 {
					t.Fatalf("리스너 연결을 끊지 못했다: killed=%d err=%v", killed, err)
				}
			}

			// 두 번 끊는다. 다시 붙으면 attempt 와 대기가 처음으로 돌아가야 두 번째 끊김도 1번째·200ms 다.
			for round := 1; round <= 2; round++ {
				kill()
				re := log.waitFor(t, "listener.reconnect", round)[round-1]
				if re["level"] != "WARN" || re["attempt"] != float64(1) || re["after_ms"] != float64(200) {
					t.Errorf("%d번째 끊김의 재접속 사건: %v", round, re)
				}
				again := log.waitFor(t, "listener.connected", round+1)[round]
				if again["level"] != "INFO" || again["attempt"] != float64(1) {
					t.Errorf("%d번째 끊김 뒤 다시 붙은 사건: %v", round, again)
				}
			}
			for _, r := range log.records() {
				if m, _ := r["msg"].(string); !strings.HasPrefix(m, "listener.") {
					t.Errorf("사건 이름이 아닌 로그: %q", m)
				}
			}
			// 관리자가 끊은 연결의 오류는 DB 가 답한 것이라 SQLSTATE 로 남거나, 소켓이 먼저 닫혔으면 error 다. 원문(접속 정보)은 없다
			for _, r := range log.waitFor(t, "listener.reconnect", 2) {
				if k, _ := r["err_kind"].(string); r["err"] != nil || (k != "57P01" && k != "error") {
					t.Errorf("끊김의 재접속 사건은 err_kind 만 실어야 한다: %v", r)
				}
			}
			if strings.Contains(log.text(), "user=") || strings.Contains(log.text(), "database=") {
				t.Errorf("로그에 접속 정보가 있다:\n%s", log.text())
			}
		})
	})
}

// 받기만 하고 답하지 않는 DB(장애 조치 중 패킷을 버리는 옛 주소)에 붙을 때도 리스너는 5초 안에 포기하고 listener.reconnect 를
// err_kind=timeout 으로 남긴다. 접속 문자열에 connect_timeout 이 없어도 그렇다(관리형 DB 가 주는 주소에는 흔히 없다). 전에는 시한이 없어
// TCP 가 포기할 때까지 로그 없이 멈췄다(감사 31 실측 35초 넘게).
func TestListener_답하지않는DB에는5초안에접속을포기하고reconnect를timeout으로남긴다(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // 읽지도 답하지도 않는다
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	start := time.Now()
	log := runListener(t, "postgres://nobody:secret-pw@"+ln.Addr().String()+"/chat?sslmode=disable")
	first := log.waitFor(t, "listener.reconnect", 1)[0]
	took := time.Since(start)
	if first["err_kind"] != "timeout" || first["attempt"] != float64(1) {
		t.Errorf("첫 재접속 사건 %v, attempt=1 err_kind=timeout 이어야 한다", first)
	}
	if took < 4500*time.Millisecond || took > 7*time.Second {
		t.Errorf("첫 listener.reconnect 까지 %v — 접속 시한 5초 남짓이어야 한다", took)
	}
	if strings.Contains(log.text(), "secret-pw") || strings.Contains(log.text(), "user=") {
		t.Errorf("로그에 접속 정보가 있다:\n%s", log.text())
	}
}

// pgx 의 PgError 가 logctx.ErrKind 의 SQLSTATE 모양(SQLState())을 채운다. 감싸져 있어도 꺼낸다. 드라이버를 바꾸면 여기가 먼저 깨진다.
func TestListener_DB가답한오류의종류는SQLSTATE다(t *testing.T) {
	wrapped := fmt.Errorf("failed to connect to `user=chat database=chat`: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection"})
	if k := logctx.ErrKind(wrapped); k != "57P01" {
		t.Errorf("PgError 의 종류가 %q 다. SQLSTATE 57P01 이어야 한다", k)
	}
}

// 감사 15: 기동 재시도·리스너 재접속·요청 실패 로그가 원문 대신 종류만 싣게 되면서 "DB 가 내려갔다" 와 "주소가 틀렸다" 를 가를 수 없었다.
// pgx 가 실제로 내는 접속 실패가 연결 수준 종류(conn_refused·dns)로 나오고, 그 종류에 접속 정보가 없다. 드라이버를 바꾸면 여기가 먼저 깨진다.
func TestOpen_DB에닿지못한접속실패의종류는거절과이름풀이를가르고접속정보를싣지않는다(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()
	cases := []struct{ name, url, want string }{
		{"닫힌 포트", "postgres://chat:secretpw@" + closed + "/chat_leak?sslmode=disable&connect_timeout=3", "conn_refused"},
		{"없는 이름", "postgres://chat:secretpw@no-such-host.invalid:5432/chat_leak?sslmode=disable&connect_timeout=3", "dns"},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := pgstore.Open(ctx, tc.url)
		cancel()
		if err == nil {
			t.Fatalf("%s: 붙었다", tc.name)
		}
		kind := logctx.ErrKind(err)
		if kind != tc.want {
			t.Errorf("%s: 종류가 %q 다. %q 여야 한다", tc.name, kind, tc.want)
		}
		for _, leak := range []string{"chat_leak", "secretpw", "user=", "127.0.0.1", "invalid"} {
			if strings.Contains(kind, leak) {
				t.Errorf("%s: 종류 %q 에 접속 정보 %q 가 있다", tc.name, kind, leak)
			}
		}
	}
}
