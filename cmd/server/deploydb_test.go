package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 배포와 DB 를 뗀다(D49). 서버 기동은 DB 에 쓰지 않고, DB 가 없어도 뜨며, 스키마가 모자라면 /readyz 만 503 이다.
// 마이그레이션·시드·보정은 `server migrate`·`server bootstrap` 이 한다. 여기서는 테스트 바이너리를 자식 프로세스로 띄워(main 그대로) 본다.

// child 는 자식 서버다. 표준 출력을 모으고, server.started 의 주소를 안다.
type child struct {
	t    *testing.T
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  strings.Builder
	addr string
	done chan struct{}
}

// startChild 는 자식 서버를 띄우고 server.started 줄이 나올 때까지 기다린다. 테스트가 끝나면 종료 신호를 보낸다.
func startChild(t *testing.T, env []string) *child {
	t.Helper()
	c := &child{t: t, cmd: exec.Command(os.Args[0]), done: make(chan struct{})}
	env = append(env, "CHAT_LOG_FORMAT=json")
	c.cmd.Env = env
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.cmd.Stderr = c.cmd.Stdout
	started := make(chan string, 1)
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(c.done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.mu.Lock()
			c.out.WriteString(sc.Text() + "\n")
			c.mu.Unlock()
			var l map[string]any
			if json.Unmarshal(sc.Bytes(), &l) == nil && l["msg"] == "server.started" {
				started <- l["addr"].(string)
			}
		}
		_ = c.cmd.Wait()
	}()
	t.Cleanup(c.stop)
	select {
	case c.addr = <-started:
	case <-c.done:
		t.Fatalf("자식 서버가 뜨기 전에 끝났다:\n%s", c.output())
	case <-time.After(15 * time.Second):
		t.Fatalf("자식 서버가 15초 안에 server.started 를 남기지 않았다:\n%s", c.output())
	}
	return c
}

func (c *child) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

// stop 은 종료 신호를 보내고 끝나기를 기다린다. PID 로만 끝낸다.
func (c *child) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-c.done:
	case <-time.After(20 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// get 은 상태 코드와 오류 코드(있으면)를 돌려준다.
func (c *child) get(path string) (int, string) {
	c.t.Helper()
	res, err := http.Get("http://" + c.addr + path)
	if err != nil {
		c.t.Fatalf("%s: %v", path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, errCode(out)
}

// waitReady 는 /readyz 가 want 가 될 때까지 기다린다.
func (c *child) waitReady(want int, within time.Duration) (int, string) {
	c.t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, ec := c.get("/readyz")
		if code == want || time.Now().After(deadline) {
			return code, ec
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dbProxy 는 테스트 DB 앞의 TCP 중계다. 내리면 그 주소로 붙을 수 없다(연결 거절). DB 재시작을 흉내 낸다 — Postgres 는 건드리지 않는다.
type dbProxy struct {
	addr, target string
	mu           sync.Mutex
	ln           net.Listener
	conns        []net.Conn
}

func newDBProxy(t *testing.T, dbURL string) (*dbProxy, string) {
	t.Helper()
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	p := &dbProxy{addr: closedPort(t), target: u.Host}
	t.Cleanup(p.down)
	u.Host = p.addr
	return p, u.String()
}

func (p *dbProxy) up(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); u.Close() }()
			go func() { _, _ = io.Copy(c, u); c.Close() }()
		}
	}()
}

func (p *dbProxy) down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// snapshot 은 public 스키마의 관계 목록, 시퀀스 값, 표마다 행 버전(ctid·xmin)과 내용의 해시다. 내용이 같아도 행이 한 번 쓰이면
// (같은 값으로 UPDATE 해도) 새 행 버전이 생겨 ctid·xmin 이 바뀐다 — 그래서 "바꾸지 않는다" 가 아니라 "쓰지 않는다" 를 본다(감사 31: 내용만
// 비교하던 때 기동·두 번째 migrate 에 같은 값 UPDATE 를 넣은 변이가 살았다). 통계(pg_stat_user_tables)는 백엔드가 끝날 때 늦게 반영돼
// 순서가 흔들리므로 쓰지 않는다. 행을 잠그기만 하는 것(SELECT … FOR UPDATE)은 쓰기로 보지 않는다.
func snapshot(t *testing.T, dbURL string) map[string]string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	out := map[string]string{}
	var rels, seqs string
	if err := c.QueryRow(ctx, `SELECT coalesce(string_agg(c.relname || ':' || c.relkind::text, ',' ORDER BY c.relname), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	out["(relations)"] = rels
	if err := c.QueryRow(ctx, `SELECT coalesce(string_agg(sequencename || ':' || coalesce(last_value::text, '-'), ',' ORDER BY sequencename), '')
		FROM pg_sequences WHERE schemaname = 'public'`).Scan(&seqs); err != nil {
		t.Fatal(err)
	}
	out["(sequences)"] = seqs
	rows, err := c.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range tables {
		var sum string
		if err := c.QueryRow(ctx, `SELECT count(*) || ':' || md5(coalesce(string_agg(x.ctid::text || '@' || x.xmin::text || '=' || x::text, '|' ORDER BY x.ctid), ''))
			FROM "`+tbl+`" x`).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		out[tbl] = sum
	}
	return out
}

func diffSnapshots(before, after map[string]string) []string {
	var d []string
	for k, v := range after {
		if before[k] != v {
			d = append(d, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			d = append(d, k)
		}
	}
	return d
}

// emptyDB 는 마이그레이션도 하지 않은 빈 DB 를 만든다. 끝나면 지운다.
func emptyDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testsupport.URL())
	if err != nil {
		t.Skipf("Postgres 에 붙을 수 없어 건너뛴다: %v", err)
	}
	name := "chat_e_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close(ctx)
		t.Skipf("빈 DB 를 만들 수 없어 건너뛴다: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`)
		admin.Close(ctx)
	})
	u, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// 서버 기동은 DB 에 쓰지 않는다. 전에는 기동마다 schema_migrations 를 만들고(IF NOT EXISTS), 시드 상담원·빠른 답변을 넣고,
