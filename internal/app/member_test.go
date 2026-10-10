package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 회원 토큰은 호스트 서버만 만들 수 있어야 한다. 브라우저가 흉내 낼 수 있으면 아무나 회원이 된다.
func TestVerifyMember(t *testing.T) {
	secret := []byte("shared-secret")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	claims := MemberClaims{MemberID: "u-1234", Name: "홍길동", Email: "hong@example.com", Exp: now.Add(time.Hour).Unix()}
	token, err := SignMember(secret, claims)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("올바른 서명은 통과한다", func(t *testing.T) {
		got, err := VerifyMember(secret, token, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.MemberID != "u-1234" || got.Name != "홍길동" {
			t.Errorf("내용이 다르다: %+v", got)
		}
	})

	t.Run("다른 비밀로 서명하면 거절한다", func(t *testing.T) {
		if _, err := VerifyMember([]byte("other"), token, now); err == nil {
			t.Error("서명이 다르면 믿으면 안 된다")
		}
	})

	t.Run("내용을 바꾸면 거절한다", func(t *testing.T) {
		forged, _ := SignMember([]byte("attacker"), MemberClaims{MemberID: "admin", Exp: claims.Exp})
		if _, err := VerifyMember(secret, forged, now); err == nil {
			t.Error("위조 토큰이 통과했다")
		}
	})

	t.Run("만료된 토큰은 거절한다", func(t *testing.T) {
		if _, err := VerifyMember(secret, token, now.Add(2*time.Hour)); err == nil {
			t.Error("샌 토큰이 영원히 살면 안 된다")
		}
	})

	t.Run("member_id 가 비면 거절한다", func(t *testing.T) {
		empty, _ := SignMember(secret, MemberClaims{MemberID: "  ", Exp: claims.Exp})
		if _, err := VerifyMember(secret, empty, now); err == nil {
			t.Error("빈 회원 id 를 받으면 익명과 구분이 안 된다")
		}
	})

	t.Run("비밀이 없으면 연동이 꺼진다", func(t *testing.T) {
		if _, err := VerifyMember(nil, token, now); !errors.Is(err, ErrMemberAuthDisabled) {
			t.Errorf("ErrMemberAuthDisabled 여야 한다: %v", err)
		}
	})

	t.Run("이름이 없으면 회원 id 로 부른다", func(t *testing.T) {
		noName, _ := SignMember(secret, MemberClaims{MemberID: "u-9", Exp: claims.Exp})
		got, err := VerifyMember(secret, noName, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "회원 u-9" {
			t.Errorf("기본 이름이 있어야 한다: %q", got.Name)
		}
	})
}

// 호스트가 100년짜리 토큰을 발급하면 그건 세션이 아니라 비밀번호다.
func TestVerifyMember_유효기간상한(t *testing.T) {
	secret := []byte("s")
	now := time.Now()
	long, _ := SignMember(secret, MemberClaims{MemberID: "u-1", Exp: now.Add(365 * 24 * time.Hour).Unix()})
	if _, err := VerifyMember(secret, long, now); err == nil {
		t.Error("1년짜리 토큰은 거절해야 한다")
	}
	okTok, _ := SignMember(secret, MemberClaims{MemberID: " u-2 ", Name: strings.Repeat("가", 60), Exp: now.Add(24 * time.Hour).Unix()})
	c, err := VerifyMember(secret, okTok, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.MemberID != "u-2" {
		t.Errorf("member_id 앞뒤 공백은 지운다: %q", c.MemberID)
	}
	if len([]rune(c.Name)) != 40 {
		t.Errorf("긴 이름은 40자로 자른다: %d", len([]rune(c.Name)))
	}
}

// 회원 토큰으로 연 세션은 호스트의 로그인보다 오래 살지 않는다(D46). 세션 수명 상한과 회원 토큰 만료 중 이른 쪽이다.
func TestMemberSessionTTL_세션수명상한과회원토큰만료중이른쪽이다(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		exp  time.Time
		want time.Duration
	}{
		{"회원 토큰이 1시간 남았으면 1시간", now.Add(time.Hour), time.Hour},
		{"회원 토큰이 30일(상한 720시간) 남았으면 720시간", now.Add(30 * 24 * time.Hour), 720 * time.Hour},
		{"상한보다 길면 상한", now.Add(40 * 24 * time.Hour), 720 * time.Hour},
		{"이미 지난 만료라도 0 이하로 내지 않는다(검증이 먼저 거절한다)", now.Add(-time.Minute), time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MemberSessionTTL(720*time.Hour, tc.exp, now); got != tc.want {
				t.Errorf("%s, 기대 %s", got, tc.want)
			}
		})
	}
}
