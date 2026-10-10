package app

import (
	"context"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 토큰 보안 묶음(D46)의 포트다. 쓰는 쪽(session.go·member.go)이 정의하고 Store 에 더하지 않는다.
// 저장소가 이 능력을 가지면 New 가 알아서 꽂는다(pgstore 와 그것을 감싼 테스트 대역이 가진다).
// 없으면 사용 시각 갱신·고객 세션 상한은 하지 않고, 티켓은 ErrNoSessionStore 다.

// SessionLookup 은 세션 참조로 주체를 찾는다. 판정(절대 수명·유휴 한도·활성)은 SubjectByToken 과 같다.
type SessionLookup interface {
	SubjectBySession(ctx context.Context, ref core.SessionRef) (core.Subject, error)
}

// SessionKeeper 는 세션의 부수 상태를 바꾼다.
type SessionKeeper interface {
	// TouchSession 은 마지막 사용 시각을 지금으로 올린다. 1분 안에 이미 올렸으면 쓰지 않는다. 유휴 한도가 지금 없는 세션도 적는다(감사 21).
	TouchSession(ctx context.Context, ref core.SessionRef) error
	// IssueWSTicket 은 그 세션에 묶인 ttl 짜리 일회용 티켓을 만든다. 세션이 없으면 core.ErrNotFound.
	IssueWSTicket(ctx context.Context, ref core.SessionRef, ttl time.Duration) (ticket string, expires time.Time, err error)
	// RedeemWSTicket 은 티켓을 쓰고 그 세션의 주체를 준다. 동시에 두 번 써도 하나만 이긴다(DB 가 지킨다).
	// 없음·이미 씀·만료·세션 죽음은 모두 core.ErrNotFound.
	RedeemWSTicket(ctx context.Context, ticket string) (core.Subject, error)
	// PruneCustomerSessions 는 고객 한 명의 세션을 최신 keep 개만 남긴다.
	PruneCustomerSessions(ctx context.Context, customerID string, keep int) (int64, error)
}

// SessionStore 는 New 가 저장소에서 찾아 꽂는 세션 보안 포트 전부다.
type SessionStore interface {
	SessionLookup
	SessionKeeper
}

// SessionPolicy 는 조립부가 설정에서 꽂는 세션 수명의 숫자다. 0 이면 그 주체의 유휴 만료가 없다(테스트 기본값).
type SessionPolicy struct {
	// CustomerIdle 은 고객 세션(익명·회원)을 이만큼 안 쓰면 끝난다. 운영 기본은 0 — 고객 세션에는 유휴 만료가 없다(CHAT_SESSION_IDLE_CUSTOMER, D46 고침).
	// 세션 행에 적지 않고 판정 시점에 이 값으로 본다(CustomerIdleJudge) — 켜면 이미 있는 익명·회원 세션 모두에 걸린다(감사 21).
	CustomerIdle time.Duration
	// AgentIdle 은 상담원 세션을 이만큼 안 쓰면 끝난다. 운영 기본 12시간(CHAT_SESSION_IDLE_AGENT). 세션을 만들 때 행에 적는다.
	AgentIdle time.Duration
}

// CustomerIdleJudge 는 고객 세션의 유휴 한도를 판정 시점의 값으로 보는 저장소다. 저장소가 가지면 SetSessionPolicy 가 꽂는다.
// 행에 적어 두면 설정을 켜도 그 전에 연 세션(기본 0 으로 연 익명·회원 전부)에는 걸리지 않는다(감사 21 재현).
type CustomerIdleJudge interface {
	SetCustomerIdle(d time.Duration)
}

// SetSessionPolicy 는 세션 수명의 숫자를 꽂는다. 조립부(cmd/server)가 한 번 부른다.
func (a *App) SetSessionPolicy(p SessionPolicy) {
	a.Commands.policy = p
	if a.Commands.idleJudge != nil {
		a.Commands.idleJudge.SetCustomerIdle(p.CustomerIdle)
	}
}
