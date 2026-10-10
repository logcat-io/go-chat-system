package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// CreateAttachment 는 업로드한 파일을 기록한다. 아직 메시지에 붙지 않은 상태다.
// 대화의 미전송 첨부가 maxPending 에 닿았으면 넣지 않고 ErrTooManyPending 이다. 삭제된 대화면 ErrNotFound 다.
//
// 세기와 넣기를 대화 단위 자문 잠금 안에서 한다. 잠금 없이 "세고 → 넣기" 를 하면 동시 요청이 모두 같은 수를 보고 상한을 넘긴다.
// INSERT … SELECT … WHERE count < 상한 한 문장으로도 못 막는다 — 각 문장의 스냅샷은 서로의 미커밋 행을 보지 못한다.
// 같은 트랜잭션에서 대화 행을 FOR SHARE 로 잡는다. 권한 검사 뒤에 커밋된 삭제는 여기서 걸리고, 뒤에 온 삭제는 이 넣기를 기다린다.
func (s *Store) CreateAttachment(ctx context.Context, a core.NewAttachment, maxPending int) (core.Attachment, error) {
	var createdAt time.Time
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPendingAttachments(ctx, tx, a.ConversationID); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (SELECT 1 FROM conversations WHERE id = $1 AND deleted_at IS NULL FOR SHARE) c`,
			a.ConversationID).Scan(&live); err != nil {
			return err
		}
		if live == 0 {
			return core.ErrNotFound
		}
		var pending int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM attachments WHERE conversation_id = $1 AND message_id IS NULL",
			a.ConversationID).Scan(&pending); err != nil {
			return err
		}
		if pending >= maxPending {
			return core.ErrTooManyPending
		}
		return tx.QueryRow(ctx, `
			INSERT INTO attachments (id, conversation_id, uploader_kind, uploader_id,
			                         filename, content_type, byte_size, storage_path, width, height)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, 0), nullif($10, 0))
			RETURNING created_at`,
			a.ID, a.ConversationID, a.UploaderKind, a.UploaderID,
			a.Filename, a.ContentType, a.ByteSize, a.StoragePath, a.Width, a.Height).Scan(&createdAt)
	})
	if err != nil {
		return core.Attachment{}, classify(err)
	}
	return core.Attachment{
		ID: a.ID, ConversationID: a.ConversationID, Filename: a.Filename,
		ContentType: a.ContentType, ByteSize: a.ByteSize, Width: a.Width, Height: a.Height, CreatedAt: createdAt,
	}, nil
}

// claimStatementTimeout 은 임대를 잡는 문장 하나에 주는 시간이다.
const claimStatementTimeout = 5 * time.Second

// ClaimAttachment 는 아직 확인되지 않은 첨부의 완료를 lease 동안 맡는다. 맡았으면 claimed 가 true 고 맡은 순간의 행을 준다.
// token 은 이 임대의 주인 표식이다. 도장(CompleteAttachment)과 해제(ReleaseAttachment)는 같은 token 을 낼 때만 걸린다.
// 이미 확인됐거나 다른 요청의 임대가 살아 있으면 맡지 않고 지금의 행을 claimed=false 로 준다. 행이 없으면 ErrNotFound 다.
//
// 세션 자문 잠금을 쓰지 않는 이유: 잠금을 기다리는 동안 커넥션을 쥐고 있어서, 동시 완료가 풀 크기에 닿으면 잠금 안의 작업이
// 쓸 커넥션이 남지 않아 서버 전체가 멈췄다. 임대는 짧은 UPDATE 한 문장이고 바로 커넥션을 돌려준다. 기다리는 쪽이 없다.
// 시각은 DB 시계(clock_timestamp)로만 잰다. 서버가 여러 대여도 같은 시계를 본다.
//
// 임대를 잡는 문장은 호출자의 취소·시한과 떼어 낸다. 문장이 커밋된 뒤 응답 앞에서 취소되면 호출자는 오류를 받고
// 임대는 남는다. 그러면 아무도 비우지 않아 다음 완료가 임대 시간 내내 busy 를 받는다. 떼어 내면 결과를 반드시 받는다.
func (s *Store) ClaimAttachment(ctx context.Context, attachmentID, token string, lease time.Duration) (core.AttachmentRef, bool, error) {
	stmt, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimStatementTimeout)
	defer cancel()
	var ref core.AttachmentRef
	err := s.pool.QueryRow(stmt, `
		UPDATE attachments
		   SET completing_until = clock_timestamp() + $2::interval,
		       completing_token = $3
		 WHERE id = $1 AND uploaded_at IS NULL
		   AND (completing_until IS NULL OR completing_until < clock_timestamp())
		RETURNING conversation_id, storage_path, filename, content_type, uploader_kind, uploader_id,
		          byte_size, COALESCE(width, 0), COALESCE(height, 0)`,
		attachmentID, lease.String(), token,
	).Scan(&ref.ConversationID, &ref.StoragePath, &ref.Filename, &ref.ContentType, &ref.UploaderKind, &ref.UploaderID,
		&ref.ByteSize, &ref.Width, &ref.Height)
	if err == nil {
		// 맡을 수 있는 행은 확인 전이라 메시지에 붙지 않았다. 아직 보내지 않은 업로드는 내부 취급이다.
		ref.Internal = true
		return ref, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return core.AttachmentRef{}, false, err
	}
	ref, err = s.AttachmentFile(ctx, attachmentID)
	return ref, false, err
}

// ReleaseAttachment 는 완료가 실패한 뒤 임대를 비운다. 다음 완료 요청이 임대가 끝나기를 기다리지 않고 바로 맡는다.
// token 이 지금 임대의 주인일 때만 비운다. 임대가 끝나 다른 요청이 이어받았으면 그 임대를 지우지 않는다.
// 확인된 행은 건드리지 않는다. released 는 이 호출이 임대를 비웠는지다 — false 면 이 token 의 도장은 이제 걸릴 수 없다
// (다른 주인이 있거나 이미 확인됐다).
func (s *Store) ReleaseAttachment(ctx context.Context, attachmentID, token string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE attachments SET completing_until = NULL, completing_token = NULL
		 WHERE id = $1 AND completing_token = $2 AND uploaded_at IS NULL`, attachmentID, token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteAttachment 는 저장소에 올라온 것을 확인한 첨부에 도장을 찍는다. 종류와 크기는 실제 내용으로 판정한 값이고,
