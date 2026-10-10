package pgstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/pgstore"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

func customerMessage(convID, customerID, clientMsgID, body string) core.AppendParams {
	return core.AppendParams{
		ConversationID: convID,
		ClientMsgID:    clientMsgID,
		SenderKind:     core.SenderCustomer,
		SenderCustomer: customerID,
		Body:           body,
	}
}

func TestAppendMessage_멱등(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)

	t.Run("같은 client_msg_id 로 두 번 보내면 한 행만 남고 같은 seq 를 돌려준다", func(t *testing.T) {
		cmid := id.New()
		first, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, cmid, "안녕하세요"))
		if err != nil {
			t.Fatalf("첫 전송 실패: %v", err)
		}
		if !first.Created {
			t.Fatal("첫 전송은 Created 여야 한다")
		}

		second, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, cmid, "다른 본문이어도 무시된다"))
		if err != nil {
			t.Fatalf("재전송 실패: %v", err)
		}
		if second.Created {
			t.Error("재전송은 Created 가 아니어야 한다")
		}
		if second.Message.Seq != first.Message.Seq {
			t.Errorf("seq 가 달라졌다: 처음 %d, 재전송 %d", first.Message.Seq, second.Message.Seq)
		}
		if second.Message.ID != first.Message.ID {
			t.Errorf("메시지 id 가 달라졌다: %s vs %s", first.Message.ID, second.Message.ID)
		}
		if second.Message.Body != "안녕하세요" {
			t.Errorf("재전송이 본문을 덮어썼다: %q", second.Message.Body)
		}

		seqs, err := store.MessageSeqs(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(seqs) != 1 {
			t.Errorf("행이 하나여야 한다: %v", seqs)
		}
	})
}

