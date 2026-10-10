package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

const (
	readLimitBytes = 1 << 16
	writeTimeout   = 10 * time.Second
	pingInterval   = 25 * time.Second
	drainBatch     = 200
	// typingInterval 은 한 연결이 입력 중 신호를 보낼 수 있는 최소 간격이다.
	typingInterval = 1500 * time.Millisecond
	// sessionRecheckInterval 은 열려 있는 연결의 세션을 다시 확인하는 주기다.
	// 접속 때 한 번만 보면 로그아웃·토큰 폐기가 실시간 경로에 안 먹는다.
	sessionRecheckInterval = 60 * time.Second
	// pingMissLimit 은 연결을 끊기 전에 허용하는 연속 ping 실패 횟수다.
	pingMissLimit = 3
	// inboxCoalesce 는 받은함 갱신 신호의 최소 간격이다.
	//
	// 받은함은 파생 뷰다. 메시지 한 건마다 알리면 상담원 한 명당 두 번의 조회가 따라붙고,
	// 데스크 몇 개만 열려 있어도 그게 전체 지연을 초 단위로 밀어 올린다.
	// 2초쯤 묶어도 화면이 늦는 느낌은 없고, 부하는 메시지 수가 아니라 시간에 비례하게 된다.
	inboxCoalesce = 2 * time.Second
	// connTickInterval 은 연결별 최후 안전망의 주기다. 주 안전망은 ChangeWatcher 가 한 번에 돈다.
	// 연결 수에 비례해 질의가 늘지 않도록 길게 둔다.
	connTickInterval = 60 * time.Second
	// drainRetry·drainRetryMax 는 전달용 조회가 일시 실패했을 때 스스로 다시 깨우는 간격의 처음과 상한이다(DEF-79·DEF-80).
	// 연결마다 걸린 다시 깨움은 하나뿐이고, 다시 깨움이 또 실패하면 간격을 두 배로 늘린다. 조회가 성공하면 처음으로 돌아간다.
	// 처음 0.5초는 순간 끊김(장애 조치의 앞뒤, 죽은 풀 연결 하나)을 곧바로 메운다. 상한 30초는 연결 틱(60초)보다 짧아 다시 깨움이
	// 언제나 틱보다 먼저 온다 — 안전망(5초)은 이미 알린 변경을 다시 알리지 않아(DEF-24) 실패한 전달을 대신 메우지 못한다.
	// DB 전체가 내려갔다 돌아오면 LISTEN 리스너의 재접속이 전원을 깨우므로(Resync) 복구 지연은 상한에 묶이지 않는다.
	// 상한이 없으면 장애가 길수록 연결 수 × (1/상한) 이 아니라 깨움마다 사슬이 늘어 DB 접속 시도가 폭주한다(감사 15).
	drainRetry    = 500 * time.Millisecond
	drainRetryMax = 30 * time.Second
	// dbReadTimeout 은 WebSocket 경로의 읽기 계열 DB 호출(세션 재확인·전달 조회·권한·구독 시 읽음 지점·읽음 표시·입력 중·태그)의 시한이다.
	// dbWriteTimeout 은 메시지 저장의 시한이다(DEF-71). 시한이 없으면 장애 조치 뒤 조용히 죽은 DB 연결을 붙잡고 그 연결의 전달이
	// TCP 재전송이 포기할 때까지(몇 분) 멈춘다. 지나면 일시 실패다 — 저장은 일시 오류 프레임(클라이언트 큐에 남아 다시 나간다),
	// 전달 조회는 다음 깨움, 세션 재확인은 "확인 못 함". 읽기는 커밋 뒤 알림의 시한(5초, DEF-17)과 같고, 저장은 쓰기 시한(writeTimeout)과
	// 종료 때 처리 중인 프레임을 기다리는 시한(10초, D32)에 맞춘다 — 그보다 길면 내려가는 서버가 저장을 기다리다 1001 을 못 보낸다.
	dbReadTimeout  = 5 * time.Second
	dbWriteTimeout = 10 * time.Second
)

type Server struct {
	cmd     *app.Commands
	qry     *app.Queries
	hub     *Hub
	catchup time.Duration
	log     *slog.Logger

	// mu 는 열린 연결 목록과 종료 표식을 지킨다. GoAway 가 열린 연결을 찾아 닫을 때 쓴다.
	mu      sync.Mutex
	conns   map[*conn]struct{}
	closing bool
	// handlers 는 아직 끝나지 않은 Handle 수다. closing 이 켜진 뒤에는 늘지 않는다.
	handlers sync.WaitGroup

	// connTick·sessionRecheck 는 연결별 안전망과 세션 재확인의 주기다. 운영은 위 상수이고 테스트만 줄인다.
	connTick, sessionRecheck time.Duration
	// pingEvery 는 ping 주기다. 운영은 pingInterval 이고 테스트만 줄인다.
	pingEvery time.Duration
	// retryBase·retryMax 는 전달 다시 깨움 간격의 처음과 상한이다. 운영은 drainRetry·drainRetryMax 이고 테스트만 줄인다.
	retryBase, retryMax time.Duration
}

func NewServer(a *app.App, hub *Hub, catchup time.Duration, log *slog.Logger) *Server {
	return &Server{cmd: a.Commands, qry: a.Queries, hub: hub, catchup: catchup, log: log, conns: make(map[*conn]struct{}),
		connTick: connTickInterval, sessionRecheck: sessionRecheckInterval, pingEvery: pingInterval, retryBase: drainRetry, retryMax: drainRetryMax}
}

// Connections 는 지금 붙어 있는 연결 수다. 운영 화면이 본다.
func (s *Server) Connections() int { return s.hub.Size() }

// subRequest 는 읽기 루프가 전달 루프에 보내는 구독 변경 요청이다.
// sentSeq 는 전달 루프만 만지게 해서 락을 없앴다.
type subRequest struct {
	conv     core.Conversation
	afterSeq int64
	reads    []app.ReadView
}

type conn struct {
	ws       *websocket.Conn
	actor    app.Actor
	actorKey string
	log      *slog.Logger

	writeMu sync.Mutex
	// sent 는 이 연결로 내려보낸 프레임 수다. ws.closed 에 싣는다.
	sent atomic.Int64
	// peerClose 는 클라이언트가 보낸 close 코드다. 없으면 -1. 읽기 루프만 쓰고, 그 루프가 끝난 뒤에만 읽는다.
	peerClose websocket.StatusCode

	// frameMu 는 읽기 루프가 프레임 하나를 처리하는 동안 쥔다. 서버가 내려갈 때 GoAway 가 이걸 잡고 닫으므로
	// 이미 시작한 저장은 커밋과 ack 까지 끝난 뒤에 1001 이 나간다.
	frameMu sync.Mutex
	// goingAway 는 서버가 내려가며 이 연결을 닫는 중이라는 표식이다. 켜진 뒤에 읽은 프레임은 처리하지 않는다.
	goingAway atomic.Bool
	// endedSession 은 세션 재확인이 실패해 서버가 1000 으로 닫았다는 표식이다. ws.closed 에 서버가 닫았다고 적는다.
	endedSession atomic.Bool
	// panicked 는 이 연결의 고루틴(읽기·ping·전달) 하나가 패닉으로 끝났다는 표식이다. 1011 로 닫는다(D47).
	panicked atomic.Bool
	// timedOut 은 DB 시한을 넘긴 단계의 표식(dbStep 비트)이다. 같은 단계의 연속 시한 초과는 ws.db_timeout 을 첫 번만 남긴다.
	// 읽기 루프와 전달 루프가 같이 만지므로 원자 연산으로 다룬다.
	timedOut atomic.Uint32
}

// dbStep 은 WebSocket 경로의 DB 호출 단계다. ws.db_timeout 의 step 으로 싣는다.
type dbStep uint32

const (
	stepRevalidate dbStep = 1 << iota
	stepAuthorize
	stepReadStates
	stepSend
	stepMarkRead
	stepTyping
	stepDeliver
	stepTags
)

