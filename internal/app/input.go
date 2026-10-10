package app

import "strings"

// 입력 형식 가운데 여러 유스케이스가 같은 판단을 하는 것. 오류 문구가 호출부마다 다르면 여기 두지 않는다.

// maxNameRunes 는 사람 이름(상담원·고객·회원)의 글자 수 상한이다. 바이트가 아니라 글자다.
const maxNameRunes = 40

// maxEmailBytes 는 이메일 주소의 상한이다. 글자가 아니라 바이트다.
const maxEmailBytes = 254

// normalizeEmail 은 저장·비교에 쓰는 모양이다. 앞뒤 공백을 떼고 소문자로 바꾼다. 형식은 보지 않는다.
// 로그인은 이것만 한다 — 형식이 틀린 주소도 "없는 계정" 과 같은 401 로 끝나야 한다.
func normalizeEmail(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// parseEmail 은 계정을 만드는 입구(가입·초대·직접 가입)가 받는 이메일이다. 정규화한 뒤 형식을 본다.
func parseEmail(v string) (string, error) {
	email := normalizeEmail(v)
	if !strings.Contains(email, "@") || len(email) > maxEmailBytes {
		return "", invalid("email_format", "이메일 형식이 올바르지 않습니다")
	}
	return email, nil
}