func TestAppendMessage_동시전송(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()

	t.Run("한 대화에 동시에 N건을 보내면 seq 가 1..N 으로 빈칸 없이 채워진다", func(t *testing.T) {
		customerID, conv := testsupport.Conversation(t, store)
		const writers, each = 20, 10
		const total = writers * each

		var wg sync.WaitGroup
		errs := make(chan error, total)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					body := fmt.Sprintf("w%d-%d", w, i)
					if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), body)); err != nil {
						errs <- err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("동시 전송 실패: %v", err)
		}

		seqs, err := store.MessageSeqs(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(seqs) != total {
			t.Fatalf("행 수가 %d 가 아니다: %d", total, len(seqs))
		}
		for i, seq := range seqs {
			if seq != int64(i+1) {
				t.Fatalf("seq 가 1..%d 연속이 아니다: 인덱스 %d 에서 %d", total, i, seq)
			}
		}

		reloaded, err := store.Conversation(ctx, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.LastSeq != int64(total) {
			t.Errorf("conversations.last_seq 가 %d 여야 한다: %d", total, reloaded.LastSeq)
		}
	})

	t.Run("중복 전송이 롤백돼도 last_seq 에 빈칸이 생기지 않는다", func(t *testing.T) {
		customerID, conv := testsupport.Conversation(t, store)
		cmid := id.New()

		var wg sync.WaitGroup
		results := make(chan int64, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, cmid, "같은 메시지"))
				if err != nil {
					t.Errorf("전송 실패: %v", err)
					return
				}
				results <- res.Message.Seq
			}()
		}
		wg.Wait()
		close(results)
		for seq := range results {
			if seq != 1 {
				t.Errorf("모두 seq 1 을 받아야 한다: %d", seq)
			}
		}

		// 빈칸이 남았다면 다음 메시지가 2 가 아니라 더 큰 숫자를 받는다.
		next, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "다음 메시지"))
		if err != nil {
			t.Fatal(err)
		}
		if next.Message.Seq != 2 {
			t.Errorf("다음 seq 는 2 여야 한다 (빈칸 없음): %d", next.Message.Seq)
		}
	})

	t.Run("서로 다른 대화는 seq 를 공유하지 않는다", func(t *testing.T) {
		c1, conv1 := testsupport.Conversation(t, store)
		c2, conv2 := testsupport.Conversation(t, store)
		for i := 0; i < 3; i++ {
			if _, err := store.AppendMessage(ctx, customerMessage(conv1.ID, c1, id.New(), "a")); err != nil {
				t.Fatal(err)
			}
		}
		res, err := store.AppendMessage(ctx, customerMessage(conv2.ID, c2, id.New(), "b"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Seq != 1 {
			t.Errorf("다른 대화의 첫 메시지는 seq 1 이어야 한다: %d", res.Message.Seq)
		}
	})
}

func TestListMessages_캐치업(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	for i := 0; i < 5; i++ {
		if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("after_seq 다음 메시지만 오름차순으로 돌려준다", func(t *testing.T) {
		msgs, err := store.ListMessages(ctx, conv.ID, 2, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{3, 4, 5}
		if len(msgs) != len(want) {
			t.Fatalf("3건이어야 한다: %d건", len(msgs))
		}
		for i, m := range msgs {
			if m.Seq != want[i] {
				t.Errorf("순서가 틀렸다: %d 번째가 %d", i, m.Seq)
			}
			if m.SenderName != "테스트고객" {
				t.Errorf("보낸 사람 이름이 비었다: %q", m.SenderName)
			}
		}
	})

	t.Run("마지막 seq 이후를 물으면 빈 결과다", func(t *testing.T) {
		msgs, err := store.ListMessages(ctx, conv.ID, 5, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Errorf("빈 결과여야 한다: %d건", len(msgs))
		}
	})
}

func TestAppendMessage_없는대화(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, _ := testsupport.Conversation(t, store)

	_, err := store.AppendMessage(ctx, customerMessage(id.New(), customerID, id.New(), "허공"))
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("ErrNotFound 여야 한다: %v", err)
	}
}

// 같은 client_msg_id 를 다른 발신자가 보내면 재전송이 아니다. 남의 메시지를 돌려주면 안 된다.
func TestAppendMessage_다른발신자의client_msg_id는거절한다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	cmid := id.New()
	if _, err := store.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: cmid, SenderKind: core.SenderAgent, SenderAgentID: agent.ID,
		Visibility: core.Private, Body: "내부 메모",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, cmid, "같은 id 로 고객이 보냄"))
	if !errors.Is(err, core.ErrClientMsgIDTaken) {
		t.Fatalf("다른 발신자의 id 는 ErrClientMsgIDTaken 이어야 한다: %v", err)
	}
	seqs, _ := store.MessageSeqs(ctx, conv.ID)
	if len(seqs) != 1 {
		t.Errorf("거절된 전송은 행을 남기지 않는다: %v", seqs)
	}
}

// 전송 응답의 번호는 보낸 사람의 번호 공간이다. 상담원에게 고객 번호를 주면 데스크 커서가 오염된다.
func TestAppendMessage_응답번호는발신자의공간이다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	send := func(kind core.SenderKind, vis core.Visibility, body string) core.AppendResult {
		t.Helper()
		p := core.AppendParams{ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: kind, Visibility: vis, Body: body}
		if kind == core.SenderAgent {
			p.SenderAgentID = agent.ID
		} else {
			p.SenderCustomer = customerID
		}
		res, err := store.AppendMessage(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	send(core.SenderCustomer, core.Public, "1 공개")             // seq 1, public 1
	send(core.SenderAgent, core.Private, "2 내부 메모")            // seq 2
	agentPublic := send(core.SenderAgent, core.Public, "3 공개") // seq 3, public 2
	if agentPublic.Message.Seq != 3 {
		t.Errorf("상담원의 공개 메시지 응답은 전체 번호 3 이어야 한다: %d", agentPublic.Message.Seq)
	}
	custPublic := send(core.SenderCustomer, core.Public, "4 공개") // seq 4, public 3
	if custPublic.Message.Seq != 3 {
		t.Errorf("고객의 응답은 공개 번호 3 이어야 한다: %d", custPublic.Message.Seq)
	}
	// 재전송(멱등)도 같은 규칙이다.
	again, err := store.AppendMessage(ctx, core.AppendParams{
		ConversationID: conv.ID, ClientMsgID: agentPublic.Message.ClientMsgID, SenderKind: core.SenderAgent,
		SenderAgentID: agent.ID, Visibility: core.Public, Body: "3 공개",
	})
	if err != nil || again.Created || again.Message.Seq != 3 {
		t.Errorf("재전송도 전체 번호 3: created=%v seq=%d err=%v", again.Created, again.Message.Seq, err)
	}
}

// 환영 문구·자동 응답은 발신자 id 가 없다. 같은 client_msg_id 로 다시 들어와도 재전송으로 통과해야 한다.
func TestAppendMessage_봇메시지의재전송은거절되지않는다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	_, conv := testsupport.Conversation(t, store)
	bot := core.AppendParams{ConversationID: conv.ID, ClientMsgID: "system-welcome", SenderKind: core.SenderBot, Visibility: core.Public, Body: "환영"}
	first, err := store.AppendMessage(ctx, bot)
	if err != nil || !first.Created {
		t.Fatalf("첫 전송: created=%v err=%v", first.Created, err)
	}
	again, err := store.AppendMessage(ctx, bot)
	if err != nil || again.Created || again.Message.Seq != first.Message.Seq {
		t.Errorf("재전송은 같은 번호, created=false: created=%v seq=%d/%d err=%v", again.Created, again.Message.Seq, first.Message.Seq, err)
	}
}

