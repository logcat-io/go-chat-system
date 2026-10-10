package httpapi_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 기록을 지우지 않는다는 전제 위의 정책들: 수정·삭제, 시스템 줄, 읽음 표시, 회원 재상담, 업무 기록.

func TestE2E_메시지수정과삭제(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("수정 상점")
	cust, convID := h.widgetCustomer(tn)
	mine := h.send(convID, cust, "주문번호 1234 입니다", false)
	reply := h.send(convID, tn.token, "확인했습니다", false)
	memo := h.send(convID, tn.token, "내부: 환불 대상", true)
	mineID, replyID, memoID := mine["id"].(string), reply["id"].(string), memo["id"].(string)

	t.Run("자기 메시지를 고치면 본문이 바뀌고 원문은 이력에 남는다", func(t *testing.T) {
		code, _ := h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+mineID, cust, map[string]any{"body": "주문번호 5678 입니다"})
		if code != http.StatusOK {
			t.Fatalf("수정: %d", code)
		}
		var edited map[string]any
		for _, m := range h.messagesOf(convID, tn.token) {
			if m["id"] == mineID {
				edited = m
			}
		}
		if edited["body"] != "주문번호 5678 입니다" || edited["edited_at"] == nil {
			t.Errorf("본문과 수정 표시: %+v", edited)
		}
		revs := firstList(h.get("/api/agent/conversations/"+convID+"/messages/"+mineID+"/revisions", tn.token), "revisions")
		if len(revs) != 1 || revs[0].(map[string]any)["body"] != "주문번호 1234 입니다" {
			t.Errorf("원문이 이력에 남아야 한다: %+v", revs)
		}
	})

	t.Run("수정은 같은 로그의 이벤트 행으로 전달되고 번호에 빈칸이 없다", func(t *testing.T) {
		msgs := h.messagesOf(convID, cust)
		last := msgs[len(msgs)-1]
		if last["kind"] != "EDIT" || last["ref_message_id"] != mineID {
			t.Errorf("고객 로그의 마지막은 EDIT 이벤트여야 한다: %+v", last)
		}
		for i, m := range msgs {
			if m["seq"] != float64(i+1) {
				t.Fatalf("고객 번호에 빈칸: %d번째 seq=%v", i+1, m["seq"])
			}
		}
	})

	t.Run("남의 메시지는 못 고친다 — 고객은 상담원 것을, 상담원은 고객 것을", func(t *testing.T) {
		if code, _ := h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+replyID, cust, map[string]any{"body": "x"}); code != http.StatusForbidden {
			t.Errorf("고객이 상담원 메시지 수정: %d", code)
		}
		if code, _ := h.call(http.MethodDelete, "/api/conversations/"+convID+"/messages/"+mineID, tn.token, nil); code != http.StatusForbidden {
			t.Errorf("관리자라도 고객 메시지는 못 지운다: %d", code)
		}
	})

	t.Run("내부 메모의 수정은 고객에게 가지 않는다", func(t *testing.T) {
		before := len(h.messagesOf(convID, cust))
		if code, _ := h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+memoID, tn.token, map[string]any{"body": "내부: 환불 완료"}); code != http.StatusOK {
			t.Fatalf("메모 수정: %d", code)
		}
		if after := len(h.messagesOf(convID, cust)); after != before {
			t.Errorf("고객 로그가 늘면 안 된다: %d → %d", before, after)
		}
	})

	t.Run("지우면 자리는 남고 본문은 사라지며 검색에도 안 걸린다", func(t *testing.T) {
		if code, _ := h.call(http.MethodDelete, "/api/conversations/"+convID+"/messages/"+replyID, tn.token, nil); code != http.StatusOK {
			t.Fatalf("삭제: %d", code)
		}
		for _, m := range h.messagesOf(convID, cust) {
			if m["id"] == replyID && (m["body"] != "" || m["deleted_at"] == nil) {
				t.Errorf("삭제된 메시지는 본문이 비고 표시가 있어야 한다: %+v", m)
			}
		}
		if n := len(firstList(h.get("/api/agent/search?q=확인했습니다", tn.token), "hits")); n != 0 {
			t.Errorf("삭제된 본문이 검색된다: %d", n)
		}
		revs := firstList(h.get("/api/agent/conversations/"+convID+"/messages/"+replyID+"/revisions", tn.token), "revisions")
		if len(revs) != 1 || revs[0].(map[string]any)["reason"] != "DELETE" {
			t.Errorf("지운 원문도 이력에 남는다: %+v", revs)
		}
		if code, _ := h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+replyID, tn.token, map[string]any{"body": "되살리기"}); code != http.StatusNotFound {
			t.Errorf("지운 메시지는 다시 못 고친다: %d", code)
		}
	})

	t.Run("수정·삭제는 답할 차례를 바꾸지 않는다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", tn.token, map[string]any{"agent_id": "me"})
		h.send(convID, tn.token, "답변", false)
		before := h.get("/api/agent/inbox/counts", tn.token)["todo"]
		h.call(http.MethodPatch, "/api/conversations/"+convID+"/messages/"+mineID, cust, map[string]any{"body": "주문번호 9999"})
		if after := h.get("/api/agent/inbox/counts", tn.token)["todo"]; after != before {
			t.Errorf("고객이 옛 메시지를 고쳤다고 답할 차례가 되면 안 된다: %v → %v", before, after)
		}
	})
}

