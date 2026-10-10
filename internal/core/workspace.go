package core

import (
	"strings"
	"time"
)

// DefaultWorkspaceID 는 마이그레이션이 만든 첫 워크스페이스다. 기존 데이터가 전부 여기 속한다.
// 설치형 단일 워크스페이스라(D35) 토큰 없이 들어오는 입구(위젯·회원 세션·로그인·상담원 가입)는 전부 여기로 간다.
const DefaultWorkspaceID = "00000000-0000-0000-0000-000000000001"

type Workspace struct {
	ID             string    `json:"id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	MemberSecret   string    `json:"-"`
	AllowedOrigins []string  `json:"allowed_origins"`
	CreatedAt      time.Time `json:"created_at"`
}

// OriginAllowed 는 허용 목록이 비어 있으면 전부, 아니면 목록에 있는 오리진만 받는다.
func (w Workspace) OriginAllowed(origin string) bool {
	if len(w.AllowedOrigins) == 0 {
		return true
	}
	origin = NormalizeOrigin(origin)
	for _, o := range w.AllowedOrigins {
		if strings.ToLower(o) == origin {
			return true
		}
	}
	return false
}

// NormalizeOrigin 은 오리진을 비교할 모양으로 바꾼다. 스킴과 호스트는 대소문자를 가리지 않고, 끝 슬래시는 뗀다.
func NormalizeOrigin(origin string) string {
	return strings.ToLower(strings.TrimRight(origin, "/"))
}

var ErrSlugTaken = NewError("slug_taken", "core: 이 슬러그를 다른 워크스페이스가 이미 쓴다")

type AuditEntry struct {
	ID          int64          `json:"id"`
	WorkspaceID string         `json:"-"`
	ActorID     string         `json:"actor_id,omitempty"`
	ActorName   string         `json:"actor_name"`
	Action      string         `json:"action"`
	Target      string         `json:"target,omitempty"`
	Detail      map[string]any `json:"detail"`
	CreatedAt   time.Time      `json:"created_at"`
}
