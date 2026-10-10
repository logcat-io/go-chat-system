package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 대화의 수명과 운영 조작. 상태 전환·담당자·팔로워·태그가 여기 모인다.

// StartCustomerSession 은 익명 고객을 준비한다. 대화는 만들지 않는다.
//
// 위젯을 열어 보기만 해도 대화가 생기면 상담원 받은함이 빈 대화로 찬다.
// 대화는 고객이 "문의하기" 를 눌렀을 때 생긴다. 열려 있는 대화가 있으면 그걸 돌려준다.
func (s *Commands) StartCustomerSession(ctx context.Context, wsID, customerID, displayName string) (string, *core.Conversation, error) {
	if customerID == "" {
		name, err := trimmedRunes(displayName, maxNameRunes, "name_too_long", "이름")
		if err != nil {
			return "", nil, err
		}
		if name == "" {
			name = "고객"
		}
		created, err := s.write.CreateCustomer(ctx, wsID, name)
		if err != nil {
			return "", nil, err
		}
		customerID = created
	}
	_ = s.write.TouchCustomer(ctx, customerID)

	conv, err := s.read.LatestOpenConversation(ctx, customerID)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return customerID, nil, nil
	case err != nil:
		return "", nil, translate(err)
	}
	return customerID, &conv, nil
}

// StartConversation 은 고객이 "새 문의하기" 를 눌렀을 때 상담을 연다. 상담마다 대화 하나다.
//
// 회원은 열린 상담이 있으면 그 방으로 간다(버튼을 두 번 눌러도 방은 하나다). 종료된 상담은 다시 열지 않고 새 대화를 만든다.
// 상담원은 고객 단위로 이전 상담을 이어 본다(PriorHistory). 익명 고객은 묶을 신원이 없어 매번 새 대화다.
func (s *Commands) StartConversation(ctx context.Context, actor Actor) (core.Conversation, error) {
	if actor.Kind != ActorCustomer || actor.CustomerID == "" {
		return core.Conversation{}, ErrForbidden
	}
	customer, err := s.read.Customer(ctx, actor.CustomerID)
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	member := customer.ExternalID != ""
	// 이미 열린 상담이 있는 회원은 한도와 무관하게 그 방을 계속 쓴다.
	if member {
		open, err := s.read.LatestOpenConversation(ctx, actor.CustomerID)
		if err == nil {
			return open, nil
		}
		if !errors.Is(err, core.ErrNotFound) {
			return core.Conversation{}, translate(err)
		}
	}
	// 새 문의 수에는 한도가 없다(D21). 남용은 입구의 IP 당 한도가 막는다.
	var conv core.Conversation
	if member {
		var opened core.OpenResult
		conv, opened, err = s.write.OpenMemberConversation(ctx, actor.WorkspaceID, actor.CustomerID)
		if err != nil {
			return core.Conversation{}, translate(err)
		}
		if opened == core.OpenedExisting {
			// 동시에 온 다른 요청이 먼저 열었다. 환영 문구도 기록도 다시 남기지 않는다.
			return conv, nil
		}
	} else if conv, err = s.write.CreateConversation(ctx, actor.WorkspaceID, actor.CustomerID, ""); err != nil {
		return core.Conversation{}, translate(err)
	}
	// 몇 번째 상담인지 남긴다. 상담원은 이 줄을 보고 위의 이전 상담을 읽는다.
	nth, err := s.read.CustomerConversationCount(ctx, actor.CustomerID)
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	if nth > 1 {
		s.systemNote(ctx, conv.ID, fmt.Sprintf("이 고객의 %d번째 상담입니다. 이전 상담이 위에 이어집니다.", nth), core.Private)
	}
	s.event(ctx, conv.ID, "", core.EventOpened, map[string]any{"nth": nth})
	// 상담 하나의 생애가 여기서 시작한다. 그 전의 세션 로그와는 customer_id 로 잇는다.
	slog.InfoContext(ctx, "conversation.opened", "conversation_id", conv.ID, "customer_id", actor.CustomerID, "nth", nth)
	// 환영 문구는 설정에서 온다. 비워 뒀으면 안 보낸다. 고정 client_msg_id 라 두 번 들어가지 않는다.
	settings, err := s.q.settings(ctx, actor.WorkspaceID)
	if err != nil {
		return core.Conversation{}, err
	}
	if body := strings.TrimSpace(settings.Messages.Welcome); body != "" {
		if _, err := s.write.AppendMessage(ctx, core.AppendParams{
			ConversationID: conv.ID,
			ClientMsgID:    welcomeClientMsg,
			SenderKind:     core.SenderBot,
			Visibility:     core.Public,
			Body:           body,
		}); err != nil {
			return core.Conversation{}, translate(err)
		}
	}
	conv, err = s.read.Conversation(ctx, conv.ID)
	return conv, translate(err)
}

