package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 감사 15(DEF-80): 전달 조회가 실패할 때마다 0.5초 타이머를 새로 걸었다. 그 깨움이 또 실패하면 또 걸어 사슬이 끝나지 않았고, 장애 동안
// 깨움(연결 틱·알림)이 올 때마다 사슬이 하나씩 늘었다 — DB 를 끊은 135초 동안 연결 40개의 DB 접속 시도가 초당 0.8 → 320 번이었다.
// 여기서는 조회 시각을 세는 저장소 대역으로 연결 하나의 다시 묻기 간격을 본다. 연결별 안전망(틱)은 돌지 않는다.

// retryStore 는 메시지 조회(빠른 길·느린 길 공통)와 대화 행 조회(느린 길)를 실패하게 할 수 있고, 메시지 조회 시각을 남긴다.
type retryStore struct {
	*pgstore.Store
	failList, failMeta atomic.Bool
	mu                 sync.Mutex
	listAt             []time.Time
	// listBy·metaBy 는 대화별 메시지 조회·대화 행 조회 시각이다. 대화마다 연결 하나를 붙여 연결별 간격을 본다.
	listBy, metaBy map[string][]time.Time
}

func (s *retryStore) ListMessages(ctx context.Context, convID string, afterSeq int64, limit int, internal bool) ([]core.Message, error) {
	s.mu.Lock()
	now := time.Now()
	s.listAt = append(s.listAt, now)
	if s.listBy == nil {
		s.listBy = map[string][]time.Time{}
	}
	s.listBy[convID] = append(s.listBy[convID], now)
	s.mu.Unlock()
	if s.failList.Load() {
		return nil, leakErr{}
	}
	return s.Store.ListMessages(ctx, convID, afterSeq, limit, internal)
}

func (s *retryStore) ConversationMeta(ctx context.Context, convID string) (core.Conversation, error) {
	s.mu.Lock()
	if s.metaBy == nil {
		s.metaBy = map[string][]time.Time{}
	}
	s.metaBy[convID] = append(s.metaBy[convID], time.Now())
	s.mu.Unlock()
	if s.failMeta.Load() {
		return core.Conversation{}, leakErr{}
	}
	return s.Store.ConversationMeta(ctx, convID)
}

// callsOf 는 since 뒤 그 대화의 조회 시각이다. meta 면 대화 행 조회, 아니면 메시지 조회.
func (s *retryStore) callsOf(convID string, meta bool, since time.Time) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.listBy[convID]
	if meta {
		src = s.metaBy[convID]
	}
	var out []time.Time
	for _, at := range src {
		if at.After(since) {
			out = append(out, at)
		}
	}
	return out
}

// calls 는 since 뒤의 메시지 조회 시각이다.
func (s *retryStore) calls(since time.Time) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Time
	for _, at := range s.listAt {
		if at.After(since) {
			out = append(out, at)
		}
	}
	return out
}

func retryServer(shared *pgstore.Store) (*Server, *retryStore, *syncBuffer) {
	st := &retryStore{Store: shared}
	logs := &syncBuffer{}
	srv := NewServer(app.New(st), NewHub(), 10*time.Millisecond, slog.New(logctx.NewHandler(slog.NewJSONHandler(logs, nil))))
	srv.connTick = time.Hour
	return srv, st, logs
}

func gaps(at []time.Time) []time.Duration {
	var out []time.Duration
	for i := 1; i < len(at); i++ {
		out = append(out, at[i].Sub(at[i-1]))
	}
	return out
}

func postAgentReply(t *testing.T, ctx context.Context, shared *pgstore.Store, convID, agentID, body string) {
	t.Helper()
	if _, err := shared.AppendMessage(ctx, core.AppendParams{ConversationID: convID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
		SenderAgentID: agentID, Visibility: core.Public, Body: body}); err != nil {
		t.Fatal(err)
	}
}

