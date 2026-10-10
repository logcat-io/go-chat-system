-- 0006: 제품으로 내보내기 위한 뼈대.
--  1. 상담원 역할·활성 여부·초대
--  2. 로그인 고객 연동 (호스트 사이트의 회원 id)
--  3. 워크스페이스 설정 (운영시간·자동 메시지·위젯 모양)
--  4. 자동 응답을 보낸 흔적과 BOT 발신자

-- ── 상담원 ─────────────────────────────────────────────────────────
ALTER TABLE agents
    ADD COLUMN role   TEXT    NOT NULL DEFAULT 'AGENT' CHECK (role IN ('ADMIN', 'AGENT')),
    ADD COLUMN active BOOLEAN NOT NULL DEFAULT true;

-- 첫 상담원이 관리자다. 관리자가 한 명도 없으면 아무도 팀을 못 들인다.
UPDATE agents SET role = 'ADMIN'
 WHERE id = (SELECT id FROM agents ORDER BY created_at LIMIT 1);

CREATE TABLE agent_invites (
    token_hash BYTEA       PRIMARY KEY,
    email      TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    role       TEXT        NOT NULL CHECK (role IN ('ADMIN', 'AGENT')),
    invited_by UUID        REFERENCES agents (id) ON DELETE SET NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX agent_invites_email_idx ON agent_invites (lower(email)) WHERE used_at IS NULL;

-- ── 고객 ───────────────────────────────────────────────────────────
-- external_id 는 호스트 사이트의 회원 식별자다. 서명된 토큰으로만 들어온다.
ALTER TABLE customers ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX customers_external_idx ON customers (external_id) WHERE external_id IS NOT NULL;

-- ── 설정 ───────────────────────────────────────────────────────────
-- 키마다 JSON 한 덩어리. 운영시간처럼 모양이 자주 바뀌는 값에 컬럼을 파지 않는다.
CREATE TABLE settings (
    key        TEXT        PRIMARY KEY,
    value      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID        REFERENCES agents (id) ON DELETE SET NULL
);

-- ── 자동 응답 ──────────────────────────────────────────────────────
ALTER TABLE messages DROP CONSTRAINT messages_sender_kind_check;
ALTER TABLE messages ADD CONSTRAINT messages_sender_kind_check
    CHECK (sender_kind IN ('CUSTOMER', 'AGENT', 'SYSTEM', 'BOT'));

ALTER TABLE messages DROP CONSTRAINT messages_sender_matches;
ALTER TABLE messages ADD CONSTRAINT messages_sender_matches CHECK (
    (sender_kind = 'CUSTOMER' AND sender_customer IS NOT NULL AND sender_agent_id IS NULL) OR
    (sender_kind = 'AGENT'    AND sender_agent_id IS NOT NULL AND sender_customer IS NULL) OR
    (sender_kind IN ('SYSTEM', 'BOT') AND sender_agent_id IS NULL AND sender_customer IS NULL)
);

-- 대화마다 자동 첫 응답은 한 번이다. 보낸 시각을 남겨 두 번 보내지 않는다.
ALTER TABLE conversations ADD COLUMN auto_replied_at TIMESTAMPTZ;
-- 상담원이 처음 답한 시각. 첫 응답 시간 통계의 원천이고, 자동 응답을 보낼지 판단하는 근거다.
ALTER TABLE conversations ADD COLUMN first_agent_reply_at TIMESTAMPTZ;
