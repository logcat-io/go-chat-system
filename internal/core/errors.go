package core

import "errors"

// CodedError 는 고정 코드를 가진 오류다(D47). 코드는 로그의 err_kind·reason 에 실리는 안정된 이름이고, 문구(Error)는 개발자가 읽는 설명이다.
//
// 로그에는 오류 원문을 싣지 않는다(D30) — 원문에는 입력값·접속 정보가 섞일 수 있다. 원문 대신 코드를 싣는다.
// 코드는 리포에 고정된 영어 소문자 이름이다(lease_lost, s3_403 …). 사용자 입력이나 바깥 응답의 글자를 코드에 넣지 않는다.
// parent 가 있으면 그 오류이기도 하다(errors.Is). 예: app.ErrAlreadyTaken 은 already_taken 이면서 ErrConflict 다.
type CodedError struct {
	code, msg string
	parent    error
}

func (e *CodedError) Error() string { return e.msg }

// ErrCode 는 고정 코드다. logctx.ErrKind 가 이 메서드를 먼저 본다(인터페이스로 본다 — logctx 는 core 를 모른다).
func (e *CodedError) ErrCode() string { return e.code }

func (e *CodedError) Unwrap() error { return e.parent }

// NewError 는 고정 코드를 가진 오류를 만든다. 표지(sentinel)로 쓰면 errors.Is 로 비교한다.
func NewError(code, msg string) error { return &CodedError{code: code, msg: msg} }

// WrapError 는 parent 의 한 갈래인 오류를 만든다. errors.Is(err, parent) 가 참이고 코드는 더 좁은 이름이다.
func WrapError(parent error, code, msg string) error {
	return &CodedError{code: code, msg: msg, parent: parent}
}

// ErrCode 는 오류 사슬에서 가장 바깥의(가장 좁은) 고정 코드다. 없으면 빈 문자열이다.
func ErrCode(err error) string {
	var c interface{ ErrCode() string }
	if errors.As(err, &c) {
		return c.ErrCode()
	}
	return ""
}

// 도메인 오류. 저장소·전달 계층이 이 값으로 말하고, 애플리케이션 계층이 HTTP 로 옮기기 전의 뜻을 정한다.
// 문구는 개발자가 읽는다(테스트 실패, 패닉). 사용자 문구는 httpapi·realtime 이 따로 정한다.
var (
	ErrNotFound = NewError("not_found", "core: 찾는 행이 없다(없거나, 다른 워크스페이스의 것이거나, 지운 것이다)")
	// ErrConversationEnded 는 종료된 지난 상담을 다시 열려 할 때다. 그 회원에게는 더 최근 상담이 있다.
	ErrConversationEnded = NewError("ended", "core: 종료된 지난 상담이라 다시 열거나 쓸 수 없다. 그 회원에게는 더 최근 상담이 있다")
	// ErrClosedByCustomer 는 고객이 직접 닫은 상담에 그 고객이 새 글을 쓰려 할 때다(D48). 닫은 상담은 고객 쪽에서 읽기 전용이고 다시 열리지 않는다.
	// 상담원이 다시 열기 전까지 다시 보내도 결과가 같다.
	ErrClosedByCustomer = NewError("closed_by_customer", "core: 고객이 직접 닫은 상담이라 고객의 새 글을 받지 않았다. 상담원이 다시 열기 전까지 같다")
	// ErrTooManyRetries 는 동시에 같은 행을 고치는 요청이 겹쳐 정해진 횟수만큼 다시 시도하고도 끝내지 못했을 때다.
	ErrTooManyRetries = NewError("too_many_retries", "core: 같은 행을 고치는 요청이 겹쳐 정해진 횟수만큼 다시 시도했지만 끝내지 못했다")
	ErrDuplicate      = NewError("duplicate", "core: 같은 값(유일 키)을 가진 행이 이미 있다")
	// ErrInvalidReference 는 없는 대상을 가리켰을 때다. 서버 잘못이 아니라 입력 잘못이라
	// 500 이 아니라 400 으로 나가야 한다.
	ErrInvalidReference = NewError("invalid_reference", "core: 요청이 가리키는 행(외래 키)이 없다")
	// ErrSettingMissing 은 아직 저장한 적 없는 설정이다. 기본값을 쓰라는 뜻이지 실패가 아니다.
	ErrSettingMissing = NewError("setting_missing", "core: 아직 저장한 설정이 없다. 기본값을 쓴다")
	// ErrTooManyPending 은 대화에 보내지 않은 첨부가 상한에 닿았을 때다.
	ErrTooManyPending = NewError("too_many_pending", "core: 이 대화에 보내지 않은 첨부가 상한만큼 있어 새 첨부를 받지 않았다")
	// ErrObjectExists 는 파일을 옮길 자리에 이미 파일이 있어 옮기지 않았을 때다. 덮어쓰지 않는 저장소(로컬)만 낸다.
	// 앱은 저장소 종류를 모르고 이 값만 보고 판단한다.
	ErrObjectExists = NewError("object_exists", "core: 파일을 옮길 확정 자리에 이미 파일이 있어 옮기지 않았다(로컬 저장소는 덮어쓰지 않는다)")
	// ErrLeaseLost 는 첨부 완료를 맡은 요청이 시간(임대, 기본 30초) 안에 끝내지 못해 다른 완료 요청이 그 첨부를 넘겨받았을 때다.
	// 이 요청이 옮긴 파일과 확인 결과는 저장하지 않는다. 그 사이 넘겨받은 쪽이 확인을 끝냈으면 이 오류가 아니라 확인된 첨부를 준다.
	ErrLeaseLost = NewError("lease_lost", "core: 완료 처리 시간(임대, 기본 30초)이 지나 다른 완료 요청이 이 첨부를 넘겨받았다. 이 요청의 확인 결과는 저장하지 않았다")
	// ErrAssigneeChanged 는 배정의 조건(바꾸기 전 담당자)이 DB 의 값과 다를 때다. 그 사이 다른 사람이 담당을 바꿨다(D39).
	ErrAssigneeChanged = NewError("assignee_changed", "core: 화면이 보던 담당자와 DB 의 담당자가 달라 배정을 바꾸지 않았다. 그 사이 다른 사람이 바꿨다")
)
