package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 첨부는 세 걸음이다: 주소 발급 → 브라우저가 저장소로 직접 올림 → 완료 확인.
// 서버는 파일 바이트를 받지 않는다. 대신 올라온 것이 신청한 것과 같은지 확인한 뒤에만 메시지에 붙일 수 있게 한다.
// 업로드와 전송을 나눈 이유는 그대로다 — 메시지 전송은 같은 client_msg_id 로 몇 번이고 다시 보낼 수 있어야 한다.

var (
	ErrFileTooLarge   = core.NewError("too_large", "app: 신청한 파일 크기가 첨부 한도를 넘는다")
	ErrTooManyPending = core.NewError("too_many", "app: 이 대화에 보내지 않은 첨부가 상한만큼 있어 새 첨부를 받지 않았다")
	ErrNoFileStore    = core.NewError("no_file_store", "app: 첨부 저장소를 꽂지 않았다(UseFiles 를 부르지 않았다)")
	// ErrUploadBusy 는 같은 첨부를 다른 요청이 확인하는 중이라는 뜻이다. 기다리지 않고 바로 답한다. 잠시 뒤 다시 부르면 된다.
	ErrUploadBusy = core.NewError("busy", "app: 다른 완료 요청이 이 첨부를 확인하는 중이라 기다리지 않고 돌려보냈다")
	// ErrUploadLeaseLost 는 이 요청이 확인하던 중 임대 시간이 지나 다른 요청이 첨부를 넘겨받았다는 뜻이다(core.ErrLeaseLost).
	// 응답은 busy 와 같다 — 다시 부르면 넘겨받은 쪽이 확인을 끝냈을 때 그 결과를 받는다. 로그만 lease_lost 로 가른다(D47).
	ErrUploadLeaseLost = core.WrapError(ErrUploadBusy, "lease_lost", "app: 확인하는 동안 완료 처리 시간(임대)이 지나 다른 요청이 첨부를 넘겨받았다. 이 요청의 결과는 버렸다")
	// ErrStorageTimeout 은 첨부 완료가 작업 시한 안에 끝나지 않았다는 뜻이다. 서버 잘못이 아니라 저장소가 느린 것이다.
	// 같은 첨부의 완료를 다시 불러도 되지 않을 수 있다. 확정 자리로 옮긴 뒤에 시한을 넘기면 임시 파일은 이미 없어
	// 다음 완료가 "파일을 찾지 못했다"(400)를 받는다. 그래서 파일을 다시 올리라고 답한다.
	ErrStorageTimeout = core.NewError("storage_timeout", "app: 첨부 저장소가 작업 시한 안에 답하지 않았다")
)

// fileMissing 은 완료할 임시 파일이 없을 때의 문구다. 올린 적이 없는 경우와, 앞선 완료가 옮긴 뒤 실패해 사라진 경우가 같은 답을 받는다.
const fileMissing = "파일을 찾지 못했습니다. 다시 올려 주세요"

const (
	uploadTTL   = 10 * time.Minute
	downloadTTL = 5 * time.Minute
	maxImageDim = 20000
	// attachmentLease 는 완료 요청 하나가 첨부를 맡는 시간이다. 처리 중에 프로세스가 죽거나 멈추면 이 시간이 지난 뒤 다른 요청이 이어받는다.
	attachmentLease = 30 * time.Second
	// claimedWork 는 임대를 잡은 요청의 작업 시한이다. 저장소 확인·옮기기·재확인·도장·정리를 모두 이 안에 끝낸다.
	// 한 덩어리로 묶어 임대보다 짧게 둔다. 단계마다 따로 시간을 주면 합이 임대를 넘어, 다른 요청이 이어받은 뒤에도 옛 요청이 일한다.
	claimedWork = 20 * time.Second
	// releaseStep 은 임대를 비우는 DB 한 걸음의 시간이다. 비우기는 token 조건이라 언제 닿아도 다른 요청의 임대를 지우지 않는다.
	// 그래서 작업 시한이 끝난 뒤에도 비울 수 있게 따로 둔다.
	releaseStep = 5 * time.Second
)

