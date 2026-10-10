package httpapi

import (
	"net/http"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
	"github.com/logcat-io/go-chat-system/internal/id"
)

func (a *API) customerConversations(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	convs, err := a.qry.CustomerConversations(r.Context(), actor, intQuery(r, "limit", core.CustomerConversationsPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationsResponse{Conversations: app.ForActorList(actor, convs)})
}

func (a *API) startConversation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	conv, err := a.cmd.StartConversation(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, conversationResponse{Conversation: app.ForActor(actor, conv)})
}

// closeByCustomer 는 고객이 자기 상담을 직접 닫는다(D48). 이미 닫혀 있으면 같은 결과로 200 이다(멱등).
// 상태 변경은 대화 프레임으로 데스크와 그 고객의 다른 탭에 간다(상담원의 상태 변경과 같은 알림 길).
func (a *API) closeByCustomer(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	conv, err := a.cmd.CloseByCustomer(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationResponse{Conversation: app.ForActor(actor, conv)})
}

func (a *API) conversation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	conv, err := a.qry.Authorize(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app.ForActor(actor, conv))
}

func (a *API) messages(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	// before_seq 가 있으면 "이전 대화 더 보기" 다. 그 앞 페이지를 오래된 순으로 준다.
	if before := int64Query(r, "before_seq"); before > 0 {
		msgs, err := a.qry.HistoryBefore(r.Context(), actor, r.PathValue("id"), before, intQuery(r, "limit", core.MessagesBeforePageDefault))
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, messagesResponse{Messages: msgs})
		return
	}
	msgs, err := a.qry.History(r.Context(), actor, r.PathValue("id"),
		int64Query(r, "after_seq"), intQuery(r, "limit", core.MessagesPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messagesResponse{Messages: msgs})
}

type sendRequest struct {
	ClientMsgID  string `json:"client_msg_id"`
	Body         string `json:"body"`
	AttachmentID string `json:"attachment_id"`
	Internal     bool   `json:"internal"`
}

// sendMessage 는 WebSocket 이 끊긴 동안의 대체 경로다. 멱등이라 WS 재전송과 겹쳐도 안전하다.
func (a *API) sendMessage(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req sendRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if req.ClientMsgID == "" {
		req.ClientMsgID = id.New()
	}
	res, err := a.cmd.Send(r.Context(), actor, app.SendParams{
		ConversationID: r.PathValue("id"),
		ClientMsgID:    req.ClientMsgID,
		Body:           req.Body,
		AttachmentID:   req.AttachmentID,
		Internal:       req.Internal,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	// 재전송이면 새로 만든 게 아니라 처음 저장한 그 메시지를 돌려준다.
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, sendResponse{Message: res.Message, Created: res.Created})
}

func (a *API) reads(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	states, err := a.qry.ReadStates(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, readsResponse{Reads: app.ReadViews(states, actor.Internal())})
}

type readRequest struct {
	Seq int64 `json:"seq"`
}

// markRead 는 WebSocket 이 끊긴 동안의 읽음 보고 경로다. 단조 증가라 몇 번 보내도 같다.
func (a *API) markRead(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req readRequest
	if !decodeOr(w, r, &req) {
		return
	}
	stored, err := a.cmd.MarkRead(r.Context(), actor, r.PathValue("id"), req.Seq)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, readResponse{LastReadSeq: stored})
}

func (a *API) customer(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	c, err := a.qry.Customer(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app.CustomerForActor(actor, c))
}

type customerRequest struct {
	DisplayName string         `json:"display_name"`
	Email       string         `json:"email"`
	Phone       string         `json:"phone"`
	Note        string         `json:"note"`
	Attributes  map[string]any `json:"attributes"`
}

func (a *API) updateCustomer(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req customerRequest
	if !decodeOr(w, r, &req) {
		return
	}
	c, err := a.cmd.UpdateCustomer(r.Context(), actor, r.PathValue("id"), toProfile(req))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app.CustomerForActor(actor, c))
}

type editRequest struct {
	Body string `json:"body"`
}

// editMessage 는 자기 메시지의 본문을 바꾼다. 응답은 로그에 새로 들어간 EDIT 행이다.
func (a *API) editMessage(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req editRequest
	if !decodeOr(w, r, &req) {
		return
	}
	res, err := a.cmd.Edit(r.Context(), actor, r.PathValue("id"), r.PathValue("msgId"), req.Body)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sendResponse{Message: res.Message, Created: res.Created})
}

func (a *API) deleteMessage(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	res, err := a.cmd.Delete(r.Context(), actor, r.PathValue("id"), r.PathValue("msgId"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sendResponse{Message: res.Message, Created: res.Created})
}

type revisionsResponse struct {
	Revisions []core.Revision `json:"revisions"`
}

// priorHistory 는 이 고객의 이전 상담을 이어서 준다. before_conversation·before_seq 는 지난 응답의 첫 줄이다.
func (a *API) priorHistory(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	before := core.PriorCursor{ConversationID: r.URL.Query().Get("before_conversation"), Seq: int64Query(r, "before_seq")}
	msgs, err := a.qry.PriorHistory(r.Context(), actor, r.PathValue("id"), before, intQuery(r, "limit", core.PriorHistoryPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messagesResponse{Messages: msgs})
}

func (a *API) revisions(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	revs, err := a.qry.Revisions(r.Context(), actor, r.PathValue("id"), r.PathValue("msgId"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisionsResponse{Revisions: revs})
}
