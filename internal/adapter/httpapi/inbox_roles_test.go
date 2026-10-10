package httpapi_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/id"
)

// 받은함의 권한 경계. 받은함 검색과 삭제한 상담 보기는 다른 기능의 뒷문이 되기 쉬운 자리다.

func (h *harness) inboxIDs(token, query string) []string {
	h.t.Helper()
	return listIDs(firstList(h.get("/api/agent/conversations?"+query, token), "conversations"))
}

func TestE2E_받은함검색은권한만큼만찾는다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("받은함 검색 상점")
	suffix := strings.ReplaceAll(id.New(), "-", "")[22:]
	name, word := "검색고객"+suffix, "본문단어"+suffix
	cust, convID := h.namedCustomer(tm.tn, name)
	h.send(convID, cust, "주문번호는 "+word+" 입니다", false)

	t.Run("전체 검색 권한이 없는 아르바이트는 본문에만 있는 단어로 찾으면 0건이다", func(t *testing.T) {
		if ids := h.inboxIDs(tm.partTime.token, "q="+url.QueryEscape(word)); len(ids) != 0 {
			t.Errorf("본문으로 찾아졌다: %v", ids)
		}
	})

	t.Run("아르바이트도 고객 이름으로는 찾는다", func(t *testing.T) {
		if ids := h.inboxIDs(tm.partTime.token, "q="+url.QueryEscape(name)); !slices.Contains(ids, convID) {
			t.Errorf("이름으로 안 찾아진다: %v", ids)
		}
	})

	t.Run("직원은 본문으로도 찾는다", func(t *testing.T) {
		if ids := h.inboxIDs(tm.staff.token, "q="+url.QueryEscape(word)); !slices.Contains(ids, convID) {
			t.Errorf("직원이 본문으로 못 찾는다: %v", ids)
		}
	})
}

func TestE2E_삭제한상담보기(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tm := h.teamOf("삭제 보기 상점")
	cust, deleted := h.namedCustomer(tm.tn, "지운고객")
	h.send(deleted, cust, "지운 상담의 본문", false)
	_, live := h.namedCustomer(tm.tn, "산고객")
	const reason = "같은 문의가 두 번 열렸다"
	if code, out := h.call(http.MethodDelete, "/api/agent/conversations/"+deleted, tm.admin.token, map[string]any{"reason": reason}); code >= 300 {
		t.Fatalf("삭제: %d %v", code, out)
	}

	t.Run("deleted=1 받은함은 삭제 권한이 있으면 삭제한 상담만, 없으면 403 이다", func(t *testing.T) {
		ids := h.inboxIDs(tm.admin.token, "deleted=1")
		if !slices.Contains(ids, deleted) || slices.Contains(ids, live) {
			t.Errorf("삭제한 상담만 나와야 한다: %v", ids)
		}
		if ids := h.inboxIDs(tm.admin.token, ""); slices.Contains(ids, deleted) {
			t.Errorf("보통 받은함에 삭제한 상담이 있다")
		}
		for who, token := range map[string]string{"직원": tm.staff.token, "아르바이트": tm.partTime.token} {
			if code, body := h.call(http.MethodGet, "/api/agent/conversations?deleted=1", token, nil); code != http.StatusForbidden || errorCodeOf(body) != "forbidden" {
				t.Errorf("%s: %d %v", who, code, body)
			}
		}
	})

	t.Run("삭제 권한이 있으면 삭제한 상담을 읽기 전용으로 열고 삭제 사유·사람·시각을 본다", func(t *testing.T) {
		code, out := h.call(http.MethodGet, "/api/agent/conversations/"+deleted+"/deleted", tm.admin.token, nil)
		if code != http.StatusOK {
			t.Fatalf("열기: %d %v", code, out)
		}
		d, _ := out["deletion"].(map[string]any)
		if d["reason"] != reason || d["deleted_by_name"] == "" || d["deleted_at"] == nil {
			t.Errorf("삭제 기록: %v", d)
		}
		found := false
		for _, raw := range firstList(out, "messages") {
			if m, _ := raw.(map[string]any); m["body"] == "지운 상담의 본문" {
				found = true
			}
		}
		if !found {
			t.Errorf("메시지가 없다: %v", out["messages"])
		}
	})

	t.Run("삭제 권한이 없으면 열 수 없고, 다른 모든 경로는 그대로 404 다", func(t *testing.T) {
		if code, _ := h.call(http.MethodGet, "/api/agent/conversations/"+deleted+"/deleted", tm.staff.token, nil); code != http.StatusForbidden {
			t.Errorf("직원이 열었다: %d", code)
		}
		if code, _ := h.call(http.MethodGet, "/api/agent/conversations/"+deleted+"/deleted", cust, nil); code < 400 {
			t.Errorf("고객이 열었다: %d", code)
		}
		for _, path := range []string{"/api/conversations/" + deleted, "/api/conversations/" + deleted + "/messages?after_seq=0"} {
			if code, _ := h.call(http.MethodGet, path, tm.admin.token, nil); code != http.StatusNotFound {
				t.Errorf("관리자 %s: %d", path, code)
			}
		}
	})

	t.Run("삭제하지 않은 상담은 이 길로 열리지 않는다", func(t *testing.T) {
		if code, _ := h.call(http.MethodGet, "/api/agent/conversations/"+live+"/deleted", tm.admin.token, nil); code != http.StatusNotFound {
			t.Errorf("산 상담이 삭제 보기로 열렸다: %d", code)
		}
	})

	t.Run("되살리면 삭제됨 목록에서 빠지고 보통 받은함에 돌아온다", func(t *testing.T) {
		h.post("/api/agent/conversations/"+deleted+"/restore", tm.admin.token, nil)
		if ids := h.inboxIDs(tm.admin.token, "deleted=1"); slices.Contains(ids, deleted) {
			t.Errorf("되살린 상담이 삭제됨에 남았다")
		}
		if ids := h.inboxIDs(tm.admin.token, ""); !slices.Contains(ids, deleted) {
			t.Errorf("되살린 상담이 받은함에 없다")
		}
	})
}
