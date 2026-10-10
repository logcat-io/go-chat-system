package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 기록을 남기는 기능이 본문을 새게 하면 안 된다. 수정 이력·삭제·익명화·재상담이 만나는 자리의 정책들.

func TestE2E_수정이력은그대화의것만보인다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	a, b := h.newTenant("가 상점"), h.newTenant("나 상점")
	custA, convA := h.widgetCustomer(a)
	_, convB := h.widgetCustomer(b)
	msg := h.send(convA, custA, "카드번호 1111-2222", false)
	msgID := msg["id"].(string)
	h.call(http.MethodPatch, "/api/conversations/"+convA+"/messages/"+msgID, custA, map[string]any{"body": "가렸습니다"})

	t.Run("다른 워크스페이스의 상담원이 자기 대화 주소에 남의 메시지 id 를 넣어도 원문이 안 나온다", func(t *testing.T) {
		got := h.bodyOf("/api/agent/conversations/"+convB+"/messages/"+msgID+"/revisions", b.token)
		if strings.Contains(got, "1111-2222") {
			t.Fatalf("남의 워크스페이스 원문이 나왔다: %s", got)
		}
	})

	t.Run("같은 워크스페이스라도 다른 대화 주소로는 안 나온다", func(t *testing.T) {
		_, other := h.widgetCustomer(a)
		got := h.bodyOf("/api/agent/conversations/"+other+"/messages/"+msgID+"/revisions", a.token)
		if strings.Contains(got, "1111-2222") {
			t.Fatalf("다른 대화 주소로 원문이 나왔다: %s", got)
		}
	})

	t.Run("제 대화의 상담원에게는 나온다", func(t *testing.T) {
		got := h.bodyOf("/api/agent/conversations/"+convA+"/messages/"+msgID+"/revisions", a.token)
		if !strings.Contains(got, "1111-2222") {
			t.Fatalf("원문 이력이 있어야 한다: %s", got)
		}
	})
}

func TestE2E_고친뒤지우면본문이어디에도안나온다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("삭제 상점")
	cust, convID := h.widgetCustomer(tn)
	msg := h.send(convID, tn.token, "첫 안내", false)
	msgID := msg["id"].(string)
	h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+msgID, tn.token, map[string]any{"body": "잘못보낸비밀문구"})
	if code, _ := h.call(http.MethodDelete, "/api/conversations/"+convID+"/messages/"+msgID, tn.token, nil); code != http.StatusOK {
		t.Fatalf("삭제: %d", code)
	}

	t.Run("고객 로그에 고쳤던 본문이 남지 않는다 — 수정 이벤트 행까지", func(t *testing.T) {
		got := h.bodyOf("/api/conversations/"+convID+"/messages?after_seq=0", cust)
		if strings.Contains(got, "잘못보낸비밀문구") || strings.Contains(got, "첫 안내") {
			t.Fatalf("지운 본문이 고객에게 나간다: %s", got)
		}
	})

	t.Run("상담원 로그·받은함 검색·내보내기에도 없다", func(t *testing.T) {
		if got := h.bodyOf("/api/conversations/"+convID+"/messages?after_seq=0", tn.token); strings.Contains(got, "잘못보낸비밀문구") {
			t.Errorf("상담원 로그에 남았다")
		}
		if n := len(firstList(h.get("/api/agent/conversations?q=잘못보낸비밀문구", tn.token), "conversations")); n != 0 {
			t.Errorf("받은함 검색에 걸린다: %d", n)
		}
		if got := h.bodyOf("/api/admin/export", tn.token); strings.Contains(got, "잘못보낸비밀문구") {
			t.Errorf("내보내기에 남았다")
		}
	})

	t.Run("번호는 그대로다 — 이벤트 행을 지우지 않고 비운다", func(t *testing.T) {
		msgs := h.messagesOf(convID, cust)
		for i, m := range msgs {
			if m["seq"] != float64(i+1) {
				t.Fatalf("고객 번호에 빈칸: %d번째 seq=%v", i+1, m["seq"])
			}
		}
	})
}

