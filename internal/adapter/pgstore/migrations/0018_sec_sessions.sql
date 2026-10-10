-- 토큰 보안 묶음(D46). 세션의 유휴 만료, WebSocket 일회용 티켓, 세션이 끝나면 열린 연결에 알리는 트리거.

-- 유휴 만료. 마지막으로 쓴 시각과 그 세션의 유휴 한도다. 앱은 요청마다 쓰지 않고 1분 단위로 묶어 갱신한다.
-- 한도는 세션을 만들 때 정한다(고객·상담원이 다르다). 비어 있으면 유휴 만료가 없다 — 절대 수명(expires_at)만 본다.
ALTER TABLE sessions ADD COLUMN last_used_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE sessions ADD COLUMN idle_for INTERVAL;
-- 이미 있는 세션에도 운영 기본값을 건다. 마지막 사용 시각은 이 마이그레이션 시각이라 바로 끊기지는 않는다.
UPDATE sessions SET idle_for = CASE subject_kind WHEN 'AGENT' THEN interval '12 hours' ELSE interval '7 days' END;

-- 고객 한 명의 세션을 최신 몇 개만 남길 때 고른다(회원이 identify 를 되풀이해도 행이 끝없이 쌓이지 않게).
CREATE INDEX sessions_customer_created_idx ON sessions (customer_id, created_at DESC) WHERE customer_id IS NOT NULL;

-- WebSocket 티켓. 30초짜리 일회용이고 세션에 묶인다. 쓰는 것은 DELETE ... RETURNING 한 문장이라 동시에 두 번 써도 하나만 이긴다.
-- 세션이 지워지면(로그아웃·비활성·만료 정리) 그 세션의 티켓도 같이 지워진다.
CREATE TABLE ws_tickets (
    ticket_hash  BYTEA       PRIMARY KEY,
    session_hash BYTEA       NOT NULL REFERENCES sessions (token_hash) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX ws_tickets_session_idx ON ws_tickets (session_hash);
CREATE INDEX ws_tickets_expires_at_idx ON ws_tickets (expires_at);

-- 세션 행이 지워지면 커밋 때 그 세션 참조(토큰 해시의 16진수)를 알린다. 서버마다 그 세션으로 열린 WebSocket 을 곧바로 다시 확인해 닫는다.
-- 로그아웃·비활성·비밀번호 변경·회원 비밀 재발급·고객 익명화·만료 정리, 어느 길로 지워도 여기를 지난다. 알림은 잃어도 된다 —
-- 열린 연결은 60초마다 세션을 다시 묻는다. 한 문장이 여러 행을 지우면 100개씩 묶어 보낸다(알림 한 건의 상한 8000바이트).
CREATE FUNCTION sessions_ended_notify() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    chunk TEXT;
BEGIN
    FOR chunk IN
        SELECT string_agg(encode(token_hash, 'hex'), ',')
          FROM (SELECT token_hash, (row_number() OVER () - 1) / 100 AS grp FROM ended) t
         GROUP BY grp
    LOOP
        PERFORM pg_notify('chat_session_ended', chunk);
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER sessions_ended AFTER DELETE ON sessions
    REFERENCING OLD TABLE AS ended FOR EACH STATEMENT EXECUTE FUNCTION sessions_ended_notify();