// storagePath 는 확인한 파일을 옮겨 둔 확정 위치다. 도장을 찍으면 임대도 비운다. stamped 는 이 호출이 찍었는지다.
// token 이 지금 임대의 주인일 때만 찍는다. 찍지 못했을 때:
//   - 이미 확인된 행이면 아무것도 바꾸지 않고 그 행을 stamped=false 로 돌려준다. 완료 요청을 두 번 보내도 결과가 같고,
//     임대가 끝난 뒤 늦게 온 도장이 다른 요청이 확인한 내용을 덮지 않는다.
//   - 확인 전인데 임대의 주인이 바뀌었으면 ErrLeaseLost 다.
//   - 대화가 삭제됐거나 행이 없으면 ErrNotFound 다. 대화 행을 FOR SHARE 로 잡아 앞서 커밋된 삭제를 다시 보고 걸러 내고,
//     아직 커밋되지 않은 삭제는 끝나기를 기다린다.
func (s *Store) CompleteAttachment(ctx context.Context, attachmentID, token, storagePath, contentType string, width, height int) (core.Attachment, bool, error) {
	var a core.Attachment
	var w, h *int
	err := s.pool.QueryRow(ctx, `
		WITH c AS (
		  SELECT c.id FROM attachments a JOIN conversations c ON c.id = a.conversation_id
		   WHERE a.id = $1 AND c.deleted_at IS NULL
		     FOR SHARE OF c
		)
		UPDATE attachments
		   SET content_type     = $2,
		       width            = nullif($3, 0),
		       height           = nullif($4, 0),
		       storage_path     = $5,
		       uploaded_at      = clock_timestamp(),
		       completing_until = NULL,
		       completing_token = NULL
		  FROM c
		 WHERE attachments.id = $1 AND attachments.conversation_id = c.id AND attachments.uploaded_at IS NULL
		   AND attachments.completing_token = $6
		RETURNING attachments.id, attachments.conversation_id, attachments.filename, attachments.content_type,
		          attachments.byte_size, attachments.width, attachments.height, attachments.created_at`,
		attachmentID, contentType, width, height, storagePath, token,
	).Scan(&a.ID, &a.ConversationID, &a.Filename, &a.ContentType, &a.ByteSize, &w, &h, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.unstamped(ctx, attachmentID)
	}
	if err != nil {
		return core.Attachment{}, false, err
	}
	a.Width, a.Height = deref(w), deref(h)
	return a, true, nil
}

