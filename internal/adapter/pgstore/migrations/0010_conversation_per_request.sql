-- 상담마다 대화 하나. 0009 는 회원이 방 하나를 이어 쓰게 했고, 고객에게 이전 회차를 가리려고 가시 시작점을 뒀다.
-- 그 바닥은 본문이 나가는 길마다 다시 검사해야 했다(목록 미리보기·수정 이벤트·첨부·읽음 지점·고객의 옛 글 수정).
-- 이제 종료된 상담은 그대로 닫히고 다시 문의하면 새 대화가 열린다. 상담원은 고객 단위로 이전 상담을 이어 본다.
-- 고객에게 보이는 것은 대화 하나의 전부라 바닥이 필요 없다.
--
-- 전제: 0009 로 운영한 데이터가 없다(0009 와 0010 은 같은 날 만들었고 배포 전이다). 0009 에서 재상담(episodes > 1)이
-- 있던 방이 있다면 이 마이그레이션 뒤에 가려졌던 이전 회차가 그 고객에게 보인다. 그런 DB 라면 먼저 회차 경계로 대화를 쪼갠다.
-- 컬럼을 지우므로 0009 까지만 아는 바이너리로는 되돌릴 수 없다.
ALTER TABLE conversations
    DROP COLUMN customer_visible_from,
    DROP COLUMN episode_from_seq,
    DROP COLUMN episodes;

-- 고객의 상담을 시간순으로 훑는다: 이전 상담 이어 보기, 가장 최근 상담 찾기.
CREATE INDEX conversations_customer_timeline_idx ON conversations (customer_id, created_at DESC, id DESC);