// DefaultMaxUploadBytes 는 첨부 한 건의 기본 한도(100MB)다. 휴대폰으로 찍은 1~2분 영상이 들어가는 크기다(D37).
// 설정(CHAT_MAX_UPLOAD_MB)의 기본값과 같다.
const DefaultMaxUploadBytes = 100 << 20

// UploadLimits 는 첨부의 한도다. 0 이면 바닥값을 쓴다 — 설정 하나를 빠뜨렸다고 업로드가 통째로 막히면 안 된다.
type UploadLimits struct {
	MaxBytes   int64
	MaxPending int
	// Lease 는 완료 임대 시간, Work 는 임대를 잡은 뒤의 작업 시한이다. 0 이면 attachmentLease·claimedWork 다.
	// 시한 초과를 짧게 재현하는 테스트가 줄인다. Work 가 Lease 보다 짧지 않으면 Lease 의 2/3 로 줄인다.
	Lease time.Duration
	Work  time.Duration
}

func (l UploadLimits) normalized() UploadLimits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxUploadBytes
	}
	if l.MaxPending <= 0 {
		l.MaxPending = 10
	}
	if l.Lease <= 0 {
		l.Lease = attachmentLease
	}
	if l.Work <= 0 {
		l.Work = claimedWork
	}
	if l.Work >= l.Lease {
		l.Work = l.Lease * 2 / 3
	}
	return l
}

// UploadRequest 는 브라우저가 올리겠다고 신청하는 내용이다. 종류와 이미지 크기는 참고값이고, 완료 때 실제 내용으로 다시 판정한다.
type UploadRequest struct {
	Filename    string
	ContentType string
	ByteSize    int64
	Width       int
	Height      int
}

// UseFiles 는 첨부 저장소를 꽂는다. 조립부(cmd/server)가 한 번 부른다.
func (a *App) UseFiles(files FileStore, limits UploadLimits) {
	a.Commands.files, a.Commands.limits = files, limits.normalized()
	a.Queries.files = files
}

// UploadLimits 는 지금 적용되는 한도다. 전달 계층이 안내 문구에 쓴다.
func (s *Commands) UploadLimits() UploadLimits { return s.limits.normalized() }

// StartUpload 는 첨부 시작과 그 사건 로그다. 파일 이름과 서명된 주소·양식 필드는 싣지 않는다.
func (s *Commands) StartUpload(ctx context.Context, actor Actor, convID string, req UploadRequest) (core.Attachment, core.UploadForm, error) {
	att, form, err := s.startUpload(ctx, actor, convID, req)
	if err != nil {
		logRejected(ctx, "upload.rejected", err, "step", "start", "byte_size", req.ByteSize)
		return att, form, err
	}
	slog.InfoContext(ctx, "upload.started", "conversation_id", att.ConversationID, "attachment_id", att.ID,
		"byte_size", att.ByteSize, "content_type", declaredClass(att.ContentType))
	return att, form, nil
}