// event 는 업무 처리 기록이다. 실패해도 본 작업을 되돌리지 않는다.
func (s *Commands) event(ctx context.Context, convID, agentID string, kind core.EventKind, detail map[string]any) {
	// 기록이 실패해도 고객의 요청은 막지 않는다. 대신 조용히 잃지 않게 남긴다 — 대시보드의 숫자가 비면 여기를 본다.
	if err := s.write.RecordEvent(ctx, core.ConversationEvent{ConversationID: convID, AgentID: agentID, Kind: kind, Detail: detail}); err != nil {
		slog.WarnContext(ctx, "worklog.failed", "conversation_id", convID, "kind", kind, "err_kind", logctx.ErrKind(err))
	}
}

// CustomerConversations 는 위젯의 대화 목록이다.
func (s *Queries) CustomerConversations(ctx context.Context, actor Actor, limit int) ([]core.Conversation, error) {
	if actor.Kind != ActorCustomer || actor.CustomerID == "" {
		return nil, ErrForbidden
	}
	convs, err := s.read.CustomerConversations(ctx, actor.CustomerID, limit)
	return convs, translate(err)
}

// WithTags 는 대화 프레임을 보낼 때만 부른다. 태그 조회를 전달 루프에서 뺀 대가다.
// 읽지 못하면 오류를 돌려준다 — 부르는 쪽이 "태그 없음" 과 "태그를 모름" 을 가른다(DEF-83). 태그가 없으면 빈 목록이다.
func (s *Queries) WithTags(ctx context.Context, conv core.Conversation) (core.Conversation, error) {
	tags, err := s.read.ConversationTags(ctx, conv.ID)
	if err != nil {
		return conv, translate(err)
	}
	if tags == nil {
		tags = []core.Tag{}
	}
	conv.Tags = tags
	return conv, nil
}

// Inbox 는 상담원 전용 대화 목록이다.
func (s *Queries) Inbox(ctx context.Context, actor Actor, f core.InboxFilter) ([]core.Conversation, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return nil, err
	}
	if f.State != "" && !f.State.Valid() {
		return nil, invalid("state_invalid", "state 는 OPEN·SNOOZED·CLOSED 중 하나여야 합니다")
	}
	// 입력 순서로 본다. 둘 다 틀려도 문구가 매번 같다.
	if f.AssigneeID != "" {
		if _, err := id.Normalize(f.AssigneeID); err != nil {
			return nil, invalid("assignee_format", "assignee 형식이 올바르지 않습니다")
		}
	}
	if f.TagID != "" {
		if _, err := id.Normalize(f.TagID); err != nil {
			return nil, invalid("tag_id_format", "tag_id 형식이 올바르지 않습니다")
		}
	}
	// 가린 상담 목록은 되살릴 수 있는 사람만 본다.
	if f.Deleted {
		if err := require(ctx, actor, PermConversationDelete); err != nil {
			return nil, err
		}
	}
	// 전체 검색 권한이 없으면 고객 이름으로만 찾는다. 받은함 검색이 본문 검색의 뒷문이 되면 안 된다.
	f.NameOnly = !actor.Can(PermSearchAll)
	f.ReaderID = actor.AgentID
	f.WorkspaceID = actor.WorkspaceID
	convs, err := s.read.ListConversations(ctx, f)
	return convs, translate(err)
}

// InboxCounts 는 상담원이 봐야 할 것이 몇 건인지 센다.
func (s *Queries) InboxCounts(ctx context.Context, actor Actor) (core.InboxCounts, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return core.InboxCounts{}, err
	}
	c, err := s.read.CountInbox(ctx, actor.WorkspaceID, actor.AgentID)
	return c, translate(err)
}

