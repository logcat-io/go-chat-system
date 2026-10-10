-- 역할 셋과 상담 삭제(D29).
--  1. 역할: ADMIN / STAFF / PART_TIME. 옛 AGENT 는 내부 직원(STAFF)이다. 무엇을 할 수 있는지는 app 의 대응표가 정한다.
--  2. 상담원 직접 가입: 승인 전 계정은 approved_at 이 비어 있다. 기존 계정은 이미 승인된 것으로 본다.
--  3. 상담 삭제는 가리기다. 행과 메시지는 그대로 두고 누가·언제·왜 가렸는지만 찍는다(D24 — 기록은 지우지 않는다).

-- ── 역할 ───────────────────────────────────────────────────────────
-- CHECK 를 먼저 풀어야 값을 옮길 수 있다. 옮긴 뒤에 새 CHECK 를 건다.
ALTER TABLE agents DROP CONSTRAINT agents_role_check;
UPDATE agents SET role = 'STAFF' WHERE role = 'AGENT';
ALTER TABLE agents
    ALTER COLUMN role SET DEFAULT 'STAFF',
    ADD CONSTRAINT agents_role_check CHECK (role IN ('ADMIN', 'STAFF', 'PART_TIME'));

ALTER TABLE agent_invites DROP CONSTRAINT agent_invites_role_check;
UPDATE agent_invites SET role = 'STAFF' WHERE role = 'AGENT';
ALTER TABLE agent_invites
    ADD CONSTRAINT agent_invites_role_check CHECK (role IN ('ADMIN', 'STAFF', 'PART_TIME'));

-- ── 승인 ───────────────────────────────────────────────────────────
-- 승인 대기 = approved_at IS NULL. 비활성(내보낸 사람)과 구분해야 "다시 활성화" 가 승인을 건너뛰지 못한다.
ALTER TABLE agents ADD COLUMN approved_at TIMESTAMPTZ;
UPDATE agents SET approved_at = created_at;
-- 팀 화면의 승인 대기 목록과 "워크스페이스당 50건" 세기가 이 색인을 탄다. 대기 계정은 드물어 작다.
CREATE INDEX agents_pending_idx ON agents (workspace_id, created_at) WHERE approved_at IS NULL;

-- ── 상담 삭제 ──────────────────────────────────────────────────────
ALTER TABLE conversations
    ADD COLUMN deleted_at    TIMESTAMPTZ,
    ADD COLUMN deleted_by    UUID REFERENCES agents (id) ON DELETE SET NULL,
    ADD COLUMN delete_reason TEXT;

-- 받은함은 가린 상담을 늘 뺀다. 같은 조건을 색인에 넣어 둬야 정렬 색인을 그대로 탄다.
-- 내보내기는 가린 것까지 훑지만 conversations_workspace_created_idx 를 쓴다.
DROP INDEX conversations_workspace_inbox_idx;
CREATE INDEX conversations_workspace_inbox_idx
    ON conversations (workspace_id, last_message_at DESC NULLS LAST, created_at DESC)
    WHERE deleted_at IS NULL;
