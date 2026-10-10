package core

// Change 는 "이 대화에 변화가 있다" 신호다. 본문은 없고 번호만 있다.
type Change struct {
	ConversationID string
	WorkspaceID    string
	Seq            int64
	PublicSeq      int64
	// Meta 가 true 면 상태·담당자·태그가 바뀌었다. 받는 쪽이 대화 행을 다시 읽어야 한다.
	Meta bool
}
