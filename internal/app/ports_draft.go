package app

import (
	"context"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// DraftProvider 는 초안을 만드는 바깥 모델이다. 판정은 코드, 모델은 표현 — 그래서 포트가 문자열만 돌려준다.
type DraftProvider interface {
	Draft(ctx context.Context, req DraftRequest) (string, error)
	Name() string
}

// DraftTurn 은 모델에 보여 주는 대화 한 줄이다. 역할은 우리 어휘(customer·agent·note)이고, 모델의 어휘로 옮기는 건 어댑터의 일이다.
type DraftTurn struct {
	Role    string
	Speaker string
	Body    string
}

type DraftRequest struct {
	CustomerName string
	AgentName    string
	Turns        []DraftTurn
	// Style 은 설정에서 온 한 줄 지시다. 예: "존댓말, 세 문장 이내, 환불 약속 금지".
	Style string
}

// ErrDraftDisabled 는 모델이 설정돼 있지 않다는 뜻이다.
var ErrDraftDisabled = core.NewError("draft_disabled", "app: 초안 모델이 설정되지 않았다(CHAT_OLLAMA_URL 이 비어 있다)")

// noDraft 는 설정이 없을 때 쓴다. 호출자가 nil 검사를 하지 않아도 된다.
type noDraft struct{}

func (noDraft) Draft(context.Context, DraftRequest) (string, error) { return "", ErrDraftDisabled }
func (noDraft) Name() string                                        { return "" }
