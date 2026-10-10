-- 0003: 메시지 시각을 문장 시각으로 바꾼다.
-- now() 는 트랜잭션 시작 시각이라, 먼저 시작하고 늦게 커밋한 트랜잭션이 더 이른 시각을 받는다.
-- seq 는 커밋 순서를 따르므로 화면에서 "#5 가 #4 보다 이른 시각" 으로 보인다.
ALTER TABLE messages ALTER COLUMN created_at SET DEFAULT clock_timestamp();
