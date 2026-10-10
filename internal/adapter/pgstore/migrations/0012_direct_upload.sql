-- 직접 업로드. 서버는 주소를 발급할 때 행을 만들고, 파일이 저장소에 올라온 것을 확인한 뒤에 uploaded_at 을 찍는다.
-- 확인되지 않은 행은 메시지에 붙일 수 없다. 주소만 받고 올리지 않은 행은 고아 정리가 치운다.
ALTER TABLE attachments ADD COLUMN uploaded_at TIMESTAMPTZ;

-- 지금까지의 첨부는 서버가 받아서 저장한 것이라 전부 확인된 것이다.
UPDATE attachments SET uploaded_at = created_at;