// startUpload 는 첨부 행을 "주소만 발급된" 상태로 만들고 올릴 양식을 돌려준다.
func (s *Commands) startUpload(ctx context.Context, actor Actor, convID string, req UploadRequest) (core.Attachment, core.UploadForm, error) {
	if s.files == nil {
		return core.Attachment{}, core.UploadForm{}, ErrNoFileStore
	}
	conv, err := s.q.Authorize(ctx, actor, convID)
	if err != nil {
		return core.Attachment{}, core.UploadForm{}, err
	}
	limits := s.limits.normalized()
	if strings.TrimSpace(req.Filename) == "" {
		return core.Attachment{}, core.UploadForm{}, invalid("filename_required", "파일 이름이 없습니다")
	}
	switch {
	case req.ByteSize <= 0:
		return core.Attachment{}, core.UploadForm{}, invalid("file_empty", "빈 파일은 올릴 수 없습니다")
	case req.ByteSize > limits.MaxBytes:
		return core.Attachment{}, core.UploadForm{}, ErrFileTooLarge
	}
	contentType := declaredType(req.ContentType)
	attID := id.New()
	// 양식은 임시 위치에만 쓸 수 있다. 확인한 파일은 확정 위치로 옮기므로, 양식이 살아 있는 동안 다시 올려도 확정 파일은 그대로다.
	key := tempKey(attID)
	// 저장소에 주는 크기는 신청한 크기다. 저장소가 이보다 큰 파일을 막지 못해도(서명된 Content-Length 를 지키지 않는 PUT 저장소 등) 완료 확인이 크기를 다시 본다(D42).
	form, err := s.files.PresignUpload(key, contentType, req.ByteSize, uploadTTL)
	if err != nil {
		return core.Attachment{}, core.UploadForm{}, err
	}
	// 붙이지 않은 업로드가 쌓이면 저장소가 요청 한도보다 먼저 찬다. 상한은 저장소가 세기와 넣기를 묶어서 지킨다.
	att, err := s.write.CreateAttachment(ctx, core.NewAttachment{
		ID: attID, ConversationID: conv.ID,
		UploaderKind: uploaderKind(actor), UploaderID: uploaderID(actor),
		Filename: core.SafeFilename(req.Filename), ContentType: contentType, ByteSize: req.ByteSize,
		StoragePath: key, Width: clampDim(req.Width), Height: clampDim(req.Height),
	}, limits.MaxPending)
	if err != nil {
		return core.Attachment{}, core.UploadForm{}, translate(err)
	}
	return att, form, nil
}

// CompleteUpload 는 첨부 완료 확인과 그 사건 로그다. content_type 은 실제 내용으로 판정한 값이다.
func (s *Commands) CompleteUpload(ctx context.Context, actor Actor, attachmentID string) (core.Attachment, error) {
	// 완료 요청의 주소에는 대화 id 가 없다(/api/attachments/{id}/complete). 첨부 행을 읽은 뒤에 알게 된 대화 id 를 거절 로그에도 싣는다 —
	// 없으면 upload.rejected 를 conversation_id 로 거를 수 없다.
	var convID string
	att, err := s.completeUpload(ctx, actor, attachmentID, &convID)
	if err != nil {
		// 주소의 id 는 형식 검사 전이라 id 모양일 때만 싣는다(D30, 감사 23).
		args := append([]any{"step", "complete"}, idArgs("attachment_id", attachmentID)...)
		if convID != "" {
			args = append(args, "conversation_id", convID)
		}
		logRejected(ctx, "upload.rejected", err, args...)
		return att, err
	}
	slog.InfoContext(ctx, "upload.completed", "conversation_id", att.ConversationID, "attachment_id", att.ID,
		"byte_size", att.ByteSize, "content_type", att.ContentType)
	return att, nil
}

