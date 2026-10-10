package core

import (
	"time"
)

// Role 은 상담원의 역할이다. 무엇을 할 수 있는지는 역할이 아니라 권한으로 묻는다 — 대응표는 app 에 한 곳만 있다.
type Role string

const (
	RoleAdmin    Role = "ADMIN"
	RoleStaff    Role = "STAFF"
	RolePartTime Role = "PART_TIME"
)

// Valid 는 정해진 셋 중 하나인지 본다. 권한 판정이 아니라 입력 검증이다.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleStaff, RolePartTime:
		return true
	default:
		return false
	}
}

// TeamMember 는 팀 화면에서 보는 상담원이다. Agent 보다 운영 정보가 많다.
type TeamMember struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrLastAdmin 은 활성 관리자가 0명이 되는 변경이다. 관리자가 없으면 아무도 팀을 못 고친다.
var ErrLastAdmin = NewError("last_admin", "core: 활성 관리자가 한 명뿐이라 그 사람을 강등하거나 비활성화하지 않았다")

type Invite struct {
	WorkspaceID string    `json:"-"`
	Email       string    `json:"email"`
	Name        string    `json:"name"`
	Role        Role      `json:"role"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	// Token 은 만들 때만 한 번 돌려준다. 저장은 해시만 한다.
	Token string `json:"token,omitempty"`
}

// PendingSignup 은 상담원이 직접 가입하겠다는 요청이다. 저장소는 이걸 승인 대기 계정으로 남긴다.
type PendingSignup struct {
	WorkspaceID string
	Email       string
	Name        string
	Password    string
}