// SetState 는 대화를 완료·보류·재개한다. 상담원만 한다.
func (s *Commands) SetState(ctx context.Context, actor Actor, convID string, state core.State, until *time.Time) (core.Conversation, error) {
	switch {
	case !state.Valid():
		return core.Conversation{}, invalid("state_invalid", "state 는 OPEN·SNOOZED·CLOSED 중 하나여야 합니다")
	case state == core.StateSnoozed && (until == nil || until.Before(time.Now())):
		return core.Conversation{}, invalid("snooze_until_past", "보류는 미래 시각이 필요합니다")
	case state != core.StateSnoozed:
		until = nil
	}
	before, err := s.q.authorizeAgentOn(ctx, actor, convID)
	if err != nil {
		return core.Conversation{}, err
	}
	// 시스템 줄(상담원만 본다)과 업무 기록은 상태 변경과 한 트랜잭션이다(D39). 고객은 상태 자체를 대화 프레임으로 받는다.
	// 잠근 행이 이미 그 상태면 저장소는 아무것도 쓰지 않고 지금 대화를 준다 — record 가 불리지 않는다(동시 "완료" 는 기록 한 번).
	// 응답은 저장소가 트랜잭션 안에서 읽은 값이다. 커밋 뒤에 다시 읽지 않는다 — 커밋된 종료를 그 뒤의 읽기 실패로 500 이라 답하면
	// 화면은 종료가 안 된 줄 안다.
	from, changed := before.State, false
	conv, err := s.write.SetConversationState(ctx, convID, state, until, func(prev, _ core.Conversation) core.ChangeRecord {
		from, changed = prev.State, true
		return core.ChangeRecord{
			Note:  stateNote(state, actor.Name),
			Event: core.ConversationEvent{AgentID: actor.AgentID, Kind: stateEventKind(state)},
		}
	})
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	if !changed {
		return conv, nil
	}
	// from 은 잠근 행에서 읽은 바꾸기 전 상태다.
	slog.InfoContext(ctx, "conversation.state_changed", "conversation_id", conv.ID, "from", from, "to", state)
	return conv, nil
}

func stateEventKind(state core.State) core.EventKind {
	switch state {
	case core.StateClosed:
		return core.EventClosed
	case core.StateSnoozed:
		return core.EventSnoozed
	default:
		return core.EventReopened
	}
}

func stateNote(state core.State, agentName string) string {
	switch state {
	case core.StateClosed:
		return fmt.Sprintf("%s 님이 상담을 종료했습니다.", agentName)
	case core.StateSnoozed:
		return fmt.Sprintf("%s 님이 상담을 보류했습니다.", agentName)
	default:
		return fmt.Sprintf("%s 님이 상담을 다시 열었습니다.", agentName)
	}
}

