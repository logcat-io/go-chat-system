// Package realtime 은 WebSocket 연결과 전달 루프를 소유한다.
//
// 설계의 핵심 한 줄: 허브는 메시지 본문을 들고 다니지 않는다.
// 구독자마다 용량 1 짜리 깨우기 채널을 두고, 깨어난 구독자가 DB 에서 직접 읽어 내려보낸다.
// 신호가 겹치면 하나로 합쳐지고, 합쳐져도 결과는 같다. 잃을 페이로드가 없다 (docs/design.md D1).
package realtime

import (
	"sync"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 휘발성 이벤트 버퍼. 꽉 차면 버린다.
// 메시지는 DB 가 진실이라 버려도 캐치업이 메우지만, 타이핑은 메울 것이 없고 메울 필요도 없다.
const liveBuffer = 16

// Hint 는 깨우기 신호에 딸려 오는 "어디까지 왔는지" 다.
// 이게 없으면 구독자는 깨어날 때마다 대화 행을 다시 읽어야 하고,
// 그 한 번이 메시지마다 붙어 전체 처리량의 천장이 된다.
type Hint struct {
	// WorkspaceID 가 비어 있으면 받은함 신호를 전원에게 보낸다(안전망 경로). 있으면 그 테넌트에만.
	WorkspaceID string
	Seq         int64
	PublicSeq   int64
	// Meta 가 true 면 상태·담당자·태그가 바뀌었을 수 있다. 그때만 대화 행을 읽는다.
	Meta bool
}

func (h Hint) ViewerSeq(internal bool) int64 {
	if internal {
		return h.Seq
	}
	return h.PublicSeq
}

// Subscriber 는 한 WebSocket 연결의 깨우기 신호 수신구다.
type Subscriber struct {
	wake      chan struct{}
	inboxWake chan struct{}
	live      chan core.LiveEvent
	// ended 는 이 연결의 세션이 지워졌다는 신호다(D46). 받으면 기다리지 않고 세션을 다시 확인한다.
	ended chan struct{}

	mu     sync.Mutex
	convID string
	hint   Hint
}

// TakeHint 는 쌓인 힌트를 꺼내면서 비운다. 여러 알림이 겹쳤으면 가장 앞선 번호가 남는다.
func (s *Subscriber) TakeHint() Hint {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hint
	s.hint = Hint{}
	return h
}

func (s *Subscriber) mergeHint(h Hint) {
	s.mu.Lock()
	if h.Seq > s.hint.Seq {
		s.hint.Seq = h.Seq
	}
	if h.PublicSeq > s.hint.PublicSeq {
		s.hint.PublicSeq = h.PublicSeq
	}
	s.hint.Meta = s.hint.Meta || h.Meta
	s.mu.Unlock()
}

func (s *Subscriber) Wake() <-chan struct{}       { return s.wake }
func (s *Subscriber) InboxWake() <-chan struct{}  { return s.inboxWake }
func (s *Subscriber) Live() <-chan core.LiveEvent { return s.live }
func (s *Subscriber) Ended() <-chan struct{}      { return s.ended }

// signal 은 절대 막히지 않는다. 이미 신호가 대기 중이면 그냥 버린다 (합쳐진다).
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Subscriber) Signal()      { signal(s.wake) }
func (s *Subscriber) SignalInbox() { signal(s.inboxWake) }

func (s *Subscriber) ConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convID
}

type Hub struct {
	mu     sync.RWMutex
	byConv map[string]map[*Subscriber]struct{}
	all    map[*Subscriber]struct{}
	// inbox 는 받은함을 보는 구독자와 그 워크스페이스다. 신호는 같은 워크스페이스 안에서만 돈다.
	inbox map[*Subscriber]string
	// bySession 은 세션 참조별 구독자다. 세션이 지워졌다는 알림(D46)이 그 세션의 연결만 깨운다.
	bySession map[core.SessionRef]map[*Subscriber]struct{}
	session   map[*Subscriber]core.SessionRef
}

func NewHub() *Hub {
	return &Hub{
		byConv: make(map[string]map[*Subscriber]struct{}),
		all:    make(map[*Subscriber]struct{}),
		inbox:  make(map[*Subscriber]string),

		bySession: make(map[core.SessionRef]map[*Subscriber]struct{}),
		session:   make(map[*Subscriber]core.SessionRef),
	}
}

// Join 은 구독자를 등록한다. inboxWorkspace 가 비어 있지 않으면 그 워크스페이스의 받은함 갱신 알림을 받는다.
func (h *Hub) Join(inboxWorkspace string) *Subscriber {
	sub := &Subscriber{
		wake:      make(chan struct{}, 1),
		inboxWake: make(chan struct{}, 1),
		live:      make(chan core.LiveEvent, liveBuffer),
		ended:     make(chan struct{}, 1),
	}
	h.mu.Lock()
	h.all[sub] = struct{}{}
	if inboxWorkspace != "" {
		h.inbox[sub] = inboxWorkspace
	}
	h.mu.Unlock()
	return sub
}

// WatchSession 은 구독자를 그 세션에 묶는다. 세션이 지워졌다는 알림이 오면 SessionsEnded 가 이 구독자를 깨운다.
// 빈 참조(세션을 모르는 연결)는 묶지 않는다 — 그 연결은 주기 재확인(60초)으로만 끊긴다.
func (h *Hub) WatchSession(sub *Subscriber, ref core.SessionRef) {
	if ref == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.bySession[ref]
	if set == nil {
		set = make(map[*Subscriber]struct{})
		h.bySession[ref] = set
	}
	set[sub] = struct{}{}
	h.session[sub] = ref
}

