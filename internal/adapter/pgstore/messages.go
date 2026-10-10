package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/metric"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

const appendMaxAttempts = 4

// AISenderName 은 AI 응대 메시지의 보낸 사람 이름이다(D45). 저장한 줄에는 이름이 없고 읽을 때 붙인다.
const AISenderName = "AI 상담원"

// AppendMessage 는 대화에 메시지 한 건을 넣고 번호를 발급한다.
//
// 불변식 넷을 이 함수가 지킨다.
//   - I1 seq 는 대화별 1부터 빈칸 없이 증가한다
//   - I2 seq 순서와 커밋 순서가 같다
//   - I3 같은 (conversation_id, client_msg_id) 는 한 행만 만든다
//   - I10 공개 메시지의 public_seq 도 1부터 빈칸 없이 증가한다
//
// 방법은 conversations 행을 UPDATE 해서 락을 쥔 채 두 번호를 같이 발급하는 것이다.
// nextval 을 쓰면 롤백한 번호가 돌아오지 않아 빈칸이 생기고, 빈칸은 수신측에서 유실로 읽힌다.
func (s *Store) AppendMessage(ctx context.Context, p core.AppendParams) (core.AppendResult, error) {
	if p.Visibility == "" {
		p.Visibility = core.Public
	}
	var res core.AppendResult
	var notice string
	for attempt := 0; attempt < appendMaxAttempts; attempt++ {
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			var inner error
			res, notice, inner = appendOnce(ctx, tx, p)
			return inner
		})
		if err == nil {
			s.notify(ctx, notice)
			return res, nil
		}
		if errors.Is(err, errRetry) {
			continue
		}
		return core.AppendResult{}, err
	}
	return core.AppendResult{}, core.ErrTooManyRetries
}

// appendOnce 는 한 트랜잭션 안에서 네 단계를 밟는다.
// 단계마다 왜 그 순서여야 하는지가 다르므로 함수를 나눠 뒀다.
func appendOnce(ctx context.Context, tx pgx.Tx, p core.AppendParams) (core.AppendResult, string, error) {
	// 1) 멱등 조회. 재전송이면 여기서 끝난다. 번호를 새로 쓰지 않는다.
	//    다른 발신자가 같은 id 를 들고 오면 재전송이 아니다 — 남의 메시지를 돌려주지 않는다.
	existing, err := findByClientMsgID(ctx, tx, p)
	switch {
	case err == nil:
		return core.AppendResult{Message: existing, Created: false}, "", nil
	case !errors.Is(err, core.ErrNotFound):
		return core.AppendResult{}, "", err
	}

	// 2) 닫힌 대화를 다시 열 수 있는 메시지면 열어도 되는지 먼저 본다 (회원당 열린 상담 하나).
	if p.Reopen {
		if err := guardReopen(ctx, tx, p.ConversationID); err != nil {
			return core.AppendResult{}, "", err
		}
	}

	// 3) 대화 행을 잠그고 번호를 발급한다.
	alloc, err := allocate(ctx, tx, p)
	if err != nil {
		return core.AppendResult{}, "", err
	}

	// 4) 메시지를 넣는다. 같은 client_msg_id 가 동시에 들어오면 여기서 unique 위반이 난다.
	//    롤백하면 2) 의 번호 증가도 함께 되돌아가서 빈칸이 남지 않는다.
	msg, err := insertMessage(ctx, tx, p, alloc)
	if err != nil {
		return core.AppendResult{}, "", err
	}
	// 고객이 기다리던 차례를 닫은 답이면 응답 기록을 같은 트랜잭션에 남긴다(D45).
	if alloc.waitedFrom != nil {
		if err := recordResponse(ctx, tx, p, alloc, msg.CreatedAt); err != nil {
			return core.AppendResult{}, "", err
		}
	}

	// 4) 알림은 커밋 **뒤에** 보낸다. 트랜잭션 안에서 NOTIFY 하면 Postgres 가 알림 트랜잭션들의 커밋을
	//    전역 락으로 직렬화하고, 그 임계구역 안에 fsync 가 들어가 1,000 tx/s 에서 커밋이 줄을 선다.
	//    커밋 뒤에는 행이 이미 보이므로 "알림이 먼저 도착하는" 경합이 없고, 보내기 전에 죽으면 안전망이 메운다.
	//    이 메시지가 대화를 다시 열었다면 그건 메타 변경이라 플래그를 켠다.
	notice, err := changedNotice(p.ConversationID, alloc.workspaceID, alloc.seq, alloc.publicSeq, alloc.reopened)
	if err != nil {
		return core.AppendResult{}, "", err
	}
	res := core.AppendResult{Message: msg, Created: true, NeedsAutoReply: alloc.needsAutoReply, Answered: alloc.answered,
		Responded: alloc.waitedFrom != nil, Seq: alloc.seq}
	if alloc.isPublic {
		res.PublicSeq = alloc.publicSeq
	}
	return res, notice, nil
}

