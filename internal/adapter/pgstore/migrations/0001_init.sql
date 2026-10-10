-- 0001_init: 상담 대화와 메시지 로그
-- seq 는 애플리케이션이 conversations.last_seq 에서 발급한다 (docs/design.md D2).
-- IDENTITY/SEQUENCE 를 쓰면 롤백 때 빈칸이 생겨 유실 오판을 만든다.

CREATE TABLE agents (
    id            UUID        PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE customers (
    id           UUID        PRIMARY KEY,
    display_name TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash   BYTEA       PRIMARY KEY,
    subject_kind TEXT        NOT NULL CHECK (subject_kind IN ('AGENT', 'CUSTOMER')),
    agent_id     UUID        REFERENCES agents (id)    ON DELETE CASCADE,
    customer_id  UUID        REFERENCES customers (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    CONSTRAINT sessions_subject_matches CHECK (
        (subject_kind = 'AGENT'    AND agent_id IS NOT NULL AND customer_id IS NULL) OR
        (subject_kind = 'CUSTOMER' AND customer_id IS NOT NULL AND agent_id IS NULL)
    )
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE conversations (
    id              UUID        PRIMARY KEY,
    customer_id     UUID        NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    state           TEXT        NOT NULL DEFAULT 'OPEN' CHECK (state IN ('OPEN', 'CLOSED')),
    subject         TEXT        NOT NULL DEFAULT '',
    last_seq        BIGINT      NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_message_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 고객 한 명이 동시에 여는 대화는 하나다. 위젯이 세션마다 새 대화를 만들지 않게 막는다.
CREATE UNIQUE INDEX conversations_one_open_per_customer
    ON conversations (customer_id)
    WHERE state = 'OPEN';

CREATE INDEX conversations_inbox_idx
    ON conversations (last_message_at DESC NULLS LAST, created_at DESC);

CREATE TABLE messages (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             BIGINT      NOT NULL CHECK (seq > 0),
    id              UUID        NOT NULL UNIQUE,
    client_msg_id   TEXT        NOT NULL CHECK (char_length(client_msg_id) BETWEEN 1 AND 128),
    sender_kind     TEXT        NOT NULL CHECK (sender_kind IN ('CUSTOMER', 'AGENT', 'SYSTEM')),
    sender_agent_id UUID        REFERENCES agents (id)    ON DELETE SET NULL,
    sender_customer UUID        REFERENCES customers (id) ON DELETE SET NULL,
    body            TEXT        NOT NULL CHECK (char_length(body) BETWEEN 1 AND 4000),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, seq),
    CONSTRAINT messages_sender_matches CHECK (
        (sender_kind = 'CUSTOMER' AND sender_customer IS NOT NULL AND sender_agent_id IS NULL) OR
        (sender_kind = 'AGENT'    AND sender_agent_id IS NOT NULL AND sender_customer IS NULL) OR
        (sender_kind = 'SYSTEM'   AND sender_agent_id IS NULL     AND sender_customer IS NULL)
    )
);

-- 재전송이 같은 행으로 수렴하게 만드는 제약. 멱등의 마지막 방어선이다.
CREATE UNIQUE INDEX messages_idempotency_idx
    ON messages (conversation_id, client_msg_id);
