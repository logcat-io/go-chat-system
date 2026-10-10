package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 입구. 누가 들어오는지 정하는 유스케이스가 여기 모인다. 전달 계층(HTTP·WebSocket)은 이 결과만 본다.

// ── 조회 ────────────────────────────────────────────────────────────

// Subject 는 토큰의 주인이다. 만료·폐기된 세션은 ErrNotFound.
func (s *Queries) Subject(ctx context.Context, token string) (core.Subject, error) {
	return s.read.SubjectByToken(ctx, token)
}

// EntryWorkspace 는 토큰 없이 들어온 요청(위젯·회원 세션)이 갈 워크스페이스다. 어느 것인지는 부르는 쪽이 정한다 —
// 설치형 단일 워크스페이스라 HTTP 입구는 늘 기본 워크스페이스를 넘긴다(D35). 위젯 키로 테넌트를 고르던 길은 없다.
func (s *Queries) EntryWorkspace(ctx context.Context, wsID string) (core.Workspace, error) {
	return s.read.Workspace(ctx, wsID)
}

// WidgetOriginAllowed 는 이 워크스페이스의 위젯 세션을 열어도 되는 호스트 오리진인지다.
// 허용 목록이 비어 있으면 어디서든 된다. 목록이 있으면 목록에 있거나 우리 자신(selfOrigin, 독립 위젯 페이지)이어야 한다.
// 목록이 있는데 호스트 오리진을 안 보내면 거절이다. 자기 오리진은 목록과 같은 규칙(core.NormalizeOrigin)으로 비교하고,
// 자기 오리진을 모르면(공개 주소 설정 누락) 그 통로를 닫는다 — 빈 값끼리 같다고 열리면 안 된다.
// 이 값은 클라이언트가 보내므로 위조는 막지 못한다 — 진짜 문은 /widget 의 CSP frame-ancestors 다.
// 여기서 막는 건 "설치 코드를 다른 사이트에 복사해 간" 정직한 오설치다.
func WidgetOriginAllowed(ws core.Workspace, hostOrigin, selfOrigin string) bool {
	if len(ws.AllowedOrigins) == 0 {
		return true
	}
	host := core.NormalizeOrigin(strings.TrimSpace(hostOrigin))
	self := core.NormalizeOrigin(strings.TrimSpace(selfOrigin))
	return ws.OriginAllowed(host) || (self != "" && host == self)
}

// ResumableCustomer 는 익명 입구(위젯 세션)가 들고 온 토큰으로 이어 갈 고객 세션이다. 이 워크스페이스의 익명 고객 세션일 때만 ok 다.
// 만료·위조 토큰, 상담원 토큰, 다른 워크스페이스의 토큰은 조용히 버린다. 위젯이 오류 화면을 띄울 이유가 없고,
// 같은 브라우저로 두 고객사 사이트를 오가면 각각 다른 고객이다.
//
// 회원 고객의 세션도 버린다(D46, 공유 PC). 익명 입구는 "호스트가 회원이라고 말하지 않은 방문자" 다. 회원이 호스트에서 로그아웃하고
// 같은 브라우저를 다른 사람이 쓰면, 호스트가 위젯 로그아웃을 잊어도 회원의 상담 이력이 다음 사람에게 열리면 안 된다.
// 회원 세션을 이어 가는 길은 회원 입구(서명된 회원 토큰)뿐이다.
func (s *Queries) ResumableCustomer(ctx context.Context, token, wsID string) (core.Subject, bool) {
	sub, ok := s.heldCustomer(ctx, token, wsID)
	if !ok || sub.Member {
		return core.Subject{}, false
	}
	return sub, true
}