// allocation 은 2) 단계의 결과다.
type allocation struct {
	workspaceID    string
	answered       bool
	seq            int64
	publicSeq      int64
	isPublic       bool
	reopened       bool
	needsAutoReply bool
	// waitedFrom 은 이 메시지가 닫은 차례의 시작(고객이 기다리기 시작한 시각)이다. 차례를 닫는 답이 아니면 nil 이다(D45).
	waitedFrom *time.Time
}

// allocate 는 conversations 행을 UPDATE 해서 락을 쥔 채 두 번호를 같이 발급한다.
// 커밋까지 락을 쥐므로 커밋 순서가 번호 순서가 된다 (불변식 I2).
//
// 삭제된 대화에는 번호를 주지 않는다. 어떤 발신자든 ErrNotFound 다. 권한 검사는 이 트랜잭션 앞에서 끝나므로
// 검사를 지난 뒤 삭제가 끼어들 수 있다. 행 잠금을 잡은 UPDATE 가 deleted_at 을 다시 보므로 그 사이에 커밋된 삭제도 걸린다.
//
// 바꾸기 전 값(상태, "고객이 기다리고 있었나", 기다리기 시작한 시각)은 같은 문장의 FOR UPDATE CTE 가 잠근 행에서 읽는다(DEF-103).
// 전에는 RETURNING 의 서브쿼리로 읽었는데, 그 서브쿼리는 문장 시작 스냅숏을 본다. 같은 대화에 고객 글이 몇 ms 앞서 들어와
// 이 UPDATE 가 그 커밋을 기다렸으면 UPDATE 자체는 커밋된 최신 행을 다시 읽어 고치지만 서브쿼리는 기다리기 전 값을 봐서
// "기다리던 고객에게 간 답" 이 아닌 것으로 읽혔다. FOR UPDATE 는 기다린 뒤 최신 행을 잠그고 그 행을 돌려준다 —
// 잠근 행의 값이 곧 커밋 순서의 바로 앞 값이다. 같은 까닭으로 다시 여는 메시지의 상태도 따로 읽지 않는다.
func allocate(ctx context.Context, tx pgx.Tx, p core.AppendParams) (allocation, error) {
	a := allocation{isPublic: p.Visibility == core.Public}

	// 수정·삭제 이벤트는 번호는 받되 "고객이 말했다 / 상담원이 답했다" 로 세지 않는다.
	counts := p.Kind == "" || p.Kind == core.KindMessage
	isCustomer := p.SenderKind == core.SenderCustomer && counts
	isAgent := p.SenderKind == core.SenderAgent && counts
	// 차례를 닫는 답: 상담원·AI 의 공개 메시지. 접수 안내(BOT)·시스템 줄·내부 메모는 아니다(D45).
	responds := p.SenderKind.Responds() && counts && a.isPublic
	var before core.State
	var wasWaiting bool
	var firstReplyBefore *time.Time
	var autoRepliedBefore *time.Time
	var closedBy core.ClosedBy
	err := tx.QueryRow(ctx, `
		WITH old AS (
			SELECT id, state, last_customer_seq, last_agent_public_seq, waiting_since, closed_by
			  FROM conversations
			 WHERE id = $1 AND deleted_at IS NULL
			   FOR UPDATE)
		UPDATE conversations c
		   SET last_seq          = c.last_seq + 1,
		       last_public_seq   = c.last_public_seq + CASE WHEN $3 THEN 1 ELSE 0 END,
		       last_customer_seq = CASE WHEN $4 THEN c.last_seq + 1 ELSE c.last_customer_seq END,
		       last_agent_public_seq = CASE WHEN $5 AND $3 THEN c.last_seq + 1 ELSE c.last_agent_public_seq END,
		       last_message_at   = CASE WHEN $6 THEN clock_timestamp() ELSE c.last_message_at END,
		       state             = CASE WHEN $2 AND c.state <> 'OPEN' THEN 'OPEN' ELSE c.state END,
		       updated_at        = CASE WHEN ($2 AND c.state <> 'OPEN') OR NOT $6 THEN clock_timestamp() ELSE c.updated_at END,
		       snoozed_until     = CASE WHEN $2 THEN NULL ELSE c.snoozed_until END,
		       first_agent_reply_at = CASE WHEN $5 AND c.first_agent_reply_at IS NULL
		                                   THEN now() ELSE c.first_agent_reply_at END,
		       waiting_since     = CASE WHEN $4 THEN COALESCE(c.waiting_since, clock_timestamp())
		                                WHEN $7 THEN NULL ELSE c.waiting_since END
		  FROM old
		 WHERE c.id = old.id
		RETURNING c.last_seq, c.last_public_seq,
		          CASE WHEN $5 THEN NULL ELSE c.first_agent_reply_at END,
		          c.auto_replied_at, c.workspace_id, old.state,
		          old.last_customer_seq > old.last_agent_public_seq,
		          CASE WHEN $7 THEN old.waiting_since END,
		          COALESCE(old.closed_by, '')`,
		p.ConversationID, p.Reopen, a.isPublic, isCustomer, isAgent, counts, responds,
	).Scan(&a.seq, &a.publicSeq, &firstReplyBefore, &autoRepliedBefore, &a.workspaceID, &before, &wasWaiting, &a.waitedFrom, &closedBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return a, core.ErrNotFound
	case err != nil:
		return a, fmt.Errorf("pgstore: 번호 발급 실패: %w", err)
	case isCustomer && before == core.StateClosed && closedBy == core.ClosedByCustomer:
		// 고객이 직접 닫아 둔 상담이다(D48). 고객의 새 글은 받지 않는다 — 다시 열지 않는다. 판정은 잠근 행의 바꾸기 전 값이라
		// 종료와 겹친 글도 커밋 순서대로 갈린다(종료가 먼저면 여기서 거절, 글이 먼저면 글이 남고 종료가 그 뒤다).
		// 이 UPDATE 의 번호 증가는 트랜잭션과 함께 되돌아간다(I1).
		return a, core.ErrClosedByCustomer
	}
	a.reopened = p.Reopen && before != core.StateOpen
	// 잠근 행의 바꾸기 전 값이다 — "이 답변이 기다리던 고객에게 간 것인가"(업무 기록의 응답, D27).
	a.answered = isAgent && a.isPublic && wasWaiting
	// 고객이 말했는데 상담원이 아직 한 번도 안 답했고 자동 응답도 안 나갔으면 보낼 차례다.
	a.needsAutoReply = isCustomer && firstReplyBefore == nil && autoRepliedBefore == nil
	return a, nil
}