// SessionsEnded 는 지워진 세션들의 연결을 깨운다. 깨어난 연결은 세션을 다시 묻고, 정말 없으면 unauthorized 와 1000 으로 닫는다.
// 알림만으로 닫지 않는다 — 다시 묻는 것이 판정이다(알림이 늦게 오거나 다른 세션과 섞여도 살아 있는 연결을 끊지 않는다).
func (h *Hub) SessionsEnded(refs []core.SessionRef) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ref := range refs {
		for sub := range h.bySession[ref] {
			signal(sub.ended)
		}
	}
}

// signalInbox 는 워크스페이스가 맞는 받은함 구독자를 깨운다. 빈 워크스페이스는 전원이다.
func (h *Hub) signalInbox(wsID string) {
	for sub, ws := range h.inbox {
		if wsID == "" || ws == wsID {
			sub.SignalInbox()
		}
	}
}

// Bind 는 구독 대상 대화를 바꾼다. 바꾼 직후 한 번 깨워서 캐치업을 돌린다.
func (h *Hub) Bind(sub *Subscriber, convID string) {
	h.mu.Lock()
	sub.mu.Lock()
	// 대화가 바뀌면 옛 대화의 힌트는 버린다.
	sub.hint = Hint{Meta: true}
	if old := sub.convID; old != "" {
		if set := h.byConv[old]; set != nil {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.byConv, old)
			}
		}
	}
	sub.convID = convID
	sub.mu.Unlock()
	if convID != "" {
		set := h.byConv[convID]
		if set == nil {
			set = make(map[*Subscriber]struct{})
			h.byConv[convID] = set
		}
		set[sub] = struct{}{}
	}
	h.mu.Unlock()
	sub.Signal()
}

// Unbind 는 구독자가 아직 convID 를 구독 중일 때만 구독을 푼다. 그 사이 다른 대화로 옮겨 갔으면 건드리지 않는다.
// 권한이 사라진 대화(지난 상담, 가린 상담)에서 떼어낼 때 쓴다. 풀린 뒤에는 그 대화의 깨우기도 휘발성 이벤트도 오지 않는다.
func (h *Hub) Unbind(sub *Subscriber, convID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub.mu.Lock()
	if sub.convID != convID || convID == "" {
		sub.mu.Unlock()
		return false
	}
	sub.convID = ""
	sub.hint = Hint{}
	sub.mu.Unlock()
	if set := h.byConv[convID]; set != nil {
		delete(set, sub)
		if len(set) == 0 {
			delete(h.byConv, convID)
		}
	}
	return true
}

// Leave 는 연결이 끝날 때 구독자를 떼어낸다.
func (h *Hub) Leave(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub.mu.Lock()
	convID := sub.convID
	sub.convID = ""
	sub.mu.Unlock()
	if set := h.byConv[convID]; set != nil {
		delete(set, sub)
		if len(set) == 0 {
			delete(h.byConv, convID)
		}
	}
	delete(h.all, sub)
	delete(h.inbox, sub)
	if ref, ok := h.session[sub]; ok {
		delete(h.session, sub)
		if set := h.bySession[ref]; set != nil {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.bySession, ref)
			}
		}
	}
}

// NotifyChange 는 그 대화를 보는 구독자와 모든 받은목록 구독자를 깨운다.
// 번호를 모르면(안전망 경로) Meta 를 켜서 받는 쪽이 직접 확인하게 한다.
func (h *Hub) NotifyChange(convID string) {
	h.NotifyChangeHint(convID, Hint{Meta: true})
}

// NotifyChangeHint 는 "어디까지 왔는지" 를 같이 넘긴다.
func (h *Hub) NotifyChangeHint(convID string, hint Hint) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.byConv[convID] {
		sub.mergeHint(hint)
		sub.Signal()
	}
	h.signalInbox(hint.WorkspaceID)
}

// OnChange 는 저장소가 알린 변경을 깨우기 신호로 옮긴다. 알림 리스너와 안전망이 같은 길로 깨운다.
func (h *Hub) OnChange(c core.Change) {
	h.NotifyChangeHint(c.ConversationID, Hint{WorkspaceID: c.WorkspaceID, Seq: c.Seq, PublicSeq: c.PublicSeq, Meta: c.Meta})
}

// OnChange 는 알림 리스너에 꽂는 자리다. 변환은 Hub.OnChange 한 곳에 있다.
func (s *Server) OnChange(c core.Change) { s.hub.OnChange(c) }

// DeliverLive 는 그 대화를 보는 구독자에게 휘발성 이벤트를 넘긴다.
// 버퍼가 차면 버린다. 읽음 갱신을 놓쳐도 재접속할 때 ready 프레임이 실제 값을 실어 온다.
func (h *Hub) DeliverLive(ev core.LiveEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.byConv[ev.ConversationID] {
		select {
		case sub.live <- ev:
		default:
		}
	}
	// 읽음이 바뀌면 받은목록의 안 읽은 수도 바뀐다. 타이핑은 목록을 건드리지 않는다.
	if ev.Kind == core.LiveRead {
		h.signalInbox(ev.WorkspaceID)
	}
}

// Resync 는 전원을 깨운다. 알림 리스너가 재접속했을 때처럼 알림을 놓쳤을 가능성이 있을 때 쓴다.
func (h *Hub) Resync() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.all {
		// 놓친 것이 있을 수 있으니 전부 다시 확인하게 한다.
		sub.mergeHint(Hint{Meta: true})
		sub.Signal()
	}
	h.signalInbox("")
}

// Size 는 현재 연결 수다. /healthz 와 테스트에서 쓴다.
func (h *Hub) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.all)
}