// Assign 은 담당자를 지정·이관·해제한다. agentID 가 빈 문자열이면 해제다.
//
// 응대 권한만 있으면 "나에게" 와 "내 담당 해제" 만 한다. 남에게 넘기기, 남의 담당 풀기, 남이 맡은 상담 가져오기는
// 배정 권한(conversation.assign)이다 — 셋 다 다른 사람의 일을 바꾸는 일이다.
//
// expected 는 화면이 보던 담당자다(nil 이면 보내지 않았다, 빈 문자열이면 미배정을 봤다). 있으면 DB 의 담당자가 그 값일 때만 바꾼다(D39).
// "나에게" 를 미배정에서 누르면 expected 가 빈 문자열이라, 그 사이 다른 상담원이 가져갔으면 ErrAlreadyTaken 이다.
// 이관(담당자가 있는 상담을 넘기기)도 같고 그 사이 담당자가 바뀌었으면 ErrAssigneeChanged 다.
// 배정 권한이 없는 상담원은 expected 를 보내지 않아도 권한을 판정한 담당자를 조건으로 건다 — 판정과 쓰기 사이에
// 남이 가져간 상담을 덮어쓰면 "남이 맡은 상담 가져오기" 를 권한 없이 한 셈이다.
// 잠근 행의 담당자가 이미 요청한 사람이면(이미 미배정인데 해제를 요청한 것 포함) expected 와 무관하게 아무것도 쓰지 않고
// 지금 대화로 답한다 — 같은 사람이 두 번 눌렀다. 저장소가 잠근 행에서 판정하고 record 를 부르지 않는다.
func (s *Commands) Assign(ctx context.Context, actor Actor, convID, agentID string, expected *string) (core.Conversation, error) {
	before, err := s.q.authorizeAgentOn(ctx, actor, convID)
	if err != nil {
		return core.Conversation{}, err
	}
	if agentID != "" {
		normalized, err := id.Normalize(agentID)
		if err != nil {
			return core.Conversation{}, invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
		}
		agentID = normalized
	}
	if expected != nil && *expected != "" {
		normalized, err := id.Normalize(*expected)
		if err != nil {
			return core.Conversation{}, invalid("expected_assignee_format", "expected_assignee 형식이 올바르지 않습니다")
		}
		expected = &normalized
	}
	if !actor.Can(PermConversationAssign) {
		// 판정은 쓰기가 조건으로 걸 담당자로 한다. 화면이 보낸 값이 있으면 그것, 없으면 방금 읽은 값이다.
		if expected == nil {
			current := before.AssigneeAgentID
			expected = &current
		}
		toOther := agentID != "" && agentID != actor.AgentID
		fromOther := *expected != "" && *expected != actor.AgentID
		if toOther || fromOther {
			return core.Conversation{}, denied(ctx, PermConversationAssign)
		}
	}
	from, changed := before.AssigneeAgentID, false
	conv, err := s.write.AssignConversation(ctx, convID, agentID, expected, func(prev, next core.Conversation) core.ChangeRecord {
		from, changed = prev.AssigneeAgentID, true
		return assignRecord(actor, prev, next)
	})
	if errors.Is(err, core.ErrAssigneeChanged) {
		if *expected == "" {
			return core.Conversation{}, ErrAlreadyTaken
		}
		return core.Conversation{}, ErrAssigneeChanged
	}
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	if !changed {
		return conv, nil
	}
	// assignee 가 비면 해제다. 이름은 싣지 않는다. from 은 잠근 행에서 읽은 바꾸기 전 담당자다.
	slog.InfoContext(ctx, "conversation.assigned", "conversation_id", conv.ID, "assignee", agentID, "from", from)
	return conv, nil
}

// assignRecord 는 담당 변경의 시스템 줄과 업무 기록이다. 잠근 행의 바꾸기 전·뒤 값으로 정한다 — 화면이 본 값이 아니다.
// 담당 변경은 내부 활동이라 시스템 줄은 상담원만 본다. 업무 기록은 지정·이관·해제이고, 대상 상담원(agent_id)이 주체다 —
// "누가 이 고객을 맡았나" 를 세기 위해서. 미배정을 가져간 것은 assigned, 담당자가 있던 것을 넘긴 것만 transferred 다.
func assignRecord(actor Actor, prev, next core.Conversation) core.ChangeRecord {
	rec := core.ChangeRecord{Note: assignNote(actor.Name, next.AssigneeName)}
	switch {
	case next.AssigneeAgentID == "":
		rec.Event = core.ConversationEvent{AgentID: prev.AssigneeAgentID, Kind: core.EventUnassigned, Detail: map[string]any{"by": actor.AgentID}}
	case prev.AssigneeAgentID != "" && prev.AssigneeAgentID != next.AssigneeAgentID:
		rec.Event = core.ConversationEvent{AgentID: next.AssigneeAgentID, Kind: core.EventTransferred,
			Detail: map[string]any{"from": prev.AssigneeAgentID, "by": actor.AgentID}}
	default:
		rec.Event = core.ConversationEvent{AgentID: next.AssigneeAgentID, Kind: core.EventAssigned, Detail: map[string]any{"by": actor.AgentID}}
	}
	return rec
}

func assignNote(actorName, assigneeName string) string {
	if assigneeName == "" {
		return fmt.Sprintf("%s 님이 담당자를 해제했습니다.", actorName)
	}
	return fmt.Sprintf("%s 님이 담당자를 %s 님으로 지정했습니다.", actorName, assigneeName)
}

func (s *Commands) Follow(ctx context.Context, actor Actor, convID string, follow bool) error {
	if _, err := s.q.authorizeAgentOn(ctx, actor, convID); err != nil {
		return err
	}
	if follow {
		return translate(s.write.Follow(ctx, convID, actor.AgentID))
	}
	return translate(s.write.Unfollow(ctx, convID, actor.AgentID))
}

