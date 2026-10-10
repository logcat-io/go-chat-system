package pgstore

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/auth"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 토큰 보안 묶음(D46)의 저장소 쪽이다: 세션 참조로 찾기, 사용 시각 갱신, WebSocket 티켓, 고객 세션 상한.
// 포트는 app.SessionStore 다(ports_session.go). 세션 행이 지워질 때의 알림은 트리거(마이그레이션 0018)가 낸다.

// SessionEndedChannel 은 세션 행이 지워졌을 때 트리거가 알리는 채널이다. 내용은 세션 참조(토큰 해시의 16진수)를 쉼표로 이은 것이다.
const SessionEndedChannel = "chat_session_ended"

// sessionRef 는 세션 해시를 참조 값으로 바꾼다. 평문 토큰이 아니라 되돌릴 수 없다.
func sessionRef(hash []byte) core.SessionRef { return core.SessionRef(hex.EncodeToString(hash)) }

// refHash 는 참조 값을 해시로 되돌린다. 꼴이 틀리면 없는 세션이다.
func refHash(ref core.SessionRef) ([]byte, error) {
	b, err := hex.DecodeString(string(ref))
	if err != nil || len(b) != 32 {
		return nil, core.ErrNotFound
	}
	return b, nil
}

// SubjectBySession 은 세션 참조로 주체를 찾는다. 티켓으로 붙은 WebSocket 이 세션을 다시 확인할 때 쓴다. 판정은 SubjectByToken 과 같다.
func (s *Store) SubjectBySession(ctx context.Context, ref core.SessionRef) (core.Subject, error) {
	hash, err := refHash(ref)
	if err != nil {
		return core.Subject{}, err
	}
	return s.subjectByHash(ctx, hash)
}

// TouchSession 은 세션의 마지막 사용 시각을 지금으로 올린다. 1분 안에 이미 올렸으면 쓰지 않는다 — 동시에 온 요청이 같은 행을 거듭 쓰지 않게.
// 유휴 한도가 지금 없는 세션도 적는다. 고객 유휴 한도는 판정 시점의 설정이라(감사 21) 나중에 켜도 그때까지 쓴 시각으로 재야 한다 —
// 적지 않으면 켜는 순간 어제 쓴 고객 세션도 만든 날부터 잰다.
func (s *Store) TouchSession(ctx context.Context, ref core.SessionRef) error {
	hash, err := refHash(ref)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE sessions SET last_used_at = now()
		 WHERE token_hash = $1 AND last_used_at < now() - interval '1 minute'`, hash)
	return err
}

// IssueWSTicket 은 그 세션에 묶인 일회용 티켓을 만든다. 평문은 이 응답에만 있고 저장은 해시다. 세션이 없으면 core.ErrNotFound.
func (s *Store) IssueWSTicket(ctx context.Context, ref core.SessionRef, ttl time.Duration) (string, time.Time, error) {
	hash, err := refHash(ref)
	if err != nil {
		return "", time.Time{}, err
	}
	plain, ticketHash := auth.NewToken()
	var expires time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ws_tickets (ticket_hash, session_hash, expires_at)
		SELECT $1, token_hash, now() + $3::interval FROM sessions WHERE token_hash = $2
		RETURNING expires_at`, ticketHash, hash, ttl.String()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, core.ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return plain, expires, nil
}

// RedeemWSTicket 은 티켓을 쓰고 그 세션의 주체를 돌려준다. 쓰는 것은 지우는 한 문장이라 같은 티켓을 동시에 두 번 써도 하나만 행을 얻는다.
// 없는 티켓, 이미 쓴 티켓, 만료된 티켓, 세션이 죽은 티켓은 모두 core.ErrNotFound 다. 만료된 것도 지운다 — 다시 쓸 수 없다.
func (s *Store) RedeemWSTicket(ctx context.Context, ticket string) (core.Subject, error) {
	var sessionHash []byte
	var live bool
	err := s.pool.QueryRow(ctx, `
		DELETE FROM ws_tickets WHERE ticket_hash = $1
		RETURNING session_hash, expires_at > now()`, auth.HashToken(ticket)).Scan(&sessionHash, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Subject{}, core.ErrNotFound
	}
	if err != nil {
		return core.Subject{}, err
	}
	if !live {
		return core.Subject{}, core.ErrNotFound
	}
	return s.subjectByHash(ctx, sessionHash)
}

// PruneCustomerSessions 는 고객 한 명의 세션을 최신 keep 개만 남기고 지운다. 지운 수를 돌려준다.
// 회원이 페이지마다 identify 해도, 새 회원 토큰을 되풀이해 들고 와도 세션 행이 끝없이 쌓이지 않게 한다.
func (s *Store) PruneCustomerSessions(ctx context.Context, customerID string, keep int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM sessions WHERE token_hash IN (
			SELECT token_hash FROM sessions WHERE customer_id = $1
			 ORDER BY created_at DESC, token_hash OFFSET $2)`, customerID, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
