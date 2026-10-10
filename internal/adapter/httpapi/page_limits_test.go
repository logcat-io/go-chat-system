package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 목록마다 limit 을 어떻게 주든 돌아오는 개수를 리터럴로 고정한다(refactoring.md 1.6). core/page.go 의 상수를 쓰지 않는다 —
// 상수를 바꾸면 이 표가 깨져야 한다.
//
// 정책: limit 이 없거나, 숫자가 아니거나, 0 이하이거나, int 를 넘치면 그 목록의 기본값 개수다.
// 최대값을 넘으면 최대값 개수로 자른다. 기본값은 목록마다 하나다(core/page.go) — 전달 계층과 저장소가 같은 값을 쓴다.
func TestPageLimits_limit이없거나0이하면기본값이고최대값을넘으면최대값으로자른다(t *testing.T) {
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	admin := h.adminToken()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	// 한 고객(cust)의 대화 conv 에 공개 메시지 520개와 업무 기록 520개, 같은 고객의 닫힌 대화 210개.
	cust, conv := h.customerSession()
	exec(`INSERT INTO messages (id, conversation_id, seq, public_seq, client_msg_id, sender_kind, sender_customer, visibility, kind, body)
		SELECT gen_random_uuid(), c.id, c.last_seq + g, c.last_public_seq + g, 'bulk-' || g, 'CUSTOMER', c.customer_id, 'PUBLIC', 'MESSAGE', '한도검색어 ' || g
		  FROM conversations c, generate_series(1, 520) g WHERE c.id = $1`, conv)
	exec(`UPDATE conversations SET last_seq = last_seq + 520, last_public_seq = last_public_seq + 520 WHERE id = $1`, conv)
	exec(`INSERT INTO conversation_events (workspace_id, conversation_id, kind, detail)
		SELECT workspace_id, id, 'tagged', '{"tag":"bulk"}' FROM conversations, generate_series(1, 520) WHERE id = $1`, conv)
	exec(`INSERT INTO conversations (id, workspace_id, customer_id, subject, state)
		SELECT gen_random_uuid(), workspace_id, customer_id, '', 'CLOSED' FROM conversations, generate_series(1, 210) WHERE id = $1`, conv)
	exec(`INSERT INTO audit_log (workspace_id, action, target, detail)
		SELECT $1, 'bulk.test', 'x', '{}' FROM generate_series(1, 520)`, core.DefaultWorkspaceID)

	// 다른 고객의 지금 대화 now 와, 그보다 먼저 열린 대화 하나에 메시지 520개. 이전 상담은 now 에서 본다.
	_, now := h.customerSession()
	var past string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO conversations (id, workspace_id, customer_id, subject, state, last_seq, last_public_seq, created_at)
		SELECT gen_random_uuid(), workspace_id, customer_id, '', 'CLOSED', 520, 520, created_at - interval '1 day'
		  FROM conversations WHERE id = $1
		RETURNING id::text`, now).Scan(&past); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO messages (id, conversation_id, seq, public_seq, client_msg_id, sender_kind, sender_customer, visibility, kind, body)
		SELECT gen_random_uuid(), c.id, g, g, 'past-' || g, 'CUSTOMER', c.customer_id, 'PUBLIC', 'MESSAGE', '지난 ' || g
		  FROM conversations c, generate_series(1, 520) g WHERE c.id = $1`, past)

	type want struct {
		limit string // 비어 있으면 limit 을 싣지 않는다
		n     int
	}
	cases := []struct {
		name, path, key, token string
		wants                  []want
	}{
		// ListMessages: 기본 200, Max 500.
		{"메시지 after_seq", "/api/conversations/" + conv + "/messages?after_seq=0", "messages", cust, []want{
			{"", 200}, {"0", 200}, {"-1", 200}, {"abc", 200}, {"99999999999999999999", 200},
			{"1", 1}, {"200", 200}, {"500", 500}, {"501", 500}, {"1000000", 500}}},
		// ListMessagesBefore: 기본 50, Max 200.
		{"메시지 before_seq", "/api/conversations/" + conv + "/messages?before_seq=510", "messages", cust, []want{
			{"", 50}, {"0", 50}, {"-1", 50}, {"abc", 50}, {"99999999999999999999", 50},
			{"1", 1}, {"50", 50}, {"200", 200}, {"201", 200}, {"1000000", 200}}},
		// ListCustomerHistory: 기본 50, Max 200.
		{"이전 상담", "/api/agent/conversations/" + now + "/prior?x=1", "messages", admin, []want{
			{"", 50}, {"0", 50}, {"-1", 50}, {"abc", 50}, {"99999999999999999999", 50},
			{"1", 1}, {"50", 50}, {"200", 200}, {"201", 200}, {"1000000", 200}}},
		// ListConversations: 기본 50, Max 200. 대화는 213개다.
		{"받은함", "/api/agent/conversations?x=1", "conversations", admin, []want{
			{"", 50}, {"0", 50}, {"-1", 50}, {"abc", 50}, {"99999999999999999999", 50},
			{"1", 1}, {"50", 50}, {"200", 200}, {"201", 200}, {"1000000", 200}}},
		// CustomerConversations: 기본 30, Max 100. 이 고객의 대화는 211개다.
		{"고객의 대화 목록", "/api/customer/conversations?x=1", "conversations", cust, []want{
			{"", 30}, {"0", 30}, {"-1", 30}, {"abc", 30}, {"99999999999999999999", 30},
			{"1", 1}, {"30", 30}, {"100", 100}, {"101", 100}, {"1000000", 100}}},
		// SearchMessages: 기본 30, Max 100.
		{"검색", "/api/agent/search?q=%ED%95%9C%EB%8F%84%EA%B2%80%EC%83%89%EC%96%B4", "hits", admin, []want{
			{"", 30}, {"0", 30}, {"-1", 30}, {"abc", 30}, {"99999999999999999999", 30},
			{"1", 1}, {"30", 30}, {"100", 100}, {"101", 100}, {"1000000", 100}}},
		// AuditLog: 기본 100, Max 500.
		{"감사 로그", "/api/admin/audit?x=1", "entries", admin, []want{
			{"", 100}, {"0", 100}, {"-1", 100}, {"abc", 100}, {"99999999999999999999", 100},
			{"1", 1}, {"100", 100}, {"500", 500}, {"501", 500}, {"1000000", 500}}},
		// ListEvents: 기본 100, Max 500.
		{"업무 기록", "/api/agent/work?conversation=" + conv, "events", admin, []want{
			{"", 100}, {"0", 100}, {"-1", 100}, {"abc", 100}, {"99999999999999999999", 100},
			{"1", 1}, {"100", 100}, {"500", 500}, {"501", 500}, {"1000000", 500}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range tc.wants {
				path := tc.path
				if w.limit != "" {
					path += "&limit=" + w.limit
				}
				code, body := h.call(http.MethodGet, path, tc.token, nil)
				if code != http.StatusOK {
					t.Errorf("limit=%q: %d %v", w.limit, code, body)
					continue
				}
				if got := len(firstList(body, tc.key)); got != w.n {
					t.Errorf("limit=%q: %d개, %d개여야 한다", w.limit, got, w.n)
				}
			}
		})
	}
}
