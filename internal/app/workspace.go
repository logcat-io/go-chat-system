package app

import (
	"context"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 고객 프로필, 검색, 빠른 답변처럼 대화 한 건에 매이지 않는 기능.

func (s *Queries) Customer(ctx context.Context, actor Actor, customerID string) (core.Customer, error) {
	customerID, err := id.Normalize(customerID)
	if err != nil {
		return core.Customer{}, invalid("customer_id_format", "customer_id 형식이 올바르지 않습니다")
	}
	if actor.Kind == ActorCustomer && actor.CustomerID != customerID {
		return core.Customer{}, ErrForbidden
	}
	c, err := s.read.Customer(ctx, customerID)
	if err != nil {
		return core.Customer{}, translate(err)
	}
	if c.WorkspaceID != actor.WorkspaceID {
		return core.Customer{}, ErrNotFound
	}
	return c, nil
}

// UpdateCustomer 는 프로필을 덮어쓴다.
// 고객 본인은 운영 메모와 속성을 건드리지 못한다. 그건 상담원이 쓰는 기록이다.
// 상담원은 고객 수정 권한(customer.edit)이 있어야 한다. 보기만 하는 건 응대에 필요해 누구나 한다.
func (s *Commands) UpdateCustomer(ctx context.Context, actor Actor, customerID string, p core.CustomerProfile) (core.Customer, error) {
	if actor.Kind == ActorAgent {
		if err := require(ctx, actor, PermCustomerEdit); err != nil {
			return core.Customer{}, err
		}
	}
	customerID, err := id.Normalize(customerID)
	if err != nil {
		return core.Customer{}, invalid("customer_id_format", "customer_id 형식이 올바르지 않습니다")
	}
	current, err := s.q.Customer(ctx, actor, customerID)
	if err != nil {
		return core.Customer{}, err
	}
	if actor.Kind == ActorCustomer {
		p.Note, p.Attributes = current.Note, current.Attributes
	}
	name, err := trimmedRunes(p.DisplayName, maxNameRunes, "name_too_long", "이름")
	if err != nil {
		return core.Customer{}, err
	}
	if name == "" {
		return core.Customer{}, invalid("name_required", "이름이 비어 있습니다")
	}
	p.DisplayName = name
	if p.Note, err = trimmedRunes(p.Note, 2000, "note_too_long", "메모"); err != nil {
		return core.Customer{}, err
	}
	c, err := s.write.UpdateCustomer(ctx, customerID, p)
	return c, translate(err)
}

// Search 는 전체 검색 권한(search.all)이 쓴다. 워크스페이스의 모든 대화에서 내부 메모까지 찾는다.
func (s *Queries) Search(ctx context.Context, actor Actor, query string, limit int) ([]core.SearchHit, error) {
	if err := require(ctx, actor, PermSearchAll); err != nil {
		return nil, err
	}
	hits, err := s.read.SearchMessages(ctx, actor.WorkspaceID, query, true, limit)
	return hits, translate(err)
}

// Snippets 는 응대에 쓰는 빠른 답변 목록이다. 쓰기는 누구나, 만들고 고치고 지우기는 library.manage 다(고치기는 manage.go).
func (s *Queries) Snippets(ctx context.Context, actor Actor) ([]core.Snippet, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return nil, err
	}
	sn, err := s.read.ListSnippets(ctx, actor.WorkspaceID)
	return sn, translate(err)
}

func (s *Commands) CreateSnippet(ctx context.Context, actor Actor, shortcut, title, body string) (core.Snippet, error) {
	if err := require(ctx, actor, PermLibraryManage); err != nil {
		return core.Snippet{}, err
	}
	shortcut, title, body, err := snippetFields(shortcut, title, body)
	if err != nil {
		return core.Snippet{}, err
	}
	sn, err := s.write.CreateSnippet(ctx, actor.WorkspaceID, shortcut, title, body)
	return sn, translate(err)
}

// snippetFields 는 빠른 답변의 세 칸을 다듬고 검증한다. 만들기와 고치기가 같은 규칙을 쓴다.
// 단축어 앞의 "/" 는 뗀다 — 작성칸에서 치는 모양 그대로 적어도 된다.
func snippetFields(shortcut, title, body string) (string, string, string, error) {
	shortcut, err := trimmedRunes(strings.TrimPrefix(strings.TrimSpace(shortcut), "/"), 40, "shortcut_too_long", "단축어")
	if err != nil {
		return "", "", "", err
	}
	title, err = trimmedRunes(title, 80, "title_too_long", "제목")
	if err != nil {
		return "", "", "", err
	}
	body, err = trimmedRunes(body, MaxBodyRunes, "body_too_long", "본문")
	if err != nil {
		return "", "", "", err
	}
	// 입력 순서로 본다. 둘 이상 비어도 문구가 매번 같다. 사유 코드는 필드마다 리터럴이다(감사 24).
	switch {
	case shortcut == "":
		return "", "", "", invalid("shortcut_required", "단축어가 비어 있습니다")
	case title == "":
		return "", "", "", invalid("title_required", "제목이 비어 있습니다")
	case body == "":
		return "", "", "", invalid("body_required", "본문이 비어 있습니다")
	}
	return shortcut, title, body, nil
}