var dbStepNames = map[dbStep]string{
	stepRevalidate: "revalidate", stepAuthorize: "authorize", stepReadStates: "read_states", stepSend: "send",
	stepMarkRead: "mark_read", stepTyping: "typing", stepDeliver: "deliver", stepTags: "tags",
}

// timeout 은 단계마다의 시한이다. 저장만 길다.
func (st dbStep) timeout() time.Duration {
	if st == stepSend {
		return dbWriteTimeout
	}
	return dbReadTimeout
}

// errDBTimeout 은 WebSocket 경로의 DB 호출이 시한을 넘겼다는 뜻이다. 연결은 살아 있다 — 일시 실패다.
// context.DeadlineExceeded 로도 보인다(세션 재확인의 reason 이 timeout 이 된다).
var errDBTimeout = fmt.Errorf("realtime: WebSocket 경로의 DB 호출이 단계 시한(읽기 5초·저장 10초)을 넘겼다: %w", context.DeadlineExceeded)

// db 는 DB 호출 하나에 단계의 시한을 건다(DEF-71). 시한이 지났는데 연결 ctx 는 살아 있으면 errDBTimeout 이고 WARN ws.db_timeout 을
// 남긴다(연결 키와 단계 이름만, D30 — 오류 문구에는 접속 정보가 들어갈 수 있다). 같은 단계의 연속 시한 초과는 첫 번만 남기고,
// 그 단계가 한 번 성공하면 다시 센다. 연결 ctx 가 끝나서 실패한 것은 시한 초과가 아니다 — 원래 오류를 그대로 돌려준다.
func (c *conn) db(ctx context.Context, st dbStep, fn func(context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, st.timeout())
	defer cancel()
	err := fn(callCtx)
	if err != nil && ctx.Err() == nil && callCtx.Err() != nil {
		if c.timedOut.Or(uint32(st))&uint32(st) == 0 {
			c.log.WarnContext(ctx, "ws.db_timeout", "step", dbStepNames[st])
		}
		return errDBTimeout
	}
	if err == nil && c.timedOut.Load()&uint32(st) != 0 {
		c.timedOut.And(^uint32(st))
	}
	return err
}

// Handle 은 하나의 WebSocket 연결을 끝까지 돌본다.
// revalidate 는 세션이 아직 살아 있는지 되묻는다. nil 이면 확인하지 않는다. 답은 셋이다(DEF-02):
// nil 이면 살아 있다, app.ErrNotFound 면 세션이 없다(만료·로그아웃·비활성), 그 밖의 오류면 확인하지 못했다(DB 오류 등).
func (s *Server) Handle(w http.ResponseWriter, r *http.Request, actor app.Actor, revalidate func(context.Context) error) {
	// 내려가는 중이면 업그레이드하지 않는다. 받아 두면 GoAway 가 지나간 뒤라 아무도 닫지 않고 프로세스와 함께 1006 으로 끊긴다.
	// 클라이언트는 평소 백오프로 다시 붙고, 그때는 로드밸런서가 다른 서버로 보낸다.
	if s.isClosing() {
		http.Error(w, "서버가 내려가는 중입니다. 잠시 후 다시 연결해 주세요", http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 같은 오리진에서만 붙인다. 역프록시 뒤에 둘 때 Host 가 맞는지 확인해야 한다.
		OriginPatterns:  []string{r.Host},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.WarnContext(r.Context(), "ws.accept_failed", "err_kind", logctx.ErrKind(err))
		return
	}
	ws.SetReadLimit(readLimitBytes)

	// 업그레이드 요청의 HTTP 스팬은 연결이 끝날 때까지(몇 시간) 열려 있다. 전달 루프·ping·세션 재확인의 조회가 그 아래에
	// 끝없이 쌓이지 않게, 연결 ctx 에는 같은 id 의 기록하지 않는 스팬만 둔다. 로그의 trace_id 는 그대로이고, SQL 은 기록 중인
	// 부모가 있을 때만 스팬을 연다. 프레임은 자기 스팬(새 트레이스)을 열고 이 연결 스팬과 링크로 잇는다(D33).
	ctx, cancel := context.WithCancel(trace.ContextWithSpanContext(r.Context(), trace.SpanContextFromContext(r.Context())))
	defer cancel()

	c := &conn{ws: ws, actor: actor, actorKey: app.ActorKey(actor), log: s.log}
	c.peerClose = -1
	// 업그레이드하는 사이에 내려가기 시작했으면 바로 1001 로 돌려보낸다.
	if !s.track(c) {
		_ = ws.Close(websocket.StatusGoingAway, "")
		return
	}
	defer s.untrack(c)
	// 연결 수 지표는 역할만 속성으로 싣는다(id 를 넣지 않는다). 끊길 때의 -1 은 r.Context() 가 끝난 뒤라 바탕 ctx 로 낸다.
	role := metric.WithAttributes(roleAttr(actor))
	telemetry.Metrics().WSConnections.Add(r.Context(), 1, role)
	defer telemetry.Metrics().WSConnections.Add(context.WithoutCancel(r.Context()), -1, role)
	// 연결 하나의 로그는 conn_id 로 잇는다. 행위자는 HTTP 쪽이 이미 얹었어도 이 패키지만으로 완결되게 다시 얹는다.
	ctx = logctx.With(ctx, connAttrs(actor)...)
	connected := time.Now()
	c.log.InfoContext(ctx, "ws.connected")
	inboxWS := ""
	if actor.Kind == app.ActorAgent {
		inboxWS = actor.WorkspaceID
	}
	sub := s.hub.Join(inboxWS)
	defer s.hub.Leave(sub)
	// 세션이 지워지면(로그아웃·비활성) 알림을 받아 곧바로 다시 확인한다(D46). 다시 확인하지 않으면 최대 60초(sessionRecheck) 열려 있다.
	s.hub.WatchSession(sub, actor.Session)

	subReqs := make(chan subRequest, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	// 세 고루틴 모두 패닉을 되살린다(D47). 연결 밖에서 도는 GoAway 의 닫기 고루틴과 받은함 신호 타이머도 같다(where=ws.goaway·ws.inbox_signal). 하나가 패닉으로 끝나면 이 연결만 1011 로 닫고 프로세스와 다른 연결은 산다.
	go func() {
		defer wg.Done()
		defer cancel()
		if telemetry.Guard(ctx, c.log, "ws.read", func(ctx context.Context) { s.readLoop(ctx, c, sub, subReqs) }) {
			c.closeOnPanic()
		}
	}()

	// ping 이 실패하면 상대가 사라진 것이다. 연결을 정리해 구독자와 goroutine 을 놓아준다.
	go func() {
		if telemetry.Guard(ctx, c.log, "ws.ping", func(ctx context.Context) { s.pingLoop(ctx, c) }) {
			c.closeOnPanic()
		}
		cancel()
	}()

	if telemetry.Guard(ctx, c.log, "ws.deliver", func(ctx context.Context) { s.deliverLoop(ctx, c, sub, subReqs, revalidate) }) {
		c.closeOnPanic()
	}
	cancel()
	wg.Wait()
	// 서버가 내려가며 닫기 시작했으면 GoAway 와 같은 1001 로 닫는다. 닫기 시작한 뒤 읽은 프레임을 버리면 읽기 루프가 먼저 끝나
	// 여기 닿는데, 1000 으로 닫으면 GoAway 의 1001 과 경합해 클라이언트가 어느 쪽을 받을지 갈린다. 먼저 닫힌 쪽이 이긴다.
	switch {
	case c.goingAway.Load():
		_ = ws.Close(websocket.StatusGoingAway, "")
	case c.panicked.Load():
		// 서버 잘못이다. 클라이언트는 다른 끊김과 같이 백오프로 다시 붙고 after_seq 로 이어 받는다.
		_ = ws.Close(websocket.StatusInternalError, "")
	default:
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}
	closeCode, closedBy := c.peerClose, "client"
	switch {
	case c.goingAway.Load():
		// 서버가 먼저 닫았다. 클라이언트가 되돌려 준 1001 을 클라이언트의 종료로 적지 않는다.
		closeCode, closedBy = websocket.StatusGoingAway, "server"
	case c.panicked.Load():
		closeCode, closedBy = websocket.StatusInternalError, "server"
	case c.endedSession.Load() || closeCode == -1:
		closeCode, closedBy = websocket.StatusNormalClosure, "server"
	}
	c.log.InfoContext(ctx, "ws.closed", "close_code", int(closeCode), "closed_by", closedBy,
		"duration_ms", time.Since(connected).Milliseconds(), "frames_sent", c.sent.Load())
}

// closeOnPanic 은 패닉으로 끝난 고루틴 대신 연결을 1011 로 닫는다. ctx 를 끊기 전에 닫는다 — 읽기 중인 ctx 를 끊으면
// 라이브러리가 닫기 프레임 없이 연결을 끊어 클라이언트는 1006 을 본다. 닫는 동안 읽기 루프는 계속 읽고, 닫히면 그 루프도 끝난다.
func (c *conn) closeOnPanic() {
	c.panicked.Store(true)
	_ = c.ws.Close(websocket.StatusInternalError, "")
}

// connAttrs 는 연결 ctx 에 얹는 키다.
func connAttrs(actor app.Actor) []any {
	args := []any{"conn_id", id.New(), "workspace_id", actor.WorkspaceID}
	if actor.Kind == app.ActorAgent {
		return append(args, "actor_kind", "agent", "actor_id", actor.AgentID)
	}
	return append(args, "actor_kind", "customer", "actor_id", actor.CustomerID, "customer_id", actor.CustomerID)
}

// roleAttr 는 지표의 역할 속성이다.
func roleAttr(actor app.Actor) attribute.KeyValue {
	if actor.Kind == app.ActorAgent {
		return telemetry.RoleAgent
	}
	return telemetry.RoleCustomer
}

// frameKinds 는 스팬 이름·속성과 로그에 쓰는 프레임 종류다. 모르는 종류는 unknown 으로 묶는다(frameKind) —
// 클라이언트가 보낸 문자열을 그대로 쓰면 스팬 이름이 끝없이 늘어나고, 로그에는 길이 제한 없는 입력이 실린다
// (감사 23: 60KB type 의 프레임 500 개에 로그 30MB, 이메일을 섞으면 그대로 남았다. D30).
var frameKinds = map[string]bool{"subscribe": true, "send": true, "read": true, "typing": true, "ping": true}

// frameKind 는 클라이언트 프레임의 type 을 로그·스팬에 실을 값으로 바꾼다. 아는 종류는 그대로, 그 밖은 unknown 이다.
// 클라이언트가 정한 글자를 그대로 싣는 길은 이것 하나로 막는다 — 스팬(startFrameSpan)과 로그(ws.frame·ws.rejected·ws.failed)가 같이 쓴다.
func frameKind(t string) string {
	if frameKinds[t] {
		return t
	}
	return "unknown"
}

// startFrameSpan 은 프레임 하나의 스팬을 연다(D33). 프레임마다 새 트레이스다 — 연결(업그레이드 요청)의 스팬은 몇 시간을 가므로
// 그 아래에 프레임을 매달지 않고 링크로 잇는다. 연결 유지용 ping 은 스팬을 열지 않는다. 양만 많고 볼 일이 없다.
// 속성은 종류와 대화 id 뿐이다. 본문은 싣지 않는다(D30).
func startFrameSpan(ctx context.Context, f clientFrame) (context.Context, trace.Span) {
	if f.Type == "ping" {
		return ctx, trace.SpanFromContext(context.Background())
	}
	kind := frameKind(f.Type)
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("chat.ws.frame", kind)),
	}
	if link := trace.LinkFromContext(ctx); link.SpanContext.IsValid() {
		opts = append(opts, trace.WithLinks(link))
	}
	if convID, err := id.Normalize(f.ConversationID); err == nil {
		opts = append(opts, trace.WithAttributes(attribute.String("chat.conversation_id", convID)))
	}
	return telemetry.Tracer().Start(ctx, "ws "+kind, opts...)
}

