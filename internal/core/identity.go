package core

import "time"

type Agent struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Role        Role   `json:"role,omitempty"`
}

// Subject 는 세션이 가리키는 주체다. 둘 중 하나만 채워진다. 소속 워크스페이스는 항상 있다.
type Subject struct {
	Kind        string // AGENT | CUSTOMER
	WorkspaceID string
	AgentID     string
	CustomerID  string
	Name        string
	Role        Role // AGENT 일 때
	// Member 는 고객이 호스트가 서명한 회원(external_id 가 있다)일 때 true 다. 익명 입구는 회원 세션을 이어 주지 않는다(D46).
	Member bool
	// Session 은 이 주체를 가리키는 세션 행의 참조다. 평문 토큰이 아니다 — 토큰 해시의 16진수다.
	// WebSocket 티켓·사용 시각 갱신·세션이 끝났다는 알림이 이 값으로 세션을 찾는다. 로그에 싣지 않는다(D30).
	Session SessionRef
	// UsedAt 은 세션을 마지막으로 쓴 시각이다(분 단위로 묶어 갱신한다, D46). 유휴 만료의 기준이다.
	UsedAt time.Time
}

// SessionRef 는 세션 행을 가리키는 값이다. 토큰 해시(SHA-256)의 16진수라 평문 토큰으로 되돌릴 수 없다.
// DB 의 sessions.token_hash 를 그대로 옮긴 값이고, 세션이 끝났다는 알림(chat_session_ended)도 같은 꼴로 온다.
type SessionRef string
