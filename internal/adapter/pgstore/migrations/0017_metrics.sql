-- 상담 지표와 만족도 조사(D45). 추가만 한다.
--
-- 1) AI 응대의 자리. 발신 종류 'AI' 를 더한다. 지금 쓰는 길은 없고(상담 흐름 개편에서 들어온다), 지표는 이 종류로 AI 답을 센다.
--    접수 안내·부재중(BOT)과 시스템 줄(SYSTEM)은 AI 응대가 아니다. AI 도 BOT 처럼 상담원·고객 id 가 없다.
--    큰 표의 CHECK 를 다시 걸 때 표 전체를 잠근 채 훑지 않도록 NOT VALID 로 걸고 따로 확인한다.
ALTER TABLE messages DROP CONSTRAINT messages_sender_kind_check;
ALTER TABLE messages ADD CONSTRAINT messages_sender_kind_check
    CHECK (sender_kind IN ('CUSTOMER', 'AGENT', 'SYSTEM', 'BOT', 'AI')) NOT VALID;
ALTER TABLE messages VALIDATE CONSTRAINT messages_sender_kind_check;
ALTER TABLE messages DROP CONSTRAINT messages_sender_matches;
ALTER TABLE messages ADD CONSTRAINT messages_sender_matches CHECK (
    (sender_kind = 'CUSTOMER' AND sender_customer IS NOT NULL AND sender_agent_id IS NULL) OR
    (sender_kind = 'AGENT'    AND sender_agent_id IS NOT NULL AND sender_customer IS NULL) OR
    (sender_kind IN ('SYSTEM', 'BOT', 'AI') AND sender_agent_id IS NULL AND sender_customer IS NULL)
) NOT VALID;
ALTER TABLE messages VALIDATE CONSTRAINT messages_sender_matches;

-- 2) 고객이 기다리기 시작한 시각. 답이 없던 고객 글들 중 첫 글의 시각이고, 상담원·AI 의 공개 답이 오면 비운다.
--    번호 발급 UPDATE(allocate)가 잠근 행에서 고친다 — 동시에 겹친 글도 커밋 순서대로 본다(DEF-103).
ALTER TABLE conversations ADD COLUMN waiting_since TIMESTAMPTZ;
UPDATE conversations c
   SET waiting_since = (SELECT min(m.created_at) FROM messages m
                         WHERE m.conversation_id = c.id AND m.sender_kind = 'CUSTOMER' AND m.kind = 'MESSAGE'
                           AND m.seq > c.last_agent_public_seq)
 WHERE c.last_customer_seq > c.last_agent_public_seq;

-- 3) 응답 기록: 기다리던 고객에게 간 답 하나가 한 행이다. 메시지 저장과 같은 트랜잭션에 들어간다.
--    first 는 그 상담의 첫 응답이다 — 상담마다 하나뿐인 것을 DB 가 지킨다.
CREATE TABLE conversation_responses (
    id              BIGSERIAL   PRIMARY KEY,
    workspace_id    UUID        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    responder_kind  TEXT        NOT NULL CHECK (responder_kind IN ('AGENT', 'AI')),
    agent_id        UUID        REFERENCES agents (id) ON DELETE SET NULL,
    waited_from     TIMESTAMPTZ NOT NULL,
    answered_at     TIMESTAMPTZ NOT NULL,
    first           BOOLEAN     NOT NULL,
    message_seq     BIGINT      NOT NULL CHECK (message_seq > 0),
    CHECK (answered_at >= waited_from)
);
CREATE UNIQUE INDEX conversation_responses_message_idx ON conversation_responses (conversation_id, message_seq);
CREATE UNIQUE INDEX conversation_responses_first_idx   ON conversation_responses (conversation_id) WHERE first;
CREATE INDEX conversation_responses_ws_time_idx        ON conversation_responses (workspace_id, answered_at);
-- "AI 응대가 한 번이라도 있었나" 를 표를 훑지 않고 답한다. AI 응대가 없는 동안 이 인덱스는 비어 있다.
CREATE INDEX conversation_responses_ai_idx             ON conversation_responses (workspace_id) WHERE responder_kind = 'AI';

