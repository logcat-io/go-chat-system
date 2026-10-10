package pgstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

func (s *Store) CreateCustomer(ctx context.Context, wsID, displayName string) (string, error) {
	cid := id.New()
	_, err := s.pool.Exec(ctx,
		"INSERT INTO customers (id, workspace_id, display_name, last_seen_at) VALUES ($1, $2, $3, now())",
		cid, wsID, displayName)
	if err != nil {
		return "", err
	}
	return cid, nil
}

// CreateConversation 은 새 상담을 연다. 고객은 대화를 여러 개 가질 수 있다.
// 고객이 그 워크스페이스 소속이 아니면 만들지 않는다 — 소속은 고객 행에서 따라온다.
func (s *Store) CreateConversation(ctx context.Context, wsID, customerID, subject string) (core.Conversation, error) {
	convID := id.New()
	// 변경 시각은 문장 시각이다. 안전망은 이 시각으로 새 대화를 찾는다(D4) — 기본값 now() 는 트랜잭션 시작이라 커밋보다 이를 수 있다.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (id, workspace_id, customer_id, subject, updated_at)
		SELECT $1, c.workspace_id, c.id, $3, clock_timestamp() FROM customers c WHERE c.id = $2 AND c.workspace_id = $4`,
		convID, customerID, subject, wsID)
	if err != nil {
		return core.Conversation{}, err
	}
	if tag.RowsAffected() == 0 {
		return core.Conversation{}, core.ErrNotFound
	}
	return s.Conversation(ctx, convID)
}

// LatestOpenConversation 은 고객의 가장 최근 열린 대화를 준다. 없으면 ErrNotFound 다.
func (s *Store) LatestOpenConversation(ctx context.Context, customerID string) (core.Conversation, error) {
	var convID string
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM conversations
		 WHERE customer_id = $1 AND state <> 'CLOSED'
		   -- 가린 상담은 열려 있어도 없는 것이다. 회원이 다시 문의하면 새 대화가 열린다.
		   -- 지난 상담도 뺀다. 되살린 옛 상담이 회원의 "지금 상담" 자리를 되찾으면 안 된다.
		   AND deleted_at IS NULL AND NOT past_for_customer
		 ORDER BY last_message_at DESC NULLS LAST, created_at DESC
		 LIMIT 1`, customerID).Scan(&convID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Conversation{}, core.ErrNotFound
	}
	if err != nil {
		return core.Conversation{}, err
	}
	return s.Conversation(ctx, convID)
}

// CustomerConversationCount 는 이 고객이 연 상담 중 삭제되지 않은 것의 수다. 새 상담이 몇 번째인지 상담원에게 알릴 때 쓴다.
// 삭제한 상담은 이어 보기에서 빠지므로 세지 않는다. 세면 "이전 상담이 위에 이어집니다" 가 빈 자리를 가리킨다.
func (s *Store) CustomerConversationCount(ctx context.Context, customerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM conversations WHERE customer_id = $1 AND deleted_at IS NULL", customerID).Scan(&n)
	return n, err
}

const conversationColumns = `
		 c.id, c.customer_id, cu.display_name, c.state, c.subject,
		 c.last_seq, c.last_public_seq, c.last_message_at, c.snoozed_until, c.created_at, c.updated_at,
		 COALESCE(c.assignee_agent_id::text, ''), COALESCE(ag.name, ''), c.workspace_id, c.past_for_customer, c.deleted_at,
		 COALESCE(c.closed_by, '')`

func scanConversation(row pgx.Row) (core.Conversation, error) {
	var c core.Conversation
	err := row.Scan(conversationDest(&c)...)
	settleClosedBy(&c)
	return c, err
}

// conversationDest 는 conversationColumns 한 줄의 목적지다. 목록 쿼리는 뒤에 제 컬럼을 덧붙인다.
func conversationDest(c *core.Conversation, extra ...any) []any {
	return append([]any{&c.ID, &c.CustomerID, &c.CustomerName, &c.State, &c.Subject,
		&c.LastSeq, &c.LastPublicSeq, &c.LastMessageAt, &c.SnoozedUntil, &c.CreatedAt, &c.UpdatedAt,
		&c.AssigneeAgentID, &c.AssigneeName, &c.WorkspaceID, &c.PastForCustomer, &c.DeletedAt, &c.ClosedBy}, extra...)
}

