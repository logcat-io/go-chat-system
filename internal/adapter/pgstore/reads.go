package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// LiveChannel 은 휘발성 이벤트가 흐르는 채널이다. 타이핑과 읽음 갱신이 여기로 간다.
// NotifyChannel 과 나눈 이유는 받는 쪽 처리가 다르기 때문이다.
// NotifyChannel 은 "DB 를 다시 읽어라", LiveChannel 은 "이 사실을 그대로 전해라"다.
const LiveChannel = "chat_live"

// PublishLive 는 휘발성 이벤트를 뿌린다. DB 에 아무것도 쓰지 않는다.
func (s *Store) PublishLive(ctx context.Context, ev core.LiveEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, "SELECT pg_notify($1, $2)", LiveChannel, string(payload))
	return err
}

// MarkRead 는 읽음 지점을 올린다. 뒤로 가는 갱신은 GREATEST 가 무시한다 (불변식 I7).
// seq 는 보고한 쪽의 번호 공간이다. internal 이 false 면 공개 번호로 받아 전체 번호로 옮긴다.
// 돌려주는 값은 갱신 후의 읽음 지점을, 보고한 쪽의 번호 공간으로 되돌린 것이다. 삭제된 대화면 ErrNotFound 다.
func (s *Store) MarkRead(ctx context.Context, convID string, ev core.LiveEvent, seq int64, internal bool) (int64, error) {
	globalSeq := seq
	if !internal && seq > 0 {
		var mapped *int64
		err := s.pool.QueryRow(ctx, `
			SELECT max(seq) FROM messages
			 WHERE conversation_id = $1 AND public_seq IS NOT NULL AND public_seq <= $2`,
			convID, seq).Scan(&mapped)
		if err != nil {
			return 0, err
		}
		if mapped == nil {
			return 0, nil // 아직 공개 메시지가 없다. 올릴 것이 없다
		}
		globalSeq = *mapped
	}
	stored, err := s.markReadGlobal(ctx, convID, ev, globalSeq)
	if err != nil {
		return 0, err
	}
	if internal {
		return stored, nil
	}
	return s.publicSeqAtOrBefore(ctx, convID, stored)
}

// publicSeqAtOrBefore 는 전체 번호를 고객 번호로 되돌린다.
func (s *Store) publicSeqAtOrBefore(ctx context.Context, convID string, globalSeq int64) (int64, error) {
	var mapped *int64
	err := s.pool.QueryRow(ctx, `
		SELECT max(public_seq) FROM messages
		 WHERE conversation_id = $1 AND public_seq IS NOT NULL AND seq <= $2`,
		convID, globalSeq).Scan(&mapped)
	if err != nil {
		return 0, err
	}
	if mapped == nil {
		return 0, nil
	}
	return *mapped, nil
}

func (s *Store) markReadGlobal(ctx context.Context, convID string, ev core.LiveEvent, seq int64) (int64, error) {
	var column string
	switch ev.ActorKind {
	case string(core.SenderAgent):
		column = "reader_agent_id"
	case string(core.SenderCustomer):
		column = "reader_customer"
	default:
		return 0, fmt.Errorf("%w: 읽음 주체 %q", core.ErrInvalidReference, ev.ActorKind)
	}

	// 충돌 대상이 kind 마다 다른 부분 인덱스라서 문장을 둘로 나눈다.
	stmt := fmt.Sprintf(`
		INSERT INTO conversation_reads (conversation_id, reader_kind, %s, last_read_seq)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (conversation_id, %s) WHERE %s IS NOT NULL
		DO UPDATE SET last_read_seq = GREATEST(conversation_reads.last_read_seq, excluded.last_read_seq),
		              updated_at    = now()
		RETURNING last_read_seq`, column, column, column)

	var stored int64
	var live string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 삭제된 대화에는 읽음을 남기지 않는다. 권한 검사 뒤에 삭제가 끼어들 수 있어서, 대화 행을 FOR SHARE 로 잡고
		// deleted_at 을 다시 본다. 앞서 커밋된 삭제는 여기서 걸리고, 뒤에 온 삭제는 이 트랜잭션을 기다린다.
		var alive int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (SELECT 1 FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR SHARE) c`,
			convID).Scan(&alive); err != nil {
			return err
		}
		if alive == 0 {
			return core.ErrNotFound
		}
		if err := tx.QueryRow(ctx, stmt, convID, ev.ActorKind, ev.ActorID, seq).Scan(&stored); err != nil {
			return err
		}
		if stored != seq {
			// 뒤처진 보고였다. 알릴 것이 없다.
			return nil
		}
		var publicSeq *int64
		if err := tx.QueryRow(ctx, `
			SELECT max(public_seq) FROM messages
			 WHERE conversation_id = $1 AND public_seq IS NOT NULL AND seq <= $2`,
			convID, stored).Scan(&publicSeq); err != nil {
			return err
		}
		payload, err := json.Marshal(core.LiveEvent{
			Kind: core.LiveRead, ConversationID: convID, WorkspaceID: ev.WorkspaceID,
			ActorKind: ev.ActorKind, ActorID: ev.ActorID, ActorName: ev.ActorName,
			Seq: stored, PublicSeq: deref(publicSeq),
		})
		if err != nil {
			return err
		}
		live = string(payload)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if live != "" {
		// 커밋 뒤에 보낸다. 이유와 시한(notifyTimeout)은 messages.go 의 notify 와 같다. 읽음은 놓쳐도 재접속 때 ready 가 맞춘다.
		s.notifyAfterCommit(ctx, LiveChannel, live, convID)
	}
	return stored, nil
}

// ReadStates 는 대화의 모든 읽음 지점이다. 재접속 때 ready 프레임에 실어 보낸다.
func (s *Store) ReadStates(ctx context.Context, convID string) ([]core.ReadState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.reader_kind,
		       COALESCE(r.reader_agent_id, r.reader_customer)::text,
		       COALESCE(a.name, c.display_name, '') AS reader_name,
		       r.last_read_seq,
		       COALESCE((SELECT max(m.public_seq) FROM messages m
		                  WHERE m.conversation_id = r.conversation_id
		                    AND m.public_seq IS NOT NULL AND m.seq <= r.last_read_seq), 0),
		       r.updated_at
		  FROM conversation_reads r
		  LEFT JOIN agents    a ON a.id = r.reader_agent_id
		  LEFT JOIN customers c ON c.id = r.reader_customer
		 WHERE r.conversation_id = $1
		 ORDER BY r.reader_kind`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.ReadState, 0, 2)
	for rows.Next() {
		var rs core.ReadState
		if err := rows.Scan(&rs.ReaderKind, &rs.ReaderID, &rs.ReaderName,
			&rs.LastReadSeq, &rs.LastReadPublicSeq, &rs.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// ReadSeqOf 는 한 주체의 읽음 지점이다. 없으면 0 이다.
func (s *Store) ReadSeqOf(ctx context.Context, convID, readerKind, readerID string) (int64, error) {
	column := "reader_customer"
	if readerKind == string(core.SenderAgent) {
		column = "reader_agent_id"
	}
	var seq int64
	err := s.pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT last_read_seq FROM conversation_reads WHERE conversation_id = $1 AND %s = $2", column,
	), convID, readerID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}