// recordResponse 는 이 답이 닫은 차례 하나를 응답 기록으로 남긴다(D45). 메시지와 같은 트랜잭션이다 — 메시지가 남으면 기록도 남는다.
// 첫 응답인지는 그 상담의 앞선 응답 기록으로 정한다. 대화 행을 잠근 채라 같은 상담의 응답 기록은 커밋 순서대로 쌓이고,
// 상담마다 첫 응답이 하나뿐인 것은 부분 유일 인덱스가 한 번 더 지킨다.
func recordResponse(ctx context.Context, tx pgx.Tx, p core.AppendParams, a allocation, answeredAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO conversation_responses
		       (workspace_id, conversation_id, responder_kind, agent_id, waited_from, answered_at, first, message_seq)
		VALUES ($1, $2, $3, nullif($4::text, '')::uuid, $5, greatest($6::timestamptz, $5),
		        NOT EXISTS (SELECT 1 FROM conversation_responses r WHERE r.conversation_id = $2), $7)`,
		a.workspaceID, p.ConversationID, string(p.SenderKind), p.SenderAgentID, *a.waitedFrom, answeredAt, a.seq)
	if err != nil {
		return fmt.Errorf("pgstore: 응답 기록 실패: %w", err)
	}
	return nil
}

// insertMessage 는 3) 단계다. 첨부가 있으면 같은 트랜잭션에서 이어 붙인다.
func insertMessage(ctx context.Context, tx pgx.Tx, p core.AppendParams, a allocation) (core.Message, error) {
	msgID := id.New()
	var createdAt time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, seq, public_seq, id, client_msg_id, sender_kind,
		                      sender_agent_id, sender_customer, visibility, body, kind, ref_message_id)
		VALUES ($1, $2, $3, $4, $5, $6,
		        nullif($7::text, '')::uuid, nullif($8::text, '')::uuid, $9, $10, $11, nullif($12::text, '')::uuid)
		RETURNING created_at`,
		p.ConversationID, a.seq, publicSeqOrNil(a.isPublic, a.publicSeq), msgID, p.ClientMsgID,
		string(p.SenderKind), p.SenderAgentID, p.SenderCustomer, string(p.Visibility), p.Body,
		string(kindOrMessage(p.Kind)), p.RefMessageID,
	).Scan(&createdAt)
	switch {
	case isUniqueViolation(err, "messages_idempotency_idx"):
		return core.Message{}, errRetry
	case err != nil:
		return core.Message{}, fmt.Errorf("pgstore: 메시지 저장 실패: %w", err)
	}

	var attachment *core.Attachment
	if p.AttachmentID != "" {
		// 남의 대화 첨부, 남이 올린 첨부, 이미 쓴 첨부는 붙지 않는다.
		uploaderID := p.SenderCustomer
		if p.SenderKind == core.SenderAgent {
			uploaderID = p.SenderAgentID
		}
		if attachment, err = linkAttachment(ctx, tx, p.ConversationID, msgID, p.AttachmentID, string(p.SenderKind), uploaderID); err != nil {
			return core.Message{}, err
		}
	}

	name, err := senderName(ctx, tx, p)
	if err != nil {
		return core.Message{}, err
	}

	var publicSeq *int64
	if a.isPublic {
		publicSeq = &a.publicSeq
	}
	return core.Message{
		ConversationID: p.ConversationID,
		Kind:           kindOrMessage(p.Kind),
		RefMessageID:   p.RefMessageID,
		Seq:            senderSeq(p.SenderKind, a.seq, publicSeq),
		ID:             msgID,
		ClientMsgID:    p.ClientMsgID,
		SenderKind:     p.SenderKind,
		SenderID:       firstNonEmpty(p.SenderAgentID, p.SenderCustomer),
		SenderName:     name,
		Visibility:     p.Visibility,
		Body:           p.Body,
		Attachment:     attachment,
		CreatedAt:      createdAt,
	}, nil
}

