package realtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 허브는 본문을 들고 다니지 않는다. 그래서 신호가 겹쳐도 잃을 것이 없다.
//
// 여기서 보는 건 "신호가 정확히 몇 개 남는가"가 아니다. 그건 버퍼 크기라는 구현이고,
// 버퍼를 늘려도 정책은 그대로다. 봐야 할 정책은 둘이다.
//   - 아무도 안 읽어도 알림이 막히지 않는다
//   - 겹쳐 와도 최소 한 번은 깨어난다 (깨어나면 DB 를 전부 읽으므로 그걸로 충분하다)
func TestHub_신호합침(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")
	hub.Bind(sub, "conv-1")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.NotifyChange("conv-1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("읽는 쪽이 없는데 알림이 막혔다")
	}

	if !woken(sub) {
		t.Error("겹쳐 와도 최소 한 번은 깨어나야 한다")
	}
}

func TestHub_구독범위(t *testing.T) {
	hub := NewHub()
	a := hub.Join("")
	b := hub.Join("")
	hub.Bind(a, "conv-a")
	hub.Bind(b, "conv-b")
	drain(a)
	drain(b)

	hub.NotifyChange("conv-a")

	if !woken(a) {
		t.Error("그 대화를 보는 구독자는 깨어나야 한다")
	}
	if woken(b) {
		t.Error("다른 대화 구독자는 깨우지 않는다")
	}
}

func TestHub_대화바꾸기(t *testing.T) {
	hub := NewHub()
	sub := hub.Join("")
	hub.Bind(sub, "conv-a")
	hub.Bind(sub, "conv-b")
	drain(sub)

	hub.NotifyChange("conv-a")
	if woken(sub) {
		t.Error("옛 대화 알림은 받지 않아야 한다")
	}
	hub.NotifyChange("conv-b")
	if !woken(sub) {
		t.Error("새 대화 알림은 받아야 한다")
	}
	if got := sub.ConversationID(); got != "conv-b" {
		t.Errorf("구독 대상이 conv-b 여야 한다: %s", got)
	}
}

func TestHub_받은목록(t *testing.T) {
	hub := NewHub()
	agent := hub.Join("ws-1")
	customer := hub.Join("")
	hub.Bind(customer, "conv-x")
	drain(agent)
	drainInbox(agent)
	drain(customer)

	hub.NotifyChange("conv-x")

	if !wokenInbox(agent) {
		t.Error("상담원은 어느 대화가 바뀌어도 받은목록 신호를 받는다")
	}
	if wokenInbox(customer) {
		t.Error("고객에게는 받은목록 신호를 보내지 않는다")
	}
}

func TestHub_재동기화(t *testing.T) {
	hub := NewHub()
	a := hub.Join("")
	b := hub.Join("ws-1")
	hub.Bind(a, "conv-a")
	drain(a)
	drain(b)

	// 알림 리스너가 재접속했다. 놓친 알림이 있을 수 있어 전원을 깨운다.
	hub.Resync()

	if !woken(a) || !woken(b) {
		t.Error("Resync 는 전원을 깨운다")
	}
	if hub.Size() != 2 {
		t.Errorf("연결 수가 2 여야 한다: %d", hub.Size())
	}

	hub.Leave(a)
	hub.Leave(b)
	if hub.Size() != 0 {
		t.Errorf("떠난 뒤에는 0 이어야 한다: %d", hub.Size())
	}
}

func drain(s *Subscriber) {
	for {
		select {
		case <-s.Wake():
		default:
			return
		}
	}
}

func drainInbox(s *Subscriber) {
	for {
		select {
		case <-s.InboxWake():
		default:
			return
		}
	}
}

func woken(s *Subscriber) bool {
	select {
	case <-s.Wake():
		return true
	default:
		return false
	}
}

func wokenInbox(s *Subscriber) bool {
	select {
	case <-s.InboxWake():
		return true
	default:
		return false
	}
}

// 받은함 갱신 신호는 같은 워크스페이스 안에서만 돈다. 남의 테넌트 메시지가 내 받은함을 깨우면 안 된다.
func TestHub_받은함신호는워크스페이스안에서만돈다(t *testing.T) {
	hub := NewHub()
	a := hub.Join("ws-a")
	b := hub.Join("ws-b")
	defer hub.Leave(a)
	defer hub.Leave(b)

	hub.NotifyChangeHint("conv-1", Hint{Seq: 1, WorkspaceID: "ws-a"})
	select {
	case <-a.InboxWake():
	default:
		t.Fatal("같은 워크스페이스는 깨어나야 한다")
	}
	select {
	case <-b.InboxWake():
		t.Fatal("다른 워크스페이스가 깨어났다")
	default:
	}

	// 소속을 모르는 신호(안전망·재접속)는 전원을 깨운다. 놓치는 것보다 한 번 더 읽는 게 낫다.
	hub.NotifyChangeHint("conv-2", Hint{Meta: true})
	select {
	case <-b.InboxWake():
	default:
		t.Fatal("소속 없는 신호는 전원에게 가야 한다")
	}
}

