package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 메시지 전송과 조회. 번호 공간과 내부 메모 노출 여부가 actor 로 갈린다.

type SendParams struct {
	ConversationID string
	ClientMsgID    string
	Body           string
	AttachmentID   string
	// Internal 이 true 면 내부 메모다. 상담원만 쓸 수 있다.
	Internal bool
}

// validate 는 저장소에 닿기 전에 입력을 거른다. 통과하면 저장 가능한 형태가 보장된다.
func (p SendParams) validate() (SendParams, error) {
	convID, err := id.Normalize(p.ConversationID)
	if err != nil {
		return p, invalid("conversation_id_format", "conversation_id 형식이 올바르지 않습니다")
	}
	p.ConversationID = convID

	p.ClientMsgID = strings.TrimSpace(p.ClientMsgID)
	switch {
	case p.ClientMsgID == "" || len(p.ClientMsgID) > MaxClientMsgID:
		return p, invalid("client_msg_id_length", "client_msg_id 는 1~%d 바이트여야 합니다", MaxClientMsgID)
	case p.ClientMsgID == welcomeClientMsg:
		return p, invalid("client_msg_id_reserved", "예약된 client_msg_id 입니다")
	}

	p.Body = strings.TrimSpace(p.Body)
	if utf8.RuneCountInString(p.Body) > MaxBodyRunes {
		return p, invalid("body_too_long", "본문은 %d자까지입니다", MaxBodyRunes)
	}
	if p.Body == "" && p.AttachmentID == "" {
		return p, invalid("body_required", "본문이 비어 있습니다")
	}
	if p.AttachmentID != "" {
		attachmentID, err := id.Normalize(p.AttachmentID)
		if err != nil {
			return p, invalid("attachment_id_format", "attachment_id 형식이 올바르지 않습니다")
		}
		p.AttachmentID = attachmentID
	}
	return p, nil
}

// Send 는 메시지 한 건을 대화에 넣고 그 결과를 사건 로그로 남긴다. HTTP 와 WebSocket 이 같이 부른다.
func (s *Commands) Send(ctx context.Context, actor Actor, p SendParams) (core.AppendResult, error) {
	res, err := s.send(ctx, actor, p)
	if err != nil {
		// 본문은 싣지 않는다. 거절을 다시 따라가려면 client_msg_id 로 클라이언트 쪽을 본다.
		// client_msg_id 는 모양이 맞을 때만 싣는다(D30, 감사 23 — 50KB id 가 그대로 실렸다).
		logRejected(ctx, "message.rejected", err, clientMsgIDArgs(p.ClientMsgID)...)
		return res, err
	}
	logAppended(ctx, res)
	return res, nil
}

// send 는 메시지 한 건을 대화에 넣는다. 재전송이면 처음 저장한 결과를 그대로 돌려준다.
func (s *Commands) send(ctx context.Context, actor Actor, p SendParams) (core.AppendResult, error) {
	p, err := p.validate()
	if err != nil {
		return core.AppendResult{}, err
	}
	if p.Internal && actor.Kind != ActorAgent {
		// 고객이 내부 메모를 쓰려 하면 공개로 낮추지 않고 거절한다.
		return core.AppendResult{}, ErrForbidden
	}
	conv, err := s.q.Authorize(ctx, actor, p.ConversationID)
	if err != nil {
		return core.AppendResult{}, err
	}

	ap, err := appendParams(actor, p)
	if err != nil {
		return core.AppendResult{}, err
	}
	res, err := s.write.AppendMessage(ctx, ap)
	if errors.Is(err, core.ErrNotFound) && p.AttachmentID != "" {
		return core.AppendResult{}, invalid("attachment_unavailable", "첨부를 찾을 수 없거나 이미 보낸 첨부입니다")
	}
	if err != nil {
		return core.AppendResult{}, translate(err)
	}
	if res.Created && actor.Kind == ActorCustomer {
		s.autoReply(ctx, conv.WorkspaceID, res)
	}
	// 기다리던 고객에게 상담원의 공개 답변이 갔다 — 업무 기록의 "응답" 이다.
	if res.Created && res.Answered {
		s.event(ctx, conv.ID, actor.AgentID, core.EventAnswered, nil)
	}
	return res, nil
}

// Edit 는 본문 수정과 그 사건 로그다.
func (s *Commands) Edit(ctx context.Context, actor Actor, convID, messageID, body string) (core.AppendResult, error) {
	res, err := s.edit(ctx, actor, convID, messageID, body)
	logRevised(ctx, core.KindEdit, messageID, res, err)
	return res, err
}

