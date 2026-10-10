package pgstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// DEF-73(감사 13 변이 N8·N9): 만료 조건(expires_at > now())이나 비활성 조건을 지워도 전체 테스트가 통과했다.
// 세션 행은 지우지 않고 만료 시각·active 만 바꾼다 — 로그아웃(행 삭제)과 다른 길이다.
func TestSubjectByToken_만료된세션과비활성상담원의세션으로는주체를찾지못한다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	custID, _ := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	other := testsupport.Agent(t, store)

	custToken, err := store.CreateSession(ctx, "CUSTOMER", "", custID, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	agentToken, err := store.CreateSession(ctx, "AGENT", agent.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := store.CreateSession(ctx, "AGENT", other.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{custToken, agentToken, otherToken} {
		if _, err := store.SubjectByToken(ctx, tok); err != nil {
			t.Fatalf("살아 있는 세션인데 %v", err)
		}
	}

	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE customer_id = $1", custID)
	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET expires_at = now() - interval '1 second' WHERE agent_id = $1", agent.ID)
	testsupport.Exec(t, testsupport.URL(), "UPDATE agents SET active = false WHERE id = $1", other.ID)
	// 공유 DB 다. 활성 상담원 수를 세는 다른 테스트가 흔들리지 않게 되돌린다
	t.Cleanup(func() {
		testsupport.Exec(t, testsupport.URL(), "UPDATE agents SET active = true WHERE id = $1", other.ID)
	})

	for name, tok := range map[string]string{"만료된 고객 세션": custToken, "만료된 상담원 세션": agentToken, "비활성 상담원의 세션": otherToken} {
		if _, err := store.SubjectByToken(ctx, tok); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s 를 %v 로 답했다. 세션 없음이어야 한다", name, err)
		}
	}
}

// DEF-71: 장애 조치 뒤 조용히 죽은 연결을 풀에서 꺼낼 때의 Ping 이 시한 없이 기다렸다. 꺼낸 연결 확인은 1초, 새 연결 수립은 5초다.
// 접속 문자열이 connect_timeout 을 정하면 그것을 따른다. 숫자는 정책이라 리터럴로 고정한다.
func TestOpen_풀은꺼낸연결을1초시한으로확인하고새연결수립에5초시한을둔다_접속문자열의connect_timeout은따른다(t *testing.T) {
	testsupport.Store(t)
	ctx := context.Background()
	store, err := pgstore.OpenWithPool(ctx, testsupport.URL(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := store.Pool().Config()
	if cfg.PingTimeout != time.Second {
		t.Errorf("꺼낸 연결 확인 시한이 %v 다. 1초여야 한다", cfg.PingTimeout)
	}
	if cfg.ConnConfig.ConnectTimeout != 5*time.Second {
		t.Errorf("새 연결 수립 시한이 %v 다. 5초여야 한다", cfg.ConnConfig.ConnectTimeout)
	}

	given, err := pgstore.OpenWithPool(ctx, testsupport.URL()+"&connect_timeout=3", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer given.Close()
	if got := given.Pool().Config().ConnConfig.ConnectTimeout; got != 3*time.Second {
		t.Errorf("접속 문자열의 connect_timeout=3 을 따르지 않았다: %v", got)
	}
}

// 감사 20 A08: SQL 의 1분 묶기(last_used_at < now() - 1분)를 빼도 통과했다. 앱은 읽은 사용 시각으로 한 번 거르지만(NoteSessionUse),
// 같은 세션으로 동시에 온 요청들은 모두 같은 옛 사용 시각을 읽고 모두 TouchSession 을 부른다. 그때 행을 쓰는 것은 첫 요청 하나여야 한다 —
// 아니면 데스크 하나의 동시 요청 수만큼 세션 행에 쓰기(행 잠금·WAL)가 몰린다. 행 판(xmin)이 바뀌지 않았는지로 본다.
func TestTouchSession_동시요청이같은세션행을거듭쓰지않는다_1분안의두번째쓰기는행을바꾸지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	agent := testsupport.Agent(t, store)
	token, err := store.CreateSession(ctx, "AGENT", agent.ID, "", time.Hour, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.SubjectByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	row := func() (xmin string, usedAt time.Time) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx, "SELECT xmin::text, last_used_at FROM sessions WHERE agent_id = $1", agent.ID).Scan(&xmin, &usedAt); err != nil {
			t.Fatal(err)
		}
		return
	}
	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET last_used_at = now() - interval '5 minutes' WHERE agent_id = $1", agent.ID)
	_, stale := row()

	// 다섯 요청이 같은 옛 사용 시각을 읽고 한꺼번에 적으러 왔다
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := store.TouchSession(ctx, sub.Session); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	xmin, used := row()
	if !used.After(stale) {
		t.Fatalf("사용 시각이 오르지 않았다: %s", used)
	}
	// 1분 안에 또 왔다(앱이 읽은 값은 아직 옛 값이다)
	if err := store.TouchSession(ctx, sub.Session); err != nil {
		t.Fatal(err)
	}
	if again, usedAgain := row(); again != xmin || !usedAgain.Equal(used) {
		t.Errorf("1분 안의 두 번째 쓰기가 행을 바꿨다: xmin %s → %s, 사용 시각 %s → %s", xmin, again, used, usedAgain)
	}
}

// 감사 20 A16: 만료 정리의 유휴 조건을 빼도 pgstore·app·server 테스트가 통과했다(httpapi 의 정책 697 테스트만 잡는다).
// 고객 세션에는 이제 기본으로 유휴 한도가 없으니 상담원 세션으로 본다. 절대 수명은 남았고 유휴만 지난 세션을 정리가 지운다.
func TestDeleteExpiredSessions_유휴한도가지난상담원세션은절대수명이남아도지우고유휴한도가없는세션은남긴다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	idle := testsupport.Agent(t, store)
	busy := testsupport.Agent(t, store)
	custID, _ := testsupport.Conversation(t, store)
	_, err := store.CreateSession(ctx, "AGENT", idle.ID, "", 720*time.Hour, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateSession(ctx, "AGENT", busy.ID, "", 720*time.Hour, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateSession(ctx, "CUSTOMER", "", custID, 0, 0) // 익명 고객: 수명·유휴 한도 없음
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET last_used_at = now() - interval '12 hours 1 minute' WHERE agent_id = $1", idle.ID)
	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET last_used_at = now() - interval '11 hours' WHERE agent_id = $1", busy.ID)
	testsupport.Exec(t, testsupport.URL(), "UPDATE sessions SET last_used_at = now() - interval '400 days', created_at = now() - interval '400 days' WHERE customer_id = $1", custID)
	if _, err := store.DeleteExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
	exists := func(agentID, customerID string) bool {
		var n int
		if err := store.Pool().QueryRow(ctx, "SELECT count(*) FROM sessions WHERE agent_id::text = $1 OR customer_id::text = $2", agentID, customerID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if exists(idle.ID, "") {
		t.Error("유휴 12시간이 지난 상담원 세션을 정리가 지우지 않았다")
	}
	if !exists(busy.ID, "") {
		t.Error("유휴 한도 안의 상담원 세션을 지웠다")
	}
	if !exists("", custID) {
		t.Error("유휴 한도가 없는 익명 고객 세션을 지웠다")
	}
}
