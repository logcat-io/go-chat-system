package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 상담 지표와 만족도 조사(D45)의 저장소다. 포트는 app/ports_metrics.go 에 있다.
//
// 쓰기는 셋이다.
//   - 응답 기록(conversation_responses): 메시지 저장 트랜잭션(appendOnce → recordResponse)
//   - 종료 기록(conversation_closures): 종료 트랜잭션(SetConversationState → recordClosure)
//   - 만족도(conversation_ratings): PutRating
//
// 처리 주체를 정하는 규칙은 core.ClassifyHandler 다. 여기서는 그 규칙에 넣을 사실(상담원·AI 의 공개 답이 있었나)만 읽는다.

// recordClosure 는 상담을 끝내는 트랜잭션 안에서 처리 주체와 담당 상담원을 남긴다. 대화 행은 이미 잠겨 있다.
// 담당 상담원은 끝낼 때의 담당자, 없으면 마지막으로 공개 답을 한 상담원이다. 지운 답도 개입이다 — 고객은 그 답을 받았다.
// by 는 누가 닫았나다 — 답이 하나도 없을 때 고객이 닫았으면 고객 자체 해결(SELF), 아니면 미응답이다(D48).
func recordClosure(ctx context.Context, tx pgx.Tx, convID string, by core.ClosedBy) error {
	var agentReplied, aiReplied bool
	var lastAgent *string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(bool_or(sender_kind = 'AGENT'), false),
		       COALESCE(bool_or(sender_kind = 'AI'), false),
		       (array_agg(sender_agent_id::text ORDER BY seq DESC) FILTER (WHERE sender_kind = 'AGENT'))[1]
		  FROM messages
		 WHERE conversation_id = $1 AND kind = 'MESSAGE' AND visibility = 'PUBLIC' AND sender_kind IN ('AGENT', 'AI')`,
		convID).Scan(&agentReplied, &aiReplied, &lastAgent)
	if err != nil {
		return fmt.Errorf("pgstore: 처리 주체 판정 실패: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO conversation_closures (conversation_id, workspace_id, handler, agent_id, closed_at)
		SELECT c.id, c.workspace_id, $2, COALESCE(c.assignee_agent_id, nullif($3::text, '')::uuid), clock_timestamp()
		  FROM conversations c
		 WHERE c.id = $1
		ON CONFLICT (conversation_id) DO UPDATE
		   SET handler = EXCLUDED.handler, agent_id = EXCLUDED.agent_id, closed_at = EXCLUDED.closed_at`,
		convID, string(core.ClassifyHandler(agentReplied, aiReplied, by)), deref(lastAgent))
	if err != nil {
		return fmt.Errorf("pgstore: 종료 기록 실패: %w", err)
	}
	return nil
}

// RatingState 는 상담 하나의 만족도 조사 상태(지금 종료인지, 마지막 종료 시각, 낸 평가)다. 권한 판정은 앞에서 끝났다.
func (s *Store) RatingState(ctx context.Context, convID string) (core.RatingState, error) {
	return ratingStateOn(ctx, s.pool, convID)
}

func ratingStateOn(ctx context.Context, q querier, convID string) (core.RatingState, error) {
	var st core.RatingState
	var state core.State
	var handler *string
	var score *int
	var comment, rHandler, agentID, agentName *string
	var rClosed, rCreated, rUpdated *time.Time
	err := q.QueryRow(ctx, `
		SELECT c.state, cl.closed_at, cl.handler, clock_timestamp(),
		       r.score, r.comment, r.handler, r.agent_id::text, a.name, r.closed_at, r.created_at, r.updated_at
		  FROM conversations c
		  LEFT JOIN conversation_closures cl ON cl.conversation_id = c.id
		  LEFT JOIN conversation_ratings  r  ON r.conversation_id = c.id
		  LEFT JOIN agents                a  ON a.id = r.agent_id
		 WHERE c.id = $1`, convID).Scan(&state, &st.ClosedAt, &handler, &st.Now,
		&score, &comment, &rHandler, &agentID, &agentName, &rClosed, &rCreated, &rUpdated)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.RatingState{}, core.ErrNotFound
	}
	if err != nil {
		return core.RatingState{}, err
	}
	st.Closed = state == core.StateClosed && st.ClosedAt != nil
	st.Handler = core.Handler(deref(handler))
	if score != nil {
		st.Rating = &core.Rating{
			Score: *score, Comment: deref(comment), Handler: core.Handler(deref(rHandler)),
			AgentID: deref(agentID), AgentName: deref(agentName),
			ClosedAt: deref(rClosed), CreatedAt: deref(rCreated), UpdatedAt: deref(rUpdated),
		}
	}
	return st, nil
}