// edit 는 자기 메시지의 본문을 바꾼다. 원문은 이력에 남고, 변화는 같은 로그의 EDIT 행으로 전달된다.
func (s *Commands) edit(ctx context.Context, actor Actor, convID, messageID, body string) (core.AppendResult, error) {
	body, err := trimmedRunes(body, MaxBodyRunes, "body_too_long", "본문")
	if err != nil {
		return core.AppendResult{}, err
	}
	if body == "" {
		return core.AppendResult{}, invalid("body_required", "본문이 비어 있습니다. 지우려면 삭제해 주세요")
	}
	p, err := s.q.reviseParams(ctx, actor, convID, messageID)
	if err != nil {
		return core.AppendResult{}, err
	}
	p.Body = body
	res, err := s.write.ReviseMessage(ctx, core.KindEdit, p)
	return res, translate(err)
}

// Delete 는 메시지 삭제와 그 사건 로그다.
func (s *Commands) Delete(ctx context.Context, actor Actor, convID, messageID string) (core.AppendResult, error) {
	res, err := s.deleteMessage(ctx, actor, convID, messageID)
	logRevised(ctx, core.KindDelete, messageID, res, err)
	return res, err
}

// deleteMessage 는 본문·첨부를 숨기고 자리는 남긴다. 번호에 빈칸이 생기지 않는다.
// 메시지 관리 권한(message.moderate)이 있으면 다른 상담원·AI·봇 메시지도 지울 수 있다. 고객 글은 고객의 것이고(ErrNotOwner),
// 시스템 줄은 운영 기록이라 누구도 지우지 못한다(ErrSystemLine, 감사 28). 둘 다 저장소가 잠근 행의 발신자로 판정한다.
func (s *Commands) deleteMessage(ctx context.Context, actor Actor, convID, messageID string) (core.AppendResult, error) {
	p, err := s.q.reviseParams(ctx, actor, convID, messageID)
	if err != nil {
		return core.AppendResult{}, err
	}
	p.AdminOverride = actor.Can(PermMessageModerate)
	res, err := s.write.ReviseMessage(ctx, core.KindDelete, p)
	return res, translate(err)
}

func (s *Queries) reviseParams(ctx context.Context, actor Actor, convID, messageID string) (core.ReviseParams, error) {
	conv, err := s.Authorize(ctx, actor, convID)
	if err != nil {
		return core.ReviseParams{}, err
	}
	messageID, err = id.Normalize(messageID)
	if err != nil {
		return core.ReviseParams{}, invalid("message_id_format", "message_id 형식이 올바르지 않습니다")
	}
	kind := core.SenderCustomer
	if actor.Kind == ActorAgent {
		kind = core.SenderAgent
	}
	return core.ReviseParams{
		ConversationID: conv.ID, MessageID: messageID,
		ActorKind: kind, ActorAgentID: actor.AgentID, ActorCustomer: actor.CustomerID,
	}, nil
}

// Revisions 는 이력 조회 권한(message.revisions)이 본다. 고객이 지운 원문도 운영 기록이다.
func (s *Queries) Revisions(ctx context.Context, actor Actor, convID, messageID string) ([]core.Revision, error) {
	if err := require(ctx, actor, PermMessageRevisions); err != nil {
		return nil, err
	}
	conv, err := s.authorizeAgentOn(ctx, actor, convID)
	if err != nil {
		return nil, err
	}
	messageID, err = id.Normalize(messageID)
	if err != nil {
		return nil, invalid("message_id_format", "message_id 형식이 올바르지 않습니다")
	}
	revs, err := s.read.Revisions(ctx, conv.ID, messageID)
	return revs, translate(err)
}

// HistoryBefore 는 한 대화 안에서 앞쪽 페이지를 읽는다.
func (s *Queries) HistoryBefore(ctx context.Context, actor Actor, convID string, beforeSeq int64, limit int) ([]core.Message, error) {
	conv, err := s.Authorize(ctx, actor, convID)
	if err != nil {
		return nil, err
	}
	msgs, err := s.read.ListMessagesBefore(ctx, conv.ID, beforeSeq, limit, actor.Internal())
	if msgs == nil {
		msgs = []core.Message{}
	}
	return msgs, translate(err)
}

