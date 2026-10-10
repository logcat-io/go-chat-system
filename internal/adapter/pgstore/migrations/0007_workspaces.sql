-- 워크스페이스 = 테넌트. 한 설치가 여러 고객사를 받는다.
-- 모든 테넌트 소유 행에 workspace_id 를 붙이고, 기존 행은 기본 워크스페이스로 옮긴다.
-- 메시지·읽음·팔로워·첨부는 대화를 거쳐 소속이 정해지므로 컬럼을 붙이지 않는다.

CREATE TABLE workspaces (
    id              UUID        PRIMARY KEY,
    slug            TEXT        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$'),
    name            TEXT        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    widget_key      TEXT        NOT NULL UNIQUE,
    -- member_secret 은 비어 있으면 서버가 부팅 때 난수로 채운다. SQL 의 random() 은 비밀로 쓰기에 약하다.
    member_secret   TEXT        NOT NULL DEFAULT '',
    allowed_origins TEXT[]      NOT NULL DEFAULT '{}',
    plan            TEXT        NOT NULL DEFAULT 'free',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 기본 워크스페이스는 설치 운영자 자신의 것이라 business(무제한)로 시작한다. 그 뒤는 운영자 토큰으로만 바꾼다.
INSERT INTO workspaces (id, slug, name, widget_key, plan)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', '기본 워크스페이스',
        'wk_' || md5(random()::text || clock_timestamp()::text), 'business');

-- 상담원: 이메일 유일성이 워크스페이스 안으로 좁아진다.
ALTER TABLE agents ADD COLUMN workspace_id UUID;
UPDATE agents SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE agents ALTER COLUMN workspace_id SET NOT NULL,
                   ADD CONSTRAINT agents_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
ALTER TABLE agents DROP CONSTRAINT agents_email_key;
CREATE UNIQUE INDEX agents_workspace_email_idx ON agents (workspace_id, lower(email));

-- 고객
ALTER TABLE customers ADD COLUMN workspace_id UUID, ADD COLUMN anonymized_at TIMESTAMPTZ;
UPDATE customers SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE customers ALTER COLUMN workspace_id SET NOT NULL,
                      ADD CONSTRAINT customers_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
DROP INDEX customers_external_idx;
CREATE UNIQUE INDEX customers_external_idx ON customers (workspace_id, external_id) WHERE external_id IS NOT NULL;

-- 대화: 받은함·사용량 집계가 워크스페이스 단위라 인덱스도 그 순서다.
ALTER TABLE conversations ADD COLUMN workspace_id UUID;
UPDATE conversations SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE conversations ALTER COLUMN workspace_id SET NOT NULL,
                          ADD CONSTRAINT conversations_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
CREATE INDEX conversations_workspace_inbox_idx ON conversations (workspace_id, last_message_at DESC NULLS LAST, created_at DESC);
CREATE INDEX conversations_workspace_created_idx ON conversations (workspace_id, created_at);

-- 세션: 주체 조회 한 번에 소속까지 나오게 비정규화한다.
ALTER TABLE sessions ADD COLUMN workspace_id UUID;
UPDATE sessions s SET workspace_id = a.workspace_id FROM agents a WHERE a.id = s.agent_id;
UPDATE sessions s SET workspace_id = c.workspace_id FROM customers c WHERE c.id = s.customer_id;
DELETE FROM sessions WHERE workspace_id IS NULL;
ALTER TABLE sessions ALTER COLUMN workspace_id SET NOT NULL;

-- 태그·빠른 답변·초대·설정
ALTER TABLE tags ADD COLUMN workspace_id UUID;
UPDATE tags SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE tags ALTER COLUMN workspace_id SET NOT NULL,
                 ADD CONSTRAINT tags_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
DROP INDEX tags_name_idx;
CREATE UNIQUE INDEX tags_name_idx ON tags (workspace_id, lower(name));

ALTER TABLE snippets ADD COLUMN workspace_id UUID;
UPDATE snippets SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE snippets ALTER COLUMN workspace_id SET NOT NULL,
                     ADD CONSTRAINT snippets_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
DROP INDEX snippets_shortcut_idx;
CREATE UNIQUE INDEX snippets_shortcut_idx ON snippets (workspace_id, lower(shortcut));

ALTER TABLE agent_invites ADD COLUMN workspace_id UUID;
UPDATE agent_invites SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE agent_invites ALTER COLUMN workspace_id SET NOT NULL,
                          ADD CONSTRAINT agent_invites_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;

ALTER TABLE settings ADD COLUMN workspace_id UUID;
UPDATE settings SET workspace_id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE settings ALTER COLUMN workspace_id SET NOT NULL,
                     ADD CONSTRAINT settings_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces (id) ON DELETE CASCADE;
ALTER TABLE settings DROP CONSTRAINT settings_pkey;
ALTER TABLE settings ADD PRIMARY KEY (workspace_id, key);

-- 감사 로그: 누가 설정·역할·배정을 바꿨나. 지우지 않는다.
CREATE TABLE audit_log (
    id           BIGSERIAL   PRIMARY KEY,
    workspace_id UUID        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    actor_id     UUID,
    actor_name   TEXT        NOT NULL DEFAULT '',
    action       TEXT        NOT NULL CHECK (char_length(action) BETWEEN 1 AND 60),
    target       TEXT        NOT NULL DEFAULT '',
    detail       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_workspace_idx ON audit_log (workspace_id, created_at DESC);
