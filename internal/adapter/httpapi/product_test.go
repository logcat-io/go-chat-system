package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 관리자가 팀을 들이고 내보내는 길. 이게 없으면 시드 계정 세 개가 영원히 전부다.
func TestE2E_팀관리(t *testing.T) {
	// 관리자 수를 단언하므로 이 테스트만의 DB 를 쓴다.
	store, _ := testsupport.Isolated(t)
	h := newHarnessOn(t, store, time.Second, false)
	admin := h.adminToken()
	agentToken := h.agentToken() // 일반 상담원
	// DB 는 테스트 프로세스가 공유하므로 매번 다른 이메일을 쓴다. 대문자로 보내 소문자 저장을 같이 본다.
	suffix := strings.ToLower(id.New()[28:])
	newEmail := "new-" + suffix + "@example.com"
	newEmailUpper := "New-" + suffix + "@Example.com"

	t.Run("일반 상담원은 팀을 못 본다", func(t *testing.T) {
		res := h.raw(http.MethodGet, "/api/admin/team", agentToken)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	var acceptURL string
	t.Run("관리자가 초대하면 수락 링크가 나온다", func(t *testing.T) {
		out := h.post("/api/admin/invites", admin, map[string]any{"email": newEmailUpper, "name": "신입", "role": "STAFF"})
		acceptURL, _ = out["accept_url"].(string)
		// 토큰은 주소 조각에 싣는다(D46). 쿼리에 실리면 접근 로그·Referer 에 남는다.
		if !strings.Contains(acceptURL, "/desk#invite=") || strings.Contains(acceptURL, "?") {
			t.Fatalf("수락 링크가 없다: %+v", out)
		}
		// DB 는 테스트 프로세스가 공유하므로 전체 건수가 아니라 내 초대가 보이는지를 본다.
		// 이메일은 소문자로 저장된다.
		team := h.get("/api/admin/team", admin)
		found := false
		for _, raw := range firstList(team, "invites") {
			m, _ := raw.(map[string]any)
			if m["email"] == newEmail && m["role"] == "STAFF" {
				found = true
			}
		}
		if !found {
			t.Errorf("대기 중 초대 목록에 내 초대가 소문자 이메일로 보여야 한다: %+v", team["invites"])
		}
	})

	inviteToken := acceptURL[strings.Index(acceptURL, "invite=")+len("invite="):]

	t.Run("초대 미리보기는 토큰만으로 볼 수 있다", func(t *testing.T) {
		out := h.post("/api/agent/invites/preview", "", map[string]any{"token": inviteToken})
		if out["email"] != newEmail {
			t.Errorf("이메일은 소문자로 저장된다: %v", out["email"])
		}
	})

	t.Run("짧은 비밀번호는 거절한다", func(t *testing.T) {
		res := h.postRaw("/api/agent/invites/accept", "", map[string]any{"token": inviteToken, "password": "short"})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})

	var newToken string
	t.Run("수락하면 계정이 생기고 바로 로그인된다", func(t *testing.T) {
		out := h.post("/api/agent/invites/accept", "", map[string]any{"token": inviteToken, "password": "welcome123"})
		newToken, _ = out["token"].(string)
		if newToken == "" {
			t.Fatalf("토큰이 없다: %+v", out)
		}
		me := h.get("/api/me", newToken)
		if me["name"] != "신입" || me["role"] != "STAFF" {
			t.Errorf("초대한 대로 들어와야 한다: %+v", me)
		}
	})

	t.Run("같은 초대는 두 번 쓸 수 없다", func(t *testing.T) {
		res := h.postRaw("/api/agent/invites/accept", "", map[string]any{"token": inviteToken, "password": "welcome123"})
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("404 여야 한다: %d", res.StatusCode)
		}
	})

	me := h.get("/api/me", newToken)
	newID, _ := me["agent_id"].(string)

	t.Run("역할을 바꿀 수 있다", func(t *testing.T) {
		h.post("/api/admin/agents/"+newID+"/role", admin, map[string]any{"role": "ADMIN"})
		if got := h.get("/api/me", newToken); got["role"] != "ADMIN" {
			t.Errorf("관리자가 돼야 한다: %v", got["role"])
		}
		h.post("/api/admin/agents/"+newID+"/role", admin, map[string]any{"role": "STAFF"})
	})

	t.Run("비활성화하면 열린 세션이 끊긴다", func(t *testing.T) {
		h.post("/api/admin/agents/"+newID+"/active", admin, map[string]any{"active": false})
		res := h.raw(http.MethodGet, "/api/me", newToken)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("내보낸 사람의 토큰이 살아 있다: %d", res.StatusCode)
		}
		login := h.postRaw("/api/agent/login", "", map[string]any{"email": newEmail, "password": "welcome123"})
		login.Body.Close()
		if login.StatusCode != http.StatusUnauthorized {
			t.Errorf("비활성 계정은 로그인도 막혀야 한다: %d", login.StatusCode)
		}
	})

	t.Run("마지막 관리자는 강등할 수 없다", func(t *testing.T) {
		adminMe := h.get("/api/me", admin)
		adminID, _ := adminMe["agent_id"].(string)
		res := h.postRaw("/api/admin/agents/"+adminID+"/role", admin, map[string]any{"role": "STAFF"})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("마지막 관리자 강등은 400 이어야 한다: %d", res.StatusCode)
		}
		if n, err := h.store.CountActiveAdmins(context.Background(), core.DefaultWorkspaceID); err != nil || n != 1 {
			t.Errorf("관리자가 그대로 한 명이어야 한다: n=%d err=%v", n, err)
		}
	})

	t.Run("관리자가 둘이면 강등된다", func(t *testing.T) {
		adminMe := h.get("/api/me", admin)
		adminID, _ := adminMe["agent_id"].(string)
		second := testsupport.Agent(t, h.store)
		h.post("/api/admin/agents/"+second.ID+"/role", admin, map[string]any{"role": "ADMIN"})
		res := h.postRaw("/api/admin/agents/"+second.ID+"/role", admin, map[string]any{"role": "STAFF"})
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Errorf("관리자가 더 있으면 강등은 된다: %d", res.StatusCode)
		}
		// 마지막 관리자의 비활성화는 HTTP 로는 자기 자신 거절이 먼저 걸린다. 저장소 단위 테스트가 그 보호를 덮는다.
		_ = adminID
	})

	t.Run("자기 자신은 비활성화할 수 없다", func(t *testing.T) {
		adminMe := h.get("/api/me", admin)
		adminID, _ := adminMe["agent_id"].(string)
		res := h.postRaw("/api/admin/agents/"+adminID+"/active", admin, map[string]any{"active": false})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})
}