func TestE2E_시스템줄과읽음은고객에게안보인다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("표시 상점")
	cust, convID := h.widgetCustomer(tn)
	h.send(convID, cust, "문의드립니다", false)

	t.Run("상태 변경·배정의 시스템 줄은 상담원에게만 보인다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+convID+"/assign", tn.token, map[string]any{"agent_id": "me"})
		h.post("/api/agent/conversations/"+convID+"/state", tn.token, map[string]any{"state": "CLOSED"})
		h.post("/api/agent/conversations/"+convID+"/state", tn.token, map[string]any{"state": "OPEN"})
		for _, m := range h.messagesOf(convID, cust) {
			if m["sender_kind"] == "SYSTEM" {
				t.Errorf("고객에게 시스템 줄이 보인다: %v", m["body"])
			}
		}
		system := 0
		for _, m := range h.messagesOf(convID, tn.token) {
			if m["sender_kind"] == "SYSTEM" {
				system++
			}
		}
		if system < 3 {
			t.Errorf("상담원에게는 배정·종료·재개 줄이 보여야 한다: %d", system)
		}
	})

	t.Run("상담원이 읽기만 하면 고객 화면에 읽음이 뜨지 않고, 답하면 그 지점까지 뜬다", func(t *testing.T) {
		conv := h.get("/api/conversations/"+convID, tn.token)
		h.post("/api/conversations/"+convID+"/read", tn.token, map[string]any{"seq": conv["last_seq"]})
		agentRead := func() float64 {
			for _, raw := range firstList(h.get("/api/conversations/"+convID+"/reads", cust), "reads") {
				r, _ := raw.(map[string]any)
				if r["reader_kind"] == "AGENT" {
					v, _ := r["last_read_seq"].(float64)
					return v
				}
			}
			return -1
		}
		if got := agentRead(); got != 0 {
			t.Errorf("읽기만 했으면 고객에게 보이는 읽음 지점은 0: %v", got)
		}
		reply := h.send(convID, tn.token, "답변드립니다", false)
		custConv := h.get("/api/conversations/"+convID, cust)
		if got := agentRead(); got != custConv["last_seq"] {
			t.Errorf("답한 지점(고객 번호 %v)까지 읽음: %v (reply seq %v)", custConv["last_seq"], got, reply["seq"])
		}
	})
}