// ResumableMember 는 회원 입구가 들고 온 토큰이 바로 그 회원(customerID)의 살아 있는 세션인지다. 맞으면 새 세션을 만들지 않고 이어 쓴다 —
// 호스트가 페이지마다 identify 를 불러도 세션 행이 페이지 수만큼 생기지 않는다(D46).
func (s *Queries) ResumableMember(ctx context.Context, token, wsID, customerID string) (core.Subject, bool) {
	sub, ok := s.heldCustomer(ctx, token, wsID)
	if !ok || !sub.Member || sub.CustomerID != customerID {
		return core.Subject{}, false
	}
	return sub, true
}

func (s *Queries) heldCustomer(ctx context.Context, token, wsID string) (core.Subject, bool) {
	if token == "" {
		return core.Subject{}, false
	}
	sub, err := s.read.SubjectByToken(ctx, token)
	if err != nil || sub.Kind != string(ActorCustomer) || sub.WorkspaceID != wsID {
		return core.Subject{}, false
	}
	return sub, true
}

// SubjectBySession 은 세션 참조로 주체를 찾는다. 티켓으로 붙은 WebSocket 이 세션을 다시 확인할 때 쓴다(D46).
// 세션 보안 포트가 없으면 ErrNotFound 다 — 세션을 확인할 길이 없으면 살아 있다고 보지 않는다.
func (s *Queries) SubjectBySession(ctx context.Context, ref core.SessionRef) (core.Subject, error) {
	if s.sessions == nil || ref == "" {
		return core.Subject{}, ErrNotFound
	}
	return s.sessions.SubjectBySession(ctx, ref)
}

// CustomerProfile 은 세션을 막 연 고객 자신의 프로필이다. 소속 검사는 세션을 연 쪽이 이미 했다.
func (s *Queries) CustomerProfile(ctx context.Context, customerID string) (core.Customer, error) {
	return s.read.Customer(ctx, customerID)
}

// VerifyLogin 은 로그인 판정과 그 사건 로그다. 이메일·비밀번호는 싣지 않는다 — 실패는 사유 코드만 남긴다.
// 이메일은 여기서 정규화한다. 형식은 보지 않는다 — 형식이 틀린 주소도 없는 계정과 같은 답이어야 한다.
// wsID 는 로그인할 워크스페이스다. 설치형 단일 워크스페이스라 HTTP 입구는 늘 기본 워크스페이스를 넘긴다(D35).
// 같은 이메일이 여러 워크스페이스에 있을 때 하나를 고르게 하던 길은 걷어냈다.
func (s *Queries) VerifyLogin(ctx context.Context, wsID, email, password string) (core.Agent, error) {
	agent, err := s.checkLogin(ctx, wsID, normalizeEmail(email), password)
	switch {
	case errors.Is(err, core.ErrNotFound):
		// 이메일이 없는 것과 비밀번호가 틀린 것을 로그에서도 가르지 않는다. 응답과 같은 정보만 남긴다.
		slog.WarnContext(ctx, "agent.login_failed", "reason", "invalid_credentials")
	case errors.Is(err, ErrPendingApproval):
		slog.WarnContext(ctx, "agent.login_failed", "reason", "pending_approval")
	case err != nil:
		slog.ErrorContext(ctx, "agent.login_failed", "reason", "internal", "err_kind", logctx.ErrKind(err))
	default:
		slog.InfoContext(ctx, "agent.login", "workspace_id", agent.WorkspaceID,
			"actor_kind", "agent", "actor_id", agent.ID)
	}
	return agent, err
}

// checkLogin 은 활성 계정을 먼저 찾고, 맞는 활성 계정이 없을 때만 승인 대기 계정을 본다.
// 비밀번호까지 맞으면 ErrPendingApproval 이다. 비밀번호가 틀리면 그냥 ErrNotFound 라 "가입 신청한 이메일인가" 가 새지 않는다.
//
// 실패하는 길마다 비밀번호 비교를 같은 횟수로 한다(활성 계정 자리 한 번, 대기 계정 자리 한 번). 계정이 없는 길은
// 저장소가 고정 해시와 비교한다. 응답 시간이 계정의 유무를 말하면 안 된다.
func (s *Queries) checkLogin(ctx context.Context, wsID, email, password string) (core.Agent, error) {
	agent, err := s.read.VerifyAgent(ctx, wsID, email, password)
	if !errors.Is(err, core.ErrNotFound) {
		return agent, err
	}
	pending, pendErr := s.read.PendingAgentMatches(ctx, wsID, email, password)
	if pendErr != nil {
		return core.Agent{}, pendErr
	}
	if pending {
		return core.Agent{}, ErrPendingApproval
	}
	return core.Agent{}, err
}