// frameContext 는 프레임 하나의 ctx 다. 프레임마다 새 trace_id 를 얹고, 프레임이 가리키는 대화가 있으면 conversation_id 를 얹는다.
// 구독한 대화가 아니라 프레임의 대화를 쓴다 — 한 연결이 구독과 다른 대화에 읽음·전송을 보낼 수 있다.
func frameContext(ctx context.Context, f clientFrame) context.Context {
	ctx = logctx.NewTrace(ctx)
	if convID, err := id.Normalize(f.ConversationID); err == nil {
		ctx = logctx.With(ctx, "conversation_id", convID)
	}
	return ctx
}

// readLoop 는 클라이언트 프레임을 처리한다. 저장은 여기서 하고, 전달은 전달 루프가 한다.
func (s *Server) readLoop(ctx context.Context, c *conn, sub *Subscriber, subReqs chan<- subRequest) {
	var lastTyping time.Time
	for {
		var f clientFrame
		if err := readJSON(ctx, c.ws, &f); err != nil {
			c.peerClose = websocket.CloseStatus(err)
			if ctx.Err() == nil && !isClosed(err) {
				// 클라이언트가 규칙을 어긴 프레임(바이너리·JSON 아님)이면 서버가 오류 프레임 없이 닫는다. 그 까닭을 INFO 로 남긴다.
				// 그 밖(상대가 사라짐 등)은 흔해서 DEBUG 다.
				level := slog.LevelDebug
				if errors.Is(err, errNotText) || errors.Is(err, errBadJSON) {
					level = slog.LevelInfo
				}
				c.log.Log(ctx, level, "ws.read_closed", "err_kind", logctx.ErrKind(err))
			}
			return
		}
		received := time.Now()
		// 스팬을 먼저 열고 trace 를 얹는다. 스팬이 있으면 로그의 trace_id 는 그 스팬의 것이다(logctx.SetSpanSource).
		fctx, span := startFrameSpan(ctx, f)
		ok := s.handleFrame(frameContext(fctx, f), c, sub, subReqs, f, &lastTyping, received)
		span.End()
		if !ok {
			return
		}
	}
}

// faultHook 도 테스트 전용 길이다(운영에서는 늘 nil). 읽기 말고 ping·GoAway·받은함 신호 고루틴 안에서 패닉을 일으켜 각 recover 를 확인한다(D47).
// 운영에서 켤 길이 없다: 소문자 패키지 변수라 이 패키지(의 _test.go)만 닿고, 비테스트 코드에는 Store 하는 곳이 없으며
// (internal/archtest 의 TestHook_… 가 지킨다) 설정·환경 변수·요청으로 켜는 길도 없다. nil 이면 Load 한 번이 비용의 전부다.
var faultHook atomic.Pointer[func(where string)]

func fault(where string) {
	if hook := faultHook.Load(); hook != nil {
		(*hook)(where)
	}
}

// frameHook 은 테스트 전용 길이다. 운영에서는 늘 nil 이다(켤 길이 없는 까닭은 faultHook 과 같다). 테스트가 프레임 처리 안에서 패닉을 일으켜
// 읽기 고루틴의 recover 를 확인한다(D47).
var frameHook atomic.Pointer[func(clientFrame)]

// handleFrame 은 프레임 하나를 처리한다. 처리하는 동안 frameMu 를 쥐어, 내려가는 서버가 저장 도중에 연결을 닫지 않게 한다.
// 서버가 이미 닫기 시작했으면 처리하지 않고 false 다. 그 프레임의 메시지는 ack 를 못 받았으니 클라이언트가 다시 보낸다.
func (s *Server) handleFrame(ctx context.Context, c *conn, sub *Subscriber, subReqs chan<- subRequest, f clientFrame, lastTyping *time.Time, received time.Time) bool {
	c.frameMu.Lock()
	defer c.frameMu.Unlock()
	if c.goingAway.Load() {
		return false
	}
	c.log.DebugContext(ctx, "ws.frame", "type", frameKind(f.Type))
	if hook := frameHook.Load(); hook != nil {
		(*hook)(f)
	}

	switch f.Type {
	case "subscribe":
		return s.handleSubscribe(ctx, c, sub, subReqs, f)
	case "send":
		s.handleSend(ctx, c, f, received)
	case "read":
		// 읽음 지점은 DB 에 남는다. 되돌아가는 보고는 저장소가 무시한다.
		if err := c.db(ctx, stepMarkRead, func(ctx context.Context) error {
			_, err := s.cmd.MarkRead(ctx, c.actor, f.ConversationID, f.Seq)
			return err
		}); err != nil {
			c.writeError(ctx, f.Type, errorCode(err), err, "")
		}
	case "typing":
		// 입력 중 신호는 잃어도 되는 대신 쏟아지면 안 된다. 연결당 간격을 둔다.
		if time.Since(*lastTyping) < typingInterval {
			return true
		}
		*lastTyping = time.Now()
		if err := c.db(ctx, stepTyping, func(ctx context.Context) error {
			return s.cmd.Typing(ctx, c.actor, f.ConversationID)
		}); err != nil {
			c.writeError(ctx, f.Type, errorCode(err), err, "")
		}
	case "ping":
		_ = c.write(ctx, serverFrame{Type: framePong})
	default:
		c.writeError(ctx, f.Type, errorCode(errBadFrame), errBadFrame, "")
	}
	return true
}

