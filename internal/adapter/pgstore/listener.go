package pgstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// Listener 는 Postgres LISTEN 전용 커넥션을 하나 쥐고 두 채널을 중계한다.
//
//	NotifyChannel        "이 대화에 변화가 있다" → 받는 쪽이 DB 를 다시 읽는다
//	LiveChannel          타이핑·읽음 갱신 → 받는 쪽이 그대로 전달한다. 잃어도 된다
//	SessionEndedChannel  세션 행이 지워졌다(트리거, D46) → 그 세션의 열린 연결이 곧바로 세션을 다시 묻는다. 잃어도 된다(60초 재확인)
//
// 알림은 전달 보장이 없다. 커넥션이 끊긴 사이의 알림은 사라진다.
// 그래서 재접속할 때마다 OnResync 를 불러 "전부 다시 읽어라"를 알린다.
// 구독자 쪽 주기 재조회가 두 번째 안전망이다 (docs/design.md D4).
type Listener struct {
	url      string
	OnChange func(core.Change)
	OnLive   func(core.LiveEvent)
	OnResync func()
	// OnSessionsEnded 는 지워진 세션들의 참조를 받는다(D46). nil 이면 그 채널을 듣지 않는다.
	OnSessionsEnded func([]core.SessionRef)
	Log             *slog.Logger
}

func NewListener(url string, log *slog.Logger) *Listener {
	return &Listener{url: url, Log: log}
}

// Run 은 ctx 가 끝날 때까지 재접속하며 알림을 받는다.
//
// 로그는 사건 이름 둘이다(D30). 로그 지표 필터와 알람이 이 이름을 센다.
//
//	listener.reconnect  WARN. 끊겼거나 못 붙었다. attempt 는 마지막 연결 뒤 몇 번째 실패인가, after_ms 는 다음 시도까지 대기,
//	                    err_kind 는 오류의 종류(SQLSTATE·timeout·error)다. 원문은 접속 정보가 들어가 싣지 않는다.
//	                    답하지 않는 DB 면 접속 시한(5초, listenerSetupTimeout)마다 timeout 으로 한 줄이다
//	listener.connected  INFO. LISTEN 까지 마쳤다. attempt 는 이번 연결 전에 실패한 횟수(첫 기동은 0)
func (l *Listener) Run(ctx context.Context) {
	const initialBackoff, maxBackoff = 200 * time.Millisecond, 5 * time.Second
	backoff := initialBackoff
	attempt := 0

	for ctx.Err() == nil {
		err := l.listenOnce(ctx, func() {
			l.Log.InfoContext(ctx, "listener.connected", "attempt", attempt, "channels", l.channels())
			backoff, attempt = initialBackoff, 0
		})
		if ctx.Err() != nil {
			return
		}
		attempt++
		// 오류 원문은 싣지 않는다. 접속 실패 문구에는 user=·database=·호스트가 들어간다(D30). 종류(SQLSTATE·timeout·error)만 남긴다.
		l.Log.WarnContext(ctx, "listener.reconnect", "attempt", attempt, "after_ms", backoff.Milliseconds(), "err_kind", logctx.ErrKind(err))
		telemetry.Metrics().ListenerReconnects.Add(ctx, 1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listenerSetupTimeout 은 접속부터 LISTEN 까지의 시한이다. 풀의 새 연결 수립 시한(poolConnectTimeout)과 같은 5초다.
// 시한이 없으면 패킷을 버리는 DB(장애 조치 중의 옛 주소, 보안 그룹)에 붙는 동안 리스너가 TCP 가 포기할 때까지 아무 로그 없이 멈춘다 —
// 감사 31 실측: 받기만 하고 답하지 않는 주소에서 35초 넘게 listener.reconnect 가 없었다. 그동안 실시간 전달은 5초 안전망으로만 간다.
// 접속 문자열에 connect_timeout 이 있으면 접속(TCP·TLS·인증)은 그것을 따르고, LISTEN 까지 합친 전체는 이 시한 안이다.
const listenerSetupTimeout = 5 * time.Second

func (l *Listener) listenOnce(ctx context.Context, onConnected func()) error {
	cfg, err := pgx.ParseConfig(l.url)
	if err != nil {
		return err
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = listenerSetupTimeout
	}
	setupCtx, cancel := context.WithTimeout(ctx, listenerSetupTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(setupCtx, cfg)
	if err != nil {
		return err
	}
	// 종료로 ctx 가 취소된 뒤에도 서버에 종료 메시지를 보내고 닫는다. 취소된 ctx 를 넘기면 pgx 가 연결에 바로 시한을 걸어 못 보낼 수 있다.
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	for _, ch := range l.channels() {
		if _, err := conn.Exec(setupCtx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return err
		}
	}
	cancel()
	onConnected()

	// 끊겼던 동안의 알림은 복구할 수 없다. 모든 구독자를 깨워 DB 를 다시 읽게 한다.
	if l.OnResync != nil {
		l.OnResync()
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch n.Channel {
		case NotifyChannel:
			var p changedPayload
			if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
				l.Log.WarnContext(ctx, "listener.decode_failed", "channel", NotifyChannel, "payload_bytes", len(n.Payload), "err_kind", logctx.ErrKind(err))
				continue
			}
			if l.OnChange != nil {
				l.OnChange(core.Change{
					ConversationID: p.ConversationID, WorkspaceID: p.WorkspaceID, Seq: p.Seq,
					PublicSeq: p.PublicSeq, Meta: p.Meta,
				})
			}
		case LiveChannel:
			var ev core.LiveEvent
			if err := json.Unmarshal([]byte(n.Payload), &ev); err != nil {
				// 휘발성 알림에는 행위자 이름이 들어 있다. 운영 로그에는 길이만 남긴다.
				l.Log.WarnContext(ctx, "listener.decode_failed", "channel", LiveChannel, "payload_bytes", len(n.Payload), "err_kind", logctx.ErrKind(err))
				continue
			}
			if l.OnLive != nil {
				l.OnLive(ev)
			}
		case SessionEndedChannel:
			if l.OnSessionsEnded != nil {
				l.OnSessionsEnded(parseSessionRefs(n.Payload))
			}
		}
	}
}

// channels 는 LISTEN 할 채널이다. 세션 종료 알림은 받을 곳이 있을 때만 듣는다.
func (l *Listener) channels() []string {
	chs := []string{NotifyChannel, LiveChannel}
	if l.OnSessionsEnded != nil {
		chs = append(chs, SessionEndedChannel)
	}
	return chs
}

// parseSessionRefs 는 트리거가 쉼표로 이은 세션 참조를 나눈다. 값은 토큰 해시의 16진수라 로그에 싣지 않는다.
func parseSessionRefs(payload string) []core.SessionRef {
	parts := strings.Split(payload, ",")
	refs := make([]core.SessionRef, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			refs = append(refs, core.SessionRef(p))
		}
	}
	return refs
}