// 정책 숫자는 리터럴로 고정한다. 처음 0.5초, 상한 30초.
func TestRetry_전달다시깨움의간격은0점5초에서시작해30초를넘지않는다(t *testing.T) {
	srv := NewServer(&app.App{}, NewHub(), time.Second, slog.Default())
	if srv.retryBase != 500*time.Millisecond || srv.retryMax != 30*time.Second {
		t.Fatalf("다시 깨움 간격이 처음 %v·상한 %v 다. 0.5초·30초여야 한다", srv.retryBase, srv.retryMax)
	}
	// 흔들림은 아래 TestRetry_같은순간실패한… 가 실제 다시 묻는 시각으로 본다(정책 611).
}

// 실패가 이어지면 다시 묻는 간격이 0.5·1·2·4초로 늘고, 조회가 성공하면 다음 장애는 다시 0.5초부터다.
func TestRetry_전달조회가이어서실패하면다시묻는간격이두배씩늘고성공하면처음으로돌아간다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, _ := retryServer(shared)
	listen(t, ctx, srv)
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	st.failList.Store(true)
	start := time.Now()
	postAgentReply(t, ctx, shared, conv.ID, agent.ID, "장애 1 동안 쓴 답")
	time.Sleep(8 * time.Second) // 첫 조회 + 다시 묻기 0.5·1·2·4초 = 7.5초
	at := st.calls(start)
	g := gaps(at)
	if len(g) < 4 {
		t.Fatalf("8초 동안 조회가 %d 번이다(간격 %v). 첫 조회와 다시 묻기 4번이어야 한다", len(at), g)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if g[i] < w*8/10-30*time.Millisecond || g[i] > w+150*time.Millisecond {
			t.Errorf("%d번째 다시 묻기까지 %v 다. %v 의 80~100%% 여야 한다(전체 %v)", i+1, g[i], w, g)
		}
	}
	if len(at) > 5 {
		t.Errorf("8초 동안 조회가 %d 번이다. 다시 깨움이 하나면 5번이다: %v", len(at), g)
	}

	// 성공하면 처음으로 — 다음 알림의 조회가 성공해 간격을 되돌린다
	st.failList.Store(false)
	postAgentReply(t, ctx, shared, conv.ID, agent.ID, "복구 뒤 답")
	for got := 0; got < 2; {
		if f := awaitFrame(t, frames, closed, 5*time.Second, notInbox, "복구 뒤 메시지"); f.Type == frameMessage {
			got++
		}
	}
	st.failList.Store(true)
	start = time.Now()
	postAgentReply(t, ctx, shared, conv.ID, agent.ID, "장애 2 동안 쓴 답")
	time.Sleep(1200 * time.Millisecond)
	g = gaps(st.calls(start))
	if len(g) == 0 || g[0] > 550*time.Millisecond {
		t.Fatalf("성공한 뒤 다음 장애의 첫 다시 묻기까지 %v 다. 처음(0.5초)으로 돌아가야 한다", g)
	}
	st.failList.Store(false)
	f := awaitFrame(t, frames, closed, 3*time.Second, notInbox, "두 번째 장애 뒤 메시지")
	if f.Type != frameMessage || f.Body != "장애 2 동안 쓴 답" {
		t.Fatalf("두 번째 장애 뒤 첫 프레임이 %+v 다", f)
	}
}

// 장애 동안 알림이 여러 번 와도 걸린 다시 깨움은 하나다. 알림마다 사슬이 늘면 조회가 알림 수만큼 곱해진다.
func TestRetry_장애동안알림이여러번와도연결당다시깨움은하나다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := retryServer(shared)
	listen(t, ctx, srv)
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	st.failList.Store(true)
	for i := range 10 {
		postAgentReply(t, ctx, shared, conv.ID, agent.ID, "장애 동안 쓴 답 "+string(rune('0'+i)))
		time.Sleep(170 * time.Millisecond)
	}
	// 알림이 그친 뒤 6초 동안 조회는 다시 깨움 사슬 하나의 몫뿐이다(0.5·1·2·4초 간격 중 남은 것). 사슬이 알림마다 늘었다면 10개 × 0.5초 = 120번이다.
	quiet := time.Now().Add(300 * time.Millisecond)
	time.Sleep(6300 * time.Millisecond)
	if n := len(st.calls(quiet)); n > 3 {
		t.Errorf("알림이 그친 뒤 6초 동안 조회가 %d 번이다. 다시 깨움은 연결당 하나여야 한다(3번 이하): %v", n, gaps(st.calls(quiet)))
	}
	if n := len(logLines(logs, "ws.deliver_failed")); n != 1 {
		t.Errorf("ws.deliver_failed 가 %d 줄이다. 연속 실패는 첫 번만 남긴다", n)
	}
	// 복구 뒤 다음 다시 깨움에 열 건이 다 온다
	st.failList.Store(false)
	got := 0
	for got < 10 {
		if f := awaitFrame(t, frames, closed, 10*time.Second, notInbox, "복구 뒤 장애 동안 쓴 답"); f.Type == frameMessage {
			got++
		}
	}
}

