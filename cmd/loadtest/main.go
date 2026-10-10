// 부하 테스트. 고객 N명과 상담원 M명을 실제 WebSocket 으로 붙여 놓고 메시지를 흘린다.
//
// 재는 것은 셋이다.
//  1. 유실 — 보낸 메시지가 전부 저장되고(ack), 전부 되돌아오는가(echo)
//  2. 번호 연속성 — 고객이 받은 번호에 빈칸이 없는가
//  3. 지연 — ack 까지, 그리고 저장부터 되돌아오기까지 얼마나 걸리는가
//
// 통과 기준은 "지연이 얼마 이하" 가 아니라 "하나도 안 잃었는가" 다. 지연은 숫자로 남긴다.
//
// -reconnect 는 배포·장애 훈련용이다. 끊기면 브라우저(chat.js)처럼 다시 붙는다: 1001 이면 지터(250ms)만 두고,
// 아니면 0.5초부터 두 배씩 최대 8초 백오프. 붙으면 after_seq 로 이어 받고, ack 못 받은 메시지를 같은 client_msg_id 로
// 다시 보낸다(ready 를 받을 때, 그리고 6초 넘게 ack 가 없을 때 3초마다). 끝에 REST 로 대화를 다시 읽어 ack 받은 메시지가
// 전부 DB 에 있는지, 번호에 빈칸이 없는지 대조한다. 판정은 "최종 유실 0 · DB 빈칸 0 · ack 받은 것 전부 DB 에 있음" 이다.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	base       = flag.String("base", "http://localhost:8090", "서버 주소")
	customers  = flag.Int("customers", 3000, "동시 고객 수")
	agents     = flag.Int("agents", 3, "상담원 수")
	duration   = flag.Duration("duration", 60*time.Second, "메시지를 흘리는 시간")
	perMin     = flag.Float64("rate", 2, "고객 한 명이 1분에 보내는 메시지 수")
	setupPar   = flag.Int("setup-parallel", 64, "준비 단계 동시 요청 수")
	agentPass  = flag.String("agent-password", "chat1234", "상담원 비밀번호")
	memoEvery  = flag.Int("memo-every", 5, "상담원이 회신 N건마다 내부 메모를 남긴다 (0이면 안 남김)")
	deskDetail = flag.Bool("desk", true, "상담원이 받은함 신호마다 실제 데스크처럼 목록·건수를 다시 조회한다")
	agentMail  = flag.String("agent-email", "agent@example.com", "상담원 이메일(첫 번째)")
	reconnect  = flag.Bool("reconnect", false, "끊기면 다시 붙어 이어 받고 ack 못 받은 메시지를 다시 보낸다. 끝에 REST 로 DB 를 대조한다 (배포·장애 훈련용)")
	settle     = flag.Duration("settle", 15*time.Second, "보내기를 멈춘 뒤 남은 전달을 기다리는 시간. -reconnect 면 ack 못 받은 것이 남아 있는 동안 최대 4배까지 늘린다")
)

// 재접속 모드의 숫자. chat.js 와 같은 값이다.
const (
	reconnectBackoffMin = 500 * time.Millisecond
	reconnectBackoffMax = 8 * time.Second
	goingAwayJitter     = 250 * time.Millisecond
	resendTick          = 3 * time.Second
	resendAfter         = 6 * time.Second
)

var (
	reconnects atomic.Int64 // 끊긴 뒤 다시 붙은 횟수
	dialFails  atomic.Int64 // 다시 붙으려다 실패한 횟수
	resends    atomic.Int64 // 같은 client_msg_id 로 다시 보낸 프레임 수
	goingAways atomic.Int64 // 서버가 1001 로 닫은 횟수
)

type frame struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversation_id"`
	Seq            int64  `json:"seq"`
	ClientMsgID    string `json:"client_msg_id"`
	SenderKind     string `json:"sender_kind"`
	Body           string `json:"body"`
	LastSeq        int64  `json:"last_seq"`
	Code           string `json:"code"`
	Message        string `json:"message"`
}