// completeUpload 는 저장소에 올라온 것을 확인한다. 크기가 신청과 같아야 하고, 종류는 실제 내용으로 판정한다.
// 확인한 파일은 임시 위치에서 확정 위치로 옮긴다. 올린 본인만 완료할 수 있다. 이미 확인된 첨부면 그대로 돌려준다.
//
// 판정부터 도장까지는 첨부를 임대로 맡은 요청 하나만 한다. 아무도 맡지 않고 두 완료 요청이 겹치면, 먼저 끝난 요청이
// 확인·전송한 뒤에 늦은 요청이 그 사이 같은 양식으로 다시 올린 임시 파일을 옮겨 내용을 바꾼다.
// 맡지 못한 요청은 기다리지 않고 ErrUploadBusy 로 답한다. 기다리는 동안 무엇이든(커넥션, 고루틴) 쥐고 있으면
// 같은 첨부에 몰린 요청이 서버 자원을 다 쥔다.
//
// 임대는 시간으로 끝나므로 멈췄던 옛 주인이 임대가 끝난 뒤 깨어날 수 있다. 그래서 임대마다 새 token 을 쓰고, 도장·해제는
// token 이 맞을 때만 걸리며, 확정 키에도 token 이 들어간다. 옛 주인이 늦게 옮기거나 지우는 것은 자기 확정 키뿐이다.
func (s *Commands) completeUpload(ctx context.Context, actor Actor, attachmentID string, convID *string) (core.Attachment, error) {
	if s.files == nil {
		return core.Attachment{}, ErrNoFileStore
	}
	attachmentID, err := id.Normalize(attachmentID)
	if err != nil {
		return core.Attachment{}, invalid("attachment_id_format", "attachment_id 형식이 올바르지 않습니다")
	}
	ref, err := s.read.AttachmentFile(ctx, attachmentID)
	if err != nil {
		return core.Attachment{}, translate(err)
	}
	*convID = ref.ConversationID
	// 아래 정리 로그(upload.release_failed·upload.cleanup_failed)도 대화로 거를 수 있게 얹는다.
	ctx = logctx.With(ctx, "conversation_id", ref.ConversationID)
	conv, err := s.q.Authorize(ctx, actor, ref.ConversationID)
	if err != nil {
		return core.Attachment{}, err
	}
	if ref.UploaderKind != uploaderKind(actor) || ref.UploaderID != uploaderID(actor) {
		return core.Attachment{}, ErrForbidden
	}
	limits := s.limits.normalized()
	// 작업 시한은 임대를 잡기 전에 건다. 임대는 DB 가 문장을 실행한 때부터 재므로 시한은 언제나 임대보다 먼저 끝난다.
	// 요청 취소와는 떼어 낸다. 옮긴 뒤의 단계는 클라이언트가 끊어도 끝까지 간다.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), limits.Work)
	defer cancel()
	token := id.New()
	ref, claimed, err := s.write.ClaimAttachment(work, attachmentID, token, limits.Lease)
	if err != nil {
		// 임대가 커밋됐는데 응답만 잃었을 수 있다. 비우기는 token 조건이라 맡지 못했으면 아무것도 바꾸지 않는다.
		s.release(ctx, attachmentID, token)
		return core.Attachment{}, translate(err)
	}
	if !claimed {
		if !ref.Uploaded {
			return core.Attachment{}, ErrUploadBusy
		}
		// 앞선 요청이 확인을 끝냈다. 다시 옮기지 않는다 — 임시 위치에 있는 것은 확인 뒤에 다시 올린 다른 내용이다.
		att, err := s.read.ConfirmedAttachment(ctx, attachmentID)
		return att, translate(err)
	}
	stamped := false
	defer func() {
		if !stamped {
			// 실패한 요청이 임대를 쥔 채 끝나면 다음 완료가 임대 시간만큼 busy 를 받는다. 요청이 취소돼도 비운다.
			s.release(ctx, attachmentID, token)
		}
	}()
	if err := ctx.Err(); err != nil {
		// 맡는 사이 클라이언트가 끊었다. 일을 시작하지 않고 임대를 비운다.
		return core.Attachment{}, err
	}
	att, stamped, err := s.confirmUpload(ctx, work, conv, attachmentID, token, ref)
	return att, storageTimeout(work, err)
}