// 관리자 0명을 보정하고, 빈 회원 서명 비밀을 채웠다 — 배포마다 DB 에 쓰기가 갔다. 시드를 켜 둔 설정으로 띄워도, 그 쓰기가 다 일어날
// 상태(상담원 0명, 빈 회원 서명 비밀)에서도 기동 전후 모든 표의 행 버전(같은 값으로 고쳐도 바뀐다)과 관계 목록·시퀀스가 같다.
func TestStartup_서버기동은시드를켜도DB에쓰지않는다(t *testing.T) {
	_, dbURL := testsupport.Isolated(t)
	testsupport.Exec(t, dbURL, "UPDATE workspaces SET member_secret = ''")
	before := snapshot(t, dbURL)

	c := startChild(t, serverEnv(t, dbURL, "CHAT_SEED_DEMO=true", "CHAT_SEED_AGENT_PASSWORD=seed-pass-1234"))
	if code, ec := c.waitReady(http.StatusOK, 10*time.Second); code != http.StatusOK {
		t.Fatalf("readyz %d %s:\n%s", code, ec, c.output())
	}
	for _, p := range []string{"/healthz", "/widget", "/desk"} {
		if code, _ := c.get(p); code != http.StatusOK {
			t.Errorf("%s %d", p, code)
		}
	}
	c.stop()

	if d := diffSnapshots(before, snapshot(t, dbURL)); len(d) > 0 {
		t.Errorf("서버 기동이 DB 에 썼다: %v\n%s", d, c.output())
	}
	for _, msg := range []string{"seed.agent_ready", "seed.admin_promoted", "seed.member_secret_filled"} {
		if strings.Contains(c.output(), msg) {
			t.Errorf("서버 기동이 %s 를 남겼다", msg)
		}
	}
}