// handleSubscribe 는 구독 대상을 바꾼다. 권한은 여기서 한 번 확인하고,
// 전달 루프는 그 결과를 믿고 빠른 길을 탄다. false 면 연결을 끝낸다.
func (s *Server) handleSubscribe(ctx context.Context, c *conn, sub *Subscriber, subReqs chan<- subRequest, f clientFrame) bool {
	var conv core.Conversation
	err := c.db(ctx, stepAuthorize, func(ctx context.Context) (err error) {
		conv, err = s.qry.Authorize(ctx, c.actor, f.ConversationID)
		return err
	})
	if err != nil {
		c.writeError(ctx, f.Type, errorCode(err), err, "")
		return true
	}
	var reads []core.ReadState
	err = c.db(ctx, stepReadStates, func(ctx context.Context) (err error) {
		reads, err = s.qry.ReadStatesOf(ctx, c.actor, conv)
		return err
	})
	if err != nil {
		c.writeError(ctx, f.Type, errorCode(err), err, "")
		return true
	}

	after := f.AfterSeq
	if after < 0 {
		after = 0
	}
	// 클라이언트가 가진 번호가 서버보다 앞설 수는 없다. 어긋나면 서버 쪽으로 맞춘다.
	// 비교 대상은 보는 사람의 번호 공간이다.
	if latest := conv.ViewerSeq(c.actor.Internal()); after > latest {
		after = latest
	}

	s.hub.Bind(sub, conv.ID)
	c.log.InfoContext(ctx, "ws.subscribed", "conversation_id", conv.ID, "after_seq", after)
	select {
	case subReqs <- subRequest{conv: conv, afterSeq: after, reads: app.ReadViews(reads, c.actor.Internal())}:
		return true
	case <-ctx.Done():
		return false
	}
}

// handleSend 는 메시지를 저장하고 ack 를 보낸다.
// 커밋이 끝난 뒤에만 ack 를 보낸다 (불변식 I4). 프레임을 받은 때부터 ack 를 다 쓴 때까지를 지표로 낸다.
func (s *Server) handleSend(ctx context.Context, c *conn, f clientFrame, received time.Time) {
	// 시한을 넘기면 일시 오류 프레임이 나가고 글은 클라이언트 큐에 남아 같은 client_msg_id 로 다시 나간다. 시한 안에 커밋됐는데 ack 를
	// 못 받은 경우도 다시 보낸 글이 멱등 키로 처음 저장한 번호를 받는다(I3).
	var res core.AppendResult
	err := c.db(ctx, stepSend, func(ctx context.Context) (err error) {
		res, err = s.cmd.Send(ctx, c.actor, app.SendParams{
			ConversationID: f.ConversationID,
			ClientMsgID:    f.ClientMsgID,
			Body:           f.Body,
			AttachmentID:   f.AttachmentID,
			Internal:       f.Internal,
		})
		return err
	})
	if err != nil {
		c.writeError(ctx, f.Type, errorCode(err), err, f.ClientMsgID)
		return
	}
	if err := c.write(ctx, serverFrame{
		Type:           frameAck,
		ConversationID: res.Message.ConversationID,
		ClientMsgID:    res.Message.ClientMsgID,
		Seq:            res.Message.Seq,
		ID:             res.Message.ID,
	}); err != nil {
		return
	}
	telemetry.Metrics().AckDuration.Record(ctx, float64(time.Since(received).Microseconds())/1000,
		metric.WithAttributes(roleAttr(c.actor)))
}

// delivery 는 전달 루프가 들고 있는 상태다.
// 이 값들을 전달 루프 혼자만 만지게 해서 잠금 없이 돌아간다.
type delivery struct {
	// sub 는 이 연결의 허브 구독이다. 권한이 사라진 대화에서 떼어낼 때 쓴다.
	sub         *Subscriber
	convID      string
	sentSeq     int64
	fingerprint string
	lastAuth    time.Time
	// recheckSlack 은 재확인 판정의 여유다(틱의 절반). 틱과 재확인 주기가 같으면(운영 60초) 틱이 몇 ms 일찍 깨기만 해도
	// "아직 60초가 안 됐다" 로 그 틱을 건너뛰어 실제 간격이 120초가 됐다(감사 20: exp 70초 세션의 연결이 180초에 닫혔다).
	recheckSlack time.Duration
	// authUnknown 은 직전 세션 재확인이 답을 못 받았다는 표식이다. 연속 실패의 WARN 을 첫 번만 남긴다.
	authUnknown bool
	// deliverFailing 은 직전 전달용 조회가 일시 실패했다는 표식이다. 연속 실패의 WARN 을 첫 번만 남긴다(DEF-74).
	deliverFailing bool
	// retry 는 걸려 있는 다시 깨움이다(DEF-80). 연결마다 하나뿐이다 — 이미 걸려 있으면 새로 걸지 않고 retryHint 에 알림만 합친다.
	// retryArmed 가 거짓이면 타이머는 멈춰 있다. retryDelay 는 다음에 걸 간격이고 0 이면 처음(retryBase)이다.
	retry      *time.Timer
	retryArmed bool
	retryDelay time.Duration
	retryHint  Hint
	// 받은함 신호는 묶어서 보낸다. 메시지마다 보내면 상담원 쪽 조회가 그만큼 따라붙는다.
	lastInbox    time.Time
	inboxPending bool
}

// deliverLoop 는 깨우기 신호를 받아 DB 에서 아직 안 보낸 메시지를 읽어 내려보낸다.
// 보낼 내용의 출처는 DB 뿐이다 (불변식 I5).
func (s *Server) deliverLoop(ctx context.Context, c *conn, sub *Subscriber, subReqs <-chan subRequest, revalidate func(context.Context) error) {
	// 연결별 틱은 길게 돈다. 짧은 주기의 안전망은 ChangeWatcher 가 모아서 맡는다.
	tick := s.connTick
	if s.catchup > tick {
		tick = s.catchup
	}
	// 접속 때 세션을 확인했다. 확인 시각은 틱보다 먼저 찍는다 — 첫 틱이 확인 시각보다 앞서면 그 틱을 건너뛰어 재확인이 두 주기 뒤로 밀린다.
	d := &delivery{sub: sub, lastAuth: time.Now(), recheckSlack: tick / 2}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	defer d.cancelRetry()

	for {
		select {
		case <-ctx.Done():
			return

		case <-d.retryC():
			// 걸어 둔 다시 깨움이다. 실패한 조회를 부른 알림을 다시 얹고 알림과 같은 길로 간다 — 빠른 길은 같은 번호로, 느린 길은 Meta 로.
			// 그 사이 다른 알림이 쌓였으면 함께 합쳐진다.
			d.retryArmed = false
			sub.mergeHint(d.retryHint)
			d.retryHint = Hint{}
			if !s.onWake(ctx, c, sub, d) {
				return
			}

		case req := <-subReqs:
			if !s.onSubscribe(ctx, c, d, req) {
				return
			}

		case <-sub.Wake():
			if !s.onWake(ctx, c, sub, d) {
				return
			}

		case ev := <-sub.Live():
			if !s.onLive(ctx, c, d, ev) {
				return
			}

		case <-sub.InboxWake():
			if !s.onInboxWake(ctx, c, sub, d) {
				return
			}

		case <-ticker.C:
			if !s.onTick(ctx, c, sub, d, revalidate) {
				return
			}

		case <-sub.Ended():
			// 이 연결의 세션이 지워졌다는 알림이다. 주기를 기다리지 않고 지금 묻는다. 답이 "없다" 면 닫히고, "있다" 면 이어 간다.
			if revalidate != nil && !s.recheckSession(ctx, c, d, revalidate) {
				return
			}
		}
	}
}

