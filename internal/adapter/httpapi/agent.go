package httpapi

import (
	"github.com/logcat-io/go-chat-system/internal/id"
	"net/http"
	"time"

	"github.com/logcat-io/go-chat-system/internal/app"
	"github.com/logcat-io/go-chat-system/internal/core"
)

// 상담원 데스크가 쓰는 엔드포인트. 전부 app 계층에서 상담원 여부를 확인한다.

func (a *API) inbox(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	convs, err := a.qry.Inbox(r.Context(), actor, inboxFilter(r, actor))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conversationsResponse{Conversations: app.ForActorList(actor, convs)})
}

// inboxFilter 는 쿼리 문자열을 받은함 조건으로 옮긴다.
// assignee 는 "me"·"none"·상담원 id 세 가지를 받는다.
func inboxFilter(r *http.Request, actor app.Actor) core.InboxFilter {
	q := r.URL.Query()
	f := core.InboxFilter{
		State:       core.State(q.Get("state")),
		TagID:       q.Get("tag_id"),
		Query:       q.Get("q"),
		OnlyWaiting: q.Get("waiting") == "1" || q.Get("unread") == "1", // unread 는 옛 이름이다
		Limit:       intQuery(r, "limit", core.InboxPageDefault),
	}
	switch assignee := q.Get("assignee"); assignee {
	case "":
	case "me":
		f.AssigneeID = actor.AgentID
	case "none":
		f.Unassigned = true
	default:
		f.AssigneeID = assignee
	}
	if q.Get("following") == "1" {
		f.FollowedBy = actor.AgentID
	}
	f.Deleted = q.Get("deleted") == "1"
	return f
}