func (s *Commands) DeleteSnippet(ctx context.Context, actor Actor, snippetID string) error {
	if err := require(ctx, actor, PermLibraryManage); err != nil {
		return err
	}
	snippetID, err := id.Normalize(snippetID)
	if err != nil {
		return invalid("snippet_id_format", "snippet_id 형식이 올바르지 않습니다")
	}
	return translate(s.write.DeleteSnippet(ctx, actor.WorkspaceID, snippetID))
}

func (s *Queries) Agents(ctx context.Context, actor Actor) ([]core.Agent, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return nil, err
	}
	agents, err := s.read.ListAgents(ctx, actor.WorkspaceID)
	return agents, translate(err)
}

func (s *Queries) Tags(ctx context.Context, actor Actor) ([]core.Tag, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return nil, err
	}
	tags, err := s.read.ListTags(ctx, actor.WorkspaceID)
	return tags, translate(err)
}

// ── 업무 기록 ───────────────────────────────────────────────────────

// WorkLog 는 "누가 어떤 고객의 어떤 대화에 무엇을 했나" 다. 전체 조회 권한(work.view_all)이 있으면 팀 전체를 본다.
// 없으면 조회가 자기 id 로 고정된다. 남의 id 를 콕 집어 물으면 조용히 바꾸지 않고 거절한다 — 빈 결과는 "기록이 없다" 로 읽힌다.
func (s *Queries) WorkLog(ctx context.Context, actor Actor, f core.EventFilter) ([]core.ConversationEvent, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return nil, err
	}
	if !actor.Can(PermWorkViewAll) {
		if f.AgentID != "" && f.AgentID != actor.AgentID {
			return nil, denied(ctx, PermWorkViewAll)
		}
		f.AgentID = actor.AgentID
	}
	f.WorkspaceID = actor.WorkspaceID
	// 삭제된 상담의 기록은 삭제 권한이 있는 사람만 본다. 다른 경로에서 없는 상담이 업무 기록에서만 보이면 안 된다.
	f.IncludeDeleted = actor.Can(PermConversationDelete)
	if f.ConversationID != "" {
		if err := s.authorizeWorkConversation(ctx, actor, f.ConversationID); err != nil {
			return nil, err
		}
	}
	events, err := s.read.ListEvents(ctx, f)
	return events, translate(err)
}

// authorizeWorkConversation 은 업무 기록을 대화 하나로 좁혀 볼 수 있는지다. 삭제 권한이 있으면 삭제한 상담으로도 좁힌다 —
// 삭제됨 보기에서 그 상담에 누가 무엇을 했는지 봐야 되살릴지 정할 수 있다. 다른 워크스페이스의 대화는 여전히 404 다.
// 그 밖에는 대화 조회와 같은 문(Authorize)을 지난다.
func (s *Queries) authorizeWorkConversation(ctx context.Context, actor Actor, convID string) error {
	if actor.Can(PermConversationDelete) {
		conv, err := s.conversationIncludingDeleted(ctx, actor, convID)
		if err != nil {
			return err
		}
		if conv.DeletedAt != nil {
			return nil
		}
	}
	_, err := s.Authorize(ctx, actor, convID)
	return err
}

// WorkSummaryDefaultRange 는 업무 집계의 기간을 주지 않았을 때의 길이다(끝은 DB 시계의 지금).
const WorkSummaryDefaultRange = 7 * 24 * time.Hour

// WorkSummaryView 는 업무 집계와 실제로 센 기간이다.
type WorkSummaryView struct {
	From, To time.Time
	Agents   []core.AgentWork
}

// WorkSummary 는 기간 안의 상담원별 집계다. 대시보드의 첫 숫자. 전체 조회 권한이 없으면 자기 줄만 남는다.
// to 가 비면 끝은 DB 시계의 지금(DEF-148), from 이 비면 그 7일 전이다.
func (s *Queries) WorkSummary(ctx context.Context, actor Actor, from, to time.Time) (WorkSummaryView, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return WorkSummaryView{}, err
	}
	to, err := s.windowEnd(ctx, to)
	if err != nil {
		return WorkSummaryView{}, err
	}
	if from.IsZero() {
		from = to.Add(-WorkSummaryDefaultRange)
	}
	out, err := s.read.WorkSummary(ctx, actor.WorkspaceID, from, to, actor.Can(PermConversationDelete))
	if err != nil {
		return WorkSummaryView{}, translate(err)
	}
	view := WorkSummaryView{From: from, To: to, Agents: out}
	if actor.Can(PermWorkViewAll) {
		return view, nil
	}
	mine := make([]core.AgentWork, 0, 1)
	for _, w := range out {
		if w.AgentID == actor.AgentID {
			mine = append(mine, w)
		}
	}
	view.Agents = mine
	return view, nil
}