func TestE2E_고객을지우면수정이력의원문도지워진다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("익명화 상점")
	cust, convID := h.widgetCustomer(tn)
	msg := h.send(convID, cust, "제 전화번호는 010-1234-5678", false)
	msgID := msg["id"].(string)
	h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+msgID, cust, map[string]any{"body": "번호는 지웠어요"})
	customerID, _ := h.get("/api/conversations/"+convID, tn.token)["customer_id"].(string)

	if code := h.status(http.MethodDelete, "/api/customers/"+customerID, tn.token, nil); code >= 300 {
		t.Fatalf("고객 삭제: %d", code)
	}
	revs := h.bodyOf("/api/agent/conversations/"+convID+"/messages/"+msgID+"/revisions", tn.token)
	if strings.Contains(revs, "010-1234-5678") {
		t.Errorf("수정 이력에 원문이 남았다: %s", revs)
	}
	if got := h.bodyOf("/api/conversations/"+convID+"/messages?after_seq=0", tn.token); strings.Contains(got, "010-1234-5678") || strings.Contains(got, "번호는 지웠어요") {
		t.Errorf("로그에 고객 본문이 남았다: %s", got)
	}
}

func TestE2E_회원의방열기는몇번을눌러도한번이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("경합 상점")

	open := func(cust string, n int) map[string]bool {
		ids := map[string]bool{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := h.postRaw("/api/customer/conversations", cust, nil)
				defer res.Body.Close()
				var out struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				raw, _ := io.ReadAll(res.Body)
				_ = json.Unmarshal(raw, &out)
				if out.Conversation.ID == "" {
					t.Logf("방 열기 실패 응답 %d: %s", res.StatusCode, raw)
				}
				mu.Lock()
				ids[out.Conversation.ID] = true
				mu.Unlock()
			}()
		}
		wg.Wait()
		return ids
	}
	rooms := func(convID string) (count int) {
		err := h.store.Pool().QueryRow(context.Background(), `
			SELECT count(*) FROM conversations
			 WHERE customer_id = (SELECT customer_id FROM conversations WHERE id = $1)`, convID).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	welcomes := func(convID, cust string) (n int) {
		for _, m := range h.messagesOf(convID, cust) {
			if m["sender_kind"] == "BOT" {
				n++
			}
		}
		return
	}

	t.Run("첫 요청 여덟 개가 동시에 와도 방은 하나고 환영 문구도 한 번이다", func(t *testing.T) {
		cust := h.memberSession(tn)
		ids := open(cust, 8)
		if len(ids) != 1 || ids[""] {
			t.Fatalf("모두 같은 방을 받아야 한다: %v", ids)
		}
		for convID := range ids {
			if count := rooms(convID); count != 1 {
				t.Errorf("방 %d개", count)
			}
			if n := welcomes(convID, cust); n != 1 {
				t.Errorf("환영 문구가 %d번 나갔다", n)
			}
		}
	})

	t.Run("열린 상담이 있는데 다시 눌러도 방이 늘지 않고 방금 보낸 질문이 그대로 보인다", func(t *testing.T) {
		cust := h.memberSession(tn)
		convID, _ := h.startConversation(cust)["id"].(string)
		h.send(convID, cust, "아직 답 못 받은 질문", false)
		before := len(h.messagesOf(convID, tn.token))
		if again, _ := h.startConversation(cust)["id"].(string); again != convID {
			t.Errorf("같은 방이어야 한다: %s vs %s", again, convID)
		}
		if count := rooms(convID); count != 1 {
			t.Errorf("방 %d개", count)
		}
		if got := h.bodyOf("/api/conversations/"+convID+"/messages?after_seq=0", cust); !strings.Contains(got, "아직 답 못 받은 질문") {
			t.Errorf("질문이 고객 화면에서 사라졌다: %s", got)
		}
		if after := len(h.messagesOf(convID, tn.token)); after != before {
			t.Errorf("환영 문구나 시스템 줄이 또 쌓였다: %d → %d", before, after)
		}
	})

	t.Run("종료 뒤 재요청 여덟 개가 동시에 와도 새 방은 하나다", func(t *testing.T) {
		cust := h.memberSession(tn)
		convID, _ := h.startConversation(cust)["id"].(string)
		h.send(convID, cust, "첫 상담", false)
		h.post("/api/agent/conversations/"+convID+"/state", tn.token, map[string]any{"state": "CLOSED"})
		ids := open(cust, 8)
		if len(ids) != 1 || ids[convID] || ids[""] {
			t.Fatalf("모두 같은 새 방을 받아야 한다: %v", ids)
		}
		for next := range ids {
			if count := rooms(next); count != 2 {
				t.Errorf("이 회원의 상담은 2건이어야 한다: %d", count)
			}
			if n := welcomes(next, cust); n != 1 {
				t.Errorf("환영 문구가 %d번 나갔다", n)
			}
			// 먼저 연 요청만 흔적을 남긴다. 나머지 일곱은 같은 방을 받을 뿐이다.
			if n := strings.Count(h.bodyOf("/api/conversations/"+next+"/messages?after_seq=0", tn.token), "번째 상담"); n != 1 {
				t.Errorf("몇 번째 상담인지 알리는 줄이 %d개다", n)
			}
			var opened int
			if err := h.store.Pool().QueryRow(context.Background(),
				"SELECT count(*) FROM conversation_events WHERE conversation_id = $1 AND kind = 'opened'", next).Scan(&opened); err != nil || opened != 1 {
				t.Errorf("문의 시작 기록이 %d건이다 (%v)", opened, err)
			}
		}
	})
}