// confirmUpload 는 임대를 쥔 요청의 확인이다. ref 는 임대를 잡은 순간의 행이다.
// stamped 는 이 요청의 도장이 찍혔는지다. 그러면 임대를 비울 필요가 없다(도장이 비웠다).
//
// 판정까지는 요청 취소를 따른다. 옮기기부터는 따르지 않는다 — 올리고 옮긴 파일이 클라이언트가 끊었다는 이유로
// 사라지면 안 된다. S3 의 옮기기는 복사 뒤 원본 삭제라서, 도중에 끊기면 원본은 지워졌는데 확정 파일은 정리되는 일이 생긴다.
// 어느 단계든 work 의 시한은 지킨다.
func (s *Commands) confirmUpload(ctx, work context.Context, conv core.Conversation, attachmentID, token string, ref core.AttachmentRef) (att core.Attachment, stamped bool, err error) {
	judge, stop := withCancelOf(work, ctx)
	info, err := s.files.Stat(judge, ref.StoragePath)
	stop()
	if errors.Is(err, core.ErrNotFound) {
		return core.Attachment{}, false, invalid("file_missing", fileMissing)
	}
	if err != nil {
		return core.Attachment{}, false, err
	}
	if info.Size != ref.ByteSize {
		// 신청한 것과 다른 파일이다. 남기지 않는다. 행은 고아 정리가 치운다.
		// 저장소가 크기를 지키든 못 지키든 언제나 본다(D42). 신청한 크기는 한도 이하라 한도를 넘는 파일도 여기서 걸린다.
		s.RemoveFiles(work, []string{ref.StoragePath})
		return core.Attachment{}, false, invalid("size_mismatch", "올라온 파일의 크기가 처음 알려 준 크기와 다릅니다. 파일을 다시 올려 주세요")
	}
	contentType, width, height := inspect(info.Head, ref.Width, ref.Height)
	if err := ctx.Err(); err != nil {
		return core.Attachment{}, false, err
	}
	// 확정 키에 워크스페이스·대화와 이 임대의 token 이 들어간다. 워크스페이스·대화는 테넌트 단위로 지우거나 옮길 때 접두사로 고른다.
	// token 은 임대마다 확정 자리를 나눈다. 임대가 끝난 옛 주인과 이어받은 새 주인이 같은 자리를 쓰면, 옛 주인의 늦은 삭제·덮어쓰기가
	// 새 주인이 확인·전송한 파일을 깬다(감사에서 재현했다). 자리가 다르면 각자 자기 파일만 옮기고 지운다.
	final := finalKey(conv.WorkspaceID, conv.ID, attachmentID, token)
	if err := s.files.Promote(work, ref.StoragePath, final); err != nil {
		// 옮기다 끊기면 확정 자리에 사본이 남을 수 있다. 이 임대의 자리라 지워도 남의 파일에 닿지 않는다.
		s.removeOwn(work, attachmentID, final)
		if errors.Is(err, core.ErrNotFound) {
			// 판정과 옮기기 사이에 임시 파일이 사라졌다(청소, 또는 임대가 끝난 다른 요청이 옮겨 갔다).
			return core.Attachment{}, false, invalid("file_missing", fileMissing)
		}
		return core.Attachment{}, false, err
	}
	// 판정한 것과 옮긴 것이 같은지 다시 본다. 양식은 판정과 옮기기 사이에도 임시 위치에 쓸 수 있다.
	// 임대는 다른 완료 요청만 막지 브라우저의 재업로드는 막지 못한다.
	moved, err := s.files.Stat(work, final)
	if err != nil {
		s.removeOwn(work, attachmentID, final)
		return core.Attachment{}, false, err
	}
	if moved.Size != info.Size || !bytes.Equal(moved.Head, info.Head) {
		s.removeOwn(work, attachmentID, final)
		return core.Attachment{}, false, invalid("file_changed", "확인하는 동안 파일이 바뀌었습니다. 파일을 다시 올려 주세요")
	}
	att, stamped, err = s.write.CompleteAttachment(work, attachmentID, token, final, contentType, width, height)
	switch {
	case err == nil && stamped:
		return att, true, nil
	case err == nil:
		// 임대가 끝난 사이 다른 요청이 확인을 끝냈다. 이 요청이 옮긴 파일은 아무도 가리키지 않는다.
		s.removeOwn(work, attachmentID, final)
		return att, false, nil
	case errors.Is(err, core.ErrLeaseLost), errors.Is(err, core.ErrNotFound):
		// 도장이 걸리지 않았다고 DB 가 답했다. 이 token 의 도장은 앞으로도 걸리지 않으니 자기 파일을 지운다.
		s.removeOwn(work, attachmentID, final)
		return core.Attachment{}, false, translate(err)
	}
	return s.settle(work, attachmentID, token, final, err)
}

