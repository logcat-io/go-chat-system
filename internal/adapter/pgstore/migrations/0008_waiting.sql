-- "답할 차례" 는 읽음이 아니라 답변 기준이다. 고객이 마지막으로 말한 뒤 상담원의 공개 답변이 있었는지를 센다.
-- 내부 메모와 자동 응답(BOT)은 답이 아니다. 고객은 여전히 사람을 기다린다.
ALTER TABLE conversations
    ADD COLUMN last_agent_public_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_agent_public_seq >= 0);
UPDATE conversations c
   SET last_agent_public_seq = COALESCE(
        (SELECT max(m.seq) FROM messages m
          WHERE m.conversation_id = c.id AND m.sender_kind = 'AGENT' AND m.visibility = 'PUBLIC'), 0);
CREATE INDEX conversations_unanswered_idx
    ON conversations (workspace_id, assignee_agent_id)
    WHERE state <> 'CLOSED' AND last_customer_seq > last_agent_public_seq;