// 권한 검사는 저장 트랜잭션 앞에서 끝난다. 검사를 지난 뒤 삭제가 끼어든 요청이 저장소에 닿아도 지운 대화에는 쓰지 못한다.
func TestAppendMessage_삭제된대화에는어떤발신자도쓸수없다(t *testing.T) {
	store := testsupport.Store(t)
	ctx := context.Background()
	customerID, conv := testsupport.Conversation(t, store)
	agent := testsupport.Agent(t, store)
	if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "지우기 전")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteConversation(ctx, conv.ID, agent.ID, "정리"); err != nil {
		t.Fatal(err)
	}
	reopening := customerMessage(conv.ID, customerID, id.New(), "지운 뒤 다시 문의")
	reopening.Reopen = true
	for name, p := range map[string]core.AppendParams{
		"고객":     customerMessage(conv.ID, customerID, id.New(), "지운 뒤"),
		"고객 재개":  reopening,
		"상담원 답변": {ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Visibility: core.Public, Body: "답변", Reopen: true},
		"내부 메모":  {ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderAgent, SenderAgentID: agent.ID, Visibility: core.Private, Body: "메모"},
		"시스템":    {ConversationID: conv.ID, ClientMsgID: id.New(), SenderKind: core.SenderSystem, Visibility: core.Private, Body: "시스템 줄"},
	} {
		if _, err := store.AppendMessage(ctx, p); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s: ErrNotFound 여야 한다: %v", name, err)
		}
	}
	if seqs, _ := store.MessageSeqs(ctx, conv.ID); len(seqs) != 1 {
		t.Errorf("지운 대화에 행이 늘었다: %v", seqs)
	}
	// 되살리면 다시 쓴다.
	if err := store.RestoreConversation(ctx, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "되살린 뒤")); err != nil {
		t.Errorf("되살린 대화에는 쓴다: %v", err)
	}
}