// PriorHistory 는 이 고객의 이전 상담을 이어서 읽는다. 상담원만 본다 — 고객에게 지난 상담은 새 방 뒤에 없다.
// before 가 비어 있으면 이 대화보다 먼저 열린 상담의 마지막 줄부터다.
func (s *Queries) PriorHistory(ctx context.Context, actor Actor, convID string, before core.PriorCursor, limit int) ([]core.Message, error) {
	conv, err := s.authorizeAgentOn(ctx, actor, convID)
	if err != nil {
		return nil, err
	}
	if before.ConversationID == "" {
		before = core.PriorCursor{ConversationID: conv.ID}
	} else {
		normalized, err := id.Normalize(before.ConversationID)
		if err != nil {
			return nil, invalid("before_conversation_format", "before_conversation 형식이 올바르지 않습니다")
		}
		before.ConversationID = normalized
	}
	if before.Seq < 0 {
		before.Seq = 0
	}
	msgs, err := s.read.ListCustomerHistory(ctx, conv.WorkspaceID, conv.CustomerID, conv.ID, before, limit)
	if msgs == nil {
		msgs = []core.Message{}
	}
	return msgs, translate(err)
}

// autoReply 는 사람 대신 한 번 답한다. 실패해도 고객 메시지는 이미 저장됐으니 되돌리지 않는다.
//
// 두 문장이 있다. 운영시간 밖이면 부재중 안내, 아니면 접수 안내. 대화당 한 번이다.
// "한 번" 은 저장소가 auto_replied_at 으로 지킨다. 같은 순간 두 메시지가 와도 하나만 나간다.
func (s *Commands) autoReply(ctx context.Context, wsID string, res core.AppendResult) {
	if !res.NeedsAutoReply {
		return
	}
	settings, err := s.q.settings(ctx, wsID)
	if err != nil {
		return
	}
	body := strings.TrimSpace(settings.Messages.FirstReply)
	if !settings.Hours.Open(time.Now()) {
		if away := strings.TrimSpace(settings.Messages.Away); away != "" {
			body = away
		}
	}
	if body == "" {
		return
	}
	first, err := s.write.MarkAutoReplied(ctx, res.Message.ConversationID)
	if err != nil || !first {
		return
	}
	_, _ = s.write.AppendMessage(ctx, core.AppendParams{
		ConversationID: res.Message.ConversationID,
		ClientMsgID:    "auto-reply-" + res.Message.ConversationID,
		SenderKind:     core.SenderBot,
		Visibility:     core.Public,
		Body:           body,
	})
}

func appendParams(actor Actor, p SendParams) (core.AppendParams, error) {
	ap := core.AppendParams{
		ConversationID: p.ConversationID,
		ClientMsgID:    p.ClientMsgID,
		Body:           p.Body,
		AttachmentID:   p.AttachmentID,
		Visibility:     core.Public,
		// 새 메시지는 닫히거나 보류된 대화를 다시 연다. 채널톡과 같은 동작이다.
		Reopen: true,
	}
	if p.Internal {
		ap.Visibility = core.Private
		// 내부 메모는 대화를 다시 열지 않는다. 고객에게 보이지 않는 활동이다.
		ap.Reopen = false
	}
	switch actor.Kind {
	case ActorAgent:
		ap.SenderKind = core.SenderAgent
		ap.SenderAgentID = actor.AgentID
	case ActorCustomer:
		ap.SenderKind = core.SenderCustomer
		ap.SenderCustomer = actor.CustomerID
	default:
		return ap, ErrForbidden
	}
	return ap, nil
}

// Stream 은 전달 루프가 쓰는 조회다. 권한 확인 한 번으로 대화와 메시지를 같이 가져온다.
//
// 보낼 것이 없으면 메시지 조회를 하지 않는다. 유휴 연결이 많을수록 이 한 번이 크게 불어난다.
func (s *Queries) Stream(ctx context.Context, actor Actor, convID string, afterSeq int64, limit int) (core.Conversation, []core.Message, error) {
	if afterSeq < 0 {
		return core.Conversation{}, nil, invalid("after_seq_negative", "after_seq 는 0 이상이어야 합니다")
	}
	convID, err := id.Normalize(convID)
	if err != nil {
		return core.Conversation{}, nil, invalid("conversation_id_format", "conversation_id 형식이 올바르지 않습니다")
	}
	conv, err := s.read.ConversationMeta(ctx, convID)
	if err != nil {
		return core.Conversation{}, nil, translate(err)
	}
	if err := s.authorizeConversation(actor, conv); err != nil {
		return core.Conversation{}, nil, err
	}
	if conv.ViewerSeq(actor.Internal()) <= afterSeq {
		return conv, nil, nil
	}
	msgs, err := s.read.ListMessages(ctx, conv.ID, afterSeq, limit, actor.Internal())
	if err != nil {
		return core.Conversation{}, nil, translate(err)
	}
	return conv, msgs, nil
}

