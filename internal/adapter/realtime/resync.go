package realtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// ChangeWatcher 는 알림을 놓쳤을 때를 대비한 안전망이다.
//
// 처음에는 연결마다 자기 대화를 주기적으로 되묻게 했다. 그러면 안전망의 비용이
// 연결 수에 비례한다 — 3000 연결이 5초마다 물으면 그것만으로 초당 600 질의다.
// 바뀐 대화를 한 번의 조회로 모아 가져오면 주기당 한 번이면 끝난다.
//
// 놓침을 막으려고 조회 구간을 매 주기 조금 겹쳐 둔다. 변경 시각은 문장 시각(clock_timestamp)이고 커밋은 그보다
// 뒤라서, 먼저 시작해 늦게 커밋한 트랜잭션의 행은 커서보다 과거 시각을 갖는다. 겹침이 없으면 그 행을 영영 못 본다.
// 같은 대화를 두 번 깨워도 구독자는 DB 를 다시 볼 뿐이라 결과가 달라지지 않는다.
//
// 다만 겹침 구간의 같은 변경을 매 주기 다시 알리면 비용이 든다. 커서는 마지막 변경 시각에 멈춰 있어서, 쉬는 동안에도
// 마지막 변경이 주기마다 다시 잡혀 받은함 구독자(데스크)가 5초마다 받은함을 다시 조회한다 — 그 부하가 상담원 수에 비례한다.
// 그래서 겹침 구간에서 이미 알린 (대화, 변경 시각·번호) 는 다시 알리지 않는다. 겹침 구간 자체는 그대로 훑으므로,
// 늦게 커밋한 변경은 같은 대화라도 시각이 다르다(행 잠금 때문에 커밋된 판끼리 시각이 겹치지 않는다) — 여전히 알린다.
// ChangeSource 는 안전망이 저장소에 바라는 것이다. 쓰는 쪽(이 패키지)이 정의한다.
type ChangeSource interface {
	ChangedSince(ctx context.Context, cur core.ChangeCursor, limit int) ([]core.ChangedConv, core.ChangeCursor, bool, error)
	Now(ctx context.Context) (time.Time, error)
}

type ChangeWatcher struct {
	store    ChangeSource
	hub      *Hub
	interval time.Duration
	overlap  time.Duration
	log      *slog.Logger
}

func NewChangeWatcher(store ChangeSource, hub *Hub, interval time.Duration, log *slog.Logger) *ChangeWatcher {
	return &ChangeWatcher{
		store:    store,
		hub:      hub,
		interval: interval,
		overlap:  watchOverlap,
		log:      log,
	}
}

const (
	// watchOverlap 은 매 주기 다시 훑는 과거 구간이다. 문장 시각과 커밋 사이의 간격(보통 ms)보다 넉넉하게.
	watchOverlap = 2 * time.Second
	// watchPage 는 한 번에 가져오는 대화 수다. 더 있으면 이어서 가져온다.
	watchPage = 1000
	// watchMaxPages 는 한 주기에 훑는 상한이다. 끝없이 돌아 DB 를 붙잡지 않게 한다.
	watchMaxPages = 20
	// watchQueryTimeout 은 변경 조회 한 번의 시한이다(DEF-76). 장애 조치 뒤 조용히 죽은 DB 연결을 붙잡으면 안전망 전체가
	// TCP 재전송이 포기할 때까지 멈춘다. WebSocket 경로의 읽기 시한(5초, DEF-71)과 같다. 넘기면 그 주기를 접고 다음 주기에 같은 커서로 다시 묻는다.
	watchQueryTimeout = 5 * time.Second
)

func (w *ChangeWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// 시작점은 DB 시계로 잡는다. 앱 시계로 잡으면 두 시계가 어긋난 만큼이 사각지대가 된다.
	start, err := w.store.Now(ctx)
	if err != nil {
		w.log.Warn("resync.clock_fallback", "err_kind", logctx.ErrKind(err))
		start = time.Now()
	}
	st := &watchState{last: core.ChangeCursor{At: start}, notified: map[string]core.ChangedConv{}}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// 백그라운드 작업 한 번이 trace 하나다.
		w.poll(logctx.NewTrace(ctx), st)
	}
}

// watchState 는 주기 사이에 이어지는 안전망의 기억이다.
type watchState struct {
	// last 는 지금까지 본 가장 늦은 변경이다. 다음 주기는 여기서 겹침 구간만큼 뒤에서 시작한다.
	last core.ChangeCursor
	// notified 는 겹침 구간 안에서 이미 알린 변경이다. 구간 밖으로 밀려난 것은 다시 잡힐 일이 없어 지운다.
	notified map[string]core.ChangedConv
}

