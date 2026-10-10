-- 메시지 수정·삭제는 같은 로그의 이벤트 행이다(번호 체계를 지킨다). 원문은 message_revisions 에 남는다.
ALTER TABLE messages
    ADD COLUMN kind           TEXT NOT NULL DEFAULT 'MESSAGE' CHECK (kind IN ('MESSAGE', 'EDIT', 'DELETE')),
    ADD COLUMN ref_message_id UUID REFERENCES messages (id) ON DELETE SET NULL,
    ADD COLUMN edited_at      TIMESTAMPTZ,
    ADD COLUMN deleted_at     TIMESTAMPTZ;
CREATE INDEX messages_ref_idx ON messages (ref_message_id) WHERE ref_message_id IS NOT NULL;

CREATE TABLE message_revisions (
    id         BIGSERIAL   PRIMARY KEY,
    message_id UUID        NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    body       TEXT        NOT NULL,
    reason     TEXT        NOT NULL CHECK (reason IN ('EDIT', 'DELETE')),
    actor_kind TEXT        NOT NULL,
    actor_id   UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX message_revisions_message_idx ON message_revisions (message_id, id);

-- 회원 재상담: 같은 방을 이어 쓰되 고객에게는 새 방처럼 보인다.
--   customer_visible_from  고객이 보는 공개 번호의 시작점(이 번호 초과만 보인다)
--   episode_from_seq       이번 상담이 시작된 전체 번호. 상담원 화면은 그 직전 30개까지 먼저 보여 준다
ALTER TABLE conversations
    ADD COLUMN customer_visible_from BIGINT NOT NULL DEFAULT 0 CHECK (customer_visible_from >= 0),
    ADD COLUMN episode_from_seq      BIGINT NOT NULL DEFAULT 0 CHECK (episode_from_seq >= 0),
    ADD COLUMN episodes              INT    NOT NULL DEFAULT 1 CHECK (episodes >= 1);

-- 업무 처리 기록: 누가 어떤 고객의 어떤 대화에 무엇을 했나. 칸반·대시보드의 원천이다. 지우지 않는다.
CREATE TABLE conversation_events (
    id              BIGSERIAL   PRIMARY KEY,
    workspace_id    UUID        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    customer_id     UUID        REFERENCES customers (id) ON DELETE SET NULL,
    agent_id        UUID        REFERENCES agents (id) ON DELETE SET NULL,
    kind            TEXT        NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 40),
    detail          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX conversation_events_ws_time_idx  ON conversation_events (workspace_id, created_at DESC);
CREATE INDEX conversation_events_agent_idx    ON conversation_events (workspace_id, agent_id, created_at DESC);
CREATE INDEX conversation_events_conv_idx     ON conversation_events (conversation_id, id);