// onSubscribe 는 구독 대상이 바뀌었을 때다. 커서와 표식을 새 대화로 갈아 끼운다.
func (s *Server) onSubscribe(ctx context.Context, c *conn, d *delivery, req subRequest) bool {
	conv := req.conv
	d.convID, d.sentSeq, d.fingerprint = conv.ID, req.afterSeq, conversationFingerprint(conv)
	// 앞 대화에 걸린 다시 깨움은 거둔다. 그 힌트의 번호는 앞 대화의 것이다.
	d.cancelRetry()
	err := c.write(ctx, serverFrame{
		Type:           frameReady,
		ConversationID: conv.ID,
		Conversation:   app.ForActor(c.actor, conv),
		LastSeq:        conv.ViewerSeq(c.actor.Internal()),
		State:          conv.State.String(),
		Reads:          req.reads,
	})
	if err != nil {
		return false
	}
	// 구독 직후 첫 전달이 실패해도 다시 깨운다(DEF-81). 전에는 이 길만 다시 깨움을 걸지 않아, 새 알림이 없으면 연결 틱(60초)까지 그 대화가 비었다.
	return s.drainWith(ctx, c, d)
}

// onWake 는 알림을 받았을 때다. 알림이 번호를 들고 왔으면 대화 행을 다시 읽지 않는다.
func (s *Server) onWake(ctx context.Context, c *conn, sub *Subscriber, d *delivery) bool {
	hint := sub.TakeHint()
	if !hint.Meta {
		if hint.ViewerSeq(c.actor.Internal()) <= d.sentSeq {
			return true // 보낼 것이 없다. 질의도 하지 않는다
		}
		return s.deliver(ctx, c, d, hint)
	}
	return s.drainWith(ctx, c, d)
}

// onLive 는 타이핑·읽음처럼 잃어도 되는 이벤트다.
func (s *Server) onLive(ctx context.Context, c *conn, d *delivery, ev core.LiveEvent) bool {
	// 구독한 대화의 것만 보낸다. 누구에게 보여도 되는지는 app 이 판정한다(D25).
	if ev.ConversationID != d.convID || !app.LiveVisible(c.actor, ev) {
		return true
	}
	return c.write(ctx, liveFrame(ev, c.actor.Internal())) == nil
}

// onInboxWake 는 받은함 갱신 신호다. 묶어서 보낸다.
//
// 받은함은 파생 뷰다. 메시지 한 건마다 알리면 상담원 한 명당 두 번의 조회가 따라붙고,
// 데스크 몇 개만 열려 있어도 그게 전체 지연을 초 단위로 밀어 올린다.
func (s *Server) onInboxWake(ctx context.Context, c *conn, sub *Subscriber, d *delivery) bool {
	if wait := inboxCoalesce - time.Since(d.lastInbox); wait > 0 {
		if !d.inboxPending {
			d.inboxPending = true
			// 타이머 고루틴은 연결 밖이라 recover 가 없으면 프로세스를 끝낸다(D47). 패닉이면 pending 이 영영 켜져 이 연결의 받은함 신호가 멎으므로
			// 그 연결만 1011 로 닫아 클라이언트가 다시 붙고 받은함을 새로 받게 한다.
			time.AfterFunc(wait, func() {
				if telemetry.Guard(ctx, c.log, "ws.inbox_signal", func(context.Context) { fault("ws.inbox_signal"); sub.SignalInbox() }) {
					c.closeOnPanic()
				}
			})
		}
		return true
	}
	d.inboxPending = false
	d.lastInbox = time.Now()
	return c.write(ctx, serverFrame{Type: frameInboxChanged}) == nil
}

// onTick 은 연결별 최후 안전망이다. 세션이 아직 살아 있는지도 여기서 다시 묻는다.
func (s *Server) onTick(ctx context.Context, c *conn, sub *Subscriber, d *delivery, revalidate func(context.Context) error) bool {
	if revalidate != nil && time.Since(d.lastAuth)+d.recheckSlack >= s.sessionRecheck {
		if !s.recheckSession(ctx, c, d, revalidate) {
			return false
		}
	}
	return s.drainWith(ctx, c, d)
}

// recheckSession 은 세션을 다시 묻고 그 답대로 한다. 연결을 이어 가면 true 다.
//
//	살아 있다      — 확인 시각을 갱신한다.
//	세션이 없다    — unauthorized 를 알리고 1000 으로 닫는다. 종료 중이면 1001 쪽에 맡긴다.
//	확인하지 못했다 — DB 가 잠깐 안 되는 것을 "세션 만료" 로 끊으면 고객까지 "다시 로그인해라" 를 받는다(DEF-02).
//	                 끊지 않고 프레임도 보내지 않는다. 확인 시각을 그대로 둬 다음 틱에 다시 묻는다.
//	                 WARN 은 연속 실패의 첫 번만 남긴다 — 장애 조치 동안 연결 수 × 틱만큼 쌓이지 않게.
func (s *Server) recheckSession(ctx context.Context, c *conn, d *delivery, revalidate func(context.Context) error) bool {
	// 확인 시각은 묻기 시작한 때다. 답을 받은 때로 찍으면 DB 지연만큼 다음 재확인이 늦어진다.
	asked := time.Now()
	err := c.db(ctx, stepRevalidate, revalidate)
	switch {
	case err == nil:
		d.lastAuth, d.authUnknown = asked, false
		return true
	case ctx.Err() != nil:
		return false // 연결이 끝나는 중이다. 확인 실패가 아니다
	case errors.Is(err, app.ErrNotFound):
		if c.goingAway.Load() {
			return false // 서버가 이미 1001 로 닫는 중이다. 종료 중에는 1000 을 내지 않는다
		}
		c.writeError(ctx, "", errorCode(errSessionGone), errSessionGone, "")
		// 정상 종료(1000)로 닫는다. 그냥 돌아가면 읽기 루프의 ctx 가 끊기며 close 프레임 없이 끊겨 클라이언트는 1006 을 본다.
		// 닫는 동안 읽기 루프는 계속 읽고, 닫히면 그 루프도 끝난다(GoAway 와 같다).
		c.endedSession.Store(true)
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
		return false
	default:
		if !d.authUnknown {
			d.authUnknown = true
			// 오류 문구는 싣지 않는다. 접속 문자열(사용자·DB 이름)이 들어갈 수 있다(D30). 종류만 남긴다.
			c.log.WarnContext(ctx, "ws.revalidate_failed", "reason", revalidateReason(err))
		}
		return true
	}
}

// revalidateReason 은 세션을 확인하지 못했거나 전달용 조회가 실패한 까닭의 종류다. ws.revalidate_failed·ws.deliver_failed 에 싣는다.
func revalidateReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// deliver 는 메시지만 내려보낸다. 대화 행은 읽지 않는다.
// 알림이 "여기까지 왔다" 를 들고 온 경우에 쓰는 빠른 길이다. hint 는 그 알림이다 — 조회가 일시 실패하면 같은 알림으로 다시 깨운다.
func (s *Server) deliver(ctx context.Context, c *conn, d *delivery, hint Hint) bool {
	if d.convID == "" {
		return true // 구독이 풀린 뒤 늦게 온 다시 깨움이다
	}
	for {
		var msgs []core.Message
		err := c.db(ctx, stepDeliver, func(ctx context.Context) (err error) {
			msgs, err = s.qry.Deliver(ctx, c.actor, d.convID, d.sentSeq, drainBatch)
			return err
		})
		if err != nil {
			return s.deliveryFailed(ctx, c, d, hint, err)
		}
		d.deliverFailing = false
		// 빠른 길의 성공은 번호만 메운다. 걸린 다시 깨움이 느린 길(Meta)이면 상태·담당자·태그는 아직이다 — 곧바로(처음 간격) 다시 묻게 남긴다.
		s.retrySucceeded(d, false)
		if !c.writeMessages(ctx, msgs, &d.sentSeq) {
			return false
		}
		if len(msgs) < drainBatch {
			return true
		}
	}
}