// 간격은 상한을 넘지 않는다. 상한에 닿은 뒤에도 같은 간격(아래로 20% 까지 흔들림)으로 계속 묻는다 — 일시적이지 않은 오류라도 같다.
func TestRetry_다시묻는간격은상한을넘지않고상한에서계속묻는다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, _ := retryServer(shared)
	srv.retryBase, srv.retryMax = 20*time.Millisecond, 200*time.Millisecond // 0.02·0.04·0.08·0.16·0.2·0.2… 로 줄여 본다
	listen(t, ctx, srv)
	ws, _, _ := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	st.failList.Store(true)
	start := time.Now()
	postAgentReply(t, ctx, shared, conv.ID, agent.ID, "장애 동안 쓴 답")
	time.Sleep(2500 * time.Millisecond)
	g := gaps(st.calls(start))
	if len(g) < 10 {
		t.Fatalf("2.5초 동안 다시 묻기가 %d 번이다. 상한에 닿아도 계속 묻는다: %v", len(g), g)
	}
	for i, d := range g {
		if d > 200*time.Millisecond+60*time.Millisecond {
			t.Errorf("%d번째 간격이 %v 다. 상한 200ms 를 넘었다", i+1, d)
		}
	}
	for _, d := range g[len(g)-4:] {
		if d < 160*time.Millisecond-20*time.Millisecond {
			t.Errorf("상한에 닿은 뒤 간격이 %v 다. 상한의 80%% 아래로 흔들지 않는다: %v", d, g)
		}
	}
}

// 느린 길(대화 행을 다시 읽는 길)이 실패해도 다시 깨울 때 Meta 를 다시 얹어 느린 길로 다시 묻는다(감사 15 F2 — 빈 힌트로 깨우면 아무것도 묻지 않는다).
func TestRetry_느린길전달이실패하면Meta로다시깨워복구뒤알림없이상태와메시지가온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := retryServer(shared) // 리스너 없음 — 깨움은 아래 한 번의 Meta 알림과 다시 깨움뿐이다
	ws, frames, closed := dialSubscribed(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}, conv.ID, nil)
	defer ws.CloseNow()

	postAgentReply(t, ctx, shared, conv.ID, agent.ID, "느린 길이 실패한 동안 쓴 답")
	st.failMeta.Store(true)
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	waitLog(t, logs, "ws.deliver_failed", 3*time.Second)
	st.failMeta.Store(false)
	f := awaitFrame(t, frames, closed, 2*time.Second, notInbox, "실패한 느린 길을 다시 묻지 않는다")
	if f.Type != frameMessage || f.Body != "느린 길이 실패한 동안 쓴 답" {
		t.Fatalf("첫 프레임이 %+v 다", f)
	}
}