// settle 은 결과를 모르는 도장 뒤의 정리다. 도장이 커밋되고 응답만 못 받았을 수 있다.
// 먼저 이 token 의 임대를 비운다. 비웠으면 도장은 커밋되지 않았고 앞으로도 걸리지 않는다 — 확정 파일을 지운다.
// 못 비웠으면 이 token 은 이미 주인이 아니다. 행이 이 요청의 확정 키를 가리킬 때만 도장이 커밋된 것이다.
// 무엇이든 확인하지 못하면 지우지 않는다. 확인된 첨부의 파일을 지우는 것보다 아무도 가리키지 않는 파일이 남는 편이 낫다.
func (s *Commands) settle(work context.Context, attachmentID, token, final string, stampErr error) (core.Attachment, bool, error) {
	released, err := s.write.ReleaseAttachment(work, attachmentID, token)
	if err != nil {
		return core.Attachment{}, false, stampErr
	}
	if released {
		s.removeOwn(work, attachmentID, final)
		return core.Attachment{}, false, stampErr
	}
	ref, err := s.read.AttachmentFile(work, attachmentID)
	switch {
	case errors.Is(err, core.ErrNotFound):
		// 행이 치워졌다. 이 파일을 가리키는 것이 없다.
		s.removeOwn(work, attachmentID, final)
		return core.Attachment{}, false, stampErr
	case err != nil:
		return core.Attachment{}, false, stampErr
	case ref.Uploaded && ref.StoragePath == final:
		// 도장은 커밋됐다. 응답만 잃었다.
		att, err := s.read.ConfirmedAttachment(work, attachmentID)
		return att, true, translate(err)
	}
	s.removeOwn(work, attachmentID, final)
	if ref.Uploaded {
		att, err := s.read.ConfirmedAttachment(work, attachmentID)
		return att, false, translate(err)
	}
	return core.Attachment{}, false, stampErr
}

// release 는 token 의 임대를 비운다. 요청이 취소돼도 비운다 — 임대가 남으면 다음 완료가 임대 시간 내내 busy 를 받는다.
// 비우지 못하면 upload.release_failed 다. 임대 시간(기본 30초)이 지나면 다음 완료 요청이 첨부를 다시 맡으므로 그 뒤로는 저절로 풀린다.
func (s *Commands) release(ctx context.Context, attachmentID, token string) {
	step, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseStep)
	defer cancel()
	if _, err := s.write.ReleaseAttachment(step, attachmentID, token); err != nil {
		slog.WarnContext(ctx, "upload.release_failed", "attachment_id", attachmentID, "err_kind", logctx.ErrKind(err))
	}
}

// removeOwn 은 이 임대가 옮긴 확정 파일을 지운다. 지우지 못하면 행이 모르는 파일이 남는다 — 유실보다 낫다.
// 지우지 못하면 upload.cleanup_failed 다(저장소에 아무 행도 가리키지 않는 파일이 하나 남았다는 뜻).
// 키에 임대 token 이 들어 있어 로그에는 키와 저장소 오류 문구를 싣지 않는다.
func (s *Commands) removeOwn(ctx context.Context, attachmentID, final string) {
	if err := s.files.Remove(ctx, final); err != nil {
		slog.WarnContext(ctx, "upload.cleanup_failed",
			"attachment_id", attachmentID, "timeout", errors.Is(err, context.DeadlineExceeded))
	}
}

// storageTimeout 은 작업 시한에 끊긴 실패를 ErrStorageTimeout 으로 바꾼다. 서버 잘못(500)이 아니라 "지금은 못 한다"(503)다.
// 이미 뜻이 정해진 거절(잘못된 입력, busy, 없음)은 그대로 둔다.
func storageTimeout(work context.Context, err error) error {
	var inv InvalidError
	switch {
	case err == nil, errors.As(err, &inv), errors.Is(err, ErrUploadBusy), errors.Is(err, ErrNotFound):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(work.Err(), context.DeadlineExceeded):
		return ErrStorageTimeout
	}
	return err
}

// withCancelOf 는 base 의 시한을 지키면서 req 가 취소되면 같이 끝나는 ctx 다.
func withCancelOf(base, req context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(base)
	stop := context.AfterFunc(req, cancel)
	return merged, func() {
		stop()
		cancel()
	}
}

// tempKey 는 올리기 양식이 쓰는 임시 위치다. 로컬 저장소의 SweepTemp 와 S3 수명 주기 규칙이 이 접두사(tmp/)를 본다.
func tempKey(attachmentID string) string { return "tmp/" + attachmentID }