type stats struct {
	mu      sync.Mutex
	ack     []time.Duration
	echo    []time.Duration
	connect []time.Duration
	desk    []time.Duration
	// ticket 은 WebSocket 티켓 발급(POST /api/ws-ticket) 시간이다(D46). 붙을 때마다 하나 받는다.
	ticket   []time.Duration
	errCount map[string]int64
}

func (s *stats) addAck(d time.Duration)  { s.mu.Lock(); s.ack = append(s.ack, d); s.mu.Unlock() }
func (s *stats) addEcho(d time.Duration) { s.mu.Lock(); s.echo = append(s.echo, d); s.mu.Unlock() }
func (s *stats) addDesk(d time.Duration) { s.mu.Lock(); s.desk = append(s.desk, d); s.mu.Unlock() }
func (s *stats) addConnect(d time.Duration) {
	s.mu.Lock()
	s.connect = append(s.connect, d)
	s.mu.Unlock()
}
func (s *stats) addTicket(d time.Duration) {
	s.mu.Lock()
	s.ticket = append(s.ticket, d)
	s.mu.Unlock()
}
func (s *stats) addErr(kind string) {
	s.mu.Lock()
	if s.errCount == nil {
		s.errCount = map[string]int64{}
	}
	s.errCount[kind]++
	s.mu.Unlock()
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func summary(name string, d []time.Duration) string {
	if len(d) == 0 {
		return fmt.Sprintf("%-12s 표본 없음", name)
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, v := range sorted {
		total += v
	}
	return fmt.Sprintf("%-12s n=%-7d 평균 %-9s p50 %-9s p95 %-9s p99 %-9s 최대 %s",
		name, len(sorted), (total / time.Duration(len(sorted))).Round(time.Millisecond),
		pct(sorted, 0.50).Round(time.Millisecond), pct(sorted, 0.95).Round(time.Millisecond),
		pct(sorted, 0.99).Round(time.Millisecond), sorted[len(sorted)-1].Round(time.Millisecond))
}

var (
	st             = &stats{}
	inboxRefreshes atomic.Int64
	memos          atomic.Int64
)

var client = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     512,
	},
}