// 감사 28: 시스템 줄은 운영 기록이다. 데스크는 ⋯ 를 그리지 않지만 서버는 관리자(message.moderate)의 삭제를 받아 기록을 지웠다.
// 감사 29: 관리자만 봤다. 직원·아르바이트도 같은 409 이고, 고객에게는 시스템 줄이 보이지 않으니 없는 줄과 같은 404 다(아래 테스트).
func TestE2E_시스템줄은관리자도지우거나고치지못하고409system_line이며줄은그대로다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("기록 상점")
	_, convID := h.widgetCustomer(tm.tn)
	h.post("/api/agent/conversations/"+convID+"/assign", tm.tn.token, map[string]any{"agent_id": "me"})
	var sys map[string]any
	for _, m := range h.messagesOf(convID, tm.tn.token) {
		if m["sender_kind"] == "SYSTEM" {
			sys = m
		}
	}
	if sys == nil {
		t.Fatal("배정의 시스템 줄이 없다")
	}
	sysID, body := sys["id"].(string), sys["body"].(string)
	path := "/api/conversations/" + convID + "/messages/" + sysID

	for _, who := range []struct {
		name  string
		token string
	}{{"관리자", tm.admin.token}, {"직원", tm.staff.token}, {"아르바이트", tm.partTime.token}} {
		code, out := h.call(http.MethodDelete, path, who.token, nil)
		if code != http.StatusConflict || errorCodeOf(out) != "system_line" {
			t.Errorf("%s의 시스템 줄 삭제는 409 system_line 이어야 한다: %d %+v", who.name, code, out)
		}
		code, out = h.call(http.MethodPatch, path, who.token, map[string]any{"body": "고친 기록"})
		if code != http.StatusConflict || errorCodeOf(out) != "system_line" {
			t.Errorf("%s의 시스템 줄 수정도 409 system_line 이어야 한다: %d %+v", who.name, code, out)
		}
	}
	before := len(h.messagesOf(convID, tm.tn.token))
	for _, m := range h.messagesOf(convID, tm.tn.token) {
		if m["id"] == sysID && (m["body"] != body || m["deleted_at"] != nil || m["edited_at"] != nil) {
			t.Errorf("거절했는데 시스템 줄이 바뀌었다: %+v", m)
		}
		if m["kind"] == "DELETE" || m["kind"] == "EDIT" {
			t.Errorf("거절했는데 수정·삭제 이벤트 행이 생겼다: %+v", m)
		}
	}
	if after := len(h.messagesOf(convID, tm.tn.token)); after != before {
		t.Errorf("줄 수가 바뀌었다: %d → %d", before, after)
	}
	// 이력 조회는 막지 않는다 — 이 변경 전에 지운 시스템 줄의 원문도 운영 기록이다. 새로 생길 이력은 없다.
	if revs := firstList(h.get("/api/agent/conversations/"+convID+"/messages/"+sysID+"/revisions", tm.tn.token), "revisions"); len(revs) != 0 {
		t.Errorf("거절했는데 이력이 남았다: %+v", revs)
	}
}

// 감사 29: 시스템 줄 판정(409 system_line)이 가시성보다 먼저라, 고객이 볼 수 없는 시스템 줄의 id 로 고치거나 지우면 409 가 와서
// 그 줄이 있다는 것과 시스템 줄이라는 것을 알았다. 고객에게 보이지 않는 줄(시스템 줄·내부 메모)은 없는 메시지와 같은 답이다.
func TestE2E_고객이볼수없는줄을고치거나지우려하면없는메시지와같은404이고시스템줄이라고알리지않는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("가림 상점")
	cust, convID := h.widgetCustomer(tn)
	h.post("/api/agent/conversations/"+convID+"/assign", tn.token, map[string]any{"agent_id": "me"})
	memo := h.send(convID, tn.token, "내부: 환불 검토", true)
	memoID, _ := memo["id"].(string)
	var sysID, sysBody string
	for _, m := range h.messagesOf(convID, tn.token) {
		if m["sender_kind"] == "SYSTEM" {
			sysID, sysBody = m["id"].(string), m["body"].(string)
		}
	}
	if sysID == "" || memoID == "" {
		t.Fatalf("시스템 줄 %q · 메모 %q 가 없다", sysID, memoID)
	}
	missing := "/api/conversations/" + convID + "/messages/" + id.New()
	wantCode, wantOut := h.call(http.MethodDelete, missing, cust, nil)
	if wantCode != http.StatusNotFound {
		t.Fatalf("없는 메시지 삭제는 404 여야 한다: %d %+v", wantCode, wantOut)
	}
	for _, line := range []struct{ name, id string }{{"시스템 줄", sysID}, {"내부 메모", memoID}} {
		path := "/api/conversations/" + convID + "/messages/" + line.id
		for _, req := range []struct {
			method string
			body   any
		}{{http.MethodDelete, nil}, {http.MethodPatch, map[string]any{"body": "x"}}} {
			code, out := h.call(req.method, path, cust, req.body)
			if code != wantCode || errorCodeOf(out) != errorCodeOf(wantOut) {
				t.Errorf("고객의 %s %s: %d %+v — 없는 메시지와 같은 %d %s 여야 한다", line.name, req.method, code, out, wantCode, errorCodeOf(wantOut))
			}
		}
	}
	for _, m := range h.messagesOf(convID, tn.token) {
		if m["id"] == sysID && (m["body"] != sysBody || m["deleted_at"] != nil) {
			t.Errorf("시스템 줄이 바뀌었다: %+v", m)
		}
		if m["id"] == memoID && m["deleted_at"] != nil {
			t.Errorf("내부 메모가 지워졌다: %+v", m)
		}
	}
}

