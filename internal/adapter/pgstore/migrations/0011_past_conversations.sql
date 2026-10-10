-- 회원은 지난 상담을 다시 볼 수 없다. 새 상담이 열리는 순간 그 회원의 앞선 상담들은 "지난 상담" 이 된다.
-- 목록에서만 빼면 id 를 아는 옛 탭이 계속 읽고 구독한다. 권한 검사가 대화 한 줄만 보고 판정할 수 있게 표식을 대화에 둔다.
-- (대화를 읽을 때마다 "더 뒤의 상담이 있나" 를 다시 묻지 않는다. 전달 루프가 매번 지나는 길이다.)
ALTER TABLE conversations ADD COLUMN past_for_customer BOOLEAN NOT NULL DEFAULT false;

-- 이미 있는 데이터: 회원의 종료된 상담 가운데 더 뒤에 열린 상담이 있는 것.
UPDATE conversations c
   SET past_for_customer = true
  FROM customers cu
 WHERE cu.id = c.customer_id AND COALESCE(cu.external_id, '') <> '' AND c.state = 'CLOSED'
   AND EXISTS (SELECT 1 FROM conversations n
                WHERE n.customer_id = c.customer_id AND (n.created_at, n.id) > (c.created_at, c.id));