func TestE2E_비밀번호와로그아웃(t *testing.T) {
	h := newHarness(t, time.Second, false)
	agentToken := h.agentToken()

	t.Run("현재 비밀번호가 틀리면 못 바꾼다", func(t *testing.T) {
		res := h.postRaw("/api/agent/password", agentToken, map[string]any{"current": "wrong", "next": "newpass123"})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("바꾸면 다른 세션이 끊기고 지금 세션은 남는다", func(t *testing.T) {
		me := h.get("/api/me", agentToken)
		email := h.agentEmail(me["agent_id"].(string))
		other := h.post("/api/agent/login", "", map[string]any{"email": email, "password": "chat1234"})
		otherToken, _ := other["token"].(string)

		h.post("/api/agent/password", agentToken, map[string]any{"current": "chat1234", "next": "newpass123"})

		res := h.raw(http.MethodGet, "/api/me", otherToken)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("다른 세션은 끊겨야 한다: %d", res.StatusCode)
		}
		keep := h.raw(http.MethodGet, "/api/me", agentToken)
		keep.Body.Close()
		if keep.StatusCode != http.StatusOK {
			t.Errorf("지금 세션은 남아야 한다: %d", keep.StatusCode)
		}
	})

	t.Run("로그아웃하면 서버에서 세션이 사라진다", func(t *testing.T) {
		res := h.postRaw("/api/logout", agentToken, nil)
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("204 여야 한다: %d", res.StatusCode)
		}
		after := h.raw(http.MethodGet, "/api/me", agentToken)
		after.Body.Close()
		if after.StatusCode != http.StatusUnauthorized {
			t.Errorf("로그아웃한 토큰이 살아 있다: %d", after.StatusCode)
		}
	})
}

