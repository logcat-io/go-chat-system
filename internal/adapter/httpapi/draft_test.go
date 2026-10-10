package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/adapter/ollama"
	"github.com/logcat-io/go-chat-system/internal/id"
)

// 초안의 정책은 "모델은 작성칸만 채운다" 다. 저장도 전송도 하지 않는다.
// 모델 자체는 가짜 Ollama 로 대신한다. 진짜 모델이 뭐라고 답하든 여기서 보는 건 경계다.
func TestE2E_AI초안(t *testing.T) {
	// 가짜 Ollama: 받은 메시지를 그대로 기록하고 정해진 답을 준다.
	var received []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string           `json:"model"`
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = req.Messages
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": "확인해서 안내드리겠습니다. 송장번호를 알려주시겠어요?"},
		})
	}))
	t.Cleanup(fake.Close)

	h := newHarness(t, time.Second, false)
	h.svc.Queries.SetDraftProvider(ollama.New(fake.URL, "fake-model"))

	custToken, convID := h.customerSession()
	agentToken := h.agentToken()

	t.Run("켜져 있으면 /api/me 가 모델 이름을 준다 — 화면이 이걸로 버튼을 켠다", func(t *testing.T) {
		me := h.get("/api/me", agentToken)
		if me["draft_model"] != "fake-model" {
			t.Errorf("draft_model 이 없다: %+v", me)
		}
	})
	h.post("/api/conversations/"+convID+"/messages", custToken, map[string]any{"client_msg_id": id.New(), "body": "주문이 아직 안 왔어요"})
	h.post("/api/conversations/"+convID+"/messages", agentToken, map[string]any{"client_msg_id": id.New(), "body": "내부: 재고 확인 필요", "internal": true})

	before := h.get("/api/conversations/"+convID, agentToken)

	t.Run("초안을 돌려주되 저장하지 않는다", func(t *testing.T) {
		out := h.post("/api/agent/conversations/"+convID+"/draft", agentToken, nil)
		if !strings.Contains(out["draft"].(string), "송장번호") {
			t.Errorf("모델 답이 그대로 와야 한다: %v", out["draft"])
		}
		if out["model"] != "fake-model" {
			t.Errorf("어느 모델인지 알려야 한다: %v", out["model"])
		}
		after := h.get("/api/conversations/"+convID, agentToken)
		if after["last_seq"] != before["last_seq"] {
			t.Error("초안이 메시지로 저장됐다. 초안은 작성칸에만 있어야 한다")
		}
	})

	t.Run("모델에는 고객 말·상담원 말·내부 메모가 역할을 달고 간다", func(t *testing.T) {
		var sawCustomer, sawNote bool
		for _, m := range received {
			content, _ := m["content"].(string)
			if strings.Contains(content, "[고객") && strings.Contains(content, "주문이 아직") {
				sawCustomer = true
			}
			if strings.Contains(content, "[내부 메모") && m["role"] == "user" {
				sawNote = true
			}
		}
		if !sawCustomer || !sawNote {
			t.Errorf("대화 맥락이 빠졌다: customer=%v note=%v", sawCustomer, sawNote)
		}
	})

	t.Run("고객은 초안을 못 부른다", func(t *testing.T) {
		res := h.postRaw("/api/agent/conversations/"+convID+"/draft", custToken, nil)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("403 이어야 한다: %d", res.StatusCode)
		}
	})

	t.Run("모델이 죽으면 실패를 말한다", func(t *testing.T) {
		fake.Close()
		res := h.postRaw("/api/agent/conversations/"+convID+"/draft", agentToken, nil)
		res.Body.Close()
		if res.StatusCode != http.StatusBadGateway {
			t.Errorf("모델 장애는 502 로 말한다. 조용히 빈 초안을 주면 안 된다: %d", res.StatusCode)
		}
	})
}

func TestE2E_AI초안꺼짐(t *testing.T) {
	h := newHarness(t, time.Second, false)
	_, convID := h.customerSession()
	agentToken := h.agentToken()

	res := h.postRaw("/api/agent/conversations/"+convID+"/draft", agentToken, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("설정이 없으면 503 으로 이유를 말해야 한다: %d", res.StatusCode)
	}
	me := h.get("/api/me", agentToken)
	if _, ok := me["draft_model"]; ok {
		t.Error("꺼져 있으면 화면이 버튼을 숨길 수 있게 모델 이름이 없어야 한다")
	}
}

// 상담이 길어져도 모델은 가장 최근 20줄을 시간순으로 본다. 처음 몇백 줄이 아니다.
// 메시지 한 쪽(200개)을 넘겨야 드러난다 — 전에는 처음 200줄을 읽고 그 끝 20줄을 맥락으로 썼다.
func TestE2E_AI초안은메시지가200개를넘어도가장최근20줄을시간순으로맥락에쓴다(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		received = req.Messages
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": "초안"}})
	}))
	t.Cleanup(fake.Close)

	h := newHarness(t, time.Second, false)
	h.svc.Queries.SetDraftProvider(ollama.New(fake.URL, "fake-model"))
	_, convID := h.customerSession()
	agentToken := h.agentToken()

	const total = 230
	for i := 0; i < total; i++ {
		h.send(convID, agentToken, fmt.Sprintf("줄 %03d", i), false)
	}
	h.post("/api/agent/conversations/"+convID+"/draft", agentToken, nil)

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, m := range received {
		if content, _ := m["content"].(string); strings.HasPrefix(content, "줄 ") {
			got = append(got, content)
		}
	}
	var want []string
	for i := total - 20; i < total; i++ {
		want = append(want, fmt.Sprintf("줄 %03d", i))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("맥락은 가장 최근 20줄이 시간순이어야 한다\n got: %v\nwant: %v", got, want)
	}
}
