package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 수정·삭제·재상담·업무 기록. 전부 "기록을 지우지 않는다" 위에 선다.

// ReviseMessage 는 수정(EDIT) 또는 삭제(DELETE) 한다. 한 트랜잭션에서:
//  1. 대상 행을 잠그고 발신자를 확인한다
//  2. 원문을 message_revisions 에 남긴다
//  3. 대상 행의 본문·edited_at/deleted_at 을 바꾼다
//  4. 같은 로그에 EDIT/DELETE 이벤트 행을 번호와 함께 넣는다 — 구독자는 번호로 그 변화를 받는다
//
// 이벤트 행의 가시성은 대상과 같다. 내부 메모의 수정은 고객 번호를 받지 않는다.
func (s *Store) ReviseMessage(ctx context.Context, kind core.MessageKind, p core.ReviseParams) (core.AppendResult, error) {
	if kind != core.KindEdit && kind != core.KindDelete {
		return core.AppendResult{}, errors.New("pgstore: 수정 이력의 kind 가 EDIT 도 DELETE 도 아니다(호출자 잘못)")
	}
	var res core.AppendResult
	var notice string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var senderKind core.SenderKind
		var senderAgent, senderCustomer *string
		var visibility core.Visibility
		var body string
		var deletedAt *time.Time
		var rowKind core.MessageKind
		err := tx.QueryRow(ctx, `
			SELECT sender_kind, sender_agent_id::text, sender_customer::text, visibility, body, deleted_at, kind
			  FROM messages WHERE id = $1 AND conversation_id = $2 FOR UPDATE`, p.MessageID, p.ConversationID,
		).Scan(&senderKind, &senderAgent, &senderCustomer, &visibility, &body, &deletedAt, &rowKind)
		if errors.Is(err, pgx.ErrNoRows) || deletedAt != nil || rowKind != core.KindMessage {
			return core.ErrNotFound
		}
		if err != nil {
			return err
		}
		// 고객이 볼 수 없는 줄(내부 메모·시스템 줄은 PRIVATE)은 고객에게 없는 줄이다 — 없는 메시지와 같은 404 다(감사 29).
		// 아래 시스템 줄 판정보다 먼저다. 먼저 409 system_line 으로 답하면 고객이 그 id 의 줄이 있다는 것과 시스템 줄이라는 것을 안다.
		if p.ActorKind == core.SenderCustomer && visibility != core.Public {
			return core.ErrNotFound
		}
		// 시스템 줄(배정·종료·보류)은 운영 기록이다. 누구도 고치거나 지우지 못한다 — 관리자도(감사 28). 누가 언제 무엇을 했는지가 사라진다.
		if senderKind == core.SenderSystem {
			return core.ErrSystemLine
		}
		owner := senderKind == p.ActorKind && deref(senderAgent) == p.ActorAgentID && deref(senderCustomer) == p.ActorCustomer
		// 관리자는 상담원·AI·봇 메시지를 지울 수 있다. 고객 메시지는 고객의 것이다.
		adminMayDelete := p.AdminOverride && kind == core.KindDelete && senderKind != core.SenderCustomer
		if !owner && !adminMayDelete {
			return core.ErrNotOwner
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_revisions (message_id, body, reason, actor_kind, actor_id)
			VALUES ($1, $2, $3, $4, nullif($5::text, '')::uuid)`,
			p.MessageID, body, string(kind), string(p.ActorKind), firstNonEmpty(p.ActorAgentID, p.ActorCustomer)); err != nil {
			return err
		}
		if kind == core.KindEdit {
			_, err = tx.Exec(ctx, "UPDATE messages SET body = $2, edited_at = clock_timestamp() WHERE id = $1", p.MessageID, p.Body)
			if err == nil {
				// 다시 고치면 앞선 수정본은 덮인 것이다. 이벤트 행에 남겨 두면 로그와 내보내기로 계속 나간다.
				// 원문들은 message_revisions 에 있다. 새 이벤트 행은 이 뒤에 들어간다.
				_, err = tx.Exec(ctx, "UPDATE messages SET body = '' WHERE ref_message_id = $1 AND kind = 'EDIT'", p.MessageID)
			}
		} else {
			_, err = tx.Exec(ctx, "UPDATE messages SET body = '', deleted_at = clock_timestamp() WHERE id = $1", p.MessageID)
			if err == nil {
				// 지우기 전에 고쳤던 본문도 이벤트 행에 남아 있다. 같이 비운다 — "지운 본문은 어디에도 안 나온다".
				_, err = tx.Exec(ctx, "UPDATE messages SET body = '' WHERE ref_message_id = $1 AND kind = 'EDIT'", p.MessageID)
			}
		}
		if err != nil {
			return err
		}
		res, notice, err = appendOnce(ctx, tx, core.AppendParams{
			ConversationID: p.ConversationID,
			ClientMsgID:    string(kind) + "-" + id.New(),
			Kind:           kind,
			RefMessageID:   p.MessageID,
			SenderKind:     p.ActorKind,
			SenderAgentID:  p.ActorAgentID,
			SenderCustomer: p.ActorCustomer,
			Visibility:     visibility,
			Body:           p.Body,
		})
		return err
	})
	if err != nil {
		return core.AppendResult{}, err
	}
	s.notify(ctx, notice)
	return res, nil
}

// 대화 id 를 같이 받는다. 메시지 id 만으로 찾으면 다른 대화(다른 워크스페이스)의 원문이 나온다.
func (s *Store) Revisions(ctx context.Context, convID, messageID string) ([]core.Revision, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.body, r.reason, r.actor_kind, COALESCE(r.actor_id::text, ''), r.created_at
		  FROM message_revisions r
		  JOIN messages m ON m.id = r.message_id
		 WHERE m.id = $2 AND m.conversation_id = $1 ORDER BY r.id`, convID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Revision, 0, 2)
	for rows.Next() {
		var r core.Revision
		if err := rows.Scan(&r.Body, &r.Reason, &r.ActorKind, &r.ActorID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── 상담마다 대화 하나 ─────────────────────────────────────────────

// OpenMemberConversation 은 회원의 상담을 연다. 열린 상담이 있으면 그 방을 돌려주고, 없으면 새로 만든다.
// 종료된 상담은 다시 열지 않는다 — 다시 문의하면 새 대화다.
//
// 회원별로 줄을 세우고 시작한다. 안 그러면 동시 요청이 각자 "열린 상담이 없다" 를 보고 방을 둘 만든다.
// 버튼을 두 번 눌러도, 탭이 둘이어도 방은 하나다.
func (s *Store) OpenMemberConversation(ctx context.Context, wsID, customerID string) (core.Conversation, core.OpenResult, error) {
	var convID string
	var notices []string
	result := core.OpenedExisting
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 고객 행을 잠그면 메시지 저장의 외래 키 확인과 엇갈려 교착이 난다.
		// 자문 잠금은 다른 잠금과 순서가 얽히지 않고 트랜잭션이 끝나면 풀린다.
		if err := lockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1 AND workspace_id = $2)", customerID, wsID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return core.ErrNotFound
		}
		err := tx.QueryRow(ctx, `
			SELECT id FROM conversations
			 WHERE workspace_id = $1 AND customer_id = $2 AND state <> 'CLOSED'
			   -- 가린 상담은 열려 있어도 없는 것이다. 다시 문의하면 새 대화다(D29).
			   AND deleted_at IS NULL AND NOT past_for_customer
			 ORDER BY last_message_at DESC NULLS LAST, created_at DESC
			 LIMIT 1`, wsID, customerID).Scan(&convID)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err // 열린 상담이 있거나(nil) 조회가 실패했다
		}
		convID = id.New()
		result = core.OpenedNew
		// 변경 시각은 문장 시각으로 준다. 기본값 now() 는 트랜잭션 시작이라 위의 회원 잠금을 기다린 만큼 과거가 되고,
		// 안전망 겹침 구간보다 오래 기다리면 알림을 놓친 새 상담을 안전망이 못 잡는다(D4).
		if _, err = tx.Exec(ctx,
			"INSERT INTO conversations (id, workspace_id, customer_id, subject, updated_at) VALUES ($1, $2, $3, '', clock_timestamp())",
			convID, wsID, customerID); err != nil {
			return err
		}
		// 새 상담이 열리면 앞선 상담은 그 회원에게 지난 상담이다. 다시 볼 수도, 다시 열 수도 없다.
		// 이미 구독 중인 옛 탭이 있다. 메타 변화로 알려 다시 읽게 하면 권한 검사가 "없다" 로 답하고 구독이 풀린다.
		// updated_at 도 올린다. 알림을 놓쳤을 때 안전망은 변경 시각으로만 바뀐 대화를 찾는다(D4) — 안 올리면 옛 탭이 영영 안 깬다.
		rows, err := tx.Query(ctx, `
			UPDATE conversations SET past_for_customer = true, updated_at = clock_timestamp()
			 WHERE customer_id = $1 AND id <> $2 AND NOT past_for_customer
			RETURNING id, workspace_id, last_seq, last_public_seq`, customerID, convID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pastID, pastWS string
			var seq, publicSeq int64
			if err := rows.Scan(&pastID, &pastWS, &seq, &publicSeq); err != nil {
				return err
			}
			notice, err := changedNotice(pastID, pastWS, seq, publicSeq, true)
			if err != nil {
				return err
			}
			notices = append(notices, notice)
		}
		return rows.Err()
	})
	if err != nil {
		return core.Conversation{}, result, err
	}
	for _, notice := range notices {
		s.notify(ctx, notice)
	}
	conv, err := s.Conversation(ctx, convID)
	return conv, result, err
}