// finalKey 는 확인을 마친 파일의 위치다. 양식으로는 여기에 쓸 수 없다. 끝의 token 은 완료 임대마다 다르다.
func finalKey(workspaceID, convID, attachmentID, token string) string {
	return fmt.Sprintf("ws/%s/%s/%s/%s", workspaceID, convID, attachmentID, token)
}

// DownloadURL 은 권한을 확인하고 잠깐 살아 있는 내려받기 주소를 준다.
// 대화 권한만으로 끝나지 않는다. 내부 메모에 붙은 파일과 아직 보내지 않은 업로드는 고객에게 주지 않는다.
func (s *Queries) DownloadURL(ctx context.Context, actor Actor, attachmentID string) (string, error) {
	if s.files == nil {
		return "", ErrNoFileStore
	}
	attachmentID, err := id.Normalize(attachmentID)
	if err != nil {
		return "", invalid("attachment_id_format", "attachment_id 형식이 올바르지 않습니다")
	}
	ref, err := s.read.AttachmentFile(ctx, attachmentID)
	if err != nil {
		return "", translate(err)
	}
	if _, err := s.Authorize(ctx, actor, ref.ConversationID); err != nil {
		return "", err
	}
	if !canRead(actor, ref) {
		return "", ErrForbidden
	}
	if !ref.Uploaded {
		return "", ErrNotFound
	}
	// 이미지와 브라우저가 재생하는 영상만 화면에 바로 띄우고 나머지는 내려받게 한다(D37).
	inline := strings.HasPrefix(ref.ContentType, "image/") || PlayableVideo(ref.ContentType)
	return s.files.PresignDownload(ref.StoragePath, ref.Filename, ref.ContentType, inline, downloadTTL)
}

// RemoveFiles 는 저장소에서 파일을 지운다. 실패는 남기고 넘어간다 — 행이 남고 파일이 없는 것보다 파일이 남는 편이 낫다.
// 확정 키에 임대 token 이 들어 있어 로그에는 키와 저장소 오류 문구를 싣지 않고 건수만 남긴다.
func (s *Commands) RemoveFiles(ctx context.Context, keys []string) {
	if s.files == nil {
		return
	}
	failed, timeout := 0, false
	for _, key := range keys {
		if err := s.files.Remove(ctx, key); err != nil {
			failed++
			timeout = timeout || errors.Is(err, context.DeadlineExceeded)
		}
	}
	if failed > 0 {
		slog.WarnContext(ctx, "attachment.remove_failed", "failed", failed, "total", len(keys), "timeout", timeout)
	}
}

func canRead(actor Actor, ref core.AttachmentRef) bool {
	if !ref.Internal || actor.Internal() {
		return true
	}
	// 올린 본인은 전송 전에도 자기 파일을 다시 볼 수 있어야 한다.
	return ref.UploaderKind == string(core.SenderCustomer) && ref.UploaderID == actor.CustomerID
}

func uploaderKind(actor Actor) string {
	if actor.Kind == ActorAgent {
		return string(core.SenderAgent)
	}
	return string(core.SenderCustomer)
}

func uploaderID(actor Actor) string {
	if actor.Kind == ActorAgent {
		return actor.AgentID
	}
	return actor.CustomerID
}

// declaredType 은 브라우저가 말한 종류를 다듬는다. 저장소에 올릴 때의 Content-Type 으로만 쓰고, 믿지는 않는다.
func declaredType(v string) string {
	v = strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
	if v == "" || len(v) > 100 || strings.ContainsAny(v, " \t\r\n\"") || !strings.Contains(v, "/") {
		return "application/octet-stream"
	}
	return v
}

func clampDim(n int) int {
	if n < 0 || n > maxImageDim {
		return 0
	}
	return n
}

// PlayableVideo 는 화면이 <video> 로 바로 재생하는 종류인지다. inspect 가 내용으로 판정한 값만 여기에 해당한다(D37).
// 그 밖의 영상(avi, mkv 등)은 브라우저가 재생하지 못하는 경우가 많아 파일로 내려받게 둔다.
func PlayableVideo(contentType string) bool {
	switch contentType {
	case "video/mp4", "video/quicktime", "video/webm":
		return true
	}
	return false
}