// History 는 afterSeq 다음 메시지를 오름차순으로 돌려준다.
func (s *Queries) History(ctx context.Context, actor Actor, convID string, afterSeq int64, limit int) ([]core.Message, error) {
	_, msgs, err := s.Stream(ctx, actor, convID, afterSeq, limit)
	if msgs == nil {
		msgs = []core.Message{}
	}
	return msgs, err
}

// Deliver 는 전달 루프의 빠른 길이다. 알림이 이미 "보낼 것이 있다" 를 알려 준 상태라
// 대화 행을 다시 읽지 않고, 권한은 구독할 때 확인한 것을 그대로 쓴다.
func (s *Queries) Deliver(ctx context.Context, actor Actor, convID string, afterSeq int64, limit int) ([]core.Message, error) {
	if afterSeq < 0 {
		return nil, invalid("after_seq_negative", "after_seq 는 0 이상이어야 합니다")
	}
	msgs, err := s.read.ListMessages(ctx, convID, afterSeq, limit, actor.Internal())
	return msgs, translate(err)
}

// MarkRead 는 읽음 지점을 올린다. seq 는 보고한 쪽의 번호 공간이다.
func (s *Commands) MarkRead(ctx context.Context, actor Actor, convID string, seq int64) (int64, error) {
	if seq < 0 {
		return 0, invalid("seq_negative", "seq 는 0 이상이어야 합니다")
	}
	conv, err := s.q.Authorize(ctx, actor, convID)
	if err != nil {
		return 0, err
	}
	if latest := conv.ViewerSeq(actor.Internal()); seq > latest {
		seq = latest
	}
	ev, err := liveEvent(actor, core.LiveRead)
	if err != nil {
		return 0, err
	}
	ev.WorkspaceID = conv.WorkspaceID
	stored, err := s.write.MarkRead(ctx, conv.ID, ev, seq, actor.Internal())
	if err == nil {
		// seq 는 보고한 쪽의 번호 공간이다. 단조 증가라 되돌아간 보고면 저장된 값이 그대로 나온다.
		slog.InfoContext(ctx, "read.marked", "conversation_id", conv.ID, "seq", stored)
	}
	return stored, translate(err)
}

// ReadStates 는 권한을 확인하고 읽음 지점을 준다. HTTP 의 /reads 가 쓴다.
func (s *Queries) ReadStates(ctx context.Context, actor Actor, convID string) ([]core.ReadState, error) {
	conv, err := s.Authorize(ctx, actor, convID)
	if err != nil {
		return nil, err
	}
	return s.ReadStatesOf(ctx, actor, conv)
}

// ReadStatesOf 는 이미 Authorize 를 지난 대화의 읽음 지점이다. 대화를 다시 읽지 않는다.
// 구독이 쓴다. 구독은 방금 권한을 확인하며 대화를 읽었다.
func (s *Queries) ReadStatesOf(ctx context.Context, actor Actor, conv core.Conversation) ([]core.ReadState, error) {
	states, err := s.read.ReadStates(ctx, conv.ID)
	if err != nil {
		return nil, translate(err)
	}
	if actor.Internal() {
		return states, nil
	}
	// 고객에게 상담원의 "읽음" 은 답변 지점이다. 읽고 답하지 않은 건 보여 주지 않는다 — "읽었는데 왜 답이 없나" 가 CS 를 부른다.
	point, err := s.read.AgentReplyPoint(ctx, conv.ID)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]core.ReadState, 0, len(states)+1)
	for _, st := range states {
		if !agentReadHiddenFrom(actor, ActorKind(st.ReaderKind)) {
			out = append(out, st)
		}
	}
	out = append(out, core.ReadState{ReaderKind: string(ActorAgent), ReaderName: "상담원", LastReadPublicSeq: point})
	return out, nil
}

// Typing 은 입력 중 신호를 뿌린다. DB 에 남기지 않고, 받는 쪽 버퍼가 차면 버린다.
func (s *Commands) Typing(ctx context.Context, actor Actor, convID string) error {
	conv, err := s.q.Authorize(ctx, actor, convID)
	if err != nil {
		return err
	}
	ev, err := liveEvent(actor, core.LiveTyping)
	if err != nil {
		return err
	}
	ev.ConversationID = conv.ID
	ev.WorkspaceID = conv.WorkspaceID
	return translate(s.write.PublishLive(ctx, ev))
}