// 구독 직후 첫 전달이 실패해도 다시 깨운다(DEF-81, 감사 15 — 첫 전달이 0.3초 실패하면 8초 안에 0/10).
// 새 알림이 없고 연결 틱이 1시간이어도 밀린 메시지가 온다.
func TestRetry_구독직후첫전달이실패해도다시깨워복구뒤알림없이밀린메시지가온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	for range 3 {
		postAgentReply(t, ctx, shared, conv.ID, agent.ID, "오프라인 동안 온 답")
	}
	for round := range 5 {
		srv, st, _ := retryServer(shared)
		st.failList.Store(true)
		_, frames := dialJSON(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID})
		frames.send(t, ctx, map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
		frames.await(t, "ready", 5*time.Second)
		time.Sleep(300 * time.Millisecond)
		st.failList.Store(false) // DB 가 0.3초 만에 돌아왔다
		frames.await(t, "message", 2*time.Second)
		_ = round
	}
}

// 구독이 풀린 뒤 늦게 온 다시 깨움은 DB 를 묻지 않는다(감사 15 F3). 빈 대화 id 로 묻으면 조회가 형식 오류로 실패하고 그 실패가 또 다시 깨움을 건다.
func TestRetry_구독이풀린뒤늦게온다시깨움은DB를묻지않고다시깨움도걸지않는다(t *testing.T) {
	shared := testsupport.Store(t)
	srv, st, logs := retryServer(shared)
	c := &conn{log: srv.log, actor: app.Actor{Kind: app.ActorCustomer, CustomerID: id.New(), WorkspaceID: core.DefaultWorkspaceID}}
	d := &delivery{}
	start := time.Now()
	if !srv.deliver(context.Background(), c, d, Hint{Seq: 7, PublicSeq: 7}) || !srv.drainWith(context.Background(), c, d) {
		t.Fatal("빈 구독의 다시 깨움이 연결을 끝냈다")
	}
	if n := len(st.calls(start)); n != 0 {
		t.Errorf("빈 구독으로 메시지를 %d 번 조회했다", n)
	}
	if d.retryArmed {
		t.Error("빈 구독의 다시 깨움이 또 다시 깨움을 걸었다")
	}
	if n := len(logLines(logs, "ws.deliver_failed")); n != 0 {
		t.Errorf("빈 구독의 다시 깨움이 ws.deliver_failed 를 %d 줄 남겼다", n)
	}
	d.cancelRetry()
}

// DEF-83: 태그 조회가 실패하면 대화 프레임은 tags 필드 없이 나간다("태그를 모름") — 빈 목록("태그 없음")과 다르다. 화면은 필드가 없으면
// 가진 태그를 그대로 둔다. 지문을 갱신하지 않아 다음 기회(다시 깨움)에 태그를 실은 프레임을 다시 보낸다.
func TestRetry_태그조회가실패하면대화프레임에tags를싣지않고다음기회에태그를실어다시보낸다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	tag, err := shared.EnsureTag(ctx, conv.WorkspaceID, "배송-"+id.New()[:8])
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.AddConversationTag(ctx, conv.ID, tag.ID); err != nil {
		t.Fatal(err)
	}
	srv, st, _ := stallServer(shared)
	_, frames := dialJSON(t, ctx, srv, app.Actor{Kind: app.ActorAgent, AgentID: agent.ID, WorkspaceID: agent.WorkspaceID, Role: agent.Role})
	frames.send(t, ctx, map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	frames.await(t, "ready", 5*time.Second)

	st.set("ConversationTags", stallFail)
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET updated_at = clock_timestamp() WHERE id = $1", conv.ID)
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	first := frames.await(t, "conversation", 3*time.Second)
	if _, ok := first.conv()["tags"]; ok {
		t.Fatalf("태그를 못 읽었는데 tags 를 실었다: %v", first.conv()["tags"])
	}
	if first.conv()["state"] == nil {
		t.Fatalf("태그 말고는 다 실어야 한다: %v", first)
	}
	st.set("ConversationTags", stallOff)
	again := frames.await(t, "conversation", 3*time.Second) // 다시 깨움(0.5초)이 느린 길로 다시 보낸다
	tags, _ := again.conv()["tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["id"] != tag.ID {
		t.Fatalf("다음 기회의 대화 프레임에 태그가 없다: %v", again.conv()["tags"])
	}
	// 태그가 하나도 없는 대화는 빈 목록이다(모름과 다르다)
	if err := shared.RemoveConversationTag(ctx, conv.ID, tag.ID); err != nil {
		t.Fatal(err)
	}
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET updated_at = clock_timestamp() WHERE id = $1", conv.ID)
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	none := frames.await(t, "conversation", 3*time.Second)
	if raw, ok := none.conv()["tags"]; !ok || raw == nil || len(raw.([]any)) != 0 {
		t.Fatalf("태그가 없는 대화는 tags 가 빈 목록이어야 한다: %v (있음 %v)", raw, ok)
	}
}