// settleClosedBy 는 "누가 닫았나" 를 지금 종료일 때만 남긴다(D48). DB 의 값은 마지막 종료의 것이라 다시 연 상담에도 남아 있다.
func settleClosedBy(c *core.Conversation) {
	if c.State != core.StateClosed {
		c.ClosedBy = ""
	}
}

// ConversationMeta 는 전달 루프가 매 틱마다 부르는 조회다.
// 태그를 같이 읽지 않는다. 3000 연결이 5초마다 부르면 그 한 번이 초당 600 질의가 된다.
func (s *Store) ConversationMeta(ctx context.Context, convID string) (core.Conversation, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT`+conversationColumns+`
		  FROM conversations c
		  JOIN customers cu ON cu.id = c.customer_id
		  LEFT JOIN agents ag ON ag.id = c.assignee_agent_id
		 WHERE c.id = $1`, convID)
	c, err := scanConversation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Conversation{}, core.ErrNotFound
	}
	c.Tags = []core.Tag{}
	return c, err
}

func (s *Store) Conversation(ctx context.Context, convID string) (core.Conversation, error) {
	return conversationOn(ctx, s.pool, convID, false)
}

// querier 는 풀과 트랜잭션이 같이 가진 메서드다. 같은 조회를 트랜잭션 안과 밖에서 쓴다.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// conversationOn 은 대화 하나를 태그까지 읽는다. lock 이면 대화 행을 잠근다(트랜잭션 안에서만 뜻이 있다).
// 삭제된 대화도 준다. 거르는 것은 부르는 쪽이다.
func conversationOn(ctx context.Context, q querier, convID string, lock bool) (core.Conversation, error) {
	sql := `
		SELECT` + conversationColumns + `
		  FROM conversations c
		  JOIN customers cu ON cu.id = c.customer_id
		  LEFT JOIN agents ag ON ag.id = c.assignee_agent_id
		 WHERE c.id = $1`
	if lock {
		sql += " FOR UPDATE OF c"
	}
	c, err := scanConversation(q.QueryRow(ctx, sql, convID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Conversation{}, core.ErrNotFound
	}
	if err != nil {
		return core.Conversation{}, err
	}
	c.Tags, err = conversationTags(ctx, q, convID)
	if err != nil {
		return core.Conversation{}, err
	}
	return c, nil
}

// activeCondition 은 "아직 끝나지 않은 상담" 의 정의다. 종료(CLOSED)만 뺀다 — 보류도 아직 맡은 일이다.
// 배지의 내 담당(mine)·미할당(unassigned)과, 상태 필터 없이 부른 assignee 목록(assignee=me·none·id)이 이 한 문장을 쓴다(D39).
// 별칭 c 는 conversations 다.
const activeCondition = `(c.state <> 'CLOSED')`

// waitingCondition 은 "답할 차례" 의 정의다. 종료하지 않았고, 고객이 마지막으로 말한 뒤 상담원의 공개 답변이 없다.
// 받은함 목록(waiting=1)과 배지(todo)가 이 한 문장을 쓴다 — 둘이 다른 조건을 쓰면 목록과 숫자가 어긋난다(D39).
// 별칭 c 는 conversations 다.
const waitingCondition = `(c.state <> 'CLOSED' AND c.last_customer_seq > c.last_agent_public_seq)`

// CountInbox 는 필터별 건수를 한 번의 조회로 센다.
func (s *Store) CountInbox(ctx context.Context, wsID, agentID string) (core.InboxCounts, error) {
	var c core.InboxCounts
	err := s.pool.QueryRow(ctx, `
		WITH me AS (SELECT nullif($1::text, '')::uuid AS agent_id),
		     v AS (
		       SELECT c.id, c.state, c.assignee_agent_id, `+waitingCondition+` AS waiting, `+activeCondition+` AS active,
		              COALESCE(r.last_read_seq, 0) AS read_seq,
		              EXISTS (SELECT 1 FROM conversation_followers f, me
		                       WHERE f.conversation_id = c.id AND f.agent_id = me.agent_id) AS following
		         FROM conversations c
		         LEFT JOIN conversation_reads r
		                ON r.conversation_id = c.id
		               AND r.reader_agent_id = (SELECT agent_id FROM me)
		        WHERE c.workspace_id = $2 AND c.deleted_at IS NULL
		     )
		SELECT
		  count(*) FILTER (WHERE waiting
		                     AND assignee_agent_id = (SELECT agent_id FROM me))     AS todo,
		  count(*) FILTER (WHERE active
		                     AND assignee_agent_id = (SELECT agent_id FROM me))     AS mine,
		  count(*) FILTER (WHERE active AND assignee_agent_id IS NULL)              AS unassigned,
		  count(*) FILTER (WHERE following)                                         AS following,
		  count(*)                                                                  AS all_count,
		  count(*) FILTER (WHERE state = 'OPEN')                                    AS open_count,
		  count(*) FILTER (WHERE state = 'SNOOZED')                                 AS snoozed_count,
		  count(*) FILTER (WHERE state = 'CLOSED')                                  AS closed_count
		FROM v`, agentID, wsID,
	).Scan(&c.Todo, &c.Mine, &c.Unassigned, &c.Following, &c.All, &c.Open, &c.Snoozed, &c.Closed)
	return c, err
}

// ListConversations 는 받은함이다. 파생 뷰라서 알림을 놓쳐도 다음 틱에 맞춰진다.
// 담당자로 거른 목록(AssigneeID·Unassigned)에 상태 필터가 없으면 배지와 같게 끝난 상담을 뺀다(activeCondition).
// 상태를 주면(state=CLOSED 등) 그 상태를 따른다 — 끝난 상담을 보려면 상태를 명시한다.
func (s *Store) ListConversations(ctx context.Context, f core.InboxFilter) ([]core.Conversation, error) {
	f.Limit = pageLimit(f.Limit, core.InboxPageDefault, core.InboxPageMax)
	onlyActive := f.State == "" && (f.AssigneeID != "" || f.Unassigned)
	query := `
		SELECT` + conversationColumns + `,
		       COALESCE(last.body, '')        AS last_body,
		       COALESCE(last.sender_kind, '') AS last_sender_kind,
		       (SELECT count(*) FROM messages mu
		         WHERE mu.conversation_id = c.id
		           AND mu.sender_kind = 'CUSTOMER' AND mu.kind = 'MESSAGE' AND mu.deleted_at IS NULL
		           AND mu.seq > COALESCE(r.last_read_seq, 0)) AS unread
		  FROM conversations c
		  JOIN customers cu ON cu.id = c.customer_id
		  LEFT JOIN agents ag ON ag.id = c.assignee_agent_id
		  LEFT JOIN LATERAL (
		       SELECT body, sender_kind FROM messages m
		        WHERE m.conversation_id = c.id AND m.kind = 'MESSAGE'
		        ORDER BY m.seq DESC LIMIT 1
		  ) last ON true
		  LEFT JOIN conversation_reads r
		         ON r.conversation_id = c.id
		        AND r.reader_agent_id = nullif($1::text, '')::uuid
		 WHERE ($2::text = '' OR c.state = $2::text)
		   AND ($3::text = '' OR c.assignee_agent_id = $3::text::uuid)
		   AND (NOT $4::bool OR c.assignee_agent_id IS NULL)
		   AND ($5::text = '' OR EXISTS (
		           SELECT 1 FROM conversation_followers cf
		            WHERE cf.conversation_id = c.id AND cf.agent_id = $5::text::uuid))
		   AND ($6::text = '' OR EXISTS (
		           SELECT 1 FROM conversation_tags ct
		            WHERE ct.conversation_id = c.id AND ct.tag_id = $6::text::uuid))
		   AND ($7::text = '' OR cu.display_name ILIKE '%' || $7::text || '%'
		        OR (NOT $11::bool AND EXISTS (SELECT 1 FROM messages m2
		                    WHERE m2.conversation_id = c.id AND m2.kind = 'MESSAGE' AND m2.deleted_at IS NULL
		                      AND m2.body ILIKE '%' || $7::text || '%')))
		   AND (NOT $9::bool OR ` + waitingCondition + `)
		   AND (NOT $13::bool OR ` + activeCondition + `)
		   AND c.workspace_id = $10
		   AND (CASE WHEN $12::bool THEN c.deleted_at IS NOT NULL ELSE c.deleted_at IS NULL END)
		 ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC
		 LIMIT $8`

	rows, err := s.pool.Query(ctx, query,
		f.ReaderID, f.State, f.AssigneeID, f.Unassigned, f.FollowedBy, f.TagID,
		strings.TrimSpace(f.Query), f.Limit, f.OnlyWaiting, f.WorkspaceID, f.NameOnly, f.Deleted, onlyActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.Conversation, 0, 16)
	ids := make([]string, 0, 16)
	for rows.Next() {
		var c core.Conversation
		if err := rows.Scan(conversationDest(&c, &c.LastBody, &c.LastSenderKind, &c.Unread)...); err != nil {
			return nil, err
		}
		settleClosedBy(&c)
		c.Tags = []core.Tag{}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.attachTags(ctx, out, ids)
}

// CustomerConversations 는 고객 위젯의 대화 목록이다. 안 읽은 수는 고객 번호로 센다.
func (s *Store) CustomerConversations(ctx context.Context, customerID string, limit int) ([]core.Conversation, error) {
	limit = pageLimit(limit, core.CustomerConversationsPageDefault, core.CustomerConversationsPageMax)
	rows, err := s.pool.Query(ctx, `
		SELECT`+conversationColumns+`,
		       COALESCE(last.body, '')        AS last_body,
		       COALESCE(last.sender_kind, '') AS last_sender_kind,
		       (SELECT count(*) FROM messages mu
		         WHERE mu.conversation_id = c.id
		           AND mu.visibility = 'PUBLIC' AND mu.kind = 'MESSAGE'
		           AND mu.sender_kind <> 'CUSTOMER'
		           AND mu.seq > COALESCE(r.last_read_seq, 0)) AS unread
		  FROM conversations c
		  JOIN customers cu ON cu.id = c.customer_id
		  LEFT JOIN agents ag ON ag.id = c.assignee_agent_id
		  -- 카드 제목은 고객·상담원·AI 가 쓴 공개 글이다. 환영·접수 안내 같은 자동 글을 고르면 새 상담 카드가 전부 같은 제목이 된다.
		  -- AI 응대(D45)는 고객에게 한 답이라 고른다 — 빼면 AI 가 답한 상담의 카드가 고객 자신의 질문에 머문다(감사 20, logcat 결정).
		  -- 지운 글은 본문이 비어 "파일을 보냈습니다" 로 읽히므로 건너뛰고 그 앞 글을 고른다.
		  LEFT JOIN LATERAL (
		       SELECT body, sender_kind FROM messages m
		        WHERE m.conversation_id = c.id AND m.visibility = 'PUBLIC' AND m.kind = 'MESSAGE'
		          AND m.sender_kind IN ('CUSTOMER', 'AGENT', 'AI')
		          AND m.deleted_at IS NULL
		        ORDER BY m.public_seq DESC LIMIT 1
		  ) last ON true
		  LEFT JOIN conversation_reads r
		         ON r.conversation_id = c.id AND r.reader_customer = c.customer_id
		 WHERE c.customer_id = $1
		   -- 회원에게 지난 상담은 없다. 다시 문의하면 새 방이고, 앞선 상담은 상담원만 본다.
		   AND NOT c.past_for_customer
		   AND c.deleted_at IS NULL
		 ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC
		 LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.Conversation, 0, 8)
	for rows.Next() {
		var c core.Conversation
		if err := rows.Scan(conversationDest(&c, &c.LastBody, &c.LastSenderKind, &c.Unread)...); err != nil {
			return nil, err
		}
		settleClosedBy(&c)
		c.Tags = []core.Tag{}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) attachTags(ctx context.Context, convs []core.Conversation, ids []string) ([]core.Conversation, error) {
	if len(ids) == 0 {
		return convs, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ct.conversation_id, t.id, t.name
		  FROM conversation_tags ct JOIN tags t ON t.id = ct.tag_id
		 WHERE ct.conversation_id = ANY($1::uuid[])
		 ORDER BY t.name`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byConv := make(map[string][]core.Tag, len(ids))
	for rows.Next() {
		var convID string
		var t core.Tag
		if err := rows.Scan(&convID, &t.ID, &t.Name); err != nil {
			return nil, err
		}
		byConv[convID] = append(byConv[convID], t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range convs {
		if tags, ok := byConv[convs[i].ID]; ok {
			convs[i].Tags = tags
		}
	}
	return convs, nil
}

// changedPageMax·changedPageDefault 는 ChangedSince 의 페이지 한도다. limit 이 0 이하면 Default 개, Max 를 넘으면 Max 개를 준다.
// 이 메서드의 포트는 app 이 아니라 realtime 의 ChangeSource 라서 core 가 아니라 여기에 둔다.
const (
	changedPageMax     = 5000
	changedPageDefault = 1000
)

func (s *Store) ChangedSince(ctx context.Context, cur core.ChangeCursor, limit int) (ids []core.ChangedConv, next core.ChangeCursor, more bool, err error) {
	limit = pageLimit(limit, changedPageDefault, changedPageMax)
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, last_seq, last_public_seq, updated_at,
		       GREATEST(COALESCE(last_message_at, updated_at), updated_at) AS changed_at
		  FROM conversations
		 WHERE (GREATEST(COALESCE(last_message_at, updated_at), updated_at), id) > ($1, $2::text::uuid)
		 ORDER BY changed_at, id
		 LIMIT $3`, cur.At, nullUUID(cur.ID), limit)
	if err != nil {
		return nil, cur, false, err
	}
	defer rows.Close()

	next = cur
	for rows.Next() {
		var c core.ChangedConv
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Seq, &c.PublicSeq, &c.UpdatedAt, &c.At); err != nil {
			return nil, cur, false, err
		}
		ids = append(ids, c)
		next = core.ChangeCursor{At: c.At, ID: c.ID}
	}
	if err := rows.Err(); err != nil {
		return nil, cur, false, err
	}
	return ids, next, len(ids) == limit, nil
}