// ── 명령 ────────────────────────────────────────────────────────────

// 세션 수명의 숫자(D46). 정책 테스트가 리터럴로 고정한다.
const (
	// sessionTouchEvery 는 마지막 사용 시각을 다시 쓰는 간격이다. 요청마다 쓰면 읽기 요청이 전부 쓰기가 된다.
	// 유휴 한도(시간·일 단위)에 비해 1분의 오차는 뜻이 없다.
	sessionTouchEvery = time.Minute
	// WSTicketTTL 은 WebSocket 티켓의 수명이다. 발급과 연결 사이는 보통 수십 ms 다. 느린 망에서 핸드셰이크가 몇 초 걸려도 남는다.
	WSTicketTTL = 30 * time.Second
	// maxCustomerSessions 는 고객 한 명이 동시에 가질 세션의 상한이다. 회원은 기기마다(휴대폰·PC·태블릿) 세션이 하나씩이고,
	// 같은 기기는 세션을 이어 쓴다(ResumableMember). 20 을 넘는 것은 브라우저 저장소가 막혀 매번 새로 받는 경우나 회원 토큰을 되풀이해
	// 쓰는 경우다. 넘으면 오래된 것부터 지운다 — 지워진 기기는 다음 요청에서 401 을 받고 회원 토큰으로 다시 들어온다.
	maxCustomerSessions = 20
)

// IssueAgentSession 은 상담원 세션 토큰을 발급한다. 평문은 이 응답에만 있고 저장은 해시다. 유휴 한도는 정책의 상담원 값이다.
func (s *Commands) IssueAgentSession(ctx context.Context, agentID string, ttl time.Duration) (string, error) {
	return s.write.CreateSession(ctx, string(ActorAgent), agentID, "", ttl, s.policy.AgentIdle)
}

// IssueCustomerSession 은 익명 고객의 세션 토큰을 발급한다. 평문은 이 응답에만 있고 저장은 해시다.
// 절대 수명이 없다(D46 고침). 교환·반품처럼 브랜드사 처리가 길어지면 고객은 몇 주~몇 달 뒤 같은 브라우저로 돌아오고, 그때 지난 상담이 이어져야 한다.
// 로그아웃(EndSession)과 고객 삭제로만 끝난다. 고객 세션의 유휴 한도는 행에 적지 않는다 — 저장소가 판정 때마다 정책의 고객 값(기본 없음)으로 본다(감사 21).
// 회원 고객의 세션은 IssueMemberSession 이 연다 — 회원 세션은 회원 토큰 exp 에 묶인다.
func (s *Commands) IssueCustomerSession(ctx context.Context, customerID string) (string, error) {
	return s.write.CreateSession(ctx, string(ActorCustomer), "", customerID, 0, 0)
}

// MemberSessionTTL 은 회원 토큰으로 여는 세션의 수명이다. 세션 수명 상한과 회원 토큰의 만료 중 이른 쪽이다(D46).
// 호스트가 1시간짜리 회원 토큰을 주면 그 토큰으로 연 세션도 1시간 뒤 죽는다 — 세션이 호스트의 로그인보다 오래 살지 않는다.
func MemberSessionTTL(maxTTL time.Duration, memberExp, now time.Time) time.Duration {
	if left := memberExp.Sub(now); left < maxTTL {
		return max(left, time.Second)
	}
	return maxTTL
}