func TestE2E_종료된지난상담은다시열리지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("재개 상점")
	cust := h.memberSession(tn)
	oldID, _ := h.startConversation(cust)["id"].(string)
	h.send(oldID, cust, "지난 상담 질문", false)
	closeConv := func(convID string) {
		h.post("/api/agent/conversations/"+convID+"/state", tn.token, map[string]any{"state": "CLOSED"})
	}
	stateOf := func(convID string) any { return h.get("/api/conversations/"+convID, tn.token)["state"] }
	sendStatus := func(convID, token string, internal bool) int {
		return h.status(http.MethodPost, "/api/conversations/"+convID+"/messages", token,
			map[string]any{"client_msg_id": id.New(), "body": "뒤늦은 글", "internal": internal})
	}
	closeConv(oldID)

	t.Run("가장 최근 상담은 종료 뒤에 고객이 쓰면 다시 열린다", func(t *testing.T) {
		if code := sendStatus(oldID, cust, false); code >= 300 {
			t.Fatalf("전송: %d", code)
		}
		if st := stateOf(oldID); st != "OPEN" {
			t.Errorf("다시 열려야 한다: %v", st)
		}
	})

	closeConv(oldID)
	newID, _ := h.startConversation(cust)["id"].(string)
	if newID == "" || newID == oldID {
		t.Fatalf("새 상담이 열려야 한다: %s", newID)
	}

	t.Run("회원은 지난 상담을 다시 볼 수 없다 — 조회·목록·첨부·구독 모두 없는 대화다", func(t *testing.T) {
		for _, path := range []string{"", "/messages?after_seq=0", "/messages?before_seq=99", "/reads"} {
			if code := h.status(http.MethodGet, "/api/conversations/"+oldID+path, cust, nil); code != http.StatusNotFound {
				t.Errorf("GET %s: %d", path, code)
			}
		}
		if got := h.bodyOf("/api/customer/conversations", cust); strings.Contains(got, oldID) {
			t.Errorf("목록에 지난 상담이 있다")
		}
		ctx := testCtx(t, 5*time.Second)
		c := h.dial(ctx, cust)
		c.send(ctx, map[string]any{"type": "subscribe", "conversation_id": oldID, "after_seq": 0})
		if f := c.await(ctx, func(f frame) bool { return f.Type == "error" || f.Type == "ready" }); f.Type != "error" {
			t.Errorf("구독이 허용됐다: %+v", f)
		}
		if st := h.get("/api/conversations/"+oldID, tn.token)["id"]; st != oldID {
			t.Errorf("상담원은 계속 본다: %v", st)
		}
	})

	t.Run("회원이 지난 상담에 쓰면 없는 대화로 답하고 그 상담은 닫힌 채다", func(t *testing.T) {
		res := h.postRaw("/api/conversations/"+oldID+"/messages", cust, map[string]any{"client_msg_id": id.New(), "body": "옛 탭에서 보낸 글"})
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("404 여야 한다: %d %s", res.StatusCode, raw)
		}
		if st := stateOf(oldID); st != "CLOSED" {
			t.Errorf("지난 상담이 다시 열렸다: %v", st)
		}
		if got := h.bodyOf("/api/conversations/"+oldID+"/messages?after_seq=0", tn.token); strings.Contains(got, "옛 탭에서 보낸 글") {
			t.Errorf("거절한 글이 저장됐다")
		}
		list := firstList(h.get("/api/customer/conversations", cust), "conversations")
		if len(list) != 1 || list[0].(map[string]any)["id"] != newID {
			t.Errorf("회원의 목록은 이번 상담 하나다: %+v", list)
		}
	})

	t.Run("상담원도 그런 지난 상담을 다시 열거나 공개 답변을 달 수 없고, 내부 메모는 남길 수 있다", func(t *testing.T) {
		if code := h.status(http.MethodPost, "/api/agent/conversations/"+oldID+"/state", tn.token, map[string]any{"state": "OPEN"}); code != http.StatusConflict {
			t.Errorf("다시 열기: %d", code)
		}
		if code := sendStatus(oldID, tn.token, false); code != http.StatusConflict {
			t.Errorf("공개 답변: %d", code)
		}
		if code := sendStatus(oldID, tn.token, true); code >= 300 {
			t.Errorf("내부 메모는 된다: %d", code)
		}
		if st := stateOf(oldID); st != "CLOSED" {
			t.Errorf("지난 상담이 다시 열렸다: %v", st)
		}
	})

	t.Run("이번 상담을 종료한 뒤에도 다시 열리는 것은 가장 최근 상담뿐이다", func(t *testing.T) {
		closeConv(newID)
		if code := sendStatus(oldID, cust, false); code != http.StatusNotFound {
			t.Errorf("지난 상담: %d", code)
		}
		if code := sendStatus(newID, cust, false); code >= 300 {
			t.Errorf("가장 최근 상담: %d", code)
		}
		if oldSt, newSt := stateOf(oldID), stateOf(newID); oldSt != "CLOSED" || newSt != "OPEN" {
			t.Errorf("지난 상담 %v, 최근 상담 %v", oldSt, newSt)
		}
	})

	t.Run("익명 고객의 종료된 대화는 예전처럼 쓰면 다시 열린다", func(t *testing.T) {
		anon, first := h.widgetCustomer(tn)
		closeConv(first)
		h.startConversation(anon)
		if code := sendStatus(first, anon, false); code >= 300 {
			t.Errorf("전송: %d", code)
		}
		if st := stateOf(first); st != "OPEN" {
			t.Errorf("다시 열려야 한다: %v", st)
		}
	})
}

func TestE2E_업무기록조회의잘못된입력은400이다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("입력 상점")
	for _, path := range []string{
		"/api/agent/work?agent=not-a-uuid",
		"/api/agent/work?from=yesterday",
		"/api/agent/work/summary?to=2026-13-99",
	} {
		if code := h.status(http.MethodGet, path, tn.token, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, code)
		}
	}
}
