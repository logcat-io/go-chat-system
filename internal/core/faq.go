package core

// FAQItem 은 위젯 홈의 "자주 묻는 질문" 한 줄이다(D36). 질문을 누르면 답이 펼쳐진다.
// 순서는 목록의 순서 그대로다. 정렬 키를 따로 두지 않는다 — 관리자가 고른 순서가 곧 보이는 순서다.
type FAQItem struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}