func TestE2E_상담마다새대화(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("재상담 상점")
	cust := h.memberSession(tn)
	firstID, _ := h.startConversation(cust)["id"].(string)
	for i := 0; i < 35; i++ {
		h.send(firstID, cust, "첫 상담 메시지 "+itoa(int64(i+1)), false)
	}
	h.send(firstID, tn.token, "첫 상담 답변", false)
	h.post("/api/agent/conversations/"+firstID+"/state", tn.token, map[string]any{"state": "CLOSED"})

	t.Run("종료된 상담은 다시 문의하기 전까지 회원의 목록에 남아 답을 다시 볼 수 있다", func(t *testing.T) {
		list := firstList(h.get("/api/customer/conversations", cust), "conversations")
		if len(list) != 1 || list[0].(map[string]any)["id"] != firstID {
			t.Errorf("가장 최근 상담 하나가 보여야 한다: %+v", list)
		}
	})

	secondID, _ := h.startConversation(cust)["id"].(string)

	t.Run("종료 뒤 회원이 다시 문의하면 새 대화가 열리고 종료된 상담은 닫힌 채다", func(t *testing.T) {
		if secondID == "" || secondID == firstID {
			t.Fatalf("새 대화여야 한다: %v vs %v", secondID, firstID)
		}
		if st := h.get("/api/conversations/"+firstID, tn.token)["state"]; st != "CLOSED" {
			t.Errorf("종료된 상담이 다시 열렸다: %v", st)
		}
	})

	t.Run("고객의 새 대화에는 지난 상담의 글이 없고, 목록에도 지난 상담이 없다", func(t *testing.T) {
		for _, path := range []string{"/messages?after_seq=0", "/messages?before_seq=1000"} {
			if got := h.bodyOf("/api/conversations/"+secondID+path, cust); strings.Contains(got, "첫 상담") {
				t.Errorf("%s 에 지난 상담이 보인다", path)
			}
		}
		list := firstList(h.get("/api/customer/conversations", cust), "conversations")
		if len(list) != 1 || list[0].(map[string]any)["id"] != secondID {
			t.Errorf("목록에는 이번 상담 하나: %+v", list)
		}
		if got := h.bodyOf("/api/customer/conversations", cust); strings.Contains(got, "첫 상담") {
			t.Errorf("목록 미리보기에 지난 상담이 보인다: %s", got)
		}
	})

	t.Run("상담원에게는 몇 번째 상담인지 내부 줄로 알린다", func(t *testing.T) {
		if got := h.bodyOf("/api/conversations/"+secondID+"/messages?after_seq=0", tn.token); !strings.Contains(got, "2번째 상담") {
			t.Errorf("상담원 로그에 안내가 없다: %s", got)
		}
		if got := h.bodyOf("/api/conversations/"+secondID+"/messages?after_seq=0", cust); strings.Contains(got, "번째 상담") {
			t.Errorf("그 줄이 고객에게 보인다")
		}
	})

	t.Run("상담원은 새 상담에서 직전 상담의 마지막 30줄을 받고, 그 앞은 커서로 이어 받는다", func(t *testing.T) {
		page := firstList(h.get("/api/agent/conversations/"+secondID+"/prior?limit=30", tn.token), "messages")
		if len(page) != 30 {
			t.Fatalf("직전 30줄: %d", len(page))
		}
		head, tail := page[0].(map[string]any), page[len(page)-1].(map[string]any)
		if tail["conversation_id"] != firstID || !strings.Contains(h.bodyOf("/api/agent/conversations/"+secondID+"/prior?limit=3", tn.token), "첫 상담 답변") {
			t.Errorf("직전 상담의 끝부분이어야 한다: %+v", tail)
		}
		seen := len(page)
		for {
			path := "/api/agent/conversations/" + secondID + "/prior?limit=30&before_conversation=" + head["conversation_id"].(string) + "&before_seq=" + itoa(int64(head["seq"].(float64)))
			older := firstList(h.get(path, tn.token), "messages")
			if len(older) == 0 {
				break
			}
			if last := older[len(older)-1].(map[string]any); last["seq"].(float64) >= head["seq"].(float64) {
				t.Fatalf("앞 페이지는 커서보다 앞이어야 한다: %v >= %v", last["seq"], head["seq"])
			}
			seen += len(older)
			head = older[0].(map[string]any)
		}
		all := 0
		for _, m := range h.messagesOf(firstID, tn.token) {
			if m["kind"] == "MESSAGE" {
				all++
			}
		}
		if seen != all || head["seq"] != float64(1) {
			t.Errorf("끝까지 이으면 첫 상담의 메시지 전부(%d줄)다: %d줄, 첫 줄 seq=%v", all, seen, head["seq"])
		}
	})

	t.Run("이전 상담 이어 보기는 상담원만, 자기 워크스페이스 것만 쓴다", func(t *testing.T) {
		if code := h.status(http.MethodGet, "/api/agent/conversations/"+secondID+"/prior", cust, nil); code != http.StatusForbidden {
			t.Errorf("고객: %d", code)
		}
		other := h.newTenant("옆 상점")
		if code := h.status(http.MethodGet, "/api/agent/conversations/"+secondID+"/prior", other.token, nil); code != http.StatusNotFound {
			t.Errorf("다른 워크스페이스의 상담원: %d", code)
		}
		_, strangerConv := h.widgetCustomer(tn)
		h.send(strangerConv, tn.token, "다른 고객에게 한 답", false)
		got := h.bodyOf("/api/agent/conversations/"+secondID+"/prior?before_conversation="+strangerConv+"&before_seq=99", tn.token)
		if strings.Contains(got, "다른 고객") {
			t.Errorf("다른 고객의 대화를 커서로 주면 그 대화가 나온다: %s", got)
		}
	})

	t.Run("커서를 지금 대화나 다른 상담으로 줘도 지금 대화의 글은 이전 상담으로 나오지 않는다", func(t *testing.T) {
		h.send(secondID, cust, "이번 상담에서 한 말", false)
		got := h.bodyOf("/api/agent/conversations/"+secondID+"/prior?before_conversation="+secondID+"&before_seq=999", tn.token)
		if strings.Contains(got, "이번 상담에서 한 말") {
			t.Errorf("지금 대화가 이전 상담으로 나온다: %s", got)
		}
	})

	t.Run("익명 고객은 매번 새 대화다", func(t *testing.T) {
		anon, firstConv := h.widgetCustomer(tn)
		out := h.post("/api/customer/conversations", anon, nil)
		c, _ := out["conversation"].(map[string]any)
		if c["id"] == firstConv {
			t.Error("익명 고객은 새 대화가 생겨야 한다")
		}
	})

	t.Run("지난 상담의 첨부도 회원은 받을 수 없고 상담원은 받는다", func(t *testing.T) {
		att, _ := h.upload(secondID, tn.token, "guide.png", pngBytes(4, 4))["attachment"].(map[string]any)
		attID, _ := att["id"].(string)
		h.post("/api/conversations/"+secondID+"/messages", tn.token, map[string]any{"client_msg_id": id.New(), "body": "안내 그림", "attachment_id": attID})
		if res := h.raw(http.MethodGet, "/api/attachments/"+attID, cust); res.StatusCode != http.StatusOK {
			t.Fatalf("이번 상담의 첨부는 받는다: %d", res.StatusCode)
		}
		h.post("/api/agent/conversations/"+secondID+"/state", tn.token, map[string]any{"state": "CLOSED"})
		next, _ := h.startConversation(cust)["id"].(string)
		if next == secondID {
			t.Fatal("새 상담이 열려야 한다")
		}
		if res := h.raw(http.MethodGet, "/api/attachments/"+attID, cust); res.StatusCode == http.StatusOK {
			t.Errorf("지난 상담의 첨부를 회원이 받았다")
		}
		if res := h.raw(http.MethodGet, "/api/attachments/"+attID, tn.token); res.StatusCode != http.StatusOK {
			t.Errorf("상담원은 받아야 한다: %d", res.StatusCode)
		}
	})
}

