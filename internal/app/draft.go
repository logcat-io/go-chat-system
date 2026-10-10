package app

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// 답변 초안. 모델이 쓴 글이 작성칸에 들어가고, 보내는 건 사람이다.

const draftContextTurns = 20

// ErrDraftFailed 는 모델 호출이 실패한 것이다. 서버 잘못(500)이 아니라 뒤에 선 모델의 잘못이다.
var ErrDraftFailed = core.NewError("draft_failed", "app: 초안 모델이 답하지 못했다(연결 실패·시한 초과·오류 응답)")

// SetDraftProvider 는 서버가 설정에 따라 꽂는다. 안 꽂으면 꺼진 상태다.
func (s *Queries) SetDraftProvider(p DraftProvider) {
	if p == nil {
		p = noDraft{}
	}
	s.drafts = p
}

// DraftModel 은 켜져 있을 때 모델 이름, 꺼져 있으면 빈 문자열이다. 화면이 이걸로 버튼을 켠다.
func (s *Queries) DraftModel() string {
	if !s.DraftEnabled() {
		return ""
	}
	return s.drafts.Name()
}

func (s *Queries) DraftEnabled() bool {
	_, disabled := s.drafts.(noDraft)
	return s.drafts != nil && !disabled
}

// DraftResult 는 초안과 그걸 만든 모델 이름이다. 화면이 "AI 초안" 임을 표시할 수 있게 한다.
type DraftResult struct {
	Draft string `json:"draft"`
	Model string `json:"model"`
}

// Draft 는 상담원만 부른다. 대화의 최근 줄을 모델에 보여 주고 초안을 받는다.
// DB 에 아무것도 쓰지 않는다. 초안이 고객에게 가려면 상담원이 Send 를 눌러야 한다.
func (s *Queries) Draft(ctx context.Context, actor Actor, convID string) (DraftResult, error) {
	if err := require(ctx, actor, PermConversationWork); err != nil {
		return DraftResult{}, err
	}
	// 권한을 먼저 본다. 꺼짐(503)을 먼저 답하면 경로마다 남의 대화에 대한 답이 달라진다.
	conv, err := s.Authorize(ctx, actor, convID)
	if err != nil {
		return DraftResult{}, err
	}
	if !s.DraftEnabled() {
		return DraftResult{}, ErrDraftDisabled
	}
	// 가장 최근 draftContextTurns 줄을 시간순으로 받는다. 앞에서부터 한 쪽을 읽고 끝을 자르면
	// 한 쪽(core.MessagesPageDefault)보다 긴 상담에서 최근이 아니라 처음 쪽의 끝을 보게 된다.
	msgs, err := s.read.ListMessagesBefore(ctx, conv.ID, math.MaxInt64, draftContextTurns, true)
	if err != nil {
		return DraftResult{}, translate(err)
	}

	turns := make([]DraftTurn, 0, len(msgs))
	for _, m := range msgs {
		body := strings.TrimSpace(m.Body)
		if body == "" {
			if m.Attachment != nil {
				body = "(파일 첨부: " + m.Attachment.Filename + ")"
			} else {
				continue
			}
		}
		switch {
		case m.Visibility == core.Private:
			turns = append(turns, DraftTurn{Role: "note", Speaker: m.SenderName, Body: body})
		case m.SenderKind == core.SenderAgent || m.SenderKind == core.SenderBot || m.SenderKind == core.SenderAI:
			turns = append(turns, DraftTurn{Role: "agent", Speaker: m.SenderName, Body: body})
		case m.SenderKind == core.SenderCustomer:
			turns = append(turns, DraftTurn{Role: "customer", Speaker: m.SenderName, Body: body})
		}
	}
	if len(turns) == 0 {
		return DraftResult{}, invalid("draft_no_conversation", "초안을 만들 대화가 아직 없습니다")
	}

	settings, err := s.settings(ctx, conv.WorkspaceID)
	if err != nil {
		return DraftResult{}, err
	}
	text, err := s.drafts.Draft(ctx, DraftRequest{
		CustomerName: conv.CustomerName,
		AgentName:    actor.Name,
		Turns:        turns,
		Style:        settings.Messages.DraftStyle,
	})
	if errors.Is(err, ErrDraftDisabled) {
		return DraftResult{}, ErrDraftDisabled
	}
	if err != nil {
		// 바깥에는 ErrDraftFailed 하나로만 가른다. 안쪽 오류(Ollama 의 시한·연결)까지 감싸면 errors.Is 가 context.Canceled 등과도 맞아
		// 응답 분기(httpapi)가 검사 순서에 기대게 된다. 그래서 안쪽 사슬은 잇지 않고, 안쪽의 종류(ollama_http_503·timeout·conn_refused …)만
		// 고정 코드로 붙인다(D47). 로그의 err_kind 가 그 코드다. 원문은 개발자용 문구에만 남고 로그에는 실리지 않는다.
		return DraftResult{}, core.WrapError(ErrDraftFailed, logctx.ErrKind(err), "app: 초안 모델 호출이 실패했다: "+err.Error())
	}
	return DraftResult{Draft: text, Model: s.drafts.Name()}, nil
}
