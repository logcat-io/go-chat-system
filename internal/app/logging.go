package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 상담 추적 로깅의 사건 쪽 (docs/proposals/2026-10-03-권한과-로깅.md 3절).
//
// 사건 로그는 성공과 거절을 아는 유스케이스가 남긴다. 전달 계층(HTTP·WebSocket)은 같은 일을 다른 길로 부르므로
// 거기서 남기면 길마다 빠지거나 겹친다. 로거는 들고 다니지 않고 slog 기본 로거에 ctx 를 넘긴다 —
// trace_id·actor_id 같은 공통 키는 전달 계층이 ctx 에 얹어 두었고 logctx 핸들러가 붙인다.
//
// 적지 않는 것: 메시지 본문, 이름·이메일, 토큰, 비밀번호, 서명된 주소, 파일 이름. id·크기·종류·코드만 적는다.

// rejectCode 는 거절 사유를 짧은 코드로 옮긴다. 오류 문구에는 입력값이 섞일 수 있어 로그에 싣지 않는다.
func rejectCode(err error) string {
	var inv InvalidError
	switch {
	case errors.Is(err, ErrStorageTimeout):
		return "storage_timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.As(err, &inv):
		return "invalid"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrClosedByCustomer):
		return "closed_by_customer"
	case errors.Is(err, ErrSystemLine):
		return "system_line"
	case errors.Is(err, ErrEnded):
		return "ended"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrFileTooLarge):
		return "too_large"
	case errors.Is(err, ErrTooManyPending):
		return "too_many"
	case errors.Is(err, ErrNoFileStore):
		return "no_file_store"
	case errors.Is(err, ErrUploadLeaseLost):
		// 응답은 busy 지만 뜻이 다르다. busy 는 다른 요청이 확인하는 중이라 기다리면 되고, lease_lost 는 이 요청이 시간을 넘겨
		// 다른 요청이 넘겨받은 것이다. 같은 code 로 남기면 "임대가 짧다·저장소가 느리다" 를 로그로 찾을 수 없다(D47).
		return "lease_lost"
	case errors.Is(err, ErrUploadBusy):
		return "busy"
	case errors.Is(err, ErrNotClosed):
		return "not_closed"
	case errors.Is(err, ErrRatingExpired):
		return "rating_closed"
	case errors.Is(err, ErrNoReply):
		return "no_reply"
	default:
		return "internal"
	}
}

// rejectLevel 은 거절의 수준이다. 사용자 쪽 거절은 WARN, 분류되지 않은 실패는 서버 잘못이라 ERROR,
// 클라이언트가 먼저 끊은 것은 실패가 아니라 DEBUG 다.
func rejectLevel(code string) slog.Level {
	switch code {
	case "internal":
		return slog.LevelError
	case "canceled":
		return slog.LevelDebug
	default:
		return slog.LevelWarn
	}
}

// rejectReason 은 code 보다 좁은 고정 사유다(D47). 입력 거절은 InvalidError.Code(file_missing·size_mismatch …),
// 그 밖은 오류 사슬의 가장 좁은 코드가 code 와 다를 때다(conflict 아래 already_taken, forbidden 아래 not_owner …). 없으면 빈 문자열이다.
// 분류되지 않은 실패(internal)는 사유 대신 err_kind 를 싣는다.
func rejectReason(err error, code string) string {
	var inv InvalidError
	if errors.As(err, &inv) {
		return inv.Code
	}
	if code == "internal" {
		return ""
	}
	if c := core.ErrCode(err); c != code {
		return c
	}
	return ""
}

func logRejected(ctx context.Context, event string, err error, args ...any) {
	code := rejectCode(err)
	args = append(args, "code", code)
	if reason := rejectReason(err, code); reason != "" {
		args = append(args, "reason", reason)
	}
	if code == "internal" {
		// 서버 잘못이다. 무엇이 실패했는지 종류를 남긴다(원문은 싣지 않는다, D30).
		args = append(args, "err_kind", logctx.ErrKind(err))
	}
	slog.Log(ctx, rejectLevel(code), event, args...)
}