// 감사 16(X3): 느린 길(Meta) 다시 깨움이 걸린 동안 빠른 길(번호만 보는 길)이 성공하면, 빠른 길은 메시지만 메웠을 뿐 상태·담당자·태그는
// 아직이다. 그 다시 깨움을 거두면 바뀐 상태가 다음 Meta 알림이나 연결 틱(60초)까지 오지 않는다. 리스너 없음, 연결 틱 1시간.
func TestRetry_느린길다시깨움이걸린동안빠른길이성공해도바뀐상태는다시깨움으로온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := retryServer(shared)
	srv.retryBase = 2 * time.Second // 다시 깨움이 걸려 있는 동안에 빠른 길이 확실히 끼게 넓힌다
	_, frames := dialJSON(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID})
	frames.send(t, ctx, map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	frames.await(t, "ready", 5*time.Second)

	st.failMeta.Store(true)
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET subject = '빠른 길 뒤 상태', updated_at = clock_timestamp() WHERE id = $1", conv.ID)
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	waitLog(t, logs, "ws.deliver_failed", 3*time.Second)

	// 빠른 길: 번호를 든 알림. 메시지 조회는 성공한다.
	res, err := shared.AppendMessage(ctx, core.AppendParams{ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
		SenderAgentID: agent.ID, Visibility: core.Public, Body: "빠른 길 답"})
	if err != nil {
		t.Fatal(err)
	}
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Seq: res.Seq, PublicSeq: res.PublicSeq})
	if f := frames.await(t, "message", 3*time.Second); f["body"] != "빠른 길 답" {
		t.Fatalf("빠른 길 메시지 %v", f)
	}
	st.failMeta.Store(false)
	f := frames.await(t, "conversation", 4*time.Second) // 새 알림 없이 다시 깨움만으로
	if f.conv()["subject"] != "빠른 길 뒤 상태" {
		t.Fatalf("다시 깨움의 대화 프레임 subject 가 %v 다", f.conv()["subject"])
	}
}

// 감사 16(X5): 느린 길 다시 깨움이 걸린 동안 빠른 길도 실패하면, 걸린 다시 깨움에 빠른 길의 알림을 합쳐야 한다. 덮어쓰면 Meta 가
// 사라져 복구 뒤 다시 깨움이 메시지만 보내고 바뀐 상태는 오지 않는다.
func TestRetry_느린길다시깨움이걸린동안빠른길도실패하면두알림을합쳐복구뒤메타프레임이온다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	custID, conv := testsupport.Conversation(t, shared)
	agent := testsupport.Agent(t, shared)
	srv, st, logs := retryServer(shared)
	_, frames := dialJSON(t, ctx, srv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID})
	frames.send(t, ctx, map[string]any{"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0})
	frames.await(t, "ready", 5*time.Second)

	st.failMeta.Store(true)
	st.failList.Store(true)
	testsupport.Exec(t, testsupport.URL(), "UPDATE conversations SET subject = '합친 다시 깨움', updated_at = clock_timestamp() WHERE id = $1", conv.ID)
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Meta: true})
	waitLog(t, logs, "ws.deliver_failed", 3*time.Second)
	res, err := shared.AppendMessage(ctx, core.AppendParams{ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent,
		SenderAgentID: agent.ID, Visibility: core.Public, Body: "빠른 길도 실패한 답"})
	if err != nil {
		t.Fatal(err)
	}
	listBefore := len(st.calls(time.Time{}))
	srv.OnChange(core.Change{ConversationID: conv.ID, WorkspaceID: conv.WorkspaceID, Seq: res.Seq, PublicSeq: res.PublicSeq})
	for deadline := time.Now().Add(2 * time.Second); len(st.calls(time.Time{})) == listBefore; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("빠른 길이 메시지를 조회하지 않았다")
		}
	}
	st.failMeta.Store(false)
	st.failList.Store(false)
	gotConv, gotMsg := false, false
	deadline := time.After(5 * time.Second)
	for !gotConv || !gotMsg {
		select {
		case f := <-frames.ch:
			switch f["type"] {
			case "conversation":
				if f.conv()["subject"] != "합친 다시 깨움" {
					t.Fatalf("대화 프레임 subject 가 %v 다", f.conv()["subject"])
				}
				gotConv = true
			case "message":
				gotMsg = gotMsg || f["body"] == "빠른 길도 실패한 답"
			}
		case <-deadline:
			t.Fatalf("복구 뒤 5초 안에 대화 프레임 %v · 메시지 %v — 알림 없이 다시 깨움만으로 둘 다 와야 한다", gotConv, gotMsg)
		}
	}
}