// unstamped 는 도장이 걸리지 않은 까닭을 가린다.
func (s *Store) unstamped(ctx context.Context, attachmentID string) (core.Attachment, bool, error) {
	a, err := s.ConfirmedAttachment(ctx, attachmentID)
	if err == nil {
		return a, false, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return core.Attachment{}, false, err
	}
	var pending bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM attachments a JOIN conversations c ON c.id = a.conversation_id
		                WHERE a.id = $1 AND a.uploaded_at IS NULL AND c.deleted_at IS NULL)`, attachmentID,
	).Scan(&pending); err != nil {
		return core.Attachment{}, false, err
	}
	if pending {
		return core.Attachment{}, false, core.ErrLeaseLost
	}
	return core.Attachment{}, false, core.ErrNotFound
}

// ConfirmedAttachment 는 이미 확인된 첨부를 돌려준다. 확인 전이거나 대화가 삭제됐으면 ErrNotFound 다.
func (s *Store) ConfirmedAttachment(ctx context.Context, attachmentID string) (core.Attachment, error) {
	var a core.Attachment
	var w, h *int
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.conversation_id, a.filename, a.content_type, a.byte_size, a.width, a.height, a.created_at
		  FROM attachments a JOIN conversations c ON c.id = a.conversation_id
		 WHERE a.id = $1 AND a.uploaded_at IS NOT NULL AND c.deleted_at IS NULL`, attachmentID,
	).Scan(&a.ID, &a.ConversationID, &a.Filename, &a.ContentType, &a.ByteSize, &w, &h, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Attachment{}, core.ErrNotFound
	}
	if err != nil {
		return core.Attachment{}, err
	}
	a.Width, a.Height = deref(w), deref(h)
	return a, nil
}

// linkAttachment 는 업로드해 둔 파일을 방금 만든 메시지에 건다.
// 남의 대화 첨부, 이 메시지를 보내는 사람이 올리지 않은 첨부, 이미 다른 메시지가 쓴 첨부면 아무 행도 안 걸려서 거절된다.
// 올린 사람을 보지 않으면 상담원이 올려 두고 안 보낸 첨부를 같은 대화의 고객이 id 만 알아 자기 메시지에 붙일 수 있다.
func linkAttachment(ctx context.Context, tx pgx.Tx, convID, messageID, attachmentID, uploaderKind, uploaderID string) (*core.Attachment, error) {
	if uploaderID == "" {
		return nil, core.ErrNotFound // 시스템·봇 메시지는 첨부를 붙이지 않는다
	}
	var a core.Attachment
	var width, height *int
	err := tx.QueryRow(ctx, `
		UPDATE attachments
		   SET message_id = $1
		 WHERE id = $2 AND conversation_id = $3 AND message_id IS NULL AND uploaded_at IS NOT NULL
		   AND uploader_kind = $4 AND uploader_id = $5
		RETURNING id, filename, content_type, byte_size, width, height, created_at`,
		messageID, attachmentID, convID, uploaderKind, uploaderID,
	).Scan(&a.ID, &a.Filename, &a.ContentType, &a.ByteSize, &width, &height, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.ConversationID = convID
	a.MessageID = messageID
	a.Width, a.Height = deref(width), deref(height)
	return &a, nil
}

func (s *Store) AttachmentFile(ctx context.Context, attachmentID string) (core.AttachmentRef, error) {
	var ref core.AttachmentRef
	var visibility *string
	err := s.pool.QueryRow(ctx, `
		SELECT a.conversation_id, a.storage_path, a.filename, a.content_type,
		       a.uploader_kind, a.uploader_id, m.visibility,
		       a.byte_size, COALESCE(a.width, 0), COALESCE(a.height, 0), a.uploaded_at IS NOT NULL
		  FROM attachments a
		  LEFT JOIN messages m ON m.id = a.message_id
		 WHERE a.id = $1 AND (m.id IS NULL OR m.deleted_at IS NULL)`, attachmentID,
	).Scan(&ref.ConversationID, &ref.StoragePath, &ref.Filename, &ref.ContentType,
		&ref.UploaderKind, &ref.UploaderID, &visibility,
		&ref.ByteSize, &ref.Width, &ref.Height, &ref.Uploaded)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.AttachmentRef{}, core.ErrNotFound
	}
	if err != nil {
		return core.AttachmentRef{}, err
	}
	// 아직 메시지에 안 붙은 업로드(visibility 가 NULL)도 내부 취급한다.
	ref.Internal = visibility == nil || *visibility == string(core.Private)
	return ref, nil
}

// ReapOrphanAttachments 는 메시지에 붙지 못한 채 오래된 업로드 행을 지우고 그 파일 경로를 돌려준다.
// 행을 먼저 지우는 이유: "고르고 → 파일 삭제 → 행 삭제" 로 나누면 그 사이에 메시지가 첨부를 붙여
// 커밋할 수 있고, 그러면 ack 받은 메시지의 파일이 사라진다. 한 문장으로 선점하면 붙은 행은 안 걸린다.
// 파일 삭제가 실패하면 파일이 남는다 — 행이 남고 파일이 없는 것보다 낫다.
func (s *Store) ReapOrphanAttachments(ctx context.Context, olderThan time.Duration, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		DELETE FROM attachments
		 WHERE id IN (SELECT id FROM attachments
		               WHERE message_id IS NULL AND created_at < now() - $1::interval
		               LIMIT $2)
		   AND message_id IS NULL
		RETURNING storage_path`, olderThan.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
