package core

import (
	"time"
)

const (
	LiveTyping = "typing"
	LiveRead   = "read"
)

// LiveEvent 는 잃어도 되는 이벤트다. 메시지·읽음 지점과 달리 재전송·캐치업 수단이 없다.
// 읽음은 DB 에 남으므로 이 이벤트를 놓쳐도 재접속하면 ready 프레임으로 맞춰진다.
type LiveEvent struct {
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	// WorkspaceID 는 받은함 신호를 테넌트 안에서만 돌리기 위한 것이다. 화면에는 안 나간다.
	WorkspaceID string `json:"ws,omitempty"`
	ActorKind   string `json:"actor_kind"`
	ActorID     string `json:"actor_id"`
	ActorName   string `json:"actor_name"`
	// Seq 는 전체 번호, PublicSeq 는 고객 번호다. 받는 쪽이 자기 공간의 값을 고른다.
	Seq       int64 `json:"seq,omitempty"`
	PublicSeq int64 `json:"public_seq,omitempty"`
}

// ViewerSeq 는 보는 사람의 번호 공간에 맞는 값이다.
func (e LiveEvent) ViewerSeq(internal bool) int64 {
	if internal {
		return e.Seq
	}
	return e.PublicSeq
}

// ReadState 는 한 참여자의 읽음 지점이다.
//
// 읽음 지점은 언제나 전체 번호(seq)로 저장한다. 고객은 공개 번호로 보고하므로
// 저장할 때 전체 번호로 옮기고, 돌려줄 때 보는 사람의 번호로 다시 옮긴다.
// 두 공간을 섞으면 "상담원이 3번까지 읽었다"가 고객 화면에서 엉뚱한 줄에 붙는다.
type ReadState struct {
	ReaderKind string `json:"reader_kind"`
	ReaderID   string `json:"reader_id"`
	ReaderName string `json:"reader_name"`
	// LastReadSeq 는 전체 번호 공간이다. 상담원 화면이 쓴다.
	LastReadSeq int64 `json:"last_read_seq"`
	// LastReadPublicSeq 는 고객 번호 공간으로 옮긴 값이다. 고객 화면이 쓴다.
	LastReadPublicSeq int64     `json:"last_read_public_seq"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ViewerSeq 는 보는 사람의 번호 공간에 맞는 읽음 지점이다.
func (r ReadState) ViewerSeq(internal bool) int64 {
	if internal {
		return r.LastReadSeq
	}
	return r.LastReadPublicSeq
}