// DB 가 재시작 중이어도 서버는 뜬다. 프로세스 생존(/healthz)은 200, 준비(/readyz)는 503 db_down 이다. DB 가 돌아오면 다시 띄우지 않아도
// /readyz 가 200 이 된다. 전에는 DB 에 20번(약 50초) 붙어 보다 종료 코드 1 로 끝났고, 배포 헬스체크(/readyz)가 그동안 실패했다.
func TestStartup_DB가없어도떠서healthz는200readyz는503이고DB가돌아오면readyz가200이다(t *testing.T) {
	_, dbURL := testsupport.Isolated(t)
	proxy, viaProxy := newDBProxy(t, dbURL)

	start := time.Now()
	c := startChild(t, serverEnv(t, viaProxy+"&connect_timeout=1"))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("DB 가 없을 때 뜨기까지 %v 걸렸다. 기다리지 않아야 한다", d)
	}
	if code, _ := c.get("/healthz"); code != http.StatusOK {
		t.Errorf("DB 없이 healthz %d", code)
	}
	if code, ec := c.get("/readyz"); code != http.StatusServiceUnavailable || ec != "db_down" {
		t.Errorf("DB 없이 readyz %d %s, 503 db_down 이어야 한다", code, ec)
	}

	proxy.up(t)
	if code, ec := c.waitReady(http.StatusOK, 15*time.Second); code != http.StatusOK {
		t.Fatalf("DB 가 돌아왔는데 readyz %d %s:\n%s", code, ec, c.output())
	}
	if strings.Contains(c.output(), "server.exit") {
		t.Errorf("서버가 끝났다:\n%s", c.output())
	}
}

