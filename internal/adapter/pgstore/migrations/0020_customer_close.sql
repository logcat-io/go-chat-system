-- 고객이 상담을 직접 종료한다(D48, 2026-10-09). 추가만 한다.

-- 1) 누가 마지막으로 닫았나. 상담원(AGENT)·고객(CUSTOMER)·시스템(SYSTEM). 상태를 바꾸는 트랜잭션이 잠근 행에서 같이 쓴다.
--    값은 마지막 종료의 것이고 다시 열어도 지우지 않는다 — 판정은 언제나 state = 'CLOSED' 와 같이 본다.
--    "고객이 닫아 둔 상담에는 고객의 새 글을 받지 않는다" 를 메시지 저장의 번호 발급(allocate)이 잠근 대화 행에서 판정한다.
--    다른 표(conversation_closures)에 두면 그 판정이 문장 시작 스냅숏을 봐서 겹친 종료를 놓친다(DEF-103 과 같은 함정).
ALTER TABLE conversations ADD COLUMN closed_by TEXT CHECK (closed_by IN ('AGENT', 'CUSTOMER', 'SYSTEM'));

-- 지금 닫혀 있는 상담을 채운다. 0020 전에는 고객이 닫는 길이 없었다 — 상담원의 종료 기록(closed, agent_id 있음)이 있으면 상담원,
-- 없으면 시스템이다(지난 상담을 되살릴 때 종료로 돌린 것, 상담원이 지워져 agent_id 가 비었다).
UPDATE conversations c
   SET closed_by = CASE WHEN EXISTS (SELECT 1 FROM conversation_events e
                                       WHERE e.conversation_id = c.id AND e.kind = 'closed' AND e.agent_id IS NOT NULL)
                        THEN 'AGENT' ELSE 'SYSTEM' END
 WHERE c.state = 'CLOSED';

-- 2) 처리 주체에 고객 자체 해결(SELF)을 더한다 — 사람·AI 의 답 없이 고객이 닫았다. 상담원 없이 끝난 상담이라 AI 처리율의 분자에 든다.
--    만족도 표(conversation_ratings)의 handler 는 그대로다. 답 없이 고객이 닫은 상담은 평가를 받지 않는다(앱과 저장소가 거절하고, 이 CHECK 가 한 번 더 막는다).
ALTER TABLE conversation_closures DROP CONSTRAINT conversation_closures_handler_check;
ALTER TABLE conversation_closures ADD CONSTRAINT conversation_closures_handler_check
    CHECK (handler IN ('AGENT', 'AI', 'UNANSWERED', 'SELF'));