// nullUUID 는 커서가 비었을 때 비교 기준이 되는 가장 작은 uuid 를 쓴다.
func nullUUID(id string) string {
	if id == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return id
}

// Now 는 DB 시계다. 안전망의 구간 경계는 앱 시계가 아니라 DB 시계로 잡아야 한다.
// 두 시계가 어긋나면 그 차이만큼의 변경이 영원히 안 걸린다.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&t)
	return t, err
}

// SetConversationState 는 대화를 완료·보류·재개한다. 삭제된 대화는 바꾸지 않고 ErrNotFound 다.
// 권한 검사는 이 트랜잭션 앞에서 끝나서 그 사이에 삭제가 끼어들 수 있다. 행을 잠그고 deleted_at 을 다시 본다.
// record 가 정한 시스템 줄과 업무 기록을 같은 트랜잭션에 남긴다(D39). 돌려주는 대화도 트랜잭션 안에서 읽은 값이다 —
// 커밋 뒤에 다시 읽지 않으므로 커밋된 변경이 그 뒤의 읽기 실패로 실패처럼 보이지 않는다.
func (s *Store) SetConversationState(ctx context.Context, convID string, state core.State, until *time.Time, record core.ChangeRecorder) (core.Conversation, error) {
	return s.changeConversation(ctx, convID, record, func(tx pgx.Tx, before core.Conversation) error {
		// 잠근 행이 이미 그 상태면 쓰지 않는다 — 동시에 둘이 "완료" 를 눌러도 종료 기록은 한 번이다.
		// 보류는 보류 시각까지 같아야 같은 요청이다. 시각만 바꾸는 보류는 바꾸고 기록한다.
		if before.State == state && (state != core.StateSnoozed || sameInstant(before.SnoozedUntil, until)) {
			return errUnchanged
		}
		// 닫힌 상담을 다시 여는 것도 재개다. 그 회원에게 더 최근 상담이 있으면 열지 않는다.
		// guardReopen 은 행을 잠그기 전에 자문 잠금을 잡는다(changeConversation 의 before 함수). 여기서는 상태만 바꾼다.
		// 닫는 것은 상담원이다(D48) — 이 길은 상담원의 상태 변경이다. 다시 열 때는 closed_by 를 지우지 않는다(판정은 state 와 같이 본다).
		_, err := tx.Exec(ctx, `
			UPDATE conversations
			   SET state = $2,
			       snoozed_until = CASE WHEN $2 = 'SNOOZED' THEN $3::timestamptz ELSE NULL END,
			       closed_by = CASE WHEN $2 = 'CLOSED' THEN 'AGENT' ELSE closed_by END,
			       updated_at = clock_timestamp()
			 WHERE id = $1`, convID, state, until)
		if err != nil || state != core.StateClosed {
			return err
		}
		// 끝낼 때 처리 주체와 담당 상담원을 같은 트랜잭션에 남긴다(D45). 다시 열었다 닫으면 마지막 종료로 덮는다.
		return recordClosure(ctx, tx, convID, core.ClosedByAgent)
	}, func(tx pgx.Tx) error {
		if state == core.StateClosed {
			return nil
		}
		return guardReopen(ctx, tx, convID)
	})
}

