// Package testsupport 는 테스트가 실제 Postgres 에 붙게 돕는다.
// 유실 방지 불변식은 락·롤백·unique 제약에 의존하므로 가짜 저장소로는 검증할 수 없다.
package testsupport

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

const defaultURL = "postgres://chat:chat@127.0.0.1:56432/chat?sslmode=disable"

var (
	once    sync.Once
	shared  *pgstore.Store
	openErr error // 접속 자체가 안 됨 → Skip
	prepErr error // 접속은 됐는데 마이그레이션·설정이 실패 → Fatal. 스키마가 깨졌는데 통과처럼 보이면 안 된다
)

func URL() string {
	if v := os.Getenv("CHAT_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultURL
}

// Store 는 테스트 프로세스가 공유하는 저장소다. DB 가 없으면 테스트를 건너뛴다.
func Store(t *testing.T) *pgstore.Store {
	t.Helper()
	once.Do(func() {
		ctx := context.Background()
		shared, openErr = pgstore.Open(ctx, URL())
		if openErr != nil {
			return
		}
		if prepErr = shared.Migrate(ctx); prepErr != nil {
			return
		}
		// 접수·부재중 자동 응답은 기본으로 끈다. 켜면 메시지 수를 세는 모든 테스트가 흔들린다.
		// 자동 응답을 보는 테스트는 직접 켜고 끝나면 되돌린다.
		prepErr = shared.PutSetting(ctx, core.DefaultWorkspaceID, "auto_messages", map[string]string{
			"welcome":     "안녕하세요. 무엇을 도와드릴까요? 문의를 남겨 주시면 상담원이 확인하고 답해 드립니다.",
			"first_reply": "", "away": "",
		}, "")
	})
	if openErr != nil {
		t.Skipf("Postgres 에 붙을 수 없어 건너뛴다 (make up 으로 띄운다): %v", openErr)
	}
	if prepErr != nil {
		t.Fatalf("테스트 DB 준비 실패 — 스키마가 깨졌다: %v", prepErr)
	}
	return shared
}

// StoreWithPool 은 공유 DB 에 풀 크기를 정한 저장소를 따로 연다. 동시 요청이 풀 크기를 넘을 때의 동작을 볼 때 쓴다.
// 스키마와 기본 설정은 Store 가 준비한 것을 그대로 쓴다. 테스트가 끝나면 닫는다.
func StoreWithPool(t *testing.T, maxConns int32) *pgstore.Store {
	t.Helper()
	Store(t)
	s, err := pgstore.OpenWithPool(context.Background(), URL(), maxConns)
	if err != nil {
		t.Fatalf("풀 크기 %d 로 열지 못했다: %v", maxConns, err)
	}
	t.Cleanup(s.Close)
	return s
}

// Isolated 는 이 테스트만 쓰는 DB 를 만들고 끝나면 지운다.
// 관리자 수·설정처럼 전역 상태를 단언하는 테스트는 공유 DB 에서 다른 테스트의 행에 흔들린다.
// 돌아오는 URL 은 Exec 로 직접 SQL 을 칠 때 쓴다.
func Isolated(t *testing.T) (*pgstore.Store, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, URL())
	if err != nil {
		t.Skipf("Postgres 에 붙을 수 없어 건너뛴다 (make up 으로 띄운다): %v", err)
	}
	// id 앞 48비트는 시각이라 같은 밀리초의 두 호출이 겹친다(D31). 랜덤인 끝 12자리를 쓴다.
	name := "chat_t_" + strings.ReplaceAll(id.New(), "-", "")[20:]
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close(ctx)
		t.Skipf("격리 DB 를 만들 수 없어 건너뛴다 (CREATEDB 권한 필요): %v", err)
	}
	// 만들자마자 정리를 건다. 아래가 실패해도 DB 가 남지 않는다. 남은 건 `make clean-test-dbs`.
	var store *pgstore.Store
	t.Cleanup(func() {
		if store != nil {
			store.Close()
		}
		if _, err := admin.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("격리 DB 정리 실패 %s: %v", name, err)
		}
		admin.Close(ctx)
	})
	u, err := url.Parse(URL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	store, err = pgstore.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("격리 DB 접속 실패: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("격리 DB 마이그레이션 실패: %v", err)
	}
	if err := store.PutSetting(ctx, core.DefaultWorkspaceID, "auto_messages", map[string]string{
		"welcome": "안녕하세요. 무엇을 도와드릴까요?", "first_reply": "", "away": "",
	}, ""); err != nil {
		t.Fatal(err)
	}
	return store, u.String()
}

// Exec 는 테스트가 만들기 어려운 상태(관리자 0명 등)를 SQL 로 직접 만든다.
func Exec(t *testing.T, dbURL, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// Conversation 은 새 고객과 그 고객의 열린 대화를 만든다. 테스트끼리 행을 공유하지 않는다.
func Conversation(t *testing.T, store *pgstore.Store) (customerID string, conv core.Conversation) {
	t.Helper()
	ctx := context.Background()
	customerID, err := store.CreateCustomer(ctx, core.DefaultWorkspaceID, "테스트고객")
	if err != nil {
		t.Fatalf("고객 생성 실패: %v", err)
	}
	conv, err = store.CreateConversation(ctx, core.DefaultWorkspaceID, customerID, "")
	if err != nil {
		t.Fatalf("대화 생성 실패: %v", err)
	}
	return customerID, conv
}

// Agent 는 매번 다른 이메일로 상담원을 만든다.
func Agent(t *testing.T, store *pgstore.Store) core.Agent {
	t.Helper()
	email := "agent-" + id.New() + "@test.local"
	a, err := store.CreateAgent(context.Background(), core.DefaultWorkspaceID, email, "테스트상담원", "chat1234")
	if err != nil {
		t.Fatalf("상담원 생성 실패: %v", err)
	}
	return a
}
