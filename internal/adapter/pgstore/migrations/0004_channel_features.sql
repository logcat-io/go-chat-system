-- 0004: 채널톡 수준의 상담 기능.
--
-- 핵심 변경 둘.
--  1. 고객 한 명이 대화를 여러 개 갖는다 (0001 의 "열린 대화 하나" 제약을 뗀다).
--     전송 대기 큐가 고아가 되는 문제는 브라우저가 저장된 모든 대화의 큐를 밀도록 고쳐서 이미 해결했다.
--  2. 메시지에 공개 범위가 생긴다. 내부 메모는 고객에게 전달되지 않는다.
--     이때 번호를 한 벌만 쓰면 고객 쪽 번호에 빈칸이 생기고, 클라이언트가 그걸 유실로 읽어
--     끝없이 복구를 돈다. 그래서 대화마다 번호를 두 벌 돌린다.
--       seq         모든 메시지. 상담원이 보는 번호
--       public_seq  공개 메시지만. 고객이 보는 번호. 내부 메모는 NULL
--     둘 다 같은 UPDATE 로 발급하므로 0001 의 불변식(연속·커밋 순서 일치)이 양쪽에 그대로 산다.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ── 대화 ────────────────────────────────────────────────────────────
DROP INDEX conversations_one_open_per_customer;

ALTER TABLE conversations DROP CONSTRAINT conversations_state_check;
ALTER TABLE conversations
    ADD CONSTRAINT conversations_state_check CHECK (state IN ('OPEN', 'SNOOZED', 'CLOSED'));

ALTER TABLE conversations
    ADD COLUMN assignee_agent_id UUID REFERENCES agents (id) ON DELETE SET NULL,
    ADD COLUMN snoozed_until     TIMESTAMPTZ,
    ADD COLUMN last_public_seq   BIGINT NOT NULL DEFAULT 0 CHECK (last_public_seq >= 0);

-- 기존 대화는 전부 공개 메시지만 갖고 있다.
UPDATE conversations SET last_public_seq = last_seq;

CREATE INDEX conversations_assignee_idx ON conversations (assignee_agent_id, last_message_at DESC);
CREATE INDEX conversations_customer_idx ON conversations (customer_id, last_message_at DESC NULLS LAST);
CREATE INDEX conversations_snoozed_idx ON conversations (snoozed_until) WHERE state = 'SNOOZED';

-- ── 메시지 ──────────────────────────────────────────────────────────
ALTER TABLE messages
    ADD COLUMN visibility TEXT NOT NULL DEFAULT 'PUBLIC'
        CHECK (visibility IN ('PUBLIC', 'PRIVATE')),
    ADD COLUMN public_seq BIGINT CHECK (public_seq > 0);

UPDATE messages SET public_seq = seq WHERE public_seq IS NULL;

-- 고객이 보는 번호도 대화 안에서 유일하다.
CREATE UNIQUE INDEX messages_public_seq_idx
    ON messages (conversation_id, public_seq)
    WHERE public_seq IS NOT NULL;

-- 내부 메모는 고객 번호를 받지 않는다. 공개 메시지는 반드시 받는다.
ALTER TABLE messages ADD CONSTRAINT messages_public_seq_matches CHECK (
    (visibility = 'PUBLIC'  AND public_seq IS NOT NULL) OR
    (visibility = 'PRIVATE' AND public_seq IS NULL)
);

-- 첨부만 보내는 메시지는 본문이 비어 있다.
ALTER TABLE messages DROP CONSTRAINT messages_body_check;
ALTER TABLE messages ADD CONSTRAINT messages_body_check CHECK (char_length(body) <= 4000);

CREATE INDEX messages_search_idx ON messages USING gin (body gin_trgm_ops);
CREATE INDEX messages_created_idx ON messages (created_at DESC);

-- ── 첨부 ────────────────────────────────────────────────────────────
CREATE TABLE attachments (
    id              UUID        PRIMARY KEY,
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    -- 업로드 직후에는 아직 메시지가 없다. 전송할 때 이어 붙인다.
    message_id      UUID        REFERENCES messages (id) ON DELETE CASCADE,
    uploader_kind   TEXT        NOT NULL CHECK (uploader_kind IN ('CUSTOMER', 'AGENT')),
    uploader_id     UUID        NOT NULL,
    filename        TEXT        NOT NULL CHECK (char_length(filename) BETWEEN 1 AND 255),
    content_type    TEXT        NOT NULL,
    byte_size       BIGINT      NOT NULL CHECK (byte_size > 0),
    storage_path    TEXT        NOT NULL,
    width           INT,
    height          INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX attachments_message_idx ON attachments (message_id);
CREATE INDEX attachments_orphan_idx ON attachments (created_at) WHERE message_id IS NULL;

-- ── 담당자·팔로워 ───────────────────────────────────────────────────
CREATE TABLE conversation_followers (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    agent_id        UUID        NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, agent_id)
);

CREATE INDEX conversation_followers_agent_idx ON conversation_followers (agent_id);

-- ── 태그 ────────────────────────────────────────────────────────────
CREATE TABLE tags (
    id         UUID        PRIMARY KEY,
    name       TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 40),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX tags_name_idx ON tags (lower(name));

CREATE TABLE conversation_tags (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    tag_id          UUID        NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, tag_id)
);

-- ── 빠른 답변 ───────────────────────────────────────────────────────
CREATE TABLE snippets (
    id         UUID        PRIMARY KEY,
    shortcut   TEXT        NOT NULL CHECK (char_length(shortcut) BETWEEN 1 AND 40),
    title      TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 80),
    body       TEXT        NOT NULL CHECK (char_length(body) BETWEEN 1 AND 4000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX snippets_shortcut_idx ON snippets (lower(shortcut));

-- ── 고객 프로필 ─────────────────────────────────────────────────────
ALTER TABLE customers
    ADD COLUMN email      TEXT,
    ADD COLUMN phone      TEXT,
    ADD COLUMN note       TEXT NOT NULL DEFAULT '',
    ADD COLUMN attributes JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN last_seen_at TIMESTAMPTZ;

CREATE INDEX customers_email_idx ON customers (lower(email)) WHERE email IS NOT NULL;
CREATE INDEX customers_name_idx ON customers USING gin (display_name gin_trgm_ops);