func TestE2E_업무기록(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("업무 상점")
	cust, convID := h.widgetCustomer(tn)
	me := h.get("/api/me", tn.token)
	adminID, _ := me["agent_id"].(string)
	h.send(convID, cust, "환불 문의", false)
	h.post("/api/agent/conversations/"+convID+"/assign", tn.token, map[string]any{"agent_id": "me"})
	h.send(convID, tn.token, "메모", true)
	h.send(convID, tn.token, "환불 처리했습니다", false)
	h.send(convID, tn.token, "추가 안내", false)
	h.post("/api/agent/conversations/"+convID+"/state", tn.token, map[string]any{"state": "CLOSED"})

	kinds := func(token, query string) []string {
		var out []string
		for _, raw := range firstList(h.get("/api/agent/work"+query, token), "events") {
			out = append(out, raw.(map[string]any)["kind"].(string))
		}
		return out
	}

	t.Run("누가 어떤 고객의 대화에 무엇을 했는지 남는다", func(t *testing.T) {
		got := strings.Join(kinds(tn.token, "?conversation="+convID), ",")
		// 최신이 먼저다. 기다리던 고객에게 간 첫 공개 답변만 "응답" 이고, 메모와 이어지는 답변은 세지 않는다.
		if got != "closed,answered,assigned,opened" {
			t.Errorf("업무 기록: %s", got)
		}
		ev := firstList(h.get("/api/agent/work?agent=me", tn.token), "events")[0].(map[string]any)
		if ev["agent_id"] != adminID || ev["customer_name"] != "고객" || ev["conversation_id"] != convID {
			t.Errorf("누가·어떤 고객·어떤 대화가 채워져야 한다: %+v", ev)
		}
	})

	t.Run("상담원별 집계가 나온다", func(t *testing.T) {
		agents := firstList(h.get("/api/agent/work/summary", tn.token), "agents")
		if len(agents) != 1 {
			t.Fatalf("이 워크스페이스의 상담원만: %d", len(agents))
		}
		a := agents[0].(map[string]any)
		if a["assigned"] != float64(1) || a["answered"] != float64(1) || a["closed"] != float64(1) || a["customers"] != float64(1) {
			t.Errorf("배정1·응답1·종료1·고객1: %+v", a)
		}
	})

	t.Run("다른 워크스페이스의 업무 기록은 보이지 않고, 고객은 못 본다", func(t *testing.T) {
		other := h.newTenant("남의 업무 상점")
		if n := len(kinds(other.token, "")); n != 0 {
			t.Errorf("남의 기록이 보인다: %d", n)
		}
		if got := h.status(http.MethodGet, "/api/agent/work", cust, nil); got != http.StatusForbidden {
			t.Errorf("고객은 403: %d", got)
		}
		if got := h.status(http.MethodGet, "/api/agent/work?conversation="+convID, other.token, nil); got != http.StatusNotFound {
			t.Errorf("남의 대화의 기록은 404: %d", got)
		}
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