func (s *Queries) Followers(ctx context.Context, actor Actor, convID string) ([]core.Agent, error) {
	if _, err := s.authorizeAgentOn(ctx, actor, convID); err != nil {
		return nil, err
	}
	agents, err := s.read.Followers(ctx, convID)
	return agents, translate(err)
}

// AddTag 는 이름으로 태그를 붙인다. 없는 태그는 만든다 — 만드는 것은 태그 정의 관리(library.manage)다.
// 응대 권한만 있으면 이미 있는 태그만 붙인다. 안 그러면 붙이기가 정의 관리를 우회하는 길이 된다.
func (s *Commands) AddTag(ctx context.Context, actor Actor, convID, name string) ([]core.Tag, error) {
	name, err := trimmedRunes(name, 40, "tag_name_too_long", "태그 이름")
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, invalid("tag_name_required", "태그 이름이 비어 있습니다")
	}
	if _, err := s.q.authorizeAgentOn(ctx, actor, convID); err != nil {
		return nil, err
	}
	if !actor.Can(PermLibraryManage) {
		known, err := s.read.ListTags(ctx, actor.WorkspaceID)
		if err != nil {
			return nil, translate(err)
		}
		if !hasTag(known, name) {
			return nil, denied(ctx, PermLibraryManage)
		}
	}
	tag, err := s.write.EnsureTag(ctx, actor.WorkspaceID, name)
	if err != nil {
		return nil, translate(err)
	}
	if err := s.write.AddConversationTag(ctx, convID, tag.ID); err != nil {
		return nil, translate(err)
	}
	s.event(ctx, convID, actor.AgentID, core.EventTagged, map[string]any{"tag": tag.Name})
	tags, err := s.read.ConversationTags(ctx, convID)
	return tags, translate(err)
}

func (s *Commands) RemoveTag(ctx context.Context, actor Actor, convID, tagID string) ([]core.Tag, error) {
	if _, err := s.q.authorizeAgentOn(ctx, actor, convID); err != nil {
		return nil, err
	}
	tagID, err := id.Normalize(tagID)
	if err != nil {
		return nil, invalid("tag_id_format", "tag_id 형식이 올바르지 않습니다")
	}
	if err := s.write.RemoveConversationTag(ctx, convID, tagID); err != nil {
		return nil, translate(err)
	}
	tags, err := s.read.ConversationTags(ctx, convID)
	return tags, translate(err)
}