// IssueMemberSession 은 회원 고객의 세션을 발급한다. 수명은 MemberSessionTTL 이고, 발급한 뒤 그 고객의 세션을 최신
// maxCustomerSessions 개만 남긴다. 상한 정리가 실패해도 발급은 성공이다 — 다음 발급이 다시 정리한다.
func (s *Commands) IssueMemberSession(ctx context.Context, customerID string, maxTTL time.Duration, memberExp time.Time) (string, error) {
	token, err := s.write.CreateSession(ctx, string(ActorCustomer), "", customerID, MemberSessionTTL(maxTTL, memberExp, time.Now()), 0)
	if err != nil {
		return "", err
	}
	if s.sessions != nil {
		if _, err := s.sessions.PruneCustomerSessions(ctx, customerID, maxCustomerSessions); err != nil {
			slog.WarnContext(ctx, "session.prune_failed", "customer_id", customerID, "err_kind", logctx.ErrKind(err))
		}
	}
	return token, nil
}

// EndSession 은 로그아웃이다. 서버의 세션 행을 지운다. 그 세션으로 열린 WebSocket 은 트리거의 알림을 받아 곧바로 닫힌다(D46).
func (s *Commands) EndSession(ctx context.Context, token string) error {
	return s.write.DeleteSession(ctx, token)
}

// NoteSessionUse 는 세션을 썼다고 적는다. 유휴 만료(D46)의 기준이다. 마지막으로 적은 지 1분이 안 됐으면 아무것도 하지 않는다 —
// 요청마다 쓰지 않는다. 지금 유휴 한도가 없는 세션(익명 고객 기본)도 적는다 — 고객 유휴 한도는 판정 시점의 설정이라
// 나중에 켜면 그때까지 쓴 시각으로 재야 한다(감사 21). 실패해도 요청은 그대로 간다. 다음 요청이 다시 적는다.
func (s *Commands) NoteSessionUse(ctx context.Context, sub core.Subject) {
	if s.sessions == nil || sub.Session == "" || time.Since(sub.UsedAt) < sessionTouchEvery {
		return
	}
	if err := s.sessions.TouchSession(ctx, sub.Session); err != nil {
		slog.WarnContext(ctx, "session.touch_failed", "err_kind", logctx.ErrKind(err))
	}
}

// ErrNoSessionStore 는 저장소에 세션 보안 포트가 없어 티켓을 다룰 수 없을 때다. 운영 조립에서는 나지 않는다.
var ErrNoSessionStore = core.NewError("no_session_store", "app: 조립한 저장소에 세션 보안 포트(SessionSecurityStore)가 없어 티켓을 다룰 수 없다")

// IssueWSTicket 은 이 행위자의 세션에 묶인 WebSocket 티켓을 발급한다(D46, DEF-91). 브라우저는 WebSocket 에 헤더를 붙일 수 없어
// 세션 토큰을 주소에 실었고, 앞단(ALB·CDN) 접근 로그에 30일짜리 토큰이 남았다. 티켓은 30초 일회용이라 로그에 남아도 쓸 수 없다.
func (s *Commands) IssueWSTicket(ctx context.Context, actor Actor) (string, time.Time, error) {
	if s.sessions == nil {
		return "", time.Time{}, ErrNoSessionStore
	}
	if actor.Session == "" {
		return "", time.Time{}, ErrNotFound
	}
	return s.sessions.IssueWSTicket(ctx, actor.Session, WSTicketTTL)
}

// RedeemWSTicket 은 티켓을 쓰고 그 세션의 주체를 준다. 쓰면 끝이다 — 같은 티켓으로 두 번 붙지 못한다.
func (s *Commands) RedeemWSTicket(ctx context.Context, ticket string) (core.Subject, error) {
	if s.sessions == nil {
		return core.Subject{}, ErrNoSessionStore
	}
	if ticket == "" {
		return core.Subject{}, ErrNotFound
	}
	return s.sessions.RedeemWSTicket(ctx, ticket)
}