// 서버는 스키마를 읽기만 한다. 이 코드의 마이그레이션이 DB 에 다 없으면(배포의 migrate 단계가 안 돌았다) /readyz 는 503 schema_behind
// 이고 schema.behind(WARN)에 모자란 수와 첫 파일 이름을 남긴다. 스스로 마이그레이션하지 않는다. DB 에만 있는 마이그레이션(더 새 코드가
// 적용한 것)은 상관없다 — 옛 코드는 새 스키마 위에서 서빙한다(expand–contract).
func TestStartup_스키마가모자라면readyz는503schema_behind이고스스로마이그레이션하지않는다(t *testing.T) {
	_, dbURL := testsupport.Isolated(t)
	names, err := pgstore.MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	last := names[len(names)-1]
	testsupport.Exec(t, dbURL, "DELETE FROM schema_migrations WHERE name = $1", last)
	testsupport.Exec(t, dbURL, "INSERT INTO schema_migrations (name) VALUES ('9999_from_newer_code.sql')")

	c := startChild(t, serverEnv(t, dbURL))
	if code, ec := c.waitReady(http.StatusServiceUnavailable, 5*time.Second); code != http.StatusServiceUnavailable || ec != "schema_behind" {
		t.Fatalf("readyz %d %s, 503 schema_behind 이어야 한다", code, ec)
	}
	if code, _ := c.get("/healthz"); code != http.StatusOK {
		t.Errorf("healthz %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.output(), `"schema.behind"`) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	line := jsonLine(t, c.output(), "schema.behind")
	if line["level"] != "WARN" || line["pending"] != float64(1) || line["first_pending"] != last {
		t.Errorf("schema.behind %v, WARN pending=1 first_pending=%s 여야 한다", line, last)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var applied bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", last).Scan(&applied); err != nil || applied {
		t.Fatalf("서버가 스스로 %s 를 기록했다: %v", last, err)
	}
	// migrate 단계가 돈 것과 같다 — 다시 띄우지 않아도 준비된다.
	testsupport.Exec(t, dbURL, "INSERT INTO schema_migrations (name) VALUES ($1)", last)
	if code, ec := c.waitReady(http.StatusOK, 5*time.Second); code != http.StatusOK {
		t.Errorf("스키마가 따라온 뒤 readyz %d %s", code, ec)
	}
}

// `server migrate` 는 배포의 앞 단계 하나다. 빈 DB 에 마이그레이션 전부와 보정(시드는 켰을 때만, 관리자 0명 보정, 회원 서명 비밀)을 하고
// 파일마다 migrate.file_applied 를 남긴다. 두 번째는 아무 행도 쓰지 않는다(같은 값으로 고치는 것도 쓰기다) — 배포마다 돌아도 DB 에 쓰지 않는다.
func TestMigrateCommand_빈DB에마이그레이션과보정을하고두번째는아무행도쓰지않는다(t *testing.T) {
	dbURL := emptyDB(t)
	env := serverEnv(t, dbURL, "CHAT_LOG_FORMAT=json", "CHAT_SEED_DEMO=true", "CHAT_SEED_AGENT_PASSWORD=seed-pass-1234",
		"CHAT_SEED_AGENT_EMAIL=owner@example.com")
	out, code := runServer(t, env, "migrate")
	if code != 0 {
		t.Fatalf("migrate 종료 코드 %d:\n%s", code, out)
	}
	names, err := pgstore.MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, `"msg":"migrate.file_applied"`); n != len(names) {
		t.Errorf("migrate.file_applied %d 줄, %d 줄이어야 한다", n, len(names))
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var agents, admins int
	var secret string
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM agents), (SELECT count(*) FROM agents WHERE role = 'ADMIN'),
		(SELECT member_secret FROM workspaces LIMIT 1)`).Scan(&agents, &admins, &secret); err != nil {
		t.Fatal(err)
	}
	if agents != 3 || admins != 1 || secret == "" {
		t.Errorf("상담원 %d 관리자 %d 회원 서명 비밀 %v — 시드 3명, 관리자 1명, 비밀이 있어야 한다", agents, admins, secret != "")
	}

	before := snapshot(t, dbURL)
	out, code = runServer(t, env, "migrate")
	if code != 0 {
		t.Fatalf("두 번째 migrate 종료 코드 %d:\n%s", code, out)
	}
	if l := jsonLine(t, out, "migrate.started"); l["pending"] != float64(0) {
		t.Errorf("두 번째 migrate.started %v, pending=0 이어야 한다", l)
	}
	if d := diffSnapshots(before, snapshot(t, dbURL)); len(d) > 0 {
		t.Errorf("두 번째 migrate 가 DB 에 썼다: %v\n%s", d, out)
	}
}

// 시드를 켰는데 비밀번호가 없으면 DB 에 닿기 전에 실패한다. 비밀번호 기본값(chat1234)은 없앴다 — 인터넷에 열린 설치에 아는 비밀번호의 계정이 생겼다.
func TestMigrateCommand_시드를켰는데비밀번호가없으면DB에닿기전에실패한다(t *testing.T) {
	for _, cmd := range []string{"migrate", "bootstrap"} {
		t.Run(cmd, func(t *testing.T) {
			out, code := runServer(t, serverEnv(t, "postgres://chat:x@"+closedPort(t)+"/nodb?sslmode=disable", "CHAT_LOG_FORMAT=json", "CHAT_SEED_DEMO=true"), cmd)
			if code != 1 {
				t.Errorf("종료 코드 %d", code)
			}
			exit := jsonLine(t, out, "server.exit")
			if exit["command"] != cmd || exit["stage"] != "seed" || exit["key"] != "CHAT_SEED_AGENT_PASSWORD" {
				t.Errorf("server.exit %v, stage=seed key=CHAT_SEED_AGENT_PASSWORD 여야 한다", exit)
			}
			if strings.Contains(out, "db.connect_retry") {
				t.Errorf("DB 에 붙으려 했다:\n%s", out)
			}
		})
	}
}

// `server bootstrap` 은 마이그레이션 없이 보정만 한다. 스키마가 모자라면 아무것도 쓰지 않고 실패한다 — 반쯤 된 스키마에 행을 넣지 않는다.
func TestBootstrap_스키마가모자라면아무것도쓰지않고실패한다(t *testing.T) {
	dbURL := emptyDB(t)
	before := snapshot(t, dbURL)
	out, code := runServer(t, serverEnv(t, dbURL, "CHAT_LOG_FORMAT=json", "CHAT_SEED_DEMO=true", "CHAT_SEED_AGENT_PASSWORD=seed-pass-1234"), "bootstrap")
	if code != 1 {
		t.Errorf("종료 코드 %d:\n%s", code, out)
	}
	if l := jsonLine(t, out, "bootstrap.schema_behind"); l["first_pending"] != "0001_init.sql" {
		t.Errorf("bootstrap.schema_behind %v", l)
	}
	if d := diffSnapshots(before, snapshot(t, dbURL)); len(d) > 0 {
		t.Errorf("bootstrap 이 DB 에 썼다: %v", d)
	}
}

// 모르는 명령은 아무것도 하지 않고 쓰는 법을 알리며 종료 코드 2 로 끝난다. 인자 값은 싣지 않는다.
func TestCommand_모르는명령은쓰는법을알리고2로끝난다(t *testing.T) {
	out, code := runServer(t, serverEnv(t, "postgres://chat:x@"+closedPort(t)+"/nodb?sslmode=disable"), "migrat-Leak")
	if code != 2 || !strings.Contains(out, usage) || strings.Contains(out, "Leak") {
		t.Errorf("종료 코드 %d, 출력:\n%s", code, out)
	}
}

// 이미지에는 ENTRYPOINT 가 없다. 배포의 1회성 태스크(ECS run-task 의 command 덮어쓰기 ["/app/server","migrate"])가 명령을 그대로 바꾼다.
// ENTRYPOINT 가 있으면 명령이 그 뒤에 붙어 `/app/server /app/server migrate` 가 되고 쓰는 법 오류(2)로 배포가 멈춘다.
func TestImage_ENTRYPOINT가없어배포의1회성명령이그대로돈다(t *testing.T) {
	docker, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(docker), "\n") {
		// Dockerfile 지시어는 대소문자를 가리지 않는다(entrypoint 도 ENTRYPOINT 다).
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "ENTRYPOINT") {
			t.Errorf("Dockerfile 에 ENTRYPOINT 가 있다: %s", line)
		}
	}
}

// AWS 배포는 서비스를 바꾸기 전에 같은 태스크 정의로 `migrate` 1회성 태스크를 돌리고, 그 컨테이너의 종료 코드가 0 일 때만 서비스를 바꾼다.
func TestAWSDeploy_서비스를바꾸기전에같은이미지로migrate태스크를돌리고종료코드0이어야넘어간다(t *testing.T) {
	for _, name := range []string{"deploy-dev.yml", "deploy-prod.yml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "github-actions", name))
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		run := strings.Index(s, "aws ecs run-task")
		deploy := strings.Index(s, "aws ecs update-service")
		if run < 0 || deploy < 0 || run > deploy {
			t.Errorf("%s: run-task 가 서비스 배포(update-service)보다 앞에 없다", name)
		}
		for _, want := range []string{`"command":["/app/server","migrate"]`, "aws ecs wait tasks-stopped", `[ "$EXIT" != "0" ]`} {
			if !strings.Contains(s, want) {
				t.Errorf("%s 에 %s 가 없다", name, want)
			}
		}
	}
}

// 태스크 정의는 배포마다 한 번만 등록하고, migrate 1회성 태스크와 서비스가 같은 개정(ARN)을 쓴다. 전에는 run-task 앞에서 한 번,
// 배포 액션(amazon-ecs-deploy-task-definition — 늘 파일을 새로 등록한다)이 또 한 번 등록해 migrate 를 돈 개정과 서빙하는 개정이 갈렸다(감사 31).
// 서비스 배포는 그 개정의 rolloutState 가 COMPLETED 여야 성공이다 — 서킷 브레이커가 되돌린 뒤의 안정(services-stable)을 성공으로 보지 않는다.
func TestAWSDeploy_태스크정의는한번만등록하고migrate태스크와서비스가같은개정을쓴다(t *testing.T) {
	for _, name := range []string{"deploy-dev.yml", "deploy-prod.yml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "github-actions", name))
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		if n := strings.Count(s, "aws ecs register-task-definition"); n != 1 {
			t.Errorf("%s: register-task-definition %d 번, 한 번이어야 한다", name, n)
		}
		if strings.Contains(s, "amazon-ecs-deploy-task-definition@") {
			t.Errorf("%s: 배포 액션 amazon-ecs-deploy-task-definition 은 태스크 정의를 다시 등록한다", name)
		}
		for _, want := range []string{
			`echo "arn=$TASK_DEF_ARN" >> "$GITHUB_OUTPUT"`,
			`aws ecs run-task --cluster "$ECS_CLUSTER" --task-definition "$TASK_DEF_ARN"`,
			`aws ecs update-service --cluster "$ECS_CLUSTER" --service "$ECS_SERVICE" --task-definition "$TASK_DEF_ARN"`,
			"rolloutState", "COMPLETED)",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s 에 %s 가 없다", name, want)
			}
		}
		if n := strings.Count(s, "TASK_DEF_ARN: ${{ steps.taskdef.outputs.arn }}"); n != 2 {
			t.Errorf("%s: 등록한 개정을 받는 단계가 %d 개, migrate 와 배포 둘이어야 한다", name, n)
		}
		if regexp.MustCompile(`(?m)^\s*aws ecs wait services-stable`).MatchString(s) {
			t.Errorf("%s: services-stable 은 서킷 브레이커가 되돌린 뒤에도 성공한다", name)
		}
	}
}
