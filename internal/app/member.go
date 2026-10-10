package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/logcat-io/go-chat-system/internal/core"
)

// 로그인 고객 연동.
//
// 호스트 사이트가 자기 회원을 서명해서 넘긴다. 서명 비밀은 호스트와 우리만 안다.
// 토큰 모양: base64url(json) + "." + base64url(hmac-sha256)
// 브라우저가 만들 수 없고(비밀이 없다), 호스트 서버만 만들 수 있다.
// 그래서 "이 방문자가 회원 1234다" 를 믿을 수 있다.

var ErrMemberAuthDisabled = core.NewError("member_auth_disabled", "app: 워크스페이스의 회원 서명 비밀이 비어 있어 회원 토큰을 검증할 수 없다")

// maxMemberTokenTTL 은 호스트가 발급하는 회원 토큰의 유효기간 상한이다. 100년짜리 토큰은 세션이 아니라 비밀번호다.
const maxMemberTokenTTL = 30 * 24 * time.Hour

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// MemberClaims 는 호스트가 서명하는 내용이다.
type MemberClaims struct {
	MemberID string `json:"member_id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
	// Exp 는 unix 초. 토큰이 새도 영원히 쓸 수 없게 한다.
	Exp int64 `json:"exp"`
}

// SignMember 는 호스트 쪽이 쓰는 함수다. 테스트와 문서 예제를 위해 여기 같이 둔다.
func SignMember(secret []byte, c MemberClaims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyMember 는 토큰을 풀어 회원 정보를 돌려준다. 서명·만료·필수값을 본다.
func VerifyMember(secret []byte, token string, now time.Time) (MemberClaims, error) {
	if len(secret) == 0 {
		return MemberClaims{}, ErrMemberAuthDisabled
	}
	body, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return MemberClaims{}, invalid("member_token_malformed", "회원 토큰 형식이 올바르지 않습니다")
	}
	want := hmac.New(sha256.New, secret)
	want.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return MemberClaims{}, invalid("member_token_bad_signature", "회원 토큰 서명이 맞지 않습니다")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return MemberClaims{}, invalid("member_token_malformed", "회원 토큰 형식이 올바르지 않습니다")
	}
	var c MemberClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return MemberClaims{}, invalid("member_token_malformed", "회원 토큰 내용을 읽을 수 없습니다")
	}
	c.MemberID = strings.TrimSpace(c.MemberID)
	switch {
	case c.MemberID == "" || len(c.MemberID) > 128:
		return MemberClaims{}, invalid("member_id_invalid", "member_id 가 비어 있거나 너무 깁니다")
	case c.Exp == 0 || time.Unix(c.Exp, 0).Before(now):
		return MemberClaims{}, invalid("member_token_expired", "회원 토큰이 만료되었습니다")
	case time.Unix(c.Exp, 0).After(now.Add(maxMemberTokenTTL)):
		return MemberClaims{}, invalid("member_token_ttl_too_long", "회원 토큰 유효기간이 너무 깁니다")
	}
	// 이름은 호스트 데이터라 길다고 고객을 튕기지 않는다. 자른다.
	c.Name = clipRunes(strings.TrimSpace(c.Name), maxNameRunes)
	if c.Name == "" {
		c.Name = "회원 " + c.MemberID
	}
	return c, nil
}

// StartMemberSession 은 서명된 회원 토큰으로 고객을 찾거나 만든다.
// 같은 회원은 브라우저가 바뀌어도 같은 고객이고, 대화 이력이 따라온다.
// memberExp 는 회원 토큰의 만료다. 세션은 이보다 오래 살지 않는다(MemberSessionTTL, D46).
func (s *Commands) StartMemberSession(ctx context.Context, ws core.Workspace, token string) (customerID string, conv *core.Conversation, memberExp time.Time, err error) {
	claims, err := VerifyMember([]byte(ws.MemberSecret), token, time.Now())
	if err != nil {
		return "", nil, time.Time{}, err
	}
	customerID, err = s.write.UpsertMember(ctx, ws.ID, core.MemberProfile{
		ExternalID: "member:" + claims.MemberID, DisplayName: claims.Name,
		Email: strings.TrimSpace(claims.Email), Phone: strings.TrimSpace(claims.Phone),
	})
	if err != nil {
		return "", nil, time.Time{}, translate(err)
	}
	customerID, conv, err = s.StartCustomerSession(ctx, ws.ID, customerID, "")
	return customerID, conv, time.Unix(claims.Exp, 0), err
}