// senderSeq 는 보낸 사람에게 돌려주는 번호다. 그 사람이 보는 번호 공간이어야 한다 —
// 고객은 공개 번호, 상담원은 전체 번호. 상담원의 공개 메시지에 고객 번호를 돌려주면
// 데스크의 커서가 고객 공간 값으로 오염돼 재접속 때 내부 메모를 건너뛴다.
func senderSeq(kind core.SenderKind, seq int64, publicSeq *int64) int64 {
	if kind == core.SenderCustomer && publicSeq != nil {
		return *publicSeq
	}
	return seq
}

func kindOrMessage(k core.MessageKind) core.MessageKind {
	if k == "" {
		return core.KindMessage
	}
	return k
}

func publicSeqOrNil(isPublic bool, seq int64) any {
	if isPublic {
		return seq
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// messageColumns 는 조회마다 같은 모양을 쓰려고 뽑아 둔 것이다.
// $CURSOR 자리에 보는 사람의 번호 컬럼이 들어간다.
const messageColumns = `
		m.$CURSOR, m.id, m.client_msg_id, m.sender_kind, m.visibility, m.body, m.created_at,
		COALESCE(a.name, c.display_name, CASE m.sender_kind WHEN 'AI' THEN 'AI 상담원' ELSE '시스템' END) AS sender_name,
		COALESCE(m.sender_agent_id::text, m.sender_customer::text, '') AS sender_id,
		f.id, f.filename, f.content_type, f.byte_size, f.width, f.height, f.created_at,
		m.kind, COALESCE(m.ref_message_id::text, ''), m.edited_at, m.deleted_at`

const messageJoins = `
		  FROM messages m
		  LEFT JOIN agents      a ON a.id = m.sender_agent_id
		  LEFT JOIN customers   c ON c.id = m.sender_customer
		  LEFT JOIN attachments f ON f.message_id = m.id`

// scanMessage 는 messageColumns 한 줄을 읽는다. extra 는 그 뒤에 덧붙인 컬럼의 목적지다.
func scanMessage(rows pgx.Rows, convID string, extra ...any) (core.Message, error) {
	m := core.Message{ConversationID: convID}
	var fileID, filename, contentType *string
	var byteSize *int64
	var width, height *int
	var fileCreatedAt *time.Time
	dest := append([]any{&m.Seq, &m.ID, &m.ClientMsgID, &m.SenderKind, &m.Visibility, &m.Body, &m.CreatedAt,
		&m.SenderName, &m.SenderID, &fileID, &filename, &contentType, &byteSize, &width, &height, &fileCreatedAt,
		&m.Kind, &m.RefMessageID, &m.EditedAt, &m.DeletedAt}, extra...)
	if err := rows.Scan(dest...); err != nil {
		return core.Message{}, err
	}
	// 삭제된 메시지의 첨부는 보이지 않는다.
	if fileID != nil && m.DeletedAt == nil {
		m.Attachment = &core.Attachment{
			ID: *fileID, ConversationID: convID, MessageID: m.ID,
			Filename: deref(filename), ContentType: deref(contentType),
			ByteSize: deref(byteSize), Width: deref(width), Height: deref(height), CreatedAt: deref(fileCreatedAt),
		}
	}
	return m, nil
}

// deref 는 NULL 가능 컬럼을 제로값으로 펴 준다. 타입마다 같은 함수를 세 번 쓰지 않는다.
func deref[T any](v *T) T {
	if v == nil {
		var zero T
		return zero
	}
	return *v
}

func findByClientMsgID(ctx context.Context, tx pgx.Tx, p core.AppendParams) (core.Message, error) {
	convID, clientMsgID := p.ConversationID, p.ClientMsgID
	var m core.Message
	m.ConversationID = convID
	var seq int64
	var publicSeq *int64
	var fileID, filename, contentType *string
	var byteSize *int64
	var width, height *int
	var fileCreatedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT m.seq, m.public_seq, m.id, m.client_msg_id, m.sender_kind, m.visibility, m.body, m.created_at,
		       COALESCE(a.name, c.display_name, CASE m.sender_kind WHEN 'AI' THEN 'AI 상담원' ELSE '시스템' END) AS sender_name,
		       COALESCE(m.sender_agent_id::text, m.sender_customer::text, '') AS sender_id,
		       f.id, f.filename, f.content_type, f.byte_size, f.width, f.height, f.created_at`+messageJoins+`
		 WHERE m.conversation_id = $1 AND m.client_msg_id = $2`,
		convID, clientMsgID,
	).Scan(&seq, &publicSeq, &m.ID, &m.ClientMsgID, &m.SenderKind, &m.Visibility, &m.Body, &m.CreatedAt,
		&m.SenderName, &m.SenderID, &fileID, &filename, &contentType, &byteSize, &width, &height, &fileCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Message{}, core.ErrNotFound
	}
	if err != nil {
		return core.Message{}, err
	}
	if m.SenderKind != p.SenderKind || m.SenderID != firstNonEmpty(p.SenderAgentID, p.SenderCustomer) {
		return core.Message{}, core.ErrClientMsgIDTaken
	}
	m.Seq = senderSeq(p.SenderKind, seq, publicSeq)
	m.Kind = core.KindMessage
	if fileID != nil {
		m.Attachment = &core.Attachment{
			ID: *fileID, ConversationID: convID, MessageID: m.ID,
			Filename: deref(filename), ContentType: deref(contentType),
			ByteSize: deref(byteSize), Width: deref(width), Height: deref(height), CreatedAt: deref(fileCreatedAt),
		}
	}
	return m, nil
}

func senderName(ctx context.Context, tx pgx.Tx, p core.AppendParams) (string, error) {
	switch p.SenderKind {
	case core.SenderAgent:
		var name string
		if err := tx.QueryRow(ctx, "SELECT name FROM agents WHERE id = $1", p.SenderAgentID).Scan(&name); err != nil {
			return "", err
		}
		return name, nil
	case core.SenderCustomer:
		var name string
		if err := tx.QueryRow(ctx, "SELECT display_name FROM customers WHERE id = $1", p.SenderCustomer).Scan(&name); err != nil {
			return "", err
		}
		return name, nil
	case core.SenderBot:
		return "자동 응답", nil
	case core.SenderAI:
		return AISenderName, nil
	default:
		return "시스템", nil
	}
}

// changedPayload 는 "이 대화가 여기까지 왔다" 를 알린다.
// 번호를 같이 싣는 이유는 받는 쪽이 다시 묻지 않게 하기 위해서다.
// 메시지 한 건마다 구독자가 대화 행을 되읽으면 그게 전체 처리량의 한계가 된다.
type changedPayload struct {
	ConversationID string `json:"conversation_id"`
	WorkspaceID    string `json:"ws,omitempty"`
	Seq            int64  `json:"seq"`
	PublicSeq      int64  `json:"public_seq,omitempty"`
	// Meta 가 true 면 상태·담당자·태그가 바뀌었다는 뜻이다. 그때만 대화 행을 다시 읽는다.
	Meta bool `json:"meta,omitempty"`
}

// changedNotice 는 커밋 뒤에 보낼 알림 본문이다.
func changedNotice(convID, wsID string, seq, publicSeq int64, meta bool) (string, error) {
	payload, err := json.Marshal(changedPayload{
		ConversationID: convID, WorkspaceID: wsID, Seq: seq, PublicSeq: publicSeq, Meta: meta,
	})
	return string(payload), err
}

// notifyTimeout 은 커밋 뒤 알림 한 번에 주는 시간이다. 넘기면 알림을 버린다 — 행은 이미 커밋됐고 안전망이 전달한다(D4).
const notifyTimeout = 5 * time.Second

// notify 는 커밋이 끝난 뒤 자동커밋 한 문장으로 알린다. 알림만 든 트랜잭션은 WAL 을 쓰지 않아
// 직렬화 구간이 마이크로초다. 요청이 취소돼도 보낸다 — 행은 이미 커밋됐다. 다만 notifyTimeout 까지만 기다린다.
// 실패해도 안전망이 메우므로 호출자에게 돌려주지 않고 notify.failed 한 줄에 대화 id 만 남긴다(D30).
func (s *Store) notify(ctx context.Context, payload string) {
	if payload == "" {
		return
	}
	var p changedPayload
	_ = json.Unmarshal([]byte(payload), &p)
	s.notifyAfterCommit(ctx, NotifyChannel, payload, p.ConversationID)
}

// notifyAfterCommit 은 커밋 뒤 알림 한 번이다. notifyTimeout 까지만 기다리고, 실패하면 notify.failed 에 대화 id 만 남긴다(D30).
// 읽음 갱신의 LiveChannel 알림(MarkRead)도 이 길로 보낸다.
func (s *Store) notifyAfterCommit(ctx context.Context, channel, payload, convID string) {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	if _, err := s.pool.Exec(nctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		reason := "error"
		if nctx.Err() != nil {
			reason = "timeout"
		}
		slog.WarnContext(logctx.With(ctx, "conversation_id", convID), "notify.failed", "reason", reason)
		telemetry.Metrics().NotifyFailed.Add(ctx, 1, metric.WithAttributes(telemetry.Reason(reason)))
	}
}

// ListMessages 는 보는 사람의 번호 기준으로 afterSeq 다음 메시지를 돌려준다.
// internal 이 false 면 내부 메모를 빼고 고객 번호로 센다. 그래야 고객 쪽 번호에 빈칸이 없다.
func (s *Store) ListMessages(ctx context.Context, convID string, afterSeq int64, limit int, internal bool) ([]core.Message, error) {
	limit = pageLimit(limit, core.MessagesPageDefault, core.MessagesPageMax)
	cursor, filter := "seq", ""
	if !internal {
		cursor, filter = "public_seq", " AND m.visibility = 'PUBLIC'"
	}
	query := "SELECT" + replaceCursor(messageColumns, cursor) + messageJoins +
		" WHERE m.conversation_id = $1 AND m." + cursor + " > $2" + filter +
		" ORDER BY m." + cursor + " LIMIT $3"

	rows, err := s.pool.Query(ctx, query, convID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.Message, 0, 16)
	for rows.Next() {
		m, err := scanMessage(rows, convID)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func replaceCursor(columns, cursor string) string {
	return strings.ReplaceAll(columns, "$CURSOR", cursor)
}

// MarkAutoReplied 는 자동 응답을 보냈다고 기록한다. 이미 기록돼 있으면 false 를 돌려줘
// 동시에 두 번 보내는 걸 막는다.
func (s *Store) MarkAutoReplied(ctx context.Context, convID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE conversations SET auto_replied_at = now() WHERE id = $1 AND auto_replied_at IS NULL", convID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MessageSeqs 는 검증용이다. 대화의 전체 번호를 오름차순으로 읽는다.
func (s *Store) MessageSeqs(ctx context.Context, convID string) ([]int64, error) {
	return s.seqColumn(ctx, convID, "seq", "")
}

// PublicSeqs 는 고객이 보는 번호만 읽는다.
func (s *Store) PublicSeqs(ctx context.Context, convID string) ([]int64, error) {
	return s.seqColumn(ctx, convID, "public_seq", " AND public_seq IS NOT NULL")
}

func (s *Store) seqColumn(ctx context.Context, convID, column, filter string) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+column+" FROM messages WHERE conversation_id = $1"+filter+" ORDER BY "+column, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}