// LoggableClientMsgID 는 client_msg_id 를 로그에 실어도 되는 모양이면 그 값을, 아니면 빈 문자열을 돌려준다(D30, 감사 23).
// client_msg_id 는 클라이언트가 정한 글자다. 앞뒤 공백을 걷은 뒤 1~MaxClientMsgID 바이트이고 영문·숫자·`.`·`_`·`:`·`-` 로만 됐을 때만 싣는다 —
// 클라이언트가 만드는 값(UUID·hex·"agent-1-2")과 서버가 만드는 값("system-<uuid>")은 다 이 모양이다. 길이 제한이 없으면 프레임 하나로
// 로그 한 줄이 수십 KB 가 되고, 글자 제한이 없으면 이메일·이름을 id 자리에 넣어 로그에 남길 수 있다. 빈 문자열이면 키를 싣지 않는다.
func LoggableClientMsgID(s string) string {
	if len(s) > 4*MaxClientMsgID {
		return "" // 공백을 걷어도 상한을 넘을 것이 뻔하다. 큰 입력을 다 훑지 않는다
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxClientMsgID {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return ""
		}
	}
	return s
}

// idArgs 는 경로·본문에서 온 id 를 로그 키로 싣는다. id 모양(id.Normalize)이면 정규화한 값을, 아니면 키째 뺀다(D30, 감사 23).
// 형식 거절은 정규화 전에 일어나므로 원문을 그대로 실으면 경로의 아무 글자(퍼센트 디코딩된 줄바꿈·이메일)가 로그에 남는다.
func idArgs(key, raw string) []any {
	if v, err := id.Normalize(raw); err == nil {
		return []any{key, v}
	}
	return nil
}

// declaredClass 는 업로드를 신청할 때 브라우저가 적은 종류의 큰 갈래(image/*·video/* …)다. 모르는 갈래면 other 다(D30, 감사 23).
// 신청한 종류는 클라이언트가 정한 글자라(100바이트까지, @ 같은 글자도 지난다) 그대로 싣지 않는다. 실제 종류는 완료(upload.completed)가
// 내용으로 판정한 고정 값으로 싣는다.
func declaredClass(contentType string) string {
	major, _, _ := strings.Cut(contentType, "/")
	switch major {
	case "application", "audio", "font", "image", "model", "text", "video":
		return major + "/*"
	}
	return "other"
}

// clientMsgIDArgs 는 client_msg_id 키와 값이다. 실을 수 없는 모양이면 키째 뺀다.
func clientMsgIDArgs(clientMsgID string) []any {
	if v := LoggableClientMsgID(clientMsgID); v != "" {
		return []any{"client_msg_id", v}
	}
	return nil
}

// logAppended 는 저장된 메시지 한 건이다. 재전송이면 created=false 이고 번호는 처음 저장할 때 남겼다.
// client_msg_id 는 요청의 원문이 아니라 저장된 값(검증을 지나 공백을 걷은 것)이다.
func logAppended(ctx context.Context, res core.AppendResult) {
	m := res.Message
	args := []any{
		"conversation_id", m.ConversationID, "message_id", m.ID,
		"sender_kind", m.SenderKind, "kind", m.Kind, "visibility", m.Visibility,
		"has_attachment", m.Attachment != nil, "created", res.Created,
	}
	args = append(args, clientMsgIDArgs(m.ClientMsgID)...)
	if res.Created {
		args = append(args, "seq", res.Seq, "public_seq", res.PublicSeq)
	}
	slog.InfoContext(ctx, "message.appended", args...)
}

// logRevised 는 수정·삭제다. 새로 들어간 EDIT·DELETE 행의 번호와 원래 메시지 id 를 싣는다.
func logRevised(ctx context.Context, kind core.MessageKind, messageID string, res core.AppendResult, err error) {
	if err != nil {
		logRejected(ctx, "message.rejected", err, append([]any{"kind", kind}, idArgs("ref_message_id", messageID)...)...)
		return
	}
	slog.InfoContext(ctx, "message.revised", "conversation_id", res.Message.ConversationID, "kind", kind,
		"ref_message_id", res.Message.RefMessageID, "seq", res.Seq, "public_seq", res.PublicSeq)
}