func (a *API) inboxCounts(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	counts, err := a.qry.InboxCounts(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

type stateRequest struct {
	State         string `json:"state"`
	SnoozeMinutes int    `json:"snooze_minutes"`
}

func (a *API) setState(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req stateRequest
	if !decodeOr(w, r, &req) {
		return
	}
	state := core.State(req.State)
	var until *time.Time
	if state == core.StateSnoozed {
		minutes := req.SnoozeMinutes
		if minutes <= 0 {
			minutes = 60
		}
		t := time.Now().Add(time.Duration(minutes) * time.Minute)
		until = &t
	}
	conv, err := a.cmd.SetState(r.Context(), actor, r.PathValue("id"), state, until)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

type deleteConversationRequest struct {
	Reason string `json:"reason"`
}

// deleteConversation 은 상담을 가린다. 기록은 남고 조회 경로마다 사라진다.
func (a *API) deleteConversation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req deleteConversationRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if err := a.cmd.DeleteConversation(r.Context(), actor, r.PathValue("id"), req.Reason); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deletedConversationResponse struct {
	Conversation any            `json:"conversation"`
	Deletion     core.Deletion  `json:"deletion"`
	Messages     []core.Message `json:"messages"`
}

// deletedConversation 은 가린 상담을 읽기 전용으로 보여 준다. 되살리기 화면이 쓴다.
func (a *API) deletedConversation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	conv, deletion, msgs, err := a.qry.DeletedConversation(r.Context(), actor, r.PathValue("id"),
		int64Query(r, "after_seq"), intQuery(r, "limit", core.MessagesPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deletedConversationResponse{
		Conversation: app.ForActor(actor, conv), Deletion: deletion, Messages: msgs,
	})
}

func (a *API) restoreConversation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	conv, err := a.cmd.RestoreConversation(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

type assignRequest struct {
	AgentID string `json:"agent_id"`
	// ExpectedAssignee 는 화면이 보고 있던 담당자다. 없으면(null 포함) 조건 없이 바꾼다(옛 화면 호환). 빈 문자열은 미배정을 봤다는 뜻이다.
	// 그 사이 담당자가 바뀌었으면 409 conflict 다(D39). "me" 는 자기 자신이다.
	ExpectedAssignee *string `json:"expected_assignee"`
}

// assign 은 지정·이관·해제를 한 경로로 받는다. agent_id 가 비면 해제다.
func (a *API) assign(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req assignRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if req.AgentID == "me" {
		req.AgentID = actor.AgentID
	}
	if req.ExpectedAssignee != nil && *req.ExpectedAssignee == "me" {
		self := actor.AgentID
		req.ExpectedAssignee = &self
	}
	conv, err := a.cmd.Assign(r.Context(), actor, r.PathValue("id"), req.AgentID, req.ExpectedAssignee)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

type followRequest struct {
	Follow bool `json:"follow"`
}

func (a *API) follow(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req followRequest
	if !decodeOr(w, r, &req) {
		return
	}
	if err := a.cmd.Follow(r.Context(), actor, r.PathValue("id"), req.Follow); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, followResponse{Following: req.Follow})
}

func (a *API) followers(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	agents, err := a.qry.Followers(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, followersResponse{Followers: agents})
}

type tagRequest struct {
	Name string `json:"name"`
}

func (a *API) addTag(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req tagRequest
	if !decodeOr(w, r, &req) {
		return
	}
	tags, err := a.cmd.AddTag(r.Context(), actor, r.PathValue("id"), req.Name)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags})
}

func (a *API) removeTag(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	tags, err := a.cmd.RemoveTag(r.Context(), actor, r.PathValue("id"), r.PathValue("tagId"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags})
}

func (a *API) search(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	hits, err := a.qry.Search(r.Context(), actor, r.URL.Query().Get("q"), intQuery(r, "limit", core.SearchPageDefault))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, searchResponse{Hits: hits})
}

func (a *API) listSnippets(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	sn, err := a.qry.Snippets(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snippetsResponse{Snippets: sn})
}

type snippetRequest struct {
	Shortcut string `json:"shortcut"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

func (a *API) createSnippet(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var req snippetRequest
	if !decodeOr(w, r, &req) {
		return
	}
	sn, err := a.cmd.CreateSnippet(r.Context(), actor, req.Shortcut, req.Title, req.Body)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (a *API) deleteSnippet(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := a.cmd.DeleteSnippet(r.Context(), actor, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) agents(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	agents, err := a.qry.Agents(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentsResponse{Agents: agents})
}

func (a *API) tags(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	tags, err := a.qry.Tags(r.Context(), actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags})
}

// draftReply 는 AI 초안을 돌려준다. 저장하지도 보내지도 않는다. 작성칸을 채우는 게 전부다.
func (a *API) draftReply(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	res, err := a.qry.Draft(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// toProfile 은 요청 본문을 저장소 입력으로 옮긴다.
func toProfile(req customerRequest) core.CustomerProfile {
	return core.CustomerProfile{
		DisplayName: req.DisplayName, Email: req.Email, Phone: req.Phone,
		Note: req.Note, Attributes: req.Attributes,
	}
}

// ── 업무 기록 ───────────────────────────────────────────────────────

type workResponse struct {
	Events []core.ConversationEvent `json:"events"`
}

type workSummaryResponse struct {
	From   time.Time        `json:"from"`
	To     time.Time        `json:"to"`
	Agents []core.AgentWork `json:"agents"`
}

// workLog 는 "누가 어떤 고객의 어떤 대화에 무엇을 했나" 의 원천이다. agent=me|<id>, conversation=<id>, from/to=RFC3339.
func (a *API) workLog(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	q := r.URL.Query()
	f := core.EventFilter{ConversationID: q.Get("conversation"), Limit: intQuery(r, "limit", core.EventsPageDefault)}
	switch agent := q.Get("agent"); agent {
	case "", "all":
	case "me":
		f.AgentID = actor.AgentID
	default:
		normalized, err := id.Normalize(agent)
		if err != nil {
			noteErrorReason(w, "bad_agent_filter")
			writeErr(w, http.StatusBadRequest, "invalid", "agent 는 me·all 또는 상담원 id 여야 합니다")
			return
		}
		f.AgentID = normalized
	}
	var ok bool
	if f.From, f.To, ok = timeRange(w, q.Get("from"), q.Get("to")); !ok {
		return
	}
	events, err := a.qry.WorkLog(r.Context(), actor, f)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workResponse{Events: events})
}

// workSummary 는 기간 안의 상담원별 집계다. 기본은 최근 7일(끝은 DB 시계).
func (a *API) workSummary(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	q := r.URL.Query()
	from, to, ok := timeRange(w, q.Get("from"), q.Get("to"))
	if !ok {
		return
	}
	// 비어 있는 끝은 app 이 DB 시계로 채운다(DEF-148). 앱 시계로 채우면 DB 가 적은 방금의 기록이 창 밖이 될 수 있다.
	view, err := a.qry.WorkSummary(r.Context(), actor, from, to)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workSummaryResponse{From: view.From, To: view.To, Agents: view.Agents})
}

// timeRange 는 from/to 를 읽는다. 틀린 값을 조용히 무시하면 "어제부터" 가 "전부" 가 된다. 400 으로 답한다.
func timeRange(w http.ResponseWriter, from, to string) (time.Time, time.Time, bool) {
	parse := func(v string) (time.Time, bool) {
		if v == "" {
			return time.Time{}, true
		}
		t, err := time.Parse(time.RFC3339, v)
		return t, err == nil
	}
	f, okF := parse(from)
	t, okT := parse(to)
	if !okF || !okT {
		noteErrorReason(w, "bad_time")
		writeErr(w, http.StatusBadRequest, "invalid", "from·to 는 RFC3339 시각이어야 합니다 (예: 2026-10-01T00:00:00Z)")
		return time.Time{}, time.Time{}, false
	}
	return f, t, true
}