// 오류 프레임의 code 는 바깥 계약이고 message 는 사람에게 보이는 문구다. 같은 오류의 둘이 한 짝으로 정해진다.
// 충돌(app.ErrConflict)은 "이미 있다" 는 뜻이다 — 이미 열린 대화가 있다는 뜻이 아니다. HTTP 의 409 conflict 문구와 같다.
func TestErrorFrame_오류마다code와message가한짝이고충돌은이미있다고알린다(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		code, message string
	}{
		{"충돌", app.ErrConflict, "conflict", "같은 값이 이미 있어 저장하지 않았습니다"},
		{"감싼 충돌", fmt.Errorf("저장: %w", app.ErrConflict), "conflict", "같은 값이 이미 있어 저장하지 않았습니다"},
		{"권한 없음", app.ErrForbidden, "forbidden", "이 대화에 접근할 수 없습니다"},
		// HTTP 와 같은 상황은 같은 문구다(D47). 전에는 권한표 거절도 "이 대화에 접근할 수 없습니다" 였다.
		{"권한표 거절", app.ErrPermission, "forbidden", "이 작업을 할 권한이 없습니다"},
		{"상담원 전용", app.ErrAgentOnly, "forbidden", "상담원만 할 수 있습니다"},
		{"남의 메시지", app.ErrNotOwner, "forbidden", "자기가 보낸 메시지만 고치거나 지울 수 있습니다"},
		{"없음", app.ErrNotFound, "not_found", "상담을 찾을 수 없습니다. 이미 지워졌을 수 있습니다"},
		{"종료", app.ErrEnded, "ended", "종료된 상담입니다. 새 문의를 시작해 주세요"},
		{"모르는 프레임", errBadFrame, "bad_frame", "알 수 없는 종류의 요청입니다. 페이지를 새로 고쳐 주세요"},
		{"세션이 끝났다", errSessionGone, "unauthorized", "로그인 정보가 만료되었습니다. 다시 로그인하거나 페이지를 새로 고쳐 주세요"},
		{"입력 오류는 사유를 그대로", app.InvalidError{Reason: "본문이 비어 있습니다"}, "invalid", "본문이 비어 있습니다"},
		{"닫히는 중", context.Canceled, "canceled", "서버 오류로 요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주세요"},
		{"모르는 오류는 뭉뚱그린다", errors.New("pgx: 비밀 연결 문자열"), "internal", "서버 오류로 요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주세요"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCode(tc.err); got != tc.code {
				t.Errorf("code %q, %q 여야 한다", got, tc.code)
			}
			if got := userMessage(tc.err); got != tc.message {
				t.Errorf("message %q, %q 여야 한다", got, tc.message)
			}
		})
	}
}

// 감사 20 A09: 세션이 끝났다는 알림이 허브의 모든 연결을 깨워도 테스트가 통과했다(깨어난 연결은 DB 에 다시 물어 살아 있으면 이어 가므로
// 결과는 같다). 그러면 로그아웃 한 번, 만료 정리 한 번마다 서버의 연결 수만큼 세션 조회가 몰린다. 알림은 그 세션의 연결만 깨운다.
func TestHub_세션종료알림은그세션의연결만깨운다(t *testing.T) {
	hub := NewHub()
	ended := hub.Join("")
	other := hub.Join("")
	anon := hub.Join("") // 세션을 모르는 연결(옛 길)
	hub.WatchSession(ended, "ref-ended")
	hub.WatchSession(other, "ref-other")
	defer hub.Leave(ended)
	defer hub.Leave(other)
	defer hub.Leave(anon)

	hub.SessionsEnded([]core.SessionRef{"ref-ended", "ref-unknown"})

	select {
	case <-ended.Ended():
	case <-time.After(time.Second):
		t.Fatal("끝난 세션의 연결을 깨우지 않았다")
	}
	for name, s := range map[string]*Subscriber{"다른 세션": other, "세션 없는 연결": anon} {
		select {
		case <-s.Ended():
			t.Errorf("%s의 연결까지 깨웠다 — 알림마다 모든 연결이 세션을 다시 묻는다", name)
		default:
		}
	}
}
