package app

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 관리 화면 셋(D36). 상담원 이름 바꾸기, 빠른 답변 고치기, 위젯 FAQ 편집.
// 포트는 ports_manage.go 에 있다.

// ── 상담원 이름 ─────────────────────────────────────────────────────

// AgentName 은 이름을 바꾼 결과다. 화면은 다듬어진 이름을 그대로 다시 그린다.
type AgentName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RenameSelf 는 상담원이 자기 이름을 바꾼다. 권한은 묻지 않는다 — 자기 이름은 누구나 고친다.
func (s *Commands) RenameSelf(ctx context.Context, actor Actor, name string) (AgentName, error) {
	if err := requireAgent(actor); err != nil {
		return AgentName{}, err
	}
	return s.renameAgent(ctx, actor, actor.AgentID, name)
}

// RenameAgent 는 팀 관리 권한(team.manage)이 다른 상담원의 이름을 바꾼다. 자기 자신도 이 길로 바꿀 수 있다.
// 승인 대기 계정은 없는 것으로 본다. 이름은 승인할 때까지 신청한 그대로다.
func (s *Commands) RenameAgent(ctx context.Context, actor Actor, agentID, name string) (AgentName, error) {
	if err := require(ctx, actor, PermTeamManage); err != nil {
		return AgentName{}, err
	}
	agentID, err := id.Normalize(agentID)
	if err != nil {
		return AgentName{}, invalid("agent_id_format", "agent_id 형식이 올바르지 않습니다")
	}
	return s.renameAgent(ctx, actor, agentID, name)
}

// renameAgent 는 가입·초대와 같은 이름 규칙(앞뒤 공백 정리, 빈 값 거절, maxNameRunes)을 쓴다.
// 감사 로그에는 바꾸기 전과 뒤의 이름을 남긴다. 운영 로그에는 남기지 않는다(D30).
func (s *Commands) renameAgent(ctx context.Context, actor Actor, agentID, name string) (AgentName, error) {
	name, err := trimmedRunes(name, maxNameRunes, "name_too_long", "이름")
	if err != nil {
		return AgentName{}, err
	}
	if name == "" {
		return AgentName{}, invalid("name_required", "이름이 비어 있습니다")
	}
	previous, err := s.manage.RenameAgent(ctx, actor.WorkspaceID, agentID, name)
	if err != nil {
		return AgentName{}, translate(err)
	}
	s.audit(ctx, actor.WorkspaceID, actor, "agent.name", agentID, map[string]any{"from": previous, "to": name})
	return AgentName{ID: agentID, Name: name}, nil
}

// ── 빠른 답변 고치기 ────────────────────────────────────────────────

// UpdateSnippet 은 빠른 답변의 단축어·제목·본문을 고친다. 만들 때와 같은 규칙(snippetFields)이고 권한도 같다(library.manage).
// 다른 빠른 답변과 단축어가 겹치면 ErrConflict 다.
func (s *Commands) UpdateSnippet(ctx context.Context, actor Actor, snippetID, shortcut, title, body string) (core.Snippet, error) {
	if err := require(ctx, actor, PermLibraryManage); err != nil {
		return core.Snippet{}, err
	}
	snippetID, err := id.Normalize(snippetID)
	if err != nil {
		return core.Snippet{}, invalid("snippet_id_format", "snippet_id 형식이 올바르지 않습니다")
	}
	shortcut, title, body, err = snippetFields(shortcut, title, body)
	if err != nil {
		return core.Snippet{}, err
	}
	sn, err := s.manage.UpdateSnippet(ctx, actor.WorkspaceID, snippetID, shortcut, title, body)
	return sn, translate(err)
}

// ── 위젯 FAQ ────────────────────────────────────────────────────────

// 위젯 FAQ 의 한도다. 테스트가 리터럴로 고정한다.
const (
	maxFAQItems         = 10
	maxFAQQuestionRunes = 80
	maxFAQAnswerRunes   = 500
)

// defaultFAQ 는 한 번도 저장하지 않은 워크스페이스의 FAQ 다. 위젯에 고정돼 있던 세 줄 그대로다 — 기존 설치의 화면이 바뀌지 않는다.
// 호출할 때마다 새 슬라이스를 준다. 받은 쪽이 고쳐도 다음 응답이 흔들리지 않는다.
func defaultFAQ() []core.FAQItem {
	return []core.FAQItem{
		{Question: "배송은 얼마나 걸리나요?", Answer: "결제 확인 후 영업일 기준 2~3일 안에 받아보실 수 있습니다."},
		{Question: "교환·반품은 어떻게 하나요?", Answer: "수령 후 7일 이내에 상담으로 요청해 주시면 안내해 드립니다."},
		{Question: "주문 내역은 어디서 보나요?", Answer: "로그인 후 마이페이지 > 주문 내역에서 확인하실 수 있습니다."},
	}
}

