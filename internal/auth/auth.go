// Package auth 는 세션 토큰과 비밀번호를 다룬다.
// 토큰 평문은 발급 응답에만 싣고 DB 에는 SHA-256 해시로만 둔다.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const tokenBytes = 32

var ErrEmptyToken = errors.New("auth: 토큰이 비어 있다")

// NewToken 은 평문 토큰과 그 해시를 함께 돌려준다.
func NewToken() (plain string, hash []byte) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("auth: crypto/rand 실패: " + err.Error())
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashToken(plain)
}

// HashToken 은 조회 키로 쓸 해시를 만든다. 토큰이 고엔트로피 난수라 솔트·스트레칭을 쓰지 않는다.
func HashToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// MaxPasswordBytes 는 bcrypt 가 받는 비밀번호의 상한이다. 글자 수가 아니라 바이트라 한글은 24자에서 닿는다.
// 해시는 이보다 긴 비밀번호에 오류를 내고, 비교는 뒤를 잘라 앞 72바이트만 본다.
const MaxPasswordBytes = 72

// HashPassword 는 사람이 고른 비밀번호를 bcrypt 로 저장형으로 바꾼다.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword 는 bcrypt 해시와 평문을 비교한다. 72바이트를 넘는 평문은 비교는 하되 결과를 버리고 틀린 것으로 본다.
// 비교를 건너뛰면 그 길만 빨라지고, 비교 결과를 믿으면 저장된 비밀번호 뒤에 아무 글자나 붙여도 맞는 것이 된다(bcrypt 가 뒤를 자른다).
func CheckPassword(hash, pw string) bool {
	ok := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
	return ok && len(pw) <= MaxPasswordBytes
}

// dummyHash 는 비교할 계정이 없을 때 대신 비교하는 고정 해시다. HashPassword 와 같은 비용(DefaultCost)이어야 시간이 같다.
// 어떤 비밀번호와도 맞지 않는다는 보장은 필요 없다 — 결과를 버린다.
const dummyHash = "$2a$10$0BEaE3YlbfRLp13ccnEoJ.GaGyUUNmr/qvsbYt2qY08Rg687F5q7u"

// SpendPasswordCheck 는 결과를 버리는 비밀번호 비교다. 계정이 없을 때도 있을 때와 같은 시간을 쓰게 한다.
// 이걸 안 하면 응답 시간만으로 "이 이메일의 계정이 있나" 를 가를 수 있다.
func SpendPasswordCheck(pw string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(pw))
}

// SpendPasswordHash 는 결과를 버리는 해시 만들기다. 저장하지 않는 길에서도 저장하는 길과 같은 시간을 쓰게 한다.
func SpendPasswordHash(pw string) {
	_, _ = bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
}

// EqualHash 는 해시 비교를 상수 시간으로 한다.
func EqualHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// BearerToken 은 Authorization 헤더에서 토큰을 꺼낸다.
func BearerToken(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", ErrEmptyToken
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", ErrEmptyToken
	}
	return tok, nil
}