// PutRating 은 그 상담의 고객이 낸 만족도를 남긴다. 상담당 한 행이고 다시 내면 덮는다(마지막 값).
//
// 판정은 잠근 대화 행과 DB 시계로 한다 — 그 상담의 고객이 아니거나 가린 상담이면 core.ErrNotFound, 지금 종료가 아니면
// core.ErrNotClosed, 마지막 종료에서 Window 가 지났으면 core.ErrRatingExpired 다. 대화 행을 공유 잠금으로 잡아
// 다시 열기(행을 고친다)와 겹치면 하나가 다른 하나를 기다린다 — 다시 열린 상담에 평가가 들어가지 않는다.
// 처리 주체·담당 상담원·종료 시각은 그 종료 기록을 옮겨 적는다.
func (s *Store) PutRating(ctx context.Context, in core.RatingInput) (core.RatingState, error) {
	var out core.RatingState
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var state core.State
		var deleted bool
		var customerID string
		var closedAt *time.Time
		var handler, agentID *string
		var now time.Time
		err := tx.QueryRow(ctx, `
			SELECT c.state, c.deleted_at IS NOT NULL, c.customer_id::text, cl.closed_at, cl.handler, cl.agent_id::text, clock_timestamp()
			  FROM conversations c
			  LEFT JOIN conversation_closures cl ON cl.conversation_id = c.id
			 WHERE c.id = $1
			   FOR SHARE OF c`, in.ConversationID).Scan(&state, &deleted, &customerID, &closedAt, &handler, &agentID, &now)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return core.ErrNotFound
		case err != nil:
			return err
		case deleted || customerID != in.CustomerID:
			return core.ErrNotFound
		case state != core.StateClosed || closedAt == nil:
			return core.ErrNotClosed
		case core.Handler(deref(handler)) == core.HandlerSelf:
			// 상담원·AI 의 답 없이 고객이 닫았다 — 평가할 상담이 없었다(D48). 상담원이 닫은 상담의 규칙은 그대로다(미응답도 받는다).
			return core.ErrNoReply
		case now.After(closedAt.Add(in.Window)):
			return core.ErrRatingExpired
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversation_ratings (conversation_id, workspace_id, customer_id, score, comment, handler, agent_id, closed_at)
			SELECT c.id, c.workspace_id, c.customer_id, $2, $3, $4, nullif($5::text, '')::uuid, $6
			  FROM conversations c
			 WHERE c.id = $1
			ON CONFLICT (conversation_id) DO UPDATE
			   SET score = EXCLUDED.score, comment = EXCLUDED.comment, handler = EXCLUDED.handler, agent_id = EXCLUDED.agent_id,
			       created_at = CASE WHEN conversation_ratings.closed_at = EXCLUDED.closed_at
			                         THEN conversation_ratings.created_at ELSE clock_timestamp() END,
			       closed_at = EXCLUDED.closed_at, updated_at = clock_timestamp()`,
			in.ConversationID, in.Score, in.Comment, deref(handler), deref(agentID), *closedAt); err != nil {
			return fmt.Errorf("pgstore: 만족도 저장 실패: %w", err)
		}
		out, err = ratingStateOn(ctx, tx, in.ConversationID)
		return err
	})
	if err != nil {
		return core.RatingState{}, err
	}
	return out, nil
}

// Metrics 는 기간 [from, to) 의 상담 지표다(D45). 가린 상담은 세지 않는다.
//
// 기간의 기준은 지표마다 그 사건의 시각이다 — 종료 수·처리 주체·만족도는 마지막 종료 시각, 응답 시간은 답한 시각.
// 세 표 모두 (workspace_id, 시각) 인덱스로 기간을 좁힌다.
func (s *Store) Metrics(ctx context.Context, wsID string, from, to time.Time) (core.Metrics, error) {
	m := core.Metrics{From: from, To: to}
	rows := map[string]*core.ResponderMetrics{}
	row := func(kind core.SenderKind, agentID string) *core.ResponderMetrics {
		key := string(kind) + ":" + agentID
		if r, ok := rows[key]; ok {
			return r
		}
		r := &core.ResponderMetrics{Kind: kind, AgentID: agentID}
		rows[key] = r
		return r
	}

	if err := s.closureMetrics(ctx, wsID, from, to, &m, row); err != nil {
		return core.Metrics{}, err
	}
	if err := s.responseMetrics(ctx, wsID, from, to, &m, row); err != nil {
		return core.Metrics{}, err
	}
	if err := s.ratingMetrics(ctx, wsID, from, to, &m, row); err != nil {
		return core.Metrics{}, err
	}
	if err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM conversation_responses WHERE workspace_id = $1 AND responder_kind = 'AI')", wsID,
	).Scan(&m.AIActive); err != nil {
		return core.Metrics{}, err
	}

	// 상담원 없이 끝난 비율이다 — AI 응대와 고객 자체 해결(SELF)이 분자다(D48). 미응답은 분모에도 넣지 않는다.
	if handled := m.Closed.AI + m.Closed.Self + m.Closed.Agent; handled > 0 {
		m.AIRate = ratio(m.Closed.AI+m.Closed.Self, handled)
	}
	// 고객 자체 해결은 만족도 조사를 받지 않는다(답이 없었다). 응답률의 분모는 조사를 받은 종료 상담이다.
	if surveyed := m.Closed.Total - m.Closed.Self; surveyed > 0 {
		m.ResponseRate = ratio(m.Satisfaction.Responses, surveyed)
	}
	names, err := s.agentNames(ctx, wsID)
	if err != nil {
		return core.Metrics{}, err
	}
	m.Responders = make([]core.ResponderMetrics, 0, len(rows))
	for _, r := range rows {
		if r.Kind == core.SenderAgent {
			if r.AgentID == "" {
				continue // 지워진 상담원의 기록은 상담원별 표에 줄을 만들지 않는다. 합계에는 든다
			}
			r.AgentName = names[r.AgentID]
		}
		m.Responders = append(m.Responders, *r)
	}
	// AI 가 먼저, 그다음 상담원 이름순이다.
	sort.Slice(m.Responders, func(i, j int) bool {
		a, b := m.Responders[i], m.Responders[j]
		if a.Kind != b.Kind {
			return a.Kind == core.SenderAI
		}
		if a.AgentName != b.AgentName {
			return a.AgentName < b.AgentName
		}
		return a.AgentID < b.AgentID
	})
	return m, nil
}

// closureFrom·closureWhere 는 기간과 "지금 종료 상태이고 가리지 않은 상담" 조건이다. 종료 기록(cl)과 대화(c)를 잇는 쿼리가 같이 쓴다.
// 다시 열려 아직 진행 중인 상담은 종료된 상담이 아니다 — 다시 닫으면 그 종료 시각으로 들어온다.
const (
	closureFrom = `
		  FROM conversation_closures cl
		  JOIN conversations c ON c.id = cl.conversation_id`
	closureWhere = `
		 WHERE cl.workspace_id = $1 AND cl.closed_at >= $2 AND cl.closed_at < $3
		   AND c.state = 'CLOSED' AND c.deleted_at IS NULL`
)

func (s *Store) closureMetrics(ctx context.Context, wsID string, from, to time.Time, m *core.Metrics,
	row func(core.SenderKind, string) *core.ResponderMetrics) error {
	rs, err := s.pool.Query(ctx, `
		SELECT cl.handler, COALESCE(cl.agent_id::text, ''), GROUPING(cl.handler, cl.agent_id), count(*),
		       count(*) FILTER (WHERE c.closed_by = 'CUSTOMER')`+closureFrom+closureWhere+`
		 GROUP BY GROUPING SETS ((), (cl.handler), (cl.handler, cl.agent_id))`, wsID, from, to)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var handler *string
		var agentID string
		var g int
		var n, byCustomer int64
		if err := rs.Scan(&handler, &agentID, &g, &n, &byCustomer); err != nil {
			return err
		}
		switch {
		case g == 3:
			m.Closed.Total = n
			m.Closed.ByCustomer = byCustomer // 누가 닫았나 — 처리 주체와 다른 축이다(D48)
		case g == 1 && core.Handler(deref(handler)) == core.HandlerAgent:
			m.Closed.Agent = n
		case g == 1 && core.Handler(deref(handler)) == core.HandlerAI:
			m.Closed.AI = n
			row(core.SenderAI, "").Handled = n
		case g == 1 && core.Handler(deref(handler)) == core.HandlerSelf:
			m.Closed.Self = n
		case g == 1:
			m.Closed.Unanswered = n
		case g == 0 && core.Handler(deref(handler)) == core.HandlerAgent:
			row(core.SenderAgent, agentID).Handled = n
		}
	}
	return rs.Err()
}

// responseMetrics 는 응답 시간의 평균·중앙값·p90 이다. 전체와 (주체, 상담원) 두 층만 묶는다 — AI 응답은 상담원이 없어
// (AI, 빈 상담원) 한 줄이 곧 AI 의 줄이다. 퍼센타일은 같은 정렬을 한 번 쓰도록 배열로 받는다(percentile_cont(ARRAY[…])).
// 시간 차는 date_part(float8)로 잰다 — extract 는 numeric 이라 47만 행에서 두 배 느렸다(D45 실측).
// 가린 상담은 NOT EXISTS 로 뺀다 — 가린 상담만 해시로 들고 응답 행마다 대화 표를 잇지 않는다.
func (s *Store) responseMetrics(ctx context.Context, wsID string, from, to time.Time, m *core.Metrics,
	row func(core.SenderKind, string) *core.ResponderMetrics) error {
	rs, err := s.pool.Query(ctx, `
		WITH r AS (
			SELECT r.responder_kind, r.agent_id, r.first, date_part('epoch', r.answered_at - r.waited_from) * 1000 AS ms
			  FROM conversation_responses r
			 WHERE r.workspace_id = $1 AND r.answered_at >= $2 AND r.answered_at < $3
			   AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.id = r.conversation_id AND c.deleted_at IS NOT NULL))
		SELECT responder_kind, COALESCE(agent_id::text, ''), GROUPING(responder_kind, agent_id),
		       count(*), avg(ms), percentile_cont(ARRAY[0.5, 0.9]) WITHIN GROUP (ORDER BY ms),
		       count(*) FILTER (WHERE first), avg(ms) FILTER (WHERE first),
		       percentile_cont(ARRAY[0.5, 0.9]) WITHIN GROUP (ORDER BY ms) FILTER (WHERE first)
		  FROM r
		 GROUP BY GROUPING SETS ((), (responder_kind, agent_id))`, wsID, from, to)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var kind *string
		var agentID string
		var g int
		var all, first core.DurationStats
		var allP, firstP []*float64
		if err := rs.Scan(&kind, &agentID, &g, &all.Count, &all.AvgMs, &allP, &first.Count, &first.AvgMs, &firstP); err != nil {
			return err
		}
		all.P50Ms, all.P90Ms = pick(allP, 0), pick(allP, 1)
		first.P50Ms, first.P90Ms = pick(firstP, 0), pick(firstP, 1)
		switch {
		case g != 0:
			m.Response, m.FirstResponse = all, first
		case core.SenderKind(deref(kind)) == core.SenderAI:
			r := row(core.SenderAI, "")
			r.Response, r.FirstResponse = all, first
		case core.SenderKind(deref(kind)) == core.SenderAgent:
			r := row(core.SenderAgent, agentID)
			r.Response, r.FirstResponse = all, first
		}
	}
	return rs.Err()
}

// pick 은 퍼센타일 배열의 i 번째다. 건수가 0 이면 배열이 NULL 이라 nil 이다.
func pick(v []*float64, i int) *float64 {
	if i < len(v) {
		return v[i]
	}
	return nil
}

// ratingMetrics 는 만족도 집계다. 그 종료에 낸 평가만 센다(rt.closed_at = cl.closed_at, 감사 20·logcat 결정) — 다시 열었다 닫은 상담에
// 새 평가가 없으면 그 종료는 평가 없음이다. 지난 종료의 평가는 앞 종료의 처리 주체·담당 상담원을 옮겨 적은 것이라 지금 종료의 주체·
// 상담원 줄에 섞이면 그 사람이 받지 않은 점수가 된다. 지난 평가는 데스크의 대화 옆 칸이 "지난 종료의 평가" 로 보인다.
func (s *Store) ratingMetrics(ctx context.Context, wsID string, from, to time.Time, m *core.Metrics,
	row func(core.SenderKind, string) *core.ResponderMetrics) error {
	rs, err := s.pool.Query(ctx, `
		SELECT rt.handler, COALESCE(rt.agent_id::text, ''), GROUPING(rt.handler, rt.agent_id),
		       count(*), avg(rt.score)::float8, count(*) FILTER (WHERE rt.score >= 4)`+closureFrom+`
		  JOIN conversation_ratings rt ON rt.conversation_id = cl.conversation_id AND rt.closed_at = cl.closed_at`+closureWhere+`
		 GROUP BY GROUPING SETS ((), (rt.handler), (rt.handler, rt.agent_id))`, wsID, from, to)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var handler *string
		var agentID string
		var g int
		var n, satisfied int64
		var avg *float64
		if err := rs.Scan(&handler, &agentID, &g, &n, &avg, &satisfied); err != nil {
			return err
		}
		st := core.SatisfactionStats{Responses: n, Average: avg}
		if n > 0 {
			st.CSAT = ratio(satisfied, n)
		}
		switch {
		case g == 3:
			m.Satisfaction = st
		case g == 1 && core.Handler(deref(handler)) == core.HandlerAI:
			row(core.SenderAI, "").Satisfaction = st
		case g == 0 && core.Handler(deref(handler)) == core.HandlerAgent:
			row(core.SenderAgent, agentID).Satisfaction = st
		}
	}
	return rs.Err()
}

func (s *Store) agentNames(ctx context.Context, wsID string) (map[string]string, error) {
	rs, err := s.pool.Query(ctx, "SELECT id::text, name FROM agents WHERE workspace_id = $1", wsID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var id, name string
		if err := rs.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rs.Err()
}

func ratio(n, d int64) *float64 {
	v := float64(n) / float64(d)
	return &v
}