// poll 은 한 주기다. 바뀐 대화를 모아 구독자를 깨우고 깨운 대화 수를 돌려준다.
func (w *ChangeWatcher) poll(ctx context.Context, st *watchState) int {
	woken := 0
	// 주기마다 겹침 구간만큼 뒤에서 시작한다. 같은 주기 안의 페이지는 커서를 그대로 잇는다.
	cursor := core.ChangeCursor{At: st.last.At.Add(-w.overlap)}
	// since 는 이번 주기 구간의 시작이다. 메타 판정에 쓴다(metaChanged).
	since := cursor.At
	for id, c := range st.notified {
		if c.At.Before(cursor.At) {
			delete(st.notified, id)
		}
	}
	for page := 0; page < watchMaxPages; page++ {
		qctx, cancel := context.WithTimeout(ctx, watchQueryTimeout)
		ids, next, more, err := w.store.ChangedSince(qctx, cursor, watchPage)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				// 오류 원문은 싣지 않는다. 접속 정보가 들어간다(D30, DEF-76). 종류(SQLSTATE·timeout·error)만 남긴다.
				w.log.WarnContext(ctx, "resync.query_failed", "err_kind", logctx.ErrKind(err))
			}
			break
		}
		for _, c := range ids {
			prev, seen := st.notified[c.ID]
			if seen && sameChange(prev, c) {
				continue
			}
			meta := metaChanged(prev, seen, c, since)
			st.notified[c.ID] = c
			w.hub.OnChange(core.Change{ConversationID: c.ID, WorkspaceID: c.WorkspaceID, Seq: c.Seq, PublicSeq: c.PublicSeq, Meta: meta})
			woken++
		}
		cursor = next
		if next.At.After(st.last.At) {
			st.last = next
		}
		if !more {
			break
		}
		if page == watchMaxPages-1 {
			// 여기까지 왔으면 변경이 한 주기에 2만 건을 넘었다는 뜻이다.
			// 다음 주기가 이어받지만, 조용히 넘어가면 안 되는 상태다.
			w.log.WarnContext(ctx, "resync.backlog",
				"pages", watchMaxPages, "page_size", watchPage)
		}
	}
	// 안전망은 알림이 이미 깨운 대화인지 모른다. 이 주기에 다시 깨운 대화 수를 남긴다 — 이미 알린 변경은 세지 않는다.
	// 알림이 처리한 변화와 놓친 변화를 가르지 못하므로 장애 신호가 아니다. 주기마다 INFO 로 남기면 소음이라 DEBUG 다.
	if woken > 0 {
		w.log.DebugContext(ctx, "delivery.catchup", "conversations", woken)
	}
	return woken
}

// sameChange 는 같은 대화의 같은 판인지 본다. 시각이나 번호 중 하나라도 다르면 새 변경이다.
// 메타 힌트는 기억하지 않는다. 기억의 updated_at 과 견줘 그때그때 정한다(metaChanged).
func sameChange(a, b core.ChangedConv) bool {
	return a.At.Equal(b.At) && a.UpdatedAt.Equal(b.UpdatedAt) && a.Seq == b.Seq && a.PublicSeq == b.PublicSeq && a.WorkspaceID == b.WorkspaceID
}

// metaChanged 는 이 변경에 상태·담당자·태그 같은 메시지 밖의 변화가 들었는지 본다. 그런 변화는 updated_at 만 올린다.
//
// 메시지 시각과 비교하지 않는다. 닫기·배정은 행을 바꾼 뒤 별도 트랜잭션으로 시스템 줄을 남겨서 마지막 메시지가 늘 더 늦다.
// 그걸 메시지 변경으로 읽으면 구독자는 메시지만 받고 대화 프레임(상태)은 연결 틱(60초)까지 못 받는다.
// 이미 알린 판이 기억에 있으면 그 뒤로 updated_at 이 바뀌었는지 본다. 없으면 이번 주기 구간 안에서 바뀌었는지 본다 —
// 구간 안의 옛 메타 변화가 다시 켜질 수는 있지만, 그 값은 구독자가 대화 행을 한 번 더 읽는 것뿐이다.
func metaChanged(prev core.ChangedConv, seen bool, c core.ChangedConv, since time.Time) bool {
	if seen {
		return !c.UpdatedAt.Equal(prev.UpdatedAt)
	}
	return !c.UpdatedAt.Before(since)
}