func postJSON(path, token string, body, out any) error {
	var r *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	} else {
		r = strings.NewReader("")
	}
	req, err := http.NewRequest(http.MethodPost, *base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("POST %s → %d", path, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// dialWS 는 브라우저(chat.js)와 같은 길로 붙는다: 세션 토큰으로 30초 일회용 티켓을 받고 /ws?ticket= 으로 붙는다(D46).
// 티켓을 못 받으면 "ticket" 오류로 세고 붙지 않는다 — 부르는 쪽의 백오프가 다시 시도한다.
func dialWS(ctx context.Context, token string) (*websocket.Conn, error) {
	var out struct {
		Ticket string `json:"ticket"`
	}
	t0 := time.Now()
	if err := postJSON("/api/ws-ticket", token, nil, &out); err != nil {
		st.addErr("ticket")
		return nil, err
	}
	st.addTicket(time.Since(t0))
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(*base, "http")+"/ws?ticket="+out.Ticket, nil)
	return ws, err
}

func getJSON(path, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, *base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("GET %s → %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// customerClient 는 고객 한 명이다.
type customerClient struct {
	idx    int
	token  string
	convID string
	ws     *websocket.Conn

	mu      sync.Mutex
	sentAt  map[string]time.Time
	ackedAt map[string]time.Time
	seqs    []int64
	// 재접속 모드: 받은 가장 큰 번호(after_seq), 마지막으로 보낸 시각(재전송 판단), 되돌아온 것(중복 집계 방지)
	lastSeq  int64
	lastSend map[string]time.Time
	echoSeen map[string]bool

	sent    atomic.Int64
	acked   atomic.Int64
	echoed  atomic.Int64
	dropped atomic.Int64
}

func (c *customerClient) record(f frame, now time.Time) {
	switch f.Type {
	case "ack":
		c.mu.Lock()
		// 재전송의 ack 가 또 올 수 있다. 처음 것만 센다.
		if t, ok := c.sentAt[f.ClientMsgID]; ok && c.ackedAt[f.ClientMsgID].IsZero() {
			st.addAck(now.Sub(t))
			c.ackedAt[f.ClientMsgID] = now
			c.acked.Add(1)
		}
		c.mu.Unlock()
	case "message":
		c.mu.Lock()
		c.seqs = append(c.seqs, f.Seq)
		c.lastSeq = max(c.lastSeq, f.Seq)
		if t, ok := c.sentAt[f.ClientMsgID]; ok && f.SenderKind == "CUSTOMER" && !c.echoSeen[f.ClientMsgID] {
			c.echoSeen[f.ClientMsgID] = true
			st.addEcho(now.Sub(t))
			c.echoed.Add(1)
		}
		c.mu.Unlock()
	case "ready":
		// 구독이 붙었다. ack 못 받은 것을 같은 client_msg_id 로 다시 보낸다(chat.js 의 flush).
		if *reconnect {
			c.resend(0)
		}
	case "error":
		st.addErr("ws:" + f.Code)
		c.dropped.Add(1)
	}
}

func (c *customerClient) conn() *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws
}

// pending 은 보냈지만 ack 를 못 받은 client_msg_id 다.
func (c *customerClient) pending(olderThan time.Duration, now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for id := range c.sentAt {
		if !c.ackedAt[id].IsZero() {
			continue
		}
		if olderThan > 0 && now.Sub(c.lastSend[id]) < olderThan {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// resend 는 ack 못 받은 메시지를 같은 client_msg_id 로 다시 보낸다. 서버는 멱등이라 같은 메시지로 처리한다(I3).
func (c *customerClient) resend(olderThan time.Duration) {
	ws := c.conn()
	if ws == nil {
		return
	}
	now := time.Now()
	for _, id := range c.pending(olderThan, now) {
		if err := writeFrame(context.Background(), ws, c.sendFrame(id)); err != nil {
			return // 끊겼다. 다시 붙으면 ready 에서 또 보낸다
		}
		c.mu.Lock()
		c.lastSend[id] = now
		c.mu.Unlock()
		resends.Add(1)
	}
}

func (c *customerClient) sendFrame(cmid string) map[string]any {
	return map[string]any{
		"type": "send", "conversation_id": c.convID,
		"client_msg_id": cmid, "body": "부하 메시지 " + cmid,
	}
}

func main() {
	flag.Parse()
	rand.New(rand.NewSource(time.Now().UnixNano()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("부하 테스트 — 고객 %d명 · 상담원 %d명 · %s 동안 · 고객당 분당 %.1f건 (데스크 동작 %v)\n\n",
		*customers, *agents, *duration, *perMin, *deskDetail)

	// 1) 준비: 고객 세션과 대화를 만든다.
	start := time.Now()
	clients := provision(ctx)
	fmt.Printf("[준비] 고객 %d명 세션·대화 생성 %s\n", len(clients), time.Since(start).Round(time.Millisecond))
	if len(clients) == 0 {
		fmt.Println("고객을 하나도 못 만들었다. 서버가 떠 있는지 확인해라.")
		cancel()
		os.Exit(1) //nolint:gocritic // exitAfterDefer: os.Exit 가 건너뛰는 defer 는 cancel 하나고, 바로 위에서 불렀다.
	}

	// 2) WebSocket 접속
	start = time.Now()
	connected := connectAll(ctx, clients)
	fmt.Printf("[접속] WebSocket %d개 연결 %s · %s\n",
		connected, time.Since(start).Round(time.Millisecond), summary("연결", st.connect))

	// 3) 상담원 투입
	agentTokens := loginAgents(ctx)
	fmt.Printf("[상담원] %d명 로그인\n", len(agentTokens))
	var agentWG sync.WaitGroup
	agentCtx, stopAgents := context.WithCancel(ctx)
	replies := &atomic.Int64{}
	for i, tok := range agentTokens {
		agentWG.Add(1)
		go func(i int, tok string) {
			defer agentWG.Done()
			runAgent(agentCtx, i, tok, replies)
		}(i, tok)
	}

	if *reconnect {
		go resendLoop(ctx, clients)
	}

	// 4) 부하
	fmt.Printf("\n[부하] %s 동안 메시지를 흘린다… (재접속 모드 %v)\n", *duration, *reconnect)
	drive(ctx, clients)

	// 5) 가라앉히기. 재접속 모드면 ack 못 받은 것이 남아 있는 동안 더 기다린다.
	fmt.Printf("[정리] 남은 전달을 기다린다 (%s)…\n", *settle)
	waitSettle(clients)
	stopAgents()
	agentWG.Wait()

	ok := report(clients, replies.Load())
	if *reconnect {
		cancel() // 재접속을 멈추고 DB 를 대조한다
		ok = verifyDB(clients) && ok
	}
	if !ok {
		os.Exit(1)
	}
}

func waitSettle(clients []*customerClient) {
	deadline := time.Now().Add(*settle)
	limit := deadline
	if *reconnect {
		limit = time.Now().Add(4 * *settle)
	}
	for {
		now := time.Now()
		if now.After(limit) {
			return
		}
		if now.After(deadline) {
			left := 0
			for _, c := range clients {
				left += len(c.pending(0, now))
			}
			if left == 0 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func provision(ctx context.Context) []*customerClient {
	out := make([]*customerClient, *customers)
	sem := make(chan struct{}, *setupPar)
	var wg sync.WaitGroup
	for i := 0; i < *customers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var session struct {
				Token string `json:"token"`
			}
			if err := postJSON("/api/customer/session", "",
				map[string]any{"display_name": fmt.Sprintf("부하고객%04d", i)}, &session); err != nil {
				st.addErr("session")
				return
			}
			var started struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			}
			if err := postJSON("/api/customer/conversations", session.Token, nil, &started); err != nil {
				st.addErr("conversation")
				return
			}
			out[i] = &customerClient{
				idx: i, token: session.Token, convID: started.Conversation.ID,
				sentAt: map[string]time.Time{}, ackedAt: map[string]time.Time{},
				lastSend: map[string]time.Time{}, echoSeen: map[string]bool{},
			}
		}(i)
	}
	wg.Wait()

	live := out[:0]
	for _, c := range out {
		if c != nil {
			live = append(live, c)
		}
	}
	return live
}

func connectAll(ctx context.Context, clients []*customerClient) int {
	sem := make(chan struct{}, 256)
	var wg sync.WaitGroup
	var ok atomic.Int64
	for _, c := range clients {
		wg.Add(1)
		go func(c *customerClient) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t0 := time.Now()
			ws, err := dialWS(ctx, c.token)
			if err != nil {
				st.addErr("dial")
				return
			}
			ws.SetReadLimit(1 << 20)
			st.addConnect(time.Since(t0))
			c.mu.Lock()
			c.ws = ws
			c.mu.Unlock()
			ok.Add(1)

			if *reconnect {
				go keepConnected(ctx, c, ws)
			} else {
				go readLoop(ctx, c)
			}
			_ = writeFrame(ctx, ws, map[string]any{
				"type": "subscribe", "conversation_id": c.convID, "after_seq": 0,
			})
		}(c)
	}
	wg.Wait()
	return int(ok.Load())
}

func readLoop(ctx context.Context, c *customerClient) {
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				st.addErr("read")
			}
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		c.record(f, time.Now())
	}
}

// keepConnected 는 재접속 모드의 고객 연결이다. 읽다가 끊기면 chat.js 처럼 다시 붙고 after_seq 로 이어 받는다.
// 다시 붙은 뒤의 재전송은 ready 프레임에서 한다(record).
func keepConnected(ctx context.Context, c *customerClient, ws *websocket.Conn) {
	backoff := reconnectBackoffMin
	for {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				wait := backoff + time.Duration(rand.Int63n(int64(goingAwayJitter)))
				if websocket.CloseStatus(err) == websocket.StatusGoingAway {
					// 서버가 정상으로 내려간다. 백오프를 올리지 않고 지터만 두고 다시 붙는다(D32).
					goingAways.Add(1)
					wait = time.Duration(rand.Int63n(int64(goingAwayJitter)))
				} else {
					st.addErr("read")
					backoff = min(backoff*2, reconnectBackoffMax)
				}
				c.mu.Lock()
				c.ws = nil
				c.mu.Unlock()
				ws.CloseNow()
				if !sleepCtx(ctx, wait) {
					return
				}
				break
			}
			var f frame
			if json.Unmarshal(data, &f) == nil {
				c.record(f, time.Now())
			}
		}
		// 다시 붙는다. 실패하면 백오프를 올려 다시 시도한다.
		for {
			next, err := dialWS(ctx, c.token)
			if err == nil {
				ws = next
				break
			}
			if ctx.Err() != nil {
				return
			}
			dialFails.Add(1)
			wait := backoff + time.Duration(rand.Int63n(int64(goingAwayJitter)))
			backoff = min(backoff*2, reconnectBackoffMax)
			if !sleepCtx(ctx, wait) {
				return
			}
		}
		ws.SetReadLimit(1 << 20)
		reconnects.Add(1)
		backoff = reconnectBackoffMin
		c.mu.Lock()
		c.ws = ws
		after := c.lastSeq
		c.mu.Unlock()
		_ = writeFrame(ctx, ws, map[string]any{
			"type": "subscribe", "conversation_id": c.convID, "after_seq": after,
		})
	}
}

// resendLoop 는 소켓은 열려 있는데 오래 ack 가 없는 메시지를 다시 보낸다. 서버가 잠시 거절한(DB 장애 조치 등) 메시지는
// 다음 재접속까지 기다리면 안 된다. chat.js 는 이 몫을 REST 로 하고, 여기서는 같은 WebSocket 으로 한다.
func resendLoop(ctx context.Context, clients []*customerClient) {
	t := time.NewTicker(resendTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, c := range clients {
			c.resend(resendAfter)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func writeFrame(ctx context.Context, ws *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func drive(parent context.Context, clients []*customerClient) {
	deadline := time.Now().Add(*duration)
	// 측정 시간이 끝나면 전원이 같이 멈춘다. 시작 지연이 측정 시간보다 길어도 매달리지 않는다.
	ctx, stop := context.WithDeadline(parent, deadline)
	defer stop()

	interval := time.Duration(float64(time.Minute) / *perMin)
	jitterMax := interval
	if jitterMax > *duration {
		jitterMax = *duration
	}
	var wg sync.WaitGroup
	for _, c := range clients {
		if c.conn() == nil {
			continue
		}
		wg.Add(1)
		go func(c *customerClient) {
			defer wg.Done()
			// 시작 시각을 흩어서 한 번에 몰리지 않게 한다.
			jitter := time.Duration(rand.Int63n(int64(jitterMax) + 1))
			select {
			case <-time.After(jitter):
			case <-ctx.Done():
				return
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			n := 0
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				n++
				cmid := fmt.Sprintf("load-%d-%d", c.idx, n)
				now := time.Now()
				c.mu.Lock()
				c.sentAt[cmid] = now
				c.lastSend[cmid] = now
				c.mu.Unlock()
				c.sent.Add(1)
				ws := c.conn()
				if ws == nil {
					// 재접속 중이다. 큐에 넣은 셈이고 다시 붙으면 나간다.
					continue
				}
				if err := writeFrame(ctx, ws, c.sendFrame(cmid)); err != nil {
					st.addErr("write")
					if *reconnect {
						continue
					}
					return
				}
			}
		}(c)
	}
	wg.Wait()
}

func loginAgents(ctx context.Context) []string {
	var tokens []string
	for i := 0; i < *agents; i++ {
		email := *agentMail
		if i > 0 {
			email = fmt.Sprintf("agent%d@example.com", i+1)
		}
		var out struct {
			Token string `json:"token"`
		}
		if err := postJSON("/api/agent/login", "",
			map[string]any{"email": email, "password": *agentPass}, &out); err != nil {
			st.addErr("agent-login")
			continue
		}
		tokens = append(tokens, out.Token)
	}
	return tokens
}

// runAgent 는 상담원 한 명이다. 받은함을 보고 가장 최근 대화를 열어 답하고 다음으로 넘어간다.
// 재접속 모드면 연결이 끊길 때마다 다시 붙는다. 상담원 회신은 판정에 들지 않는다 — 부하의 모양을 지키려고 둔다.
func runAgent(ctx context.Context, idx int, token string, replies *atomic.Int64) {
	for {
		connCtx, drop := context.WithCancel(ctx)
		agentSession(connCtx, drop, idx, token, replies)
		drop()
		if !*reconnect || ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, reconnectBackoffMin) {
			return
		}
	}
}

func agentSession(ctx context.Context, drop context.CancelFunc, idx int, token string, replies *atomic.Int64) {
	ws, err := dialWS(ctx, token)
	if err != nil {
		st.addErr("agent-dial")
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	// 실제 데스크는 받은함 신호를 받을 때마다 목록과 건수를 다시 조회한다.
	// 그 동작을 빼고 재면 제품이 실제로 쓰는 경로가 측정에서 빠진다.
	go func() {
		defer drop() // 읽기가 끝났으면 연결이 끊긴 것이다. 재접속 모드면 runAgent 가 다시 붙는다
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if !*deskDetail {
				continue
			}
			var f frame
			if json.Unmarshal(data, &f) != nil || f.Type != "inbox_changed" {
				continue
			}
			t0 := time.Now()
			var ignore any
			if err := getJSON("/api/agent/conversations?state=OPEN&limit=50", token, &ignore); err != nil {
				st.addErr("desk-inbox")
			}
			if err := getJSON("/api/agent/inbox/counts", token, &ignore); err != nil {
				st.addErr("desk-counts")
			}
			st.addDesk(time.Since(t0))
			inboxRefreshes.Add(1)
		}
	}()

	seen := agentSeen(idx)
	n := len(seen)
	for ctx.Err() == nil {
		var inbox struct {
			Conversations []struct {
				ID     string `json:"id"`
				Unread int64  `json:"unread"`
			} `json:"conversations"`
		}
		if err := getJSON("/api/agent/conversations?state=OPEN&limit=50", token, &inbox); err != nil {
			st.addErr("agent-inbox")
			time.Sleep(time.Second)
			continue
		}
		worked := false
		for _, conv := range inbox.Conversations {
			if ctx.Err() != nil {
				return
			}
			if conv.Unread == 0 || seen[conv.ID] {
				continue
			}
			seen[conv.ID] = true
			worked = true
			n++
			// 대화를 연다 → 읽음 보고 → 회신. 상담원이 실제로 하는 순서다.
			_ = writeFrame(ctx, ws, map[string]any{
				"type": "subscribe", "conversation_id": conv.ID, "after_seq": 0,
			})
			_ = writeFrame(ctx, ws, map[string]any{
				"type": "send", "conversation_id": conv.ID,
				"client_msg_id": fmt.Sprintf("agent-%d-%d", idx, n),
				"body":          "확인했습니다. 바로 처리해 드릴게요.",
			})
			replies.Add(1)
			// 내부 메모를 섞어야 두 번호 체계와 가시성 필터가 부하 경로를 지난다.
			if *memoEvery > 0 && n%*memoEvery == 0 {
				_ = writeFrame(ctx, ws, map[string]any{
					"type": "send", "conversation_id": conv.ID, "internal": true,
					"client_msg_id": fmt.Sprintf("memo-%d-%d", idx, n),
					"body":          "내부 메모: 재고 확인 필요",
				})
				memos.Add(1)
			}
			time.Sleep(300 * time.Millisecond) // 사람이 타이핑하는 속도
		}
		if !worked {
			time.Sleep(time.Second)
		}
	}
}

var (
	agentSeenMu sync.Mutex
	agentSeenBy = map[int]map[string]bool{}
)

// agentSeen 은 상담원이 이미 답한 대화다. 다시 붙어도 같은 대화에 또 답하지 않게 연결 밖에 둔다.
func agentSeen(idx int) map[string]bool {
	agentSeenMu.Lock()
	defer agentSeenMu.Unlock()
	if agentSeenBy[idx] == nil {
		agentSeenBy[idx] = map[string]bool{}
	}
	return agentSeenBy[idx]
}

func report(clients []*customerClient, replies int64) bool {
	var sent, acked, echoed, gaps, missingAck int64
	worstGap := ""
	for _, c := range clients {
		sent += c.sent.Load()
		acked += c.acked.Load()
		echoed += c.echoed.Load()

		c.mu.Lock()
		seqs := append([]int64(nil), c.seqs...)
		for id := range c.sentAt {
			if c.ackedAt[id].IsZero() {
				missingAck++
			}
		}
		c.mu.Unlock()

		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		uniq := seqs[:0]
		for i, s := range seqs {
			if i == 0 || s != seqs[i-1] {
				uniq = append(uniq, s)
			}
		}
		for i, s := range uniq {
			if s != int64(i+1) {
				gaps++
				if worstGap == "" {
					worstGap = fmt.Sprintf("고객 %d: %v", c.idx, uniq[:min(len(uniq), 12)])
				}
				break
			}
		}
	}

	fmt.Println("\n──────── 결과 ────────")
	fmt.Printf("보낸 메시지      %d\n", sent)
	fmt.Printf("ack 받은 메시지  %d  (못 받은 것 %d)\n", acked, missingAck)
	fmt.Printf("되돌아온 메시지  %d\n", echoed)
	fmt.Printf("상담원 회신      %d  (내부 메모 %d)\n", replies, memos.Load())
	fmt.Printf("받은함 재조회    %d\n", inboxRefreshes.Load())
	fmt.Printf("번호에 빈칸 생긴 고객 %d명 %s\n", gaps, worstGap)
	if *reconnect {
		fmt.Printf("재접속            %d  (1001 로 닫힘 %d, 다시 붙기 실패 %d)\n", reconnects.Load(), goingAways.Load(), dialFails.Load())
		fmt.Printf("재전송 프레임     %d\n", resends.Load())
		fmt.Printf("최종 유실         %d  (끝까지 ack 를 못 받은 메시지)\n", missingAck)
	}
	fmt.Println()
	fmt.Println(summary("연결", st.connect))
	fmt.Println(summary("티켓 발급", st.ticket))
	fmt.Println(summary("ack 지연", st.ack))
	fmt.Println(summary("전달 왕복", st.echo))
	fmt.Println(summary("받은함 조회", st.desk))

	st.mu.Lock()
	if len(st.errCount) == 0 {
		fmt.Println("\n오류 없음")
	} else {
		fmt.Println("\n오류:")
		keys := make([]string, 0, len(st.errCount))
		for k := range st.errCount {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-18s %d\n", k, st.errCount[k])
		}
	}
	st.mu.Unlock()

	verdict := "유실 없음"
	ok := !(missingAck > 0 || gaps > 0 || echoed < acked)
	if !ok {
		verdict = "유실 또는 빈칸 발생"
	}
	fmt.Printf("\n판정: %s\n", verdict)
	return ok
}

// verifyDB 는 REST 로 고객마다 대화를 처음부터 다시 읽어 대조한다. 도구가 받은 프레임이 아니라 서버가 가진 것을 본다.
//   - ack 받은 메시지는 전부 DB 에 있다 (없으면 유실이다. ack 는 커밋 뒤에만 나간다, I4)
//   - 번호가 1 부터 빈칸 없이 이어진다 (I1·I9 — 고객이 보는 번호)
//   - DB 의 이번 실행 메시지 수 ≥ ack 수 (ack 를 못 받았어도 커밋됐을 수 있다)
func verifyDB(clients []*customerClient) bool {
	var acked, inDB, ackedMissing, gapConvs, readFails, committedNoAck int64
	sem := make(chan struct{}, *setupPar)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *customerClient) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			msgs, err := readAll(c)
			c.mu.Lock()
			defer c.mu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				readFails++
				return
			}
			have := map[string]bool{}
			prefix := fmt.Sprintf("load-%d-", c.idx)
			for i, m := range msgs {
				if m.Seq != int64(i+1) {
					gapConvs++
					break
				}
			}
			for _, m := range msgs {
				if strings.HasPrefix(m.ClientMsgID, prefix) {
					have[m.ClientMsgID] = true
				}
			}
			inDB += int64(len(have))
			for id := range c.sentAt {
				switch {
				case !c.ackedAt[id].IsZero():
					acked++
					if !have[id] {
						ackedMissing++
					}
				case have[id]:
					committedNoAck++
				}
			}
		}(c)
	}
	wg.Wait()

	fmt.Println("\n──────── DB 대조 (REST) ────────")
	fmt.Printf("ack 받은 메시지        %d\n", acked)
	fmt.Printf("DB 에 있는 이번 메시지 %d  (ack 못 받았지만 커밋된 것 %d)\n", inDB, committedNoAck)
	fmt.Printf("ack 받았는데 DB 에 없음 %d\n", ackedMissing)
	fmt.Printf("번호에 빈칸 있는 대화  %d\n", gapConvs)
	if readFails > 0 {
		fmt.Printf("읽지 못한 대화         %d\n", readFails)
	}
	ok := ackedMissing == 0 && gapConvs == 0 && inDB >= acked && readFails == 0
	verdict := "통과 (ack 받은 것 전부 DB 에 있음 · 빈칸 0 · DB 수 ≥ ack 수)"
	if !ok {
		verdict = "실패"
	}
	fmt.Printf("\nDB 판정: %s\n", verdict)
	return ok
}

type storedMessage struct {
	Seq         int64  `json:"seq"`
	ClientMsgID string `json:"client_msg_id"`
}

// readAll 은 고객 토큰으로 대화 전체를 after_seq 페이지로 읽는다.
func readAll(c *customerClient) ([]storedMessage, error) {
	var all []storedMessage
	after := int64(0)
	for range 10000 {
		var page struct {
			Messages []storedMessage `json:"messages"`
		}
		path := fmt.Sprintf("/api/conversations/%s/messages?after_seq=%d&limit=500", c.convID, after)
		var err error
		for try := range 5 { // 막 재기동한 서버라 잠깐 실패할 수 있다
			if err = getJSON(path, c.token, &page); err == nil {
				break
			}
			time.Sleep(time.Duration(try+1) * 500 * time.Millisecond)
		}
		if err != nil {
			return nil, err
		}
		if len(page.Messages) == 0 {
			return all, nil
		}
		all = append(all, page.Messages...)
		after = page.Messages[len(page.Messages)-1].Seq
	}
	return nil, errors.New("페이지가 끝나지 않는다")
}
