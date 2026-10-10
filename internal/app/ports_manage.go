package app

import (
	"context"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 관리 화면 셋(D36)의 포트다. 상담원 이름, 빠른 답변 고치기, 위젯 FAQ. 쓰는 쪽(manage.go)이 정의하고 Store 에 더하지 않는다.

// AgentNameWriter 는 상담원 이름을 바꾼다.
type AgentNameWriter interface {
	// RenameAgent 는 그 워크스페이스의 승인된 상담원(활성·비활성) 이름을 바꾸고 바꾸기 전 이름을 돌려준다.
	// 다른 워크스페이스의 상담원, 승인 대기 계정, 없는 id 는 core.ErrNotFound 다.
	RenameAgent(ctx context.Context, wsID, agentID, name string) (previous string, err error)
}

// SnippetEditor 는 빠른 답변을 제자리에서 고친다. 만들기·지우기는 DirectoryWriter 에 있다.
type SnippetEditor interface {
	// UpdateSnippet 은 그 워크스페이스의 빠른 답변을 고친다. 다른 빠른 답변과 단축어가 겹치면(대소문자 무시) core.ErrDuplicate,
	// 없거나 다른 워크스페이스의 것이면 core.ErrNotFound 다. 겹침 판정은 DB 의 unique 인덱스가 한다.
	UpdateSnippet(ctx context.Context, wsID, snippetID, shortcut, title, body string) (core.Snippet, error)
}

// WidgetFAQReader 는 위젯 FAQ 를 읽는다.
type WidgetFAQReader interface {
	// WidgetFAQ 는 저장된 목록을 저장한 순서로 준다. 한 번도 저장하지 않았으면 core.ErrSettingMissing 이다.
	// 빈 목록을 저장했으면 오류 없이 빈 목록이다 — "FAQ 를 두지 않는다" 와 "아직 안 정했다" 는 다르다.
	WidgetFAQ(ctx context.Context, wsID string) ([]core.FAQItem, error)
}

// WidgetFAQWriter 는 위젯 FAQ 를 통째로 바꾼다. 항목 하나씩 고치지 않는다 — 순서 바꾸기도 목록 전체를 다시 쓰는 일이다.
type WidgetFAQWriter interface {
	PutWidgetFAQ(ctx context.Context, wsID string, items []core.FAQItem, updatedBy string) error
}

// ManageWriter 는 명령이 받는 관리 화면 쓰기 포트다.
type ManageWriter interface {
	AgentNameWriter
	SnippetEditor
	WidgetFAQWriter
}

// ManageStore 는 조립부가 한 번 꽂는 관리 화면 저장소다.
type ManageStore interface {
	ManageWriter
	WidgetFAQReader
}

// UseManage 는 관리 화면 저장소를 꽂는다. 조립부(cmd/server)가 한 번 부른다. 조회는 읽기 포트만 받는다.
func (a *App) UseManage(store ManageStore) {
	a.Queries.faq = store
	a.Commands.manage = store
}
