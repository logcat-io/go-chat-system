// Package ollama 는 상담 답변 초안을 만든다.
//
// 원칙은 하나다 — 판정은 코드, 모델은 표현. 모델은 상담원 작성칸에 글을 채워 줄 뿐이고,
// 고객에게 나가는 건 상담원이 검토하고 보내기를 누른 뒤다. 초안은 DB 에 남지 않는다.
// 모델이 환불을 약속하거나 없는 정책을 지어내도, 사람이 보기 전에는 아무 효력이 없다.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"

	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// Ollama 는 로컬 Ollama 의 /api/chat 을 부른다.
type Ollama struct {
	BaseURL string
	Model   string
	Client  *http.Client
}

func New(baseURL, model string) *Ollama {
	return &Ollama{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		Client:  &http.Client{Timeout: 45 * time.Second, Transport: telemetry.Transport("ollama", nil)},
	}
}

func (o *Ollama) Name() string { return o.Model }

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaResponse struct {
	Message ollamaMessage `json:"message"`
	Error   string        `json:"error"`
}

const systemPrompt = `너는 고객센터 상담원을 돕는 보조자다. 상담원이 그대로 보내거나 고쳐 쓸 답변 초안을 한국어로 쓴다.
규칙:
- 존댓말을 쓴다. 세 문장 안에서 끝낸다.
- 대화에 없는 사실(주문 상태, 환불 가능 여부, 배송 일정)을 지어내지 않는다. 모르면 "확인해서 안내드리겠습니다" 로 쓴다.
- 환불·보상·기한을 약속하지 않는다. 그건 상담원이 정한다.
- 인사말이나 서명은 붙이지 않는다. 본문만 쓴다.
- [내부 메모] 는 상담원끼리 적은 것이다. 참고만 하고 고객에게 그 내용을 드러내지 않는다.`

// Draft 는 모델을 한 번 부른다. 실패하면 실패라고 말한다. 조용히 빈 문자열을 주지 않는다.
func (o *Ollama) Draft(ctx context.Context, req app.DraftRequest) (string, error) {
	messages := []ollamaMessage{{Role: "system", Content: systemPrompt}}
	if style := strings.TrimSpace(req.Style); style != "" {
		messages = append(messages, ollamaMessage{Role: "system", Content: "추가 지시: " + style})
	}
	for _, t := range req.Turns {
		messages = append(messages, toOllama(t, req.CustomerName))
	}
	messages = append(messages, ollamaMessage{
		Role:    "user",
		Content: fmt.Sprintf("[지시] 위 대화에 이어 상담원 %s 이(가) 보낼 다음 답변 초안을 써라.", req.AgentName),
	})

	body, err := json.Marshal(ollamaRequest{
		Model: o.Model, Messages: messages, Stream: false,
		Options: map[string]any{"temperature": 0.3, "num_predict": 300},
	})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := o.Client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama: 모델 서버에 닿지 못했다: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode >= 400 {
		return "", core.NewError("ollama_http_"+strconv.Itoa(res.StatusCode), fmt.Sprintf("ollama: 모델 서버가 HTTP %d 로 답했다: %s", res.StatusCode, truncate(string(raw), 200)))
	}
	var out ollamaResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", core.WrapError(err, "ollama_bad_response", "ollama: 모델 서버의 응답을 JSON 으로 읽을 수 없다: "+err.Error())
	}
	if out.Error != "" {
		return "", core.NewError("ollama_error", "ollama: 모델 서버가 오류를 돌려줬다: "+out.Error)
	}
	text := strings.TrimSpace(out.Message.Content)
	if text == "" {
		return "", core.NewError("ollama_empty", "ollama: 모델이 빈 답을 돌려줬다")
	}
	return text, nil
}

// toOllama 는 대화 줄을 모델의 역할로 옮긴다.
// 고객이 user, 상담원이 assistant 다. 내부 메모는 표식을 달아 user 쪽에 넣는다 —
// assistant 로 넣으면 모델이 "내가 한 말" 로 착각해 그 어조를 따라간다.
func toOllama(t app.DraftTurn, customerName string) ollamaMessage {
	switch t.Role {
	case "agent":
		return ollamaMessage{Role: "assistant", Content: t.Body}
	case "note":
		return ollamaMessage{Role: "user", Content: "[내부 메모 · " + t.Speaker + "] " + t.Body}
	default:
		name := t.Speaker
		if name == "" {
			name = customerName
		}
		return ollamaMessage{Role: "user", Content: "[고객 " + name + "] " + t.Body}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
