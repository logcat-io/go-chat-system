-- 0005: 고객이 마지막으로 말한 번호를 대화에 둔다.
--
-- "안 읽음" 을 last_seq > last_read_seq 로 재면 담당자 지정 같은 내부 기록까지 안 읽음이 된다.
-- 상담원이 봐야 할 것은 "고객이 답을 기다리는 대화" 지 "내부 기록이 생긴 대화" 가 아니다.
-- 대화마다 집계 쿼리를 돌리지 않으려고 값을 미리 들고 있는다.

ALTER TABLE conversations
    ADD COLUMN last_customer_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_customer_seq >= 0);

UPDATE conversations c
   SET last_customer_seq = COALESCE(
       (SELECT max(m.seq) FROM messages m
         WHERE m.conversation_id = c.id AND m.sender_kind = 'CUSTOMER'), 0);

CREATE INDEX conversations_waiting_idx
    ON conversations (assignee_agent_id, last_customer_seq)
    WHERE state <> 'CLOSED';