func TestE2E_설정과자동응답(t *testing.T) {
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 30*time.Second)
	admin := h.adminToken()
	agentToken := h.agentToken()

	original := h.get("/api/admin/settings", admin)
	t.Cleanup(func() {
		// 설정은 DB 전역이다. 다른 테스트가 보는 값으로 되돌린다.
		h.put("/api/admin/settings", admin, original)
	})

	t.Run("일반 상담원은 설정을 못 바꾼다", func(t *testing.T) {
		res := h.putRaw("/api/admin/settings", agentToken, original)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("틀린 설정은 통째로 거절한다", func(t *testing.T) {
		bad := clone(original)
		bad["widget"].(map[string]any)["color"] = "blue"
		res := h.putRaw("/api/admin/settings", admin, bad)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("위젯 이름이 공백뿐이면 빈 이름으로 저장하고 위젯은 기본 이름 고객센터를 쓴다 — 앞뒤 공백은 떼고 저장한다", func(t *testing.T) {
		titled := func(title string) map[string]any {
			in := clone(original)
			in["widget"].(map[string]any)["title"] = title
			return in
		}
		h.put("/api/admin/settings", admin, titled(" \t\u3000 "))
		if got := h.get("/api/widget/settings", "")["title"]; got != "" {
			t.Errorf("공백뿐인 이름은 빈 이름이어야 한다: %q", got)
		}
		h.put("/api/admin/settings", admin, titled("  둠둠디라 상담  "))
		if got := h.get("/api/widget/settings", "")["title"]; got != "둠둠디라 상담" {
			t.Errorf("앞뒤 공백을 떼야 한다: %q", got)
		}
		// 40자 상한은 뗀 뒤의 글자로 센다
		h.put("/api/admin/settings", admin, titled("   "+strings.Repeat("가", 40)+"   "))
		h.put("/api/admin/settings", admin, original)
	})

	t.Run("공개 위젯 설정에는 색·제목·운영 여부만 있다", func(t *testing.T) {
		pub := h.get("/api/widget/settings", "")
		for _, key := range []string{"business_hours", "auto_messages"} {
			if _, leaked := pub[key]; leaked {
				t.Errorf("공개 설정에 %q 가 들어 있다", key)
			}
		}
		if _, ok := pub["open_now"]; !ok {
			t.Error("운영 여부가 있어야 위젯이 안내를 띄운다")
		}
	})

	t.Run("운영시간 안에는 접수 안내가, 밖에는 부재중 안내가 한 번 나간다", func(t *testing.T) {
		on := clone(original)
		on["auto_messages"] = map[string]any{"welcome": "환영", "first_reply": "접수됐습니다", "away": "운영시간이 아닙니다"}
		// 운영시간을 "항상" 으로 둔다.
		on["business_hours"] = map[string]any{"timezone": "Asia/Seoul", "days": map[string]any{}}
		h.put("/api/admin/settings", admin, on)

		custToken, convID := h.customerSession()
		cust := h.dial(ctx, custToken)
		ready := cust.subscribe(ctx, convID, 0)
		cust.collectSeqs(ctx, int(ready.LastSeq))

		cust.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": "안녕하세요"})
		bot := cust.await(ctx, func(f frame) bool { return f.Type == "message" && f.SenderKind == "BOT" })
		if bot.Body != "접수됐습니다" {
			t.Errorf("운영시간 안에는 접수 안내여야 한다: %q", bot.Body)
		}

		// 두 번째 메시지에는 안 나간다.
		cust.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": "또 보냅니다"})
		cust.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": "보초"})
		bots := 0
		cust.await(ctx, func(f frame) bool {
			if f.Type == "message" && f.SenderKind == "BOT" {
				bots++
			}
			return f.Type == "message" && f.Body == "보초"
		})
		if bots != 0 {
			t.Errorf("접수 안내는 대화당 한 번이다: %d번 더 나갔다", bots)
		}

		// 운영시간을 "절대 안 열림" 으로 바꾸고 새 대화를 연다.
		off := clone(on)
		off["business_hours"] = map[string]any{"timezone": "Asia/Seoul", "days": map[string]any{
			"mon": []any{map[string]any{"from": "00:00", "to": "00:01"}}}}
		h.put("/api/admin/settings", admin, off)

		_, convID2 := h.customerSessionWith(custToken)
		cust2 := h.dial(ctx, custToken)
		ready2 := cust2.subscribe(ctx, convID2, 0)
		cust2.collectSeqs(ctx, int(ready2.LastSeq))
		cust2.send(ctx, map[string]any{"type": "send", "conversation_id": convID2, "client_msg_id": id.New(), "body": "밤에 보냅니다"})
		away := cust2.await(ctx, func(f frame) bool { return f.Type == "message" && f.SenderKind == "BOT" })
		if away.Body != "운영시간이 아닙니다" {
			t.Errorf("운영시간 밖에는 부재중 안내여야 한다: %q", away.Body)
		}
	})

	t.Run("상담원이 먼저 답한 대화에는 접수 안내가 안 나간다", func(t *testing.T) {
		custToken, convID := h.customerSession()
		h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{"client_msg_id": id.New(), "body": "먼저 인사드립니다"})
		h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{"client_msg_id": id.New(), "body": "네 안녕하세요"})
		msgs := h.get("/api/conversations/"+convID+"/messages?after_seq=0", custToken)
		for _, raw := range firstList(msgs, "messages") {
			m, _ := raw.(map[string]any)
			if m["sender_kind"] == "BOT" && m["body"] == "접수됐습니다" {
				t.Error("사람이 이미 답했는데 자동 응답이 끼어들었다")
			}
		}
	})
}

func TestE2E_회원연동(t *testing.T) {
	h := newHarness(t, time.Second, false)
	secret := []byte("test-member-secret")

	t.Run("서명된 토큰으로 들어오면 회원으로 식별된다", func(t *testing.T) {
		tok, _ := app.SignMember(secret, app.MemberClaims{MemberID: "shop-77", Name: "김회원", Email: "kim@shop.com", Exp: time.Now().Add(time.Hour).Unix()})
		out := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
		customer, _ := out["customer"].(map[string]any)
		if customer["display_name"] != "김회원" {
			t.Errorf("호스트가 준 이름이어야 한다: %+v", customer)
		}
		token, _ := out["token"].(string)

		// 다른 브라우저(새 토큰)로 같은 회원이 들어오면 같은 고객이다.
		again := h.post("/api/customer/member-session", "", map[string]any{"member_token": tok})
		c2, _ := again["customer"].(map[string]any)
		if c2["id"] != customer["id"] {
			t.Error("같은 회원은 같은 고객이어야 대화 이력이 따라온다")
		}
		_ = token
	})

	t.Run("위조 토큰은 거절한다", func(t *testing.T) {
		forged, _ := app.SignMember([]byte("wrong"), app.MemberClaims{MemberID: "admin", Exp: time.Now().Add(time.Hour).Unix()})
		res := h.postRaw("/api/customer/member-session", "", map[string]any{"member_token": forged})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("400 이어야 한다: %d", res.StatusCode)
		}
	})
}

func clone(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = clone(sub)
		} else {
			out[k] = v
		}
	}
	return out
}