// inspect 는 실제 내용으로 종류를 판정한다. 브라우저가 보낸 Content-Type 을 믿지 않는다.
// 이미지 크기는 앞부분에서 읽히면 그 값을 쓰고, 안 읽히면(메타데이터가 긴 JPEG) 브라우저가 잰 값을 자리용으로 쓴다.
func inspect(head []byte, declaredW, declaredH int) (contentType string, width, height int) {
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		return "image/" + format, cfg.Width, cfg.Height
	}
	if ct := isoMediaType(head); ct != "" {
		return ct, 0, 0
	}
	if ct := ebmlType(head); ct != "" {
		return ct, 0, 0
	}
	detected := strings.SplitN(http.DetectContentType(head), ";", 2)[0]
	switch {
	case strings.HasPrefix(detected, "image/"):
		return detected, declaredW, declaredH
	case strings.HasPrefix(detected, "video/"), strings.HasPrefix(detected, "audio/"):
		return detected, 0, 0
	case len(head) >= 4 && string(head[:4]) == "%PDF":
		return "application/pdf", 0, 0
	case len(head) >= 2 && head[0] == 'P' && head[1] == 'K':
		return "application/zip", 0, 0
	case isText(head):
		return "text/plain; charset=utf-8", 0, 0
	default:
		return "application/octet-stream", 0, 0
	}
}

// ISO 기반 미디어 파일(mp4, mov, m4v, m4a)의 ftyp 주 브랜드.
// 표준 라이브러리의 DetectContentType 은 호환 브랜드에 mp41·mp42 가 있을 때만 video/mp4 라고 답해서,
// isom·avc1 만 적은 mp4 와 아이폰의 mov(qt)를 놓친다.
var (
	mp4VideoBrands = map[string]bool{
		"isom": true, "iso2": true, "iso3": true, "iso4": true, "iso5": true, "iso6": true,
		"mp41": true, "mp42": true, "avc1": true, "dash": true, "mmp4": true, "MSNV": true,
		"M4V ": true, "M4VH": true, "M4VP": true, //nolint:gocritic // mapKey: 브랜드는 네 바이트라 뒤 공백까지 브랜드다.
	}
	mp4AudioBrands = map[string]bool{"M4A ": true, "M4B ": true, "M4P ": true, "F4A ": true, "F4B ": true}
)

// isoMediaType 은 첫 상자가 ftyp 일 때 주 브랜드로 mp4·mov 를 판정한다. 아니면 "" 다.
// 모르는 주 브랜드(HEIC·AVIF 사진, CR3 등)는 여기서 정하지 않고 DetectContentType 에 맡긴다. 같은 상자 구조라 호환 브랜드에
// isom 을 적은 사진이 있어서, 호환 브랜드만 보고 영상이라고 하면 사진을 영상으로 내보낸다.
func isoMediaType(head []byte) string {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return ""
	}
	switch major := string(head[8:12]); {
	case major == "qt  ":
		return "video/quicktime"
	case mp4VideoBrands[major]:
		return "video/mp4"
	case mp4AudioBrands[major]:
		return "audio/mp4"
	}
	return ""
}

// ebmlType 은 Matroska 계열(EBML 머리)을 판정한다. DocType 이 webm 이면 video/webm, 아니면(mkv) video/x-matroska 다.
// DetectContentType 은 머리만 보고 둘 다 video/webm 이라고 답한다. mkv 를 webm 으로 내보내면 브라우저가 재생하려다 실패한다.
func ebmlType(head []byte) string {
	if len(head) < 4 || !bytes.Equal(head[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return ""
	}
	// DocType(0x4282) 은 EBML 머리 안, 앞쪽 수십 바이트에 있다.
	window := head[:min(len(head), 64)]
	if i := bytes.Index(window, []byte{0x42, 0x82}); i >= 0 && i+2 < len(window) {
		size := int(window[i+2] & 0x7F) // 1바이트 vint(0x80 | 길이)
		if window[i+2]&0x80 != 0 && i+3+size <= len(window) && string(window[i+3:i+3+size]) == "webm" {
			return "video/webm"
		}
	}
	return "video/x-matroska"
}

func isText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}
