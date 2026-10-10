-- 익명 고객 세션은 만료되지 않는다(D46 고침, 2026-10-08). 교환·반품처럼 브랜드사 처리가 길어지면 고객은 몇 주~몇 달 뒤 같은 브라우저로 돌아온다.
-- 그때 지난 상담이 그대로 이어져야 한다. 익명 고객 세션은 로그아웃과 고객 삭제로만 끝난다.
-- v0.0.5 배포 전에 고쳤다(감사 21): 업그레이드 때 이미 끝난 세션을 되살렸다. 이 파일은 어디에도 적용되기 전이다.

-- 절대 수명이 비어 있으면 절대 만료가 없다(유휴 한도 idle_for 가 비어 있으면 유휴 만료가 없는 것과 같은 뜻이다).
-- 상담원 세션과 회원 고객 세션은 앱이 늘 값을 넣는다 — 상담원은 CHAT_SESSION_TTL, 회원은 그것과 회원 토큰 exp 중 이른 쪽이다.
ALTER TABLE sessions ALTER COLUMN expires_at DROP NOT NULL;

-- 업그레이드 시점에 이미 끝난 고객 세션(절대 수명이나 0018 의 유휴 7일이 지났다)은 지운다. 아래에서 수명·한도를 지우면 되살아난다 —
-- 이 세션들은 만료 정리(1시간 주기)를 기다리던 것이고, 정리와 같은 판정이다. 지우면 트리거(0018)가 열린 연결에 알린다.
DELETE FROM sessions
 WHERE subject_kind = 'CUSTOMER'
   AND (expires_at <= now() OR (idle_for IS NOT NULL AND last_used_at + idle_for <= now()));

-- 고객 세션의 유휴 한도는 이제 행이 아니라 판정 시점의 설정(CHAT_SESSION_IDLE_CUSTOMER, 기본 0)으로 본다(감사 21). 행의 값은 쓰이지 않으니 비운다.
-- 상담원 세션의 유휴 한도(12시간)는 행의 값 그대로다.
UPDATE sessions SET idle_for = NULL WHERE subject_kind = 'CUSTOMER';
-- 익명 고객(external_id 없음)의 살아 있는 세션은 절대 수명도 지운다. 회원 고객의 세션은 회원 토큰 exp 에 묶인 수명을 그대로 둔다.
UPDATE sessions s SET expires_at = NULL
  FROM customers c
 WHERE s.customer_id = c.id AND c.external_id IS NULL AND s.expires_at > now();
