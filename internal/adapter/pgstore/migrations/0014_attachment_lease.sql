-- 첨부 완료의 임대. 완료를 확인하는 요청이 이 시각까지 그 첨부를 맡는다. 비어 있거나 지났으면 다른 요청이 가져갈 수 있다.
-- 세션 자문 잠금은 커넥션을 쥔 채 기다려서, 동시 완료가 풀 크기에 닿으면 잠금 안의 작업이 쓸 커넥션이 없어 서버 전체가 멈췄다.
-- 임대는 짧은 UPDATE 한 문장으로 잡고 커넥션을 바로 돌려준다.
ALTER TABLE attachments ADD COLUMN completing_until TIMESTAMPTZ;
