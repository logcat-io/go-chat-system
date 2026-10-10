-- 위젯 FAQ(D36). 워크스페이스마다 한 행, 목록 전체를 JSON 배열 하나로 둔다.
-- 항목은 늘 통째로 읽고 통째로 바꾼다(순서 바꾸기도 전체 다시 쓰기다). 행이 없으면 아직 정하지 않은 것이고 앱이 기본 목록을 보인다.
-- 빈 배열은 "FAQ 를 두지 않는다" 다. 개수·길이 한도는 app 이 지킨다(10개, 질문 80자, 답 500자). 여기서는 배열인지만 본다.
CREATE TABLE widget_faq (
    workspace_id UUID        PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    items        JSONB       NOT NULL CHECK (jsonb_typeof(items) = 'array'),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by   UUID        REFERENCES agents (id) ON DELETE SET NULL
);