-- 지난 대화의 응답을 메시지 로그에서 다시 만든다. 규칙은 실행 중의 것과 같다:
-- 고객 글(공개, 메시지)이 차례를 열고, 그 뒤 첫 상담원 공개 답이 그 차례의 응답이다. 사이의 고객 글은 같은 차례다.
-- g 는 그 줄 앞의 상담원 답 수다. 같은 g 의 고객 글이 한 차례이고, 그 g 를 가진 상담원 답이 그 차례의 응답이다.
INSERT INTO conversation_responses (workspace_id, conversation_id, responder_kind, agent_id, waited_from, answered_at, first, message_seq)
WITH s AS (
    SELECT conversation_id, seq, created_at, sender_kind, sender_agent_id,
           count(*) FILTER (WHERE sender_kind = 'AGENT')
               OVER (PARTITION BY conversation_id ORDER BY seq ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS g
      FROM messages
     WHERE kind = 'MESSAGE' AND visibility = 'PUBLIC' AND sender_kind IN ('CUSTOMER', 'AGENT')
), turns AS (
    SELECT conversation_id, g, min(created_at) AS waited_from FROM s WHERE sender_kind = 'CUSTOMER' GROUP BY conversation_id, g
)
SELECT c.workspace_id, a.conversation_id, 'AGENT', a.sender_agent_id, t.waited_from, greatest(a.created_at, t.waited_from),
       row_number() OVER (PARTITION BY a.conversation_id ORDER BY a.seq) = 1, a.seq
  FROM s a
  JOIN turns t ON t.conversation_id = a.conversation_id AND t.g = a.g
  JOIN conversations c ON c.id = a.conversation_id
 WHERE a.sender_kind = 'AGENT';

-- 4) 종료 기록: 상담을 끝낼 때 처리 주체(AGENT·AI·UNANSWERED)와 담당 상담원을 남긴다. 다시 열었다 닫으면 마지막 종료로 덮는다.
CREATE TABLE conversation_closures (
    conversation_id UUID        PRIMARY KEY REFERENCES conversations (id) ON DELETE CASCADE,
    workspace_id    UUID        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    handler         TEXT        NOT NULL CHECK (handler IN ('AGENT', 'AI', 'UNANSWERED')),
    agent_id        UUID        REFERENCES agents (id) ON DELETE SET NULL,
    closed_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX conversation_closures_ws_time_idx ON conversation_closures (workspace_id, closed_at);

-- 지금 종료된 상담의 종료 기록을 만든다. 종료 시각은 마지막 종료 업무 기록, 없으면 마지막 갱신 시각이다.
INSERT INTO conversation_closures (conversation_id, workspace_id, handler, agent_id, closed_at)
SELECT c.id, c.workspace_id,
       CASE WHEN x.agent THEN 'AGENT' WHEN x.ai THEN 'AI' ELSE 'UNANSWERED' END,
       COALESCE(c.assignee_agent_id, x.last_agent),
       COALESCE((SELECT max(e.created_at) FROM conversation_events e WHERE e.conversation_id = c.id AND e.kind = 'closed'), c.updated_at)
  FROM conversations c
  CROSS JOIN LATERAL (
        SELECT COALESCE(bool_or(m.sender_kind = 'AGENT'), false) AS agent,
               COALESCE(bool_or(m.sender_kind = 'AI'), false)    AS ai,
               (array_agg(m.sender_agent_id ORDER BY m.seq DESC) FILTER (WHERE m.sender_kind = 'AGENT'))[1] AS last_agent
          FROM messages m
         WHERE m.conversation_id = c.id AND m.kind = 'MESSAGE' AND m.visibility = 'PUBLIC' AND m.sender_kind IN ('AGENT', 'AI')) x
 WHERE c.state = 'CLOSED';

-- 5) 만족도: 상담당 한 행. 다시 열었다 닫은 뒤 다시 내면 마지막 값으로 덮는다.
--    처리 주체·담당 상담원·종료 시각은 낼 때의 종료 기록을 옮겨 적는다(주체별·상담원별 집계).
CREATE TABLE conversation_ratings (
    conversation_id UUID        PRIMARY KEY REFERENCES conversations (id) ON DELETE CASCADE,
    workspace_id    UUID        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    customer_id     UUID        REFERENCES customers (id) ON DELETE SET NULL,
    score           SMALLINT    NOT NULL CHECK (score BETWEEN 1 AND 5),
    comment         TEXT        NOT NULL DEFAULT '' CHECK (char_length(comment) <= 200),
    handler         TEXT        NOT NULL CHECK (handler IN ('AGENT', 'AI', 'UNANSWERED')),
    agent_id        UUID        REFERENCES agents (id) ON DELETE SET NULL,
    closed_at       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