// 커밋 뒤 알림은 5초까지만 기다린다. 알림이 늦거나 사라져도 메시지는 이미 커밋됐고 안전망이 전달한다(D4) —
// 그래서 시한을 넘기면 저장은 성공으로 돌려주고 notify.failed 한 줄에 대화 id 만 남긴다. 5초는 정책이라 리터럴로 고정한다.
// 재현: DB 앞에 중계기를 두고, 켜 두면 pg_notify 문장만 DB 로 넘기지 않고 붙든다. 그 밖의 문장(트랜잭션과 커밋)은 그대로 간다.
func TestAppendMessage_커밋뒤알림은5초를넘겨기다리지않고저장은성공으로돌려준다(t *testing.T) {
	const notifyLimit = 5 * time.Second
	testsupport.Store(t) // 스키마 준비. DB 가 없으면 건너뛴다
	proxy := newNotifyStaller(t)
	ctx := context.Background()
	store, err := pgstore.OpenWithPool(ctx, proxy.url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	customerID, conv := testsupport.Conversation(t, store)

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(logctx.NewHandler(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	proxy.armed.Store(true)
	type outcome struct {
		res core.AppendResult
		err error
	}
	appended := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := store.AppendMessage(ctx, customerMessage(conv.ID, customerID, id.New(), "알림이 막혀도 저장은 끝난다"))
		appended <- outcome{res, err}
	}()
	var got outcome
	select {
	case got = <-appended:
	case <-time.After(15 * time.Second):
		proxy.release() // 붙든 연결을 끊어 저장소를 닫을 수 있게 한다
		<-appended
		t.Fatal("커밋 뒤 알림이 시한 없이 기다린다 — 15초가 지나도 저장이 돌아오지 않았다")
	}
	elapsed := time.Since(start)
	proxy.armed.Store(false)
	proxy.release() // 붙든 연결을 놓아 저장소가 그 연결을 바로 닫게 한다
	res, err := got.res, got.err

	if proxy.stalled.Load() == 0 {
		t.Fatal("알림 문장을 붙들지 못했다 — 막힌 알림을 재현하지 못했다")
	}
	if err != nil || !res.Created || res.Message.Seq == 0 {
		t.Fatalf("저장은 성공이어야 한다: %+v %v", res, err)
	}
	if elapsed < notifyLimit-200*time.Millisecond || elapsed > notifyLimit+2*time.Second {
		t.Errorf("알림은 %v 에서 포기해야 한다: %v 걸렸다", notifyLimit, elapsed)
	}
	if seqs, err := testsupport.Store(t).MessageSeqs(ctx, conv.ID); err != nil || len(seqs) == 0 || seqs[len(seqs)-1] != res.Message.Seq {
		t.Errorf("메시지가 커밋돼 있어야 한다: %v %v", seqs, err)
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		var l map[string]any
		if json.Unmarshal([]byte(raw), &l) == nil && l["msg"] == "notify.failed" {
			line = l
		}
	}
	if line == nil {
		t.Fatalf("notify.failed 가 없다:\n%s", out)
	}
	if line["conversation_id"] != conv.ID || line["reason"] != "timeout" || line["level"] != "WARN" {
		t.Errorf("내용이 틀렸다: %v", line)
	}
	// 로그에는 id 만 적는다(D30). 알림 본문이나 오류 문구를 싣지 않는다.
	for key := range line {
		switch key {
		case "time", "level", "msg", "conversation_id", "reason":
		default:
			t.Errorf("notify.failed 에 id 가 아닌 %q 가 실렸다: %v", key, line)
		}
	}
	if strings.Contains(out, "알림이 막혀도") {
		t.Errorf("로그에 본문이 실렸다:\n%s", out)
	}
}

// notifyStaller 는 테스트 DB 앞의 TCP 중계기다. armed 동안 pg_notify 가 든 요청을 DB 로 넘기지 않고 붙든다.
// 문장 본문이 매번 오가도록 단순 프로토콜(default_query_exec_mode=simple_protocol)로 붙는다.
type notifyStaller struct {
	url     string
	armed   atomic.Bool
	stalled atomic.Int32
	done    chan struct{}
	once    sync.Once
}

func (p *notifyStaller) release() { p.once.Do(func() { close(p.done) }) }

func newNotifyStaller(t *testing.T) *notifyStaller {
	t.Helper()
	target, err := url.Parse(testsupport.URL())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &notifyStaller{done: make(chan struct{})}
	t.Cleanup(func() { p.release(); ln.Close() })
	upstream := target.Host
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			go func() { _, _ = io.Copy(client, server); client.Close() }()
			go func() {
				defer server.Close()
				chunk := make([]byte, 64<<10)
				for {
					n, err := client.Read(chunk)
					if n > 0 {
						if p.armed.Load() && bytes.Contains(chunk[:n], []byte("pg_notify")) {
							p.stalled.Add(1)
							<-p.done // 끝날 때까지 붙든다. 서버로는 영영 가지 않는다
							client.Close()
							return
						}
						if _, werr := server.Write(chunk[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	q := target.Query()
	q.Set("default_query_exec_mode", "simple_protocol")
	target.Host = ln.Addr().String()
	target.RawQuery = q.Encode()
	p.url = target.String()
	return p
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