// AssignConversation 은 담당자를 지정하거나(agentID) 해제한다(빈 문자열). 삭제된 대화는 바꾸지 않고 ErrNotFound 다.
// expected 가 있으면 잠근 행의 담당자가 그 값일 때만 바꾸고, 아니면 core.ErrAssigneeChanged 다(빈 문자열은 미배정).
// 동시에 둘이 가져가도 한 사람만 바꾼다 — 두 번째는 첫 번째가 커밋한 담당자를 보고 거절된다(D39).
// record 는 SetConversationState 와 같다.
func (s *Store) AssignConversation(ctx context.Context, convID, agentID string, expected *string, record core.ChangeRecorder) (core.Conversation, error) {
	return s.changeConversation(ctx, convID, record, func(tx pgx.Tx, before core.Conversation) error {
		// 잠근 행의 담당자가 이미 요청한 사람이면(해제 요청인데 이미 미배정인 것 포함) 쓰지 않는다. expected 와 무관하다 —
		// 같은 사람이 두 번 눌렀거나 화면이 늦게 본 것이다. 바꾸지도 기록하지도 알리지도 않는다.
		if before.AssigneeAgentID == agentID {
			return errUnchanged
		}
		if expected != nil && before.AssigneeAgentID != *expected {
			return core.ErrAssigneeChanged
		}
		tag, err := tx.Exec(ctx, `
			UPDATE conversations
			   SET assignee_agent_id = nullif($2::text, '')::uuid, updated_at = clock_timestamp()
			 WHERE id = $1
			   AND ($2::text = '' OR EXISTS (SELECT 1 FROM agents a
			                                  WHERE a.id = $2::text::uuid AND a.workspace_id = conversations.workspace_id))`,
			convID, agentID)
		if err != nil {
			// 없는 상담원에게 배정하면 FK 위반이다. 서버 잘못이 아니라 입력 잘못이다.
			return classify(err)
		}
		if tag.RowsAffected() == 0 {
			// 대화는 잠가 뒀으니 다른 워크스페이스 상담원에게 배정하려 한 것이다. 입력 잘못이다.
			return core.ErrInvalidReference
		}
		// 담당자는 자동으로 팔로워가 된다. 채널톡과 같다.
		if agentID != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO conversation_followers (conversation_id, agent_id)
				VALUES ($1, $2) ON CONFLICT DO NOTHING`, convID, agentID); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}

// changeConversation 은 상태·담당 변경의 공통 길이다(D39). 한 트랜잭션에서
//
//	lockFirst(자문 잠금 등) → 대화 행 잠금과 읽기(before) → change → 다시 읽기(after) → record 의 시스템 줄·업무 기록 → 마지막 읽기
//
// 를 하고 커밋한 뒤 알린다. 시스템 줄의 번호도 같은 트랜잭션에서 발급되므로(appendOnce) 셋이 같이 커밋되거나 같이 사라진다.
// 커밋 뒤에는 알림만 보낸다. 알림이 실패해도 안전망이 메운다(D4). 커넥션을 쥔 채 DB 밖의 자원을 기다리지 않는다(D28).
// change 가 errUnchanged 를 돌려주면 잠근 행이 이미 바라던 값이다. 아무것도 쓰지 않고(record 도 부르지 않는다)
// 알리지도 않고 잠근 행을 그대로 돌려준다.
func (s *Store) changeConversation(ctx context.Context, convID string, record core.ChangeRecorder,
	change func(tx pgx.Tx, before core.Conversation) error, lockFirst func(tx pgx.Tx) error) (core.Conversation, error) {
	var final core.Conversation
	for attempt := 0; attempt < appendMaxAttempts; attempt++ {
		unchanged := false
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			final, unchanged, err = changeOnce(ctx, tx, convID, record, change, lockFirst)
			return err
		})
		if errors.Is(err, errRetry) {
			continue
		}
		if err != nil {
			return core.Conversation{}, err
		}
		if unchanged {
			return final, nil
		}
		if notice, err := changedNotice(final.ID, final.WorkspaceID, final.LastSeq, final.LastPublicSeq, true); err == nil {
			s.notify(ctx, notice)
		}
		return final, nil
	}
	return core.Conversation{}, core.ErrTooManyRetries
}

// changeOnce 는 changeConversation 의 트랜잭션 한 번이다. 커밋과 알림은 부르는 쪽이 한다.
// unchanged 면 잠근 행이 이미 바라던 값이라 아무것도 쓰지 않았다.
func changeOnce(ctx context.Context, tx pgx.Tx, convID string, record core.ChangeRecorder,
	change func(tx pgx.Tx, before core.Conversation) error, lockFirst func(tx pgx.Tx) error) (final core.Conversation, unchanged bool, err error) {
	if lockFirst != nil {
		if err := lockFirst(tx); err != nil {
			return core.Conversation{}, false, err
		}
	}
	before, err := conversationOn(ctx, tx, convID, true)
	if err != nil {
		return core.Conversation{}, false, err
	}
	if before.DeletedAt != nil {
		return core.Conversation{}, false, core.ErrNotFound
	}
	if err := change(tx, before); errors.Is(err, errUnchanged) {
		return before, true, nil
	} else if err != nil {
		return core.Conversation{}, false, err
	}
	after, err := conversationOn(ctx, tx, convID, false)
	if err != nil {
		return core.Conversation{}, false, err
	}
	if record != nil {
		if err := writeChangeRecord(ctx, tx, convID, record(before, after)); err != nil {
			return core.Conversation{}, false, err
		}
	}
	final, err = conversationOn(ctx, tx, convID, false)
	return final, false, err
}

// errUnchanged 는 change 가 "잠근 행이 이미 바라던 값이다" 를 알리는 표시다. 바깥으로 나가지 않는다.
var errUnchanged = errors.New("pgstore: 바꾸려는 값이 DB 의 값과 이미 같아 쓰지 않았다(내부 신호)")

// sameInstant 는 두 시각이 DB 정밀도(마이크로초)로 같은지다. 둘 다 없으면 같다.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

// writeChangeRecord 는 변경과 같은 트랜잭션에 시스템 줄(상담원만 본다)과 업무 기록을 넣는다.
func writeChangeRecord(ctx context.Context, tx pgx.Tx, convID string, rec core.ChangeRecord) error {
	if rec.Note != "" {
		if _, _, err := appendOnce(ctx, tx, core.AppendParams{
			ConversationID: convID,
			ClientMsgID:    "system-" + id.New(),
			SenderKind:     core.SenderSystem,
			Visibility:     core.Private,
			Body:           rec.Note,
		}); err != nil {
			return err
		}
	}
	if rec.Event.Kind != "" {
		rec.Event.ConversationID = convID
		if err := recordEvent(ctx, tx, rec.Event); err != nil {
			return err
		}
	}
	return nil
}

// WakeSnoozed 는 보류 시각이 지난 대화를 다시 연다. 주기 실행용이다. 삭제한(가린) 상담은 건드리지 않는다.
func (s *Store) WakeSnoozed(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE conversations
		   SET state = 'OPEN', snoozed_until = NULL, updated_at = clock_timestamp()
		 WHERE state = 'SNOOZED' AND snoozed_until IS NOT NULL AND snoozed_until <= now()
		   AND deleted_at IS NULL
		RETURNING id, last_seq, workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var woken []string
	type row struct {
		id, ws string
		seq    int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.seq, &r.ws); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range all {
		woken = append(woken, r.id)
		if notice, err := changedNotice(r.id, r.ws, r.seq, 0, true); err == nil {
			s.notify(ctx, notice)
		}
	}
	return woken, nil
}

// DeleteConversation 은 상담을 가린다. 행과 메시지는 그대로다(D24). 이미 가렸거나 없으면 ErrNotFound.
// 구독 중인 화면이 다시 읽도록 메타 변화로 알린다 — 다시 읽으면 권한 검사가 "없다" 로 답한다.
func (s *Store) DeleteConversation(ctx context.Context, convID, agentID, reason string) error {
	return s.setDeleted(ctx, convID, `
		UPDATE conversations
		   SET deleted_at = clock_timestamp(), deleted_by = nullif($2::text, '')::uuid, delete_reason = $3,
		       updated_at = clock_timestamp()
		 WHERE id = $1 AND deleted_at IS NULL
		RETURNING last_seq, last_public_seq, workspace_id`, agentID, reason)
}

// ConversationDeletion 은 가린 상담의 가린 시각·사람·사유다. 가려져 있지 않으면 ErrNotFound.
func (s *Store) ConversationDeletion(ctx context.Context, convID string) (core.Deletion, error) {
	var d core.Deletion
	err := s.pool.QueryRow(ctx, `
		SELECT c.deleted_at, COALESCE(a.name, ''), COALESCE(c.delete_reason, '')
		  FROM conversations c
		  LEFT JOIN agents a ON a.id = c.deleted_by
		 WHERE c.id = $1 AND c.deleted_at IS NOT NULL`, convID).Scan(&d.DeletedAt, &d.DeletedByName, &d.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deletion{}, core.ErrNotFound
	}
	return d, err
}

// RestoreConversation 은 가린 상담을 되돌린다. 가려져 있지 않으면 ErrNotFound.
// 그 사이 회원이 새 상담을 열었으면 이 상담은 이미 지난 상담으로 표시돼 있다. 회원에게는 돌아오지 않는다.
//
// 지난 상담은 종료 상태로 되살린다. 회원은 거기에 답할 수 없고 다시 열 수도 없는데(D26), 열린 채로 돌아오면
// 받은함에 답할 수 없는 열린 상담이 남는다. 지난 상담이 아니면 삭제 전 상태 그대로다.
func (s *Store) RestoreConversation(ctx context.Context, convID string) error {
	return s.setDeleted(ctx, convID, `
		UPDATE conversations
		   SET deleted_at = NULL, deleted_by = NULL, delete_reason = NULL, updated_at = clock_timestamp(),
		       -- 열린 채 가렸던 지난 상담을 종료로 돌리는 것은 사람이 누른 것이 아니다(D48). 이미 닫혀 있었으면 닫은 쪽을 그대로 둔다.
		       closed_by     = CASE WHEN past_for_customer AND state <> 'CLOSED' THEN 'SYSTEM' ELSE closed_by END,
		       state         = CASE WHEN past_for_customer THEN 'CLOSED' ELSE state END,
		       snoozed_until = CASE WHEN past_for_customer THEN NULL ELSE snoozed_until END
		 WHERE id = $1 AND deleted_at IS NOT NULL
		RETURNING last_seq, last_public_seq, workspace_id`)
}

// setDeleted 는 가리기·되살리기의 공통 길이다. query 의 $1 은 convID 고, extra 가 $2 부터다.
func (s *Store) setDeleted(ctx context.Context, convID, query string, extra ...any) error {
	var notice string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var seq, publicSeq int64
		var wsID string
		// 회원의 열린 상담 판정(OpenMemberConversation·guardReopen)과 같은 줄에 선다.
		// 안 그러면 가리는 사이에 같은 회원의 새 문의가 "열린 상담이 있다" 를 보고 가려질 방으로 들어간다.
		if err := lockCustomerOfConversation(ctx, tx, convID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, query, append([]any{convID}, extra...)...).Scan(&seq, &publicSeq, &wsID)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		notice, err = changedNotice(convID, wsID, seq, publicSeq, true)
		return err
	})
	if err != nil {
		return err
	}
	s.notify(ctx, notice)
	return nil
}