// deliveryFailed 는 조회 실패를 다룬다. 보내지 않으면 유실이 아니라 지연이다.
// 티커만 믿으면 지연 상한이 틱 간격이 되므로 스스로 다시 깨운다(armRetry). 깨울 때 again(실패한 조회를 부른 알림)을 다시 얹는다 —
// 힌트 없이 깨우면 onWake 가 "보낼 것이 없다" 로 보고 아무것도 묻지 않는다. 그러면 다시 묻는 것은 안전망(5초)이나 연결 틱(60초)뿐이다.
// 빠른 길은 같은 번호로 다시 빠른 길을, 느린 길은 Meta 로 다시 느린 길을 탄다.
//
// 없다·권한 없음은 일시적 실패가 아니다. 회원이 새 상담을 열어 앞 상담이 지난 상담이 됐거나 상담이 가려졌다.
// 다시 시도해도 같은 답이라 재시도하지 않고 구독을 푼다. 안 그러면 경고가 영원히 쌓이고, 빠른 길은 권한을 다시 보지 않아
// 그 대화의 수정·삭제가 계속 내려간다. 그 밖의 오류는 일시적이든 아니든 같은 다시 깨움을 건다 — 간격이 상한까지 늘어 DB 를 두드리지 않는다.
func (s *Server) deliveryFailed(ctx context.Context, c *conn, d *delivery, again Hint, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
		return s.revoke(ctx, c, d)
	}
	// 오류 문구는 싣지 않는다. 접속 정보(사용자·DB 이름)가 들어간다(D30). 장애 동안 연결 수 × 깨움만큼 쌓이지 않게
	// 같은 연결의 연속 실패는 첫 번만 남긴다. 조회가 한 번 성공하면 다시 센다(DEF-74).
	if !d.deliverFailing {
		d.deliverFailing = true
		c.log.WarnContext(ctx, "ws.deliver_failed", "conversation_id", d.convID, "reason", revalidateReason(err))
	}
	s.armRetry(d, again)
	return true
}

// armRetry 는 다시 깨움을 건다(DEF-80). 연결마다 하나뿐이다 — 이미 걸려 있으면 새로 걸지 않고 알림만 합친다.
// 전에는 실패할 때마다 0.5초 타이머를 새로 걸었다. 그 타이머의 깨움이 또 실패하면 또 걸어 사슬이 끝나지 않았고, 장애 동안 깨움
// (연결 틱·알림·안전망)이 올 때마다 사슬이 하나씩 늘었다 — DB 를 끊은 135초 동안 연결 40개의 접속 시도가 초당 0.8 → 320 번이었다(감사 15).
// 간격은 처음 0.5초에서 걸 때마다 두 배로 늘고 상한(30초)을 넘지 않는다. 아래로 20% 까지 흔든다 — 같은 순간 실패한 연결이
// 한꺼번에 다시 묻지 않게. 조회가 성공하면 처음으로 돌아간다(retrySucceeded).
func (s *Server) armRetry(d *delivery, again Hint) {
	d.retryHint = mergeHints(d.retryHint, again)
	if d.retryArmed {
		return
	}
	if d.retryDelay <= 0 {
		d.retryDelay = s.retryBase
	}
	wait := jitterDown(d.retryDelay)
	d.retryDelay = min(d.retryDelay*2, s.retryMax)
	d.startRetry(wait)
}

// retrySucceeded 는 조회가 성공했을 때다. 간격을 처음으로 돌린다. full 이면(느린 길) 대화 행과 메시지를 다 봤으니 걸린 다시 깨움을 거둔다.
// 빠른 길은 번호만 봤다 — 걸린 다시 깨움이 느린 길(Meta)이면 처음 간격으로 당겨 곧 다시 묻고, 아니면 거둔다.
func (s *Server) retrySucceeded(d *delivery, full bool) {
	d.retryDelay = 0
	if !d.retryArmed {
		return
	}
	if full || !d.retryHint.Meta {
		d.cancelRetry()
		return
	}
	d.retryHint = Hint{Meta: true}
	d.startRetry(jitterDown(s.retryBase))
}

func (d *delivery) startRetry(wait time.Duration) {
	if d.retry == nil {
		d.retry = time.NewTimer(wait)
	} else {
		d.retry.Reset(wait)
	}
	d.retryArmed = true
}

// cancelRetry 는 걸린 다시 깨움을 거둔다. 간격은 그대로 둔다 — 실패가 이어지는 동안 대화를 옮겨도 간격이 처음으로 돌아가지 않는다.
func (d *delivery) cancelRetry() {
	if d.retry != nil {
		d.retry.Stop()
	}
	d.retryArmed, d.retryHint = false, Hint{}
}

// retryC 는 걸린 다시 깨움의 채널이다. 걸려 있지 않으면 nil 이라 select 가 고르지 않는다.
func (d *delivery) retryC() <-chan time.Time {
	if !d.retryArmed {
		return nil
	}
	return d.retry.C
}

// mergeHints 는 두 알림을 합친다. 번호는 앞선 쪽, Meta 는 어느 쪽이든 켜졌으면 켠다(Subscriber.mergeHint 와 같다).
func mergeHints(a, b Hint) Hint {
	return Hint{WorkspaceID: a.WorkspaceID, Seq: max(a.Seq, b.Seq), PublicSeq: max(a.PublicSeq, b.PublicSeq), Meta: a.Meta || b.Meta}
}

// jitterDown 은 간격을 아래로 20% 까지 흔든다. 위로는 흔들지 않는다 — 간격의 상한이 정책이다.
func jitterDown(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/5+1)) //nolint:gosec // G404: 재시도 간격을 흩는 값이다. 맞혀도 얻는 것이 없다.
}

// revoke 는 권한이 사라진 대화의 구독을 풀고 클라이언트에 한 번 알린다. 연결은 끊지 않는다 — 다른 대화를 구독할 수 있다.
func (s *Server) revoke(ctx context.Context, c *conn, d *delivery) bool {
	convID := d.convID
	if d.sub != nil {
		s.hub.Unbind(d.sub, convID)
	}
	d.convID, d.sentSeq, d.fingerprint = "", 0, ""
	// 풀린 대화에 걸린 다시 깨움은 거둔다. 늦게 오더라도 deliver·drainWith 가 빈 구독을 묻지 않는다.
	d.cancelRetry()
	// 전달 고루틴의 ctx 에는 요청 하나의 trace 가 없다(프레임이 아니라 알림·틱이 깨운다). 이 풀림 하나에 새 trace 를 열어
	// 프레임의 trace_id(문의 번호)와 로그 줄을 잇는다. 전에는 프레임의 trace_id 가 비어 찾을 곳이 없었다(감사 23 N13).
	// 트레이스를 내보내면 연결 ctx 에 업그레이드 요청의 스팬(기록하지 않는 것)이 있어 logctx 의 새 id 대신 그 연결 내내 같은 trace 가
	// 로그와 프레임에 실렸다(감사 24). 프레임처럼 새 루트 스팬을 열고 연결 스팬과 링크로 잇는다.
	ctx, span := telemetry.Tracer().Start(logctx.NewTrace(ctx), "ws unsubscribe", trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithLinks(trace.LinkFromContext(ctx)),
		trace.WithAttributes(attribute.String("chat.conversation_id", convID)))
	defer span.End()
	c.log.InfoContext(ctx, "ws.unsubscribed", "conversation_id", convID, "reason", "not_found")
	return c.write(ctx, serverFrame{
		Type:           frameError,
		ConversationID: convID,
		Code:           "not_found",
		Message:        userMessage(app.ErrNotFound),
		TraceID:        logctx.TraceID(ctx),
	}) == nil
}

