package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 0019 는 익명 고객 세션의 수명을 지우고 고객 세션의 행 유휴 한도를 비운다. 업그레이드 때 이미 끝난 고객 세션은 되살리지 않고 지운다(감사 21).
// 격리 DB 를 0018 까지의 모양으로 되돌린 뒤 0018 때의 행(고객 유휴 7일·상담원 12시간)을 넣고 0019 를 다시 돌린다. 운영 DB 가 겪을 길과 같다.
func TestMigration0019_살아있는익명세션만수명이지워지고끝난고객세션은되살리지않으며회원수명과상담원유휴한도는그대로다(t *testing.T) {
	store, dbURL := testsupport.Isolated(t)
	ctx := context.Background()

	testsupport.Exec(t, dbURL, `
		DELETE FROM sessions WHERE expires_at IS NULL;
		ALTER TABLE sessions ALTER COLUMN expires_at SET NOT NULL;
		DELETE FROM schema_migrations WHERE name = '0019_customer_sessions_forever.sql'`)

	ws := core.DefaultWorkspaceID
	anon := "00000000-0000-0000-0000-0000000019a1"
	member := "00000000-0000-0000-0000-0000000019b1"
	testsupport.Exec(t, dbURL, `
		INSERT INTO customers (id, workspace_id, display_name, external_id) VALUES
		  ($1, $3, '익명 고객', NULL),
		  ($2, $3, '회원 고객', 'm-0019')`, anon, member, ws)
	agent := testsupport.Agent(t, store)

	// 0018 때의 행: 고객 유휴 7일, 상담원 12시간. 이름은 token_hash 의 한 바이트다.
	testsupport.Exec(t, dbURL, `
		INSERT INTO sessions (token_hash, subject_kind, agent_id, customer_id, workspace_id, expires_at, last_used_at, idle_for) VALUES
		  ('\x01', 'CUSTOMER', NULL, $1, $4, now() + interval '20 days', now() - interval '1 day',   interval '7 days'),
		  ('\x02', 'CUSTOMER', NULL, $1, $4, now() - interval '1 hour',  now() - interval '2 hours', interval '7 days'),
		  ('\x03', 'CUSTOMER', NULL, $1, $4, now() + interval '20 days', now() - interval '8 days',  interval '7 days'),
		  ('\x04', 'CUSTOMER', NULL, $2, $4, now() + interval '1 hour',  now() - interval '1 day',   interval '7 days'),
		  ('\x05', 'AGENT',    $3, NULL, $4, now() + interval '20 days', now() - interval '1 hour',  interval '12 hours')`,
		anon, member, agent.ID, ws)

	// 회원 세션의 수명은 "그대로" 인지를 본다. 앱 시계(time.Until)와 DB 시계(now())를 섞어 1시간 상한으로 재면 두 시계의 차이만큼
	// 흔들린다(감사 23 — 앱 시계가 DB 보다 늦으면 1시간을 넘는다). 마이그레이션 전과 뒤의 DB 값을 그대로 비교한다.
	var memberExpires time.Time
	if err := store.Pool().QueryRow(ctx, "SELECT expires_at FROM sessions WHERE token_hash = $1", []byte{0x04}).Scan(&memberExpires); err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("0019 가 0018 상태 위에서 돌아야 한다: %v", err)
	}

	type row struct {
		ok      bool
		forever bool
		expires time.Time
		idle    *time.Duration
	}
	get := func(hash byte) row {
		t.Helper()
		var r row
		var expires *time.Time
		err := store.Pool().QueryRow(ctx, "SELECT expires_at, idle_for FROM sessions WHERE token_hash = $1", []byte{hash}).Scan(&expires, &r.idle)
		if err != nil {
			return r
		}
		r.ok = true
		r.forever = expires == nil
		if expires != nil {
			r.expires = *expires
		}
		return r
	}

	if r := get(0x01); !r.ok || !r.forever || r.idle != nil {
		t.Errorf("살아 있는 익명 세션은 수명도 유휴 한도도 없어야 한다: %+v", r)
	}
	if r := get(0x02); r.ok {
		t.Errorf("업그레이드 때 절대 수명이 이미 지난 익명 세션을 되살렸다: %+v", r)
	}
	if r := get(0x03); r.ok {
		t.Errorf("업그레이드 때 유휴 7일이 이미 지난 익명 세션을 되살렸다: %+v", r)
	}
	if r := get(0x04); !r.ok || r.forever || !r.expires.Equal(memberExpires) || r.idle != nil {
		t.Errorf("회원 세션은 회원 토큰 exp 에 묶인 수명(1시간)을 그대로 두고 행 유휴 한도만 비운다: %+v", r)
	}
	if r := get(0x05); !r.ok || r.forever || r.idle == nil || *r.idle != 12*time.Hour {
		t.Errorf("상담원 세션의 수명과 유휴 한도(12시간)는 그대로다: %+v", r)
	}
}