// guardReopen 은 닫힌 대화를 다시 여는 모든 길(공개 메시지, 상담원의 상태 변경)이 거치는 문이다.
// 회원에게 열린 상담은 하나다. 종료된 상담보다 뒤에 열린 상담이 있거나 다른 열린 상담이 있으면 다시 열지 않는다 —
// 열어 주면 지난 상담이 회원의 목록에 돌아오고, 열린 방이 둘이 된다.
//
// OpenMemberConversation 과 같은 자문 잠금을 잡는다. 상태는 잠금을 잡은 뒤에 읽어야 한다.
// 먼저 읽으면 그 사이에 새 상담이 열려도 모르고 지난 상담을 연다.
func guardReopen(ctx context.Context, tx pgx.Tx, convID string) error {
	if err := lockCustomerOfConversation(ctx, tx, convID); err != nil {
		return err
	}
	var ended bool
	err := tx.QueryRow(ctx, `
		SELECT c.past_for_customer OR c.state = 'CLOSED' AND COALESCE(cu.external_id, '') <> ''
		       AND EXISTS (SELECT 1 FROM conversations n
		                    WHERE n.customer_id = c.customer_id AND n.id <> c.id
		                      AND ((n.state <> 'CLOSED' AND n.deleted_at IS NULL) OR (n.created_at, n.id) > (c.created_at, c.id)))
		  FROM conversations c JOIN customers cu ON cu.id = c.customer_id
		 WHERE c.id = $1`, convID).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 없는 대화는 뒤따르는 쿼리가 ErrNotFound 로 답한다
	}
	if err != nil {
		return err
	}
	if ended {
		return core.ErrConversationEnded
	}
	return nil
}

