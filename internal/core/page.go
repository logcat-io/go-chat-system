package core

// 목록 조회의 페이지 한도. 저장소 포트의 계약이다(app/ports.go 의 각 메서드 주석).
// limit 이 0 이하면 저장소는 ...Default 개를, ...Max 를 넘으면 ...Max 개를 준다. 더 필요하면 커서로 이어 읽는다.
// 전달 계층이 limit 을 못 읽었을 때(없음, 숫자 아님, 0 이하) 넣는 값도 같은 ...Default 다 — 기본값은 여기 하나다.
const (
	// ListConversations(받은함). InboxFilter.Limit.
	InboxPageMax     = 200
	InboxPageDefault = 50
	// CustomerConversations(위젯의 대화 목록).
	CustomerConversationsPageMax     = 100
	CustomerConversationsPageDefault = 30
	// ListCustomerHistory(이전 상담 더 보기).
	PriorHistoryPageMax     = 200
	PriorHistoryPageDefault = 50
	// ListMessagesBefore(위로 더 읽기).
	MessagesBeforePageMax     = 200
	MessagesBeforePageDefault = 50
	// ListMessages(번호 뒤 이어 읽기).
	MessagesPageMax     = 500
	MessagesPageDefault = 200
	// ListEvents(업무 기록). EventFilter.Limit.
	EventsPageMax     = 500
	EventsPageDefault = 100
	// AuditLog(감사 로그).
	AuditPageMax     = 500
	AuditPageDefault = 100
	// SearchMessages(본문 검색).
	SearchPageMax     = 100
	SearchPageDefault = 30
)