// 같은 순간 실패한 연결들이 같은 시각에 다시 묻으면 장애 조치 직후의 DB 에 연결 수만큼 한꺼번에 몰린다. 그래서 다시 묻는 간격을
// 아래로 20% 까지 흔든다(D4). 여기서는 연결 40개를 같은 순간 실패시켜 각 연결이 실제로 다시 물은 시각을 잰다 — 처음 걸 때(빠른 길 실패)와
// 빠른 길 성공이 느린 길 다시 깨움을 처음 간격으로 당길 때 둘 다. 처음 간격은 넓혀(2초) 흔들림 폭(0.4초)을 시계 잡음보다 크게 둔다.
func TestRetry_같은순간실패한연결들의다시묻기는한시각에몰리지않고처음간격의80에서100퍼센트안에흩어진다(t *testing.T) {
	shared := testsupport.Store(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const n, base = 40, 2 * time.Second
	type peer struct {
		conv  core.Conversation
		actor app.Actor
	}
	peers := make([]peer, n)
	for i := range peers {
		custID, conv := testsupport.Conversation(t, shared)
		peers[i] = peer{conv, app.Actor{Kind: app.ActorCustomer, CustomerID: custID, WorkspaceID: conv.WorkspaceID}}
	}
	connect := func(srv *Server) []*websocket.Conn {
		var conns []*websocket.Conn
		for _, p := range peers {
			ws, frames := dialJSON(t, ctx, srv, p.actor)
			frames.send(t, ctx, map[string]any{"type": "subscribe", "conversation_id": p.conv.ID, "after_seq": 0})
			frames.await(t, "ready", 5*time.Second)
			conns = append(conns, ws)
		}
		return conns
	}
	// spread 는 연결마다 잰 간격(from → 그 뒤 첫 다시 묻기)이 80~100% 안인지 보고, 가장 이른 것과 늦은 것의 차를 돌려준다.
	spread := func(phase string, gap func(convID string) (time.Duration, bool)) time.Duration {
		lo, hi := time.Hour, time.Duration(0)
		for _, p := range peers {
			g, ok := gap(p.conv.ID)
			if !ok {
				t.Fatalf("%s: 대화 %s 의 다시 묻기가 없다", phase, p.conv.ID)
			}
			// 위쪽 여유는 측정 잡음이다: 실제로 다시 물은 시각을 재므로 타이머가 깬 뒤 고루틴이 돌기까지의 지연이 더해진다.
			// 코어가 적은 CI 러너에서 -race 로 돌면 150ms 를 넘은 적이 있다(2.159s, DEF-185). 흩어짐을 보는 아래쪽 한계는 그대로 엄격하다.
			if g < base*8/10-30*time.Millisecond || g > base+500*time.Millisecond {
				t.Errorf("%s: 다시 묻기까지 %v 다. 처음 간격 %v 의 80~100%% 여야 한다", phase, g, base)
			}
			lo, hi = min(lo, g), max(hi, g)
		}
		t.Logf("%s: 연결 %d개의 다시 묻기 간격 %v ~ %v", phase, n, lo, hi)
		return hi - lo
	}
	notify := func(srv *Server, meta bool) {
		for _, p := range peers {
			if meta {
				srv.OnChange(core.Change{ConversationID: p.conv.ID, WorkspaceID: p.conv.WorkspaceID, Meta: true})
			} else {
				srv.OnChange(core.Change{ConversationID: p.conv.ID, WorkspaceID: p.conv.WorkspaceID, Seq: 1 << 40, PublicSeq: 1 << 40})
			}
		}
	}

	// 1. 빠른 길이 같은 순간 실패 → 각자 다시 깨움을 건다
	srv, st, _ := retryServer(shared)
	srv.retryBase = base
	conns := connect(srv)
	st.failList.Store(true)
	start := time.Now()
	notify(srv, false)
	time.Sleep(base + 400*time.Millisecond)
	if d := spread("처음 걸 때", func(convID string) (time.Duration, bool) {
		at := st.callsOf(convID, false, start)
		if len(at) < 2 {
			return 0, false
		}
		return at[1].Sub(at[0]), true
	}); d < 150*time.Millisecond {
		t.Errorf("처음 걸 때: 같은 순간 실패한 연결 %d개가 %v 안에 몰려 다시 물었다. 흩어져야 한다", n, d)
	}
	for _, ws := range conns {
		ws.CloseNow()
	}

	// 2. 느린 길이 같은 순간 실패해 다시 깨움(Meta)을 건 뒤, 빠른 길이 같은 순간 성공해 그 다시 깨움을 처음 간격으로 당긴다
	srv, st, _ = retryServer(shared)
	srv.retryBase = base
	connect(srv)
	st.failMeta.Store(true)
	start = time.Now()
	notify(srv, true)
	time.Sleep(200 * time.Millisecond)
	notify(srv, false)
	time.Sleep(base + 400*time.Millisecond)
	if d := spread("빠른 길 성공으로 당길 때", func(convID string) (time.Duration, bool) {
		fast := st.callsOf(convID, false, start)
		if len(fast) == 0 {
			return 0, false
		}
		meta := st.callsOf(convID, true, fast[0])
		if len(meta) == 0 {
			return 0, false
		}
		return meta[0].Sub(fast[0]), true
	}); d < 150*time.Millisecond {
		t.Errorf("빠른 길 성공으로 당길 때: 연결 %d개가 %v 안에 몰려 다시 물었다. 흩어져야 한다", n, d)
	}
}

// ── 도구 ──────────────────────────────────────────────────

// jsonFrames 는 날 JSON 프레임을 받는다. 필드가 있는지 없는지를 본다(testFrame 은 없는 필드를 빈 값으로 읽는다).
type jsonFrames struct {
	ws *websocket.Conn
	ch chan jsonFrame
}

type jsonFrame map[string]any

func (f jsonFrame) conv() map[string]any { c, _ := f["conversation"].(map[string]any); return c }

func dialJSON(t *testing.T, ctx context.Context, srv *Server, actor app.Actor) (*websocket.Conn, *jsonFrames) {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { srv.Handle(w, r, actor, nil) }))
	t.Cleanup(hs.Close)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	fr := &jsonFrames{ws: ws, ch: make(chan jsonFrame, 256)}
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var f jsonFrame
			_ = json.Unmarshal(b, &f)
			fr.ch <- f
		}
	}()
	return ws, fr
}

func (fr *jsonFrames) send(t *testing.T, ctx context.Context, f map[string]any) {
	t.Helper()
	writeFrame(t, ctx, fr.ws, f)
}

// await 는 typ 프레임을 기다린다. 다른 종류는 건너뛴다.
func (fr *jsonFrames) await(t *testing.T, typ string, within time.Duration) jsonFrame {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f := <-fr.ch:
			if f["type"] == typ {
				return f
			}
		case <-deadline:
			t.Fatalf("%v 안에 %s 프레임이 오지 않는다", within, typ)
		}
	}
}