// ListCustomerHistory 는 이 고객의 이전 상담을 이어서 읽는다 — 상담원 화면의 "이전 상담 더 보기".
// 커서가 가리키는 대화의 그 번호 앞과, 그보다 먼저 열린 대화들에서 최근 limit 줄을 오래된 순으로 준다.
// 수정·삭제 이벤트 행은 빼고 메시지만 준다(본문에는 이미 반영돼 있다). 메시지마다 어느 대화의 것인지 담는다.
//
// 워크스페이스와 고객을 조건에 같이 건다. 커서의 대화가 그 고객의 것이 아니면 아무것도 나오지 않는다.
//
// currentID 는 상담원이 보고 있는 대화다. 커서를 어떻게 주든 그 대화와 그 뒤에 열린 상담은 나오지 않는다.
func (s *Store) ListCustomerHistory(ctx context.Context, wsID, customerID, currentID string, before core.PriorCursor, limit int) ([]core.Message, error) {
	limit = pageLimit(limit, core.PriorHistoryPageDefault, core.PriorHistoryPageMax)
	rows, err := s.pool.Query(ctx, "SELECT"+replaceCursor(messageColumns, "seq")+", m.conversation_id::text"+messageJoins+`
		  JOIN conversations pc  ON pc.id = m.conversation_id
		  JOIN conversations cur ON cur.id = $3 AND cur.customer_id = pc.customer_id AND cur.workspace_id = pc.workspace_id
		  JOIN conversations now ON now.id = $6 AND now.customer_id = pc.customer_id AND now.workspace_id = pc.workspace_id
		 WHERE pc.customer_id = $1 AND pc.workspace_id = $2 AND m.kind = 'MESSAGE'
		   -- 가린 상담은 이어 보기에서도 빠진다. 커서가 가린 상담을 가리켜도 그 자리(시각)만 쓴다.
		   AND pc.deleted_at IS NULL
		   AND (pc.created_at, pc.id) < (now.created_at, now.id)
		   AND ((pc.created_at, pc.id) < (cur.created_at, cur.id) OR (pc.id = cur.id AND m.seq < $4))
		 ORDER BY pc.created_at DESC, pc.id DESC, m.seq DESC
		 LIMIT $5`, customerID, wsID, before.ConversationID, before.Seq, limit, currentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Message, 0, limit)
	for rows.Next() {
		var convID string
		m, err := scanMessage(rows, "", &convID)
		if err != nil {
			return nil, err
		}
		m.ConversationID = convID
		if m.Attachment != nil {
			m.Attachment.ConversationID = convID
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 최근 것부터 잘랐으니 오래된 순으로 뒤집는다.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AgentReplyPoint 는 상담원의 마지막 공개 답변의 고객 번호다. 고객 화면의 "읽음" 은 이 지점까지다 —
// 상담원이 읽기만 한 것은 고객에게 보이지 않는다. 답하지 않은 읽음은 CS 를 부른다.
func (s *Store) AgentReplyPoint(ctx context.Context, convID string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(public_seq), 0) FROM messages
		 WHERE conversation_id = $1 AND sender_kind = 'AGENT' AND visibility = 'PUBLIC' AND kind = 'MESSAGE'
		   AND deleted_at IS NULL`, convID,
	).Scan(&seq)
	return seq, err
}

// ListMessagesBefore 는 beforeSeq 앞의 메시지를 오래된 순으로 limit 개 돌려준다.
func (s *Store) ListMessagesBefore(ctx context.Context, convID string, beforeSeq int64, limit int, internal bool) ([]core.Message, error) {
	limit = pageLimit(limit, core.MessagesBeforePageDefault, core.MessagesBeforePageMax)
	cursor, filter := "seq", ""
	if !internal {
		cursor, filter = "public_seq", " AND m.visibility = 'PUBLIC'"
	}
	query := "SELECT * FROM (SELECT" + replaceCursor(messageColumns, cursor) + messageJoins +
		" WHERE m.conversation_id = $1 AND m." + cursor + " < $2" + filter +
		" ORDER BY m." + cursor + " DESC LIMIT $3) page ORDER BY 1"
	rows, err := s.pool.Query(ctx, query, convID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows, convID)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── 업무 처리 기록 ──────────────────────────────────────────────────

// RecordEvent 는 업무 기록 한 건을 따로 남긴다. 상태·담당 변경의 기록은 이 길이 아니라 변경과 같은 트랜잭션에서 남긴다(D39).
func (s *Store) RecordEvent(ctx context.Context, e core.ConversationEvent) error {
	return recordEvent(ctx, s.pool, e)
}

func recordEvent(ctx context.Context, q querier, e core.ConversationEvent) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	raw, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO conversation_events (workspace_id, conversation_id, customer_id, agent_id, kind, detail)
		SELECT c.workspace_id, c.id, c.customer_id, nullif($2::text, '')::uuid, $3, $4
		  FROM conversations c WHERE c.id = $1`, e.ConversationID, e.AgentID, e.Kind, string(raw)) // 바이트로 넘기면 단순 프로토콜에서 bytea 로 가서 jsonb 해석이 깨진다
	return err
}

func (s *Store) ListEvents(ctx context.Context, f core.EventFilter) ([]core.ConversationEvent, error) {
	f.Limit = pageLimit(f.Limit, core.EventsPageDefault, core.EventsPageMax)
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.conversation_id, COALESCE(e.customer_id::text, ''), COALESCE(cu.display_name, ''),
		       COALESCE(e.agent_id::text, ''), COALESCE(a.name, ''), e.kind, e.detail, e.created_at
		  FROM conversation_events e
		  LEFT JOIN customers cu ON cu.id = e.customer_id
		  LEFT JOIN agents a ON a.id = e.agent_id
		 WHERE e.workspace_id = $1
		   AND ($2::text = '' OR e.agent_id = $2::text::uuid)
		   AND ($3::text = '' OR e.conversation_id = $3::text::uuid)
		   AND ($4::timestamptz IS NULL OR e.created_at >= $4)
		   AND ($5::timestamptz IS NULL OR e.created_at < $5)
		   AND ($7 OR NOT EXISTS (SELECT 1 FROM conversations c WHERE c.id = e.conversation_id AND c.deleted_at IS NOT NULL))
		 ORDER BY e.id DESC LIMIT $6`,
		f.WorkspaceID, f.AgentID, f.ConversationID, nullTime(f.From), nullTime(f.To), f.Limit, f.IncludeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.ConversationEvent, 0, 32)
	for rows.Next() {
		var e core.ConversationEvent
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.CustomerID, &e.CustomerName, &e.AgentID, &e.AgentName,
			&e.Kind, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// WorkSummary 는 기간 안의 상담원별 집계다. includeDeleted 가 false 면 삭제된 상담의 이벤트를 세지 않는다.
func (s *Store) WorkSummary(ctx context.Context, wsID string, from, to time.Time, includeDeleted bool) ([]core.AgentWork, error) {
	// 아래 SQL 의 종류 문자열은 core 의 업무 기록 종류 상수 값이다:
	// assigned = core.EventAssigned + core.EventTransferred, answered = core.EventAnswered,
	// closed = core.EventClosed, snoozed = core.EventSnoozed.
	// 상수 값과 여기 문자열이 어긋나면 숫자가 조용히 0 이 된다. TestWorkSummary_업무기록종류상수를센다 가 둘을 맞춰 본다.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.name,
		       count(*) FILTER (WHERE e.kind IN ('assigned', 'transferred')) AS assigned,
		       count(*) FILTER (WHERE e.kind = 'answered')                    AS answered,
		       count(*) FILTER (WHERE e.kind = 'closed')                      AS closed,
		       count(*) FILTER (WHERE e.kind = 'snoozed')                     AS snoozed,
		       count(DISTINCT e.customer_id)                                   AS customers
		  FROM agents a
		  LEFT JOIN conversation_events e
		         ON e.agent_id = a.id
		        AND ($2::timestamptz IS NULL OR e.created_at >= $2)
		        AND ($3::timestamptz IS NULL OR e.created_at < $3)
		        AND ($4 OR NOT EXISTS (SELECT 1 FROM conversations c WHERE c.id = e.conversation_id AND c.deleted_at IS NOT NULL))
		 WHERE a.workspace_id = $1 AND a.active
		 GROUP BY a.id, a.name ORDER BY a.name`, wsID, nullTime(from), nullTime(to), includeDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.AgentWork, 0, 8)
	for rows.Next() {
		var w core.AgentWork
		if err := rows.Scan(&w.AgentID, &w.AgentName, &w.Assigned, &w.Answered, &w.Closed, &w.Snoozed, &w.Customers); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