// WidgetFAQ 는 관리 화면이 보는 FAQ 다. IsDefault 는 아직 저장한 적이 없어 기본 세 줄을 보여 주는 중이라는 뜻이다.
type WidgetFAQ struct {
	Items     []core.FAQItem `json:"items"`
	IsDefault bool           `json:"is_default"`
}

// widgetFAQ 는 저장된 목록, 없으면 기본 목록이다. 빈 목록을 저장했으면 빈 목록이다 — 기본값으로 되돌아가지 않는다.
func (s *Queries) widgetFAQ(ctx context.Context, wsID string) (WidgetFAQ, error) {
	items, err := s.faq.WidgetFAQ(ctx, wsID)
	if errors.Is(err, core.ErrSettingMissing) {
		return WidgetFAQ{Items: defaultFAQ(), IsDefault: true}, nil
	}
	if err != nil {
		return WidgetFAQ{}, translate(err)
	}
	if items == nil {
		items = []core.FAQItem{}
	}
	return WidgetFAQ{Items: items}, nil
}

// AdminWidgetFAQ 는 워크스페이스 관리 권한(workspace.manage)이 본다. 위젯 모양과 같은 권한이다.
func (s *Queries) AdminWidgetFAQ(ctx context.Context, actor Actor) (WidgetFAQ, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return WidgetFAQ{}, err
	}
	return s.widgetFAQ(ctx, actor.WorkspaceID)
}

// UpdateWidgetFAQ 는 목록을 통째로 바꾼다. 추가·수정·삭제·순서 바꾸기가 전부 이 한 번이다.
// 다 검증한 뒤에 저장하므로 반쯤 저장되는 일이 없다. 열린 위젯은 다음 설정 조회 때 새 목록을 받는다(실시간이 아니다).
func (s *Commands) UpdateWidgetFAQ(ctx context.Context, actor Actor, items []core.FAQItem) (WidgetFAQ, error) {
	if err := require(ctx, actor, PermWorkspaceManage); err != nil {
		return WidgetFAQ{}, err
	}
	clean, err := normalizeFAQ(items)
	if err != nil {
		return WidgetFAQ{}, err
	}
	if err := translate(s.manage.PutWidgetFAQ(ctx, actor.WorkspaceID, clean, actor.AgentID)); err != nil {
		return WidgetFAQ{}, err
	}
	s.audit(ctx, actor.WorkspaceID, actor, "widget_faq.update", "", map[string]any{"items": len(clean)})
	return s.q.widgetFAQ(ctx, actor.WorkspaceID)
}

// normalizeFAQ 는 앞뒤 공백을 떼고 한도를 본다. 개수를 먼저 보고, 그다음 항목을 입력 순서대로 질문·답 순으로 본다.
// 여러 곳이 틀려도 문구는 늘 첫 오류 하나라 같은 입력에 같은 답이 나간다. 길이는 바이트가 아니라 글자로 센다.
func normalizeFAQ(items []core.FAQItem) ([]core.FAQItem, error) {
	if len(items) > maxFAQItems {
		return nil, invalid("faq_too_many", "자주 묻는 질문은 %d개까지입니다", maxFAQItems)
	}
	out := make([]core.FAQItem, 0, len(items))
	for i, it := range items {
		n := i + 1
		q, a := strings.TrimSpace(it.Question), strings.TrimSpace(it.Answer)
		switch {
		case q == "":
			return nil, invalid("faq_question_required", "%d번째 질문이 비어 있습니다", n)
		case utf8.RuneCountInString(q) > maxFAQQuestionRunes:
			return nil, invalid("faq_question_too_long", "%d번째 질문은 %d자까지입니다", n, maxFAQQuestionRunes)
		case a == "":
			return nil, invalid("faq_answer_required", "%d번째 답이 비어 있습니다", n)
		case utf8.RuneCountInString(a) > maxFAQAnswerRunes:
			return nil, invalid("faq_answer_too_long", "%d번째 답은 %d자까지입니다", n, maxFAQAnswerRunes)
		}
		out = append(out, core.FAQItem{Question: q, Answer: a})
	}
	return out, nil
}
