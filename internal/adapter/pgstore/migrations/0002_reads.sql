-- 0002_reads: 어디까지 읽었는지. 읽음 지점은 되돌아가지 않는다 (불변식 I7).
--
-- 타이핑 표시용 테이블은 두지 않는다. 타이핑은 휘발성 신호라서 pg_notify 로만 흐르고,
-- 받는 쪽 버퍼가 차면 버린다. 메시지·읽음과 보장 수준이 다르다는 걸 스키마로 드러낸다.

CREATE TABLE conversation_reads (
    conversation_id UUID        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    reader_kind     TEXT        NOT NULL CHECK (reader_kind IN ('CUSTOMER', 'AGENT')),
    reader_agent_id UUID        REFERENCES agents (id)    ON DELETE CASCADE,
    reader_customer UUID        REFERENCES customers (id) ON DELETE CASCADE,
    last_read_seq   BIGINT      NOT NULL CHECK (last_read_seq >= 0),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT conversation_reads_reader_matches CHECK (
        (reader_kind = 'AGENT'    AND reader_agent_id IS NOT NULL AND reader_customer IS NULL) OR
        (reader_kind = 'CUSTOMER' AND reader_customer IS NOT NULL AND reader_agent_id IS NULL)
    )
);

-- 읽는 주체마다 대화별로 한 행이다. 부분 unique 인덱스를 upsert 의 충돌 대상으로 쓴다.
CREATE UNIQUE INDEX conversation_reads_agent_idx
    ON conversation_reads (conversation_id, reader_agent_id)
    WHERE reader_agent_id IS NOT NULL;

CREATE UNIQUE INDEX conversation_reads_customer_idx
    ON conversation_reads (conversation_id, reader_customer)
    WHERE reader_customer IS NOT NULL;