// hasTag 는 이름이 같은 태그가 있는지다. 저장소의 유일 색인과 같게 대소문자를 가리지 않는다.
func hasTag(tags []core.Tag, name string) bool {
	for _, t := range tags {
		if strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

// ── 상담 삭제 ──────────────────────────────────────────────────────

// DeleteConversation 은 상담을 가린다(D29). 기록은 지우지 않는다 — 행과 메시지는 그대로고, 조회 경로마다 없는 것으로 본다.
// 사유는 필수다. 가린 사람·시각·사유가 감사 로그와 업무 기록에 남는다.
func (s *Commands) DeleteConversation(ctx context.Context, actor Actor, convID, reason string) error {
	if err := require(ctx, actor, PermConversationDelete); err != nil {
		return err
	}
	reason, err := trimmedRunes(reason, 200, "delete_reason_too_long", "삭제 사유")
	if err != nil {
		return err
	}
	if reason == "" {
		return invalid("delete_reason_required", "삭제 사유가 비어 있습니다")
	}
	conv, err := s.q.Authorize(ctx, actor, convID)
	if err != nil {
		return err
	}
	if err := translate(s.write.DeleteConversation(ctx, conv.ID, actor.AgentID, reason)); err != nil {
		return err
	}
	// 사유 본문은 운영 로그에도 업무 기록에도 싣지 않는다. 사유는 감사 로그와 삭제됨 보기에만 있다.
	// 업무 기록은 응대 권한이면 누구나 보는 표라서 거기 실으면 사유가 삭제 권한 밖으로 샌다.
	slog.InfoContext(ctx, "conversation.deleted", "conversation_id", conv.ID, "state", conv.State)
	s.audit(ctx, actor.WorkspaceID, actor, "conversation.delete", conv.ID, map[string]any{"reason": reason})
	s.event(ctx, conv.ID, actor.AgentID, core.EventDeleted, nil)
	return nil
}

// DeletedConversation 은 가린 상담을 읽기 전용으로 연다. 되살릴지 정하려면 무엇을 가렸는지 봐야 한다.
// 가린 상담의 메시지는 이 길로만, 상담 삭제 권한이 있는 상담원에게만 열린다. 다른 모든 경로는 그대로 "없다" 다.
func (s *Queries) DeletedConversation(ctx context.Context, actor Actor, convID string, afterSeq int64, limit int) (core.Conversation, core.Deletion, []core.Message, error) {
	if err := require(ctx, actor, PermConversationDelete); err != nil {
		return core.Conversation{}, core.Deletion{}, nil, err
	}
	// id 형식 오류가 after_seq 오류보다 먼저다. 그래서 형식만 여기서 먼저 보고, 읽기는 아래 공통 길이 다시 정규화해서 한다.
	if _, err := id.Normalize(convID); err != nil {
		return core.Conversation{}, core.Deletion{}, nil, invalid("conversation_id_format", "conversation_id 형식이 올바르지 않습니다")
	}
	if afterSeq < 0 {
		return core.Conversation{}, core.Deletion{}, nil, invalid("after_seq_negative", "after_seq 는 0 이상이어야 합니다")
	}
	conv, err := s.conversationIncludingDeleted(ctx, actor, convID)
	if err != nil {
		return core.Conversation{}, core.Deletion{}, nil, err
	}
	if conv.DeletedAt == nil {
		return core.Conversation{}, core.Deletion{}, nil, ErrNotFound
	}
	deletion, err := s.read.ConversationDeletion(ctx, conv.ID)
	if err != nil {
		return core.Conversation{}, core.Deletion{}, nil, translate(err)
	}
	msgs, err := s.read.ListMessages(ctx, conv.ID, afterSeq, limit, true)
	if err != nil {
		return core.Conversation{}, core.Deletion{}, nil, translate(err)
	}
	if msgs == nil {
		msgs = []core.Message{}
	}
	return conv, deletion, msgs, nil
}

// RestoreConversation 은 가린 상담을 되돌린다. 가린 상담은 Authorize 가 없다고 답하므로 소속을 여기서 직접 본다.
// 그 사이 회원에게 지난 상담이 됐으면 종료 상태로 돌아온다(저장소가 정한다).
func (s *Commands) RestoreConversation(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	if err := require(ctx, actor, PermConversationDelete); err != nil {
		return core.Conversation{}, err
	}
	conv, err := s.q.conversationIncludingDeleted(ctx, actor, convID)
	if err != nil {
		return core.Conversation{}, err
	}
	if err := translate(s.write.RestoreConversation(ctx, conv.ID)); err != nil {
		return core.Conversation{}, err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "conversation.restore", conv.ID, nil)
	s.event(ctx, conv.ID, actor.AgentID, core.EventRestored, nil)
	restored, err := s.read.Conversation(ctx, conv.ID)
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	// state 는 되살린 뒤의 상태다. 지난 상담이었으면 삭제 전과 다르다.
	slog.InfoContext(ctx, "conversation.restored", "conversation_id", restored.ID, "state", restored.State)
	return restored, nil
}

// conversationIncludingDeleted 는 가린 상담까지 포함해 이 워크스페이스의 대화 하나를 읽는다.
// Authorize 는 가린 상담을 없다고 답한다. 가린 상담을 다루는 길(삭제됨 보기, 되살리기, 업무 기록 좁히기)은 소속을 여기서 본다.
// 오류는 id 형식(invalid) → 저장소(없으면 not_found) → 다른 워크스페이스(not_found) 순서다.
// 권한(상담 삭제)은 이 함수가 보지 않는다. 부르는 쪽이 먼저 본다 — 거절할지, 다른 문으로 돌릴지가 길마다 다르다.
func (s *Queries) conversationIncludingDeleted(ctx context.Context, actor Actor, convID string) (core.Conversation, error) {
	convID, err := id.Normalize(convID)
	if err != nil {
		return core.Conversation{}, invalid("conversation_id_format", "conversation_id 형식이 올바르지 않습니다")
	}
	conv, err := s.read.Conversation(ctx, convID)
	if err != nil {
		return core.Conversation{}, translate(err)
	}
	if conv.WorkspaceID != actor.WorkspaceID {
		return core.Conversation{}, ErrNotFound
	}
	return conv, nil
}