// writeMessages 는 보낸 만큼만 커서를 올린다. 쓰기가 성공한 뒤에 올리는 게 핵심이다.
func (c *conn) writeMessages(ctx context.Context, msgs []core.Message, sentSeq *int64) bool {
	for _, m := range msgs {
		if err := c.write(ctx, messageFrame(m)); err != nil {
			return false
		}
		c.log.DebugContext(ctx, "delivery.message", "conversation_id", m.ConversationID, "seq", m.Seq)
		*sentSeq = m.Seq
	}
	return true
}

// drainWith 는 대화 행을 다시 읽어 메타 변화까지 확인하는 느린 길이다.
// 알림이 번호를 안 들고 왔거나 메타가 바뀌었을 때만 탄다.
func (s *Server) drainWith(ctx context.Context, c *conn, d *delivery) bool {
	if d.convID == "" {
		return true
	}
	for {
		var conv core.Conversation
		var msgs []core.Message
		err := c.db(ctx, stepDeliver, func(ctx context.Context) (err error) {
			conv, msgs, err = s.qry.Stream(ctx, c.actor, d.convID, d.sentSeq, drainBatch)
			return err
		})
		if err != nil {
			return s.deliveryFailed(ctx, c, d, Hint{Meta: true}, err)
		}
		d.deliverFailing = false
		// 상태·담당자·태그는 메시지가 아니라서 번호로는 보이지 않는다. 먼저 알린다.
		sent, tagsKnown := c.writeConversationIfChanged(ctx, s.qry, conv, &d.fingerprint)
		if !sent {
			return false
		}
		if tagsKnown {
			s.retrySucceeded(d, true)
		} else {
			// 태그를 못 읽었다(DEF-83). 지문을 그대로 뒀으니 다음 느린 길이 대화 프레임을 다시 보낸다 — 그 기회를 다시 깨움으로 만든다.
			// 실패로 센다(간격을 처음으로 돌리지 않는다). 태그 조회만 계속 실패해도 다시 깨움이 0.5초마다 돌지 않는다.
			s.armRetry(d, Hint{Meta: true})
		}
		if !c.writeMessages(ctx, msgs, &d.sentSeq) {
			return false
		}
		if len(msgs) < drainBatch {
			return true
		}
	}
}

// writeConversationIfChanged 는 지문이 바뀌었으면 대화 프레임을 보낸다. 쓰기에 실패하면 sent 가 거짓이다.
// tagsKnown 이 거짓이면 상담원에게 보낼 태그를 읽지 못했다(DEF-83) — 프레임은 tags 필드 없이 나가고(화면은 가진 태그를 둔다)
// 지문은 갱신하지 않아 다음 느린 길에 다시 보낸다. 전에는 태그 없이(빈 목록) 보내고 지문을 갱신해 화면의 태그가 지워진 채 남았다.
func (c *conn) writeConversationIfChanged(ctx context.Context, svc *app.Queries, conv core.Conversation, fingerprint *string) (sent, tagsKnown bool) {
	current := conversationFingerprint(conv)
	if current == *fingerprint {
		return true, true
	}
	tagsKnown = true
	// 태그는 상담원 화면에만 필요하다. 고객에게 보낼 땐 읽지도 않는다.
	if c.actor.Internal() {
		// 시한을 넘기면 ws.db_timeout(step=tags)을 남긴다.
		err := c.db(ctx, stepTags, func(ctx context.Context) (err error) {
			conv, err = svc.WithTags(ctx, conv)
			return err
		})
		tagsKnown = err == nil
	}
	if tagsKnown {
		*fingerprint = current
	}
	return c.write(ctx, conversationFrame(c.actor, conv, tagsKnown)) == nil, tagsKnown
}

func (s *Server) pingLoop(ctx context.Context, c *conn) {
	ticker := time.NewTicker(s.pingEvery)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fault("ws.ping")
			pingCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err == nil {
				misses = 0
				continue
			}
			misses++
			// 서버가 잠깐 밀렸을 뿐일 수 있다. 한 번 놓쳤다고 끊으면
			// 부하가 몰린 순간 전원이 동시에 끊기고 재접속이 몰려 더 나빠진다.
			if misses < pingMissLimit {
				// 한 번 더 본다. 연속 pingMissLimit 번이면 끊는다.
				c.log.WarnContext(ctx, "ws.ping_missed", "misses", misses, "err_kind", logctx.ErrKind(err))
				continue
			}
			// 연속으로 답이 없어 상대가 사라진 것으로 보고 연결을 정리한다.
			c.log.InfoContext(ctx, "ws.ping_timeout", "misses", misses, "err_kind", logctx.ErrKind(err))
			return
		}
	}
}

func (c *conn) write(ctx context.Context, f serverFrame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	// 쓰기가 타임아웃되면 연결을 버린다. 클라이언트가 재접속해서 캐치업하면 유실은 없다.
	if err := c.ws.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return err
	}
	c.sent.Add(1)
	return nil
}

// writeError 는 오류 프레임 하나를 보내고 같은 ctx 로 로그 한 줄을 남긴다. 프레임의 trace_id 와 그 줄의 trace_id 가 같다 —
// 화면이 보여 준 "문의 번호" 로 로그를 찾는다(D47). frameType 은 거절된 클라이언트 프레임의 type 이다(세션 재확인 등 프레임이 아니면 빈 값).
//
//	internal(서버 잘못)       ws.failed ERROR, err_kind
//	internal(DB 시한 초과)    ws.rejected WARN, reason=db_timeout. ws.db_timeout 은 같은 단계의 연속 시한 초과를 첫 번만 남기므로 따로 남긴다
//	그 밖(거절)               ws.rejected WARN, code, reason(고정 사유가 있을 때)
func (c *conn) writeError(ctx context.Context, frameType, code string, err error, clientMsgID string) {
	// 닫히는 중이면 보낼 곳도 알릴 것도 없다.
	if code == "canceled" {
		return
	}
	args := []any{"code", code}
	// frame·client_msg_id 는 클라이언트가 정한 글자다. 아는 종류와 모양이 맞는 id 만 싣는다(D30, 감사 23).
	if frameType != "" {
		args = append(args, "frame", frameKind(frameType))
	}
	if cmid := app.LoggableClientMsgID(clientMsgID); cmid != "" {
		args = append(args, "client_msg_id", cmid)
	}
	// 오류 원문은 싣지 않는다. DB 연결 오류 문구에는 접속 정보(user=·database=·호스트)가 들어간다(D30, DEF-77). 종류만 남긴다.
	switch {
	case code == "internal" && errors.Is(err, errDBTimeout):
		c.log.WarnContext(ctx, "ws.rejected", append(args, "reason", "db_timeout")...)
	case code == "internal":
		c.log.ErrorContext(ctx, "ws.failed", append(args, "actor", c.actorKey, "err_kind", logctx.ErrKind(err))...)
	default:
		if reason := frameReason(err, code); reason != "" {
			args = append(args, "reason", reason)
		}
		c.log.WarnContext(ctx, "ws.rejected", args...)
	}
	// 권한 거절은 HTTP 와 같은 이름으로 남긴다. 어느 길로 왔든 auth.denied 로 거른다.
	// 권한(permission) 거절은 app 이 이미 남겼으므로 여기서는 그 밖의 forbidden 만 남긴다.
	if code == "forbidden" && !errors.Is(err, app.ErrPermission) {
		c.log.WarnContext(ctx, "auth.denied", "code", code)
	}
	_ = c.write(ctx, serverFrame{
		Type:        frameError,
		Code:        code,
		Message:     userMessage(err),
		ClientMsgID: clientMsgID,
		TraceID:     logctx.TraceID(ctx),
	})
}

// frameReason 은 프레임 code 보다 좁은 고정 사유다(D47). 입력 거절은 InvalidError.Code, 그 밖은 오류 사슬의 가장 좁은 코드가 code 와 다를 때다.
func frameReason(err error, code string) string {
	var inv app.InvalidError
	if errors.As(err, &inv) {
		return inv.Code
	}
	if c := core.ErrCode(err); c != code {
		return c
	}
	return ""
}

func readJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errNotText
	}
	if json.Unmarshal(data, v) != nil {
		return errBadJSON
	}
	return nil
}

// 읽기에서 연결을 닫는 클라이언트 잘못. 오류 프레임 없이 1000 으로 닫는다(api.md 9절). ws.read_closed 의 err_kind 가 이 코드다.
var (
	errNotText = core.NewError("not_text_frame", "realtime: 텍스트 프레임만 받는데 바이너리 프레임이 왔다")
	errBadJSON = core.NewError("bad_json", "realtime: 프레임을 JSON 으로 읽을 수 없다")
)

func isClosed(err error) bool {
	return websocket.CloseStatus(err) != -1 || errors.Is(err, context.Canceled)
}

// errSessionGone 은 열린 연결의 세션 재확인에서 세션이 없다고 답했을 때다(로그아웃·비활성·만료). 오류 프레임 뒤에 1000 으로 닫는다.
var errSessionGone = core.NewError("session_gone", "realtime: 세션이 지워졌거나 만료돼 이 연결을 닫는다")

// errBadFrame 은 클라이언트가 보낸 프레임의 type 을 모를 때다. 서버 장애가 아니라 클라이언트 잘못이다.
var errBadFrame = core.NewError("bad_frame", "realtime: 프레임의 type 이 subscribe·send·read·typing·ping 중 하나가 아니다")

// errorFrames 는 사람에게 알리는 도메인 오류마다 프레임의 code 와 message 를 한 짝으로 정한다.
// code 는 바깥 계약이라 바꾸지 않는다. 둘을 따로 고르면 같은 오류의 code 와 문구가 어긋난다.
var errorFrames = []struct {
	err           error
	code, message string
}{
	// 문구는 HTTP 의 같은 상황과 같다(httpapi.fail). 순서가 뜻이다 — 좁은 오류(ErrNotOwner)가 넓은 오류(ErrForbidden)보다 앞이다.
	{app.ErrNotOwner, "forbidden", "자기가 보낸 메시지만 고치거나 지울 수 있습니다"},
	{app.ErrPermission, "forbidden", "이 작업을 할 권한이 없습니다"},
	{app.ErrAgentOnly, "forbidden", "상담원만 할 수 있습니다"},
	{app.ErrForbidden, "forbidden", "이 대화에 접근할 수 없습니다"},
	{app.ErrNotFound, "not_found", "상담을 찾을 수 없습니다. 이미 지워졌을 수 있습니다"},
	// 고객이 직접 닫은 상담에 고객이 쓴 글이다(D48). HTTP 의 409 closed_by_customer 와 같은 문구다.
	{app.ErrClosedByCustomer, "closed_by_customer", "종료한 상담에는 메시지를 보낼 수 없습니다. 새로 문의하려면 새 문의하기를 눌러 주세요"},
	{app.ErrEnded, "ended", "종료된 상담입니다. 새 문의를 시작해 주세요"},
	// 충돌은 같은 값(중복 키)이다. HTTP 의 409 conflict 기본 문구와 같다.
	{app.ErrConflict, "conflict", "같은 값이 이미 있어 저장하지 않았습니다"},
	// 클라이언트가 고칠 일이다. 내부 오류 문구("잠시 뒤 다시 시도")로 알리면 고칠 곳을 잘못 짚는다. 옛 화면이 남은 경우가 가장 흔해 새로 고침을 권한다.
	{errBadFrame, "bad_frame", "알 수 없는 종류의 요청입니다. 페이지를 새로 고쳐 주세요"},
	// 열린 연결의 세션 재확인이 실패했다(로그아웃·폐기·만료). 이 프레임 뒤에 서버가 1000 으로 닫는다.
	{errSessionGone, "unauthorized", "로그인 정보가 만료되었습니다. 다시 로그인하거나 페이지를 새로 고쳐 주세요"},
}

// internalMessage 는 분류되지 않은 실패다. HTTP 500 의 문구와 같다. 화면이 프레임의 trace_id 앞 8자리를 문의 번호로 붙인다.
const internalMessage = "서버 오류로 요청을 처리하지 못했습니다. 잠시 후 다시 시도해 주세요"

func errorCode(err error) string {
	var inv app.InvalidError
	switch {
	case errors.Is(err, errDBTimeout):
		// DB 호출이 시한을 넘겼다. 연결은 살아 있으니 일시 오류로 알린다 — 클라이언트가 같은 글·구독을 다시 보낸다(DEF-71).
		return "internal"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 연결이 닫히는 중이다. 실패가 아니라 종료 과정이다.
		return "canceled"
	case errors.As(err, &inv):
		return "invalid"
	}
	for _, f := range errorFrames {
		if errors.Is(err, f.err) {
			return f.code
		}
	}
	return "internal"
}

// userMessage 는 내부 오류 문구를 브라우저에 그대로 흘리지 않는다.
func userMessage(err error) string {
	var inv app.InvalidError
	if errors.As(err, &inv) {
		return inv.Reason
	}
	for _, f := range errorFrames {
		if errors.Is(err, f.err) {
			return f.message
		}
	}
	return internalMessage
}

// isClosing 은 GoAway 가 불렸는지다.
func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// track 은 열린 연결을 목록에 넣는다. 이미 내려가는 중이면 넣지 않고 false 다.
func (s *Server) track(c *conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.conns[c] = struct{}{}
	s.handlers.Add(1)
	return true
}

func (s *Server) untrack(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.handlers.Done()
}

// GoAway 는 서버가 내려갈 때 부른다. 새 연결을 더 받지 않고, 열린 연결마다 처리 중인 프레임이 끝나기를 기다려
// close 1001(going away)로 닫는다. 진행 중인 저장의 ctx 는 끊지 않는다 — 커밋과 ack 가 끝난 뒤에 닫는다.
// http.Server.Shutdown 은 업그레이드한 연결을 모르므로 이게 없으면 연결은 프로세스와 함께 1006 으로 끊긴다.
//
// 닫은 연결 수와, ctx 가 먼저 끝나 아직 남은 연결 수를 돌려준다. DB 커넥션을 쥔 채 기다리지 않는다(D28) —
// 기다리는 쪽은 연결의 처리 차례만 기다리고, DB 를 쥔 쪽(저장)은 다른 자원을 기다리지 않는다.
func (s *Server) GoAway(ctx context.Context) (closed, left int) {
	s.mu.Lock()
	s.closing = true
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var closing sync.WaitGroup
	for _, c := range conns {
		closing.Add(1)
		go func() {
			defer closing.Done()
			// 이 고루틴은 연결의 Handle 밖이라 recover 가 없으면 종료 중에 프로세스가 패닉으로 끝난다(D47). 패닉이면 그 연결만 1011 로 닫는다.
			if telemetry.Guard(ctx, c.log, "ws.goaway", func(context.Context) { c.goAway() }) {
				c.closeOnPanic()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		closing.Wait()
		s.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return len(conns), 0
	case <-ctx.Done():
		s.mu.Lock()
		left = len(s.conns)
		s.mu.Unlock()
		return len(conns) - left, left
	}
}

// goAway 는 처리 중인 프레임이 끝나기를 기다려 1001 로 닫는다. 닫는 동안 온 프레임은 처리하지 않는다.
func (c *conn) goAway() {
	c.goingAway.Store(true)
	c.frameMu.Lock()
	defer c.frameMu.Unlock()
	fault("ws.goaway")
	_ = c.ws.Close(websocket.StatusGoingAway, "")
}

// Resync 는 알림을 놓쳤을 가능성이 있을 때 전원을 깨운다.
func (s *Server) Resync() {
	telemetry.Metrics().Resyncs.Add(context.Background(), 1)
	s.hub.Resync()
}

// SessionsEnded 는 세션이 지워졌다는 알림을 그 세션의 연결에 넘긴다. 알림 리스너가 호출한다(D46).
func (s *Server) SessionsEnded(refs []core.SessionRef) { s.hub.SessionsEnded(refs) }

// Live 는 휘발성 이벤트를 구독자에게 넘긴다. 알림 리스너가 호출한다.
func (s *Server) Live(ev core.LiveEvent) { s.hub.DeliverLive(ev) }
