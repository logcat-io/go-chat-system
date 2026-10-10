package auth_test

import (
	"testing"

	"github.com/logcat-io/go-chat-system/internal/auth"
)

func TestNewToken(t *testing.T) {
	t.Run("토큰은 매번 다르고 해시가 같이 나온다", func(t *testing.T) {
		seen := make(map[string]bool, 100)
		for i := 0; i < 100; i++ {
			plain, hash := auth.NewToken()
			if plain == "" || len(hash) != 32 {
				t.Fatalf("토큰·해시 형식이 틀렸다: %q, %d바이트", plain, len(hash))
			}
			if seen[plain] {
				t.Fatal("토큰이 겹쳤다")
			}
			seen[plain] = true
			if !auth.EqualHash(hash, auth.HashToken(plain)) {
				t.Fatal("해시가 재계산 결과와 다르다")
			}
		}
	})
}

func TestPassword(t *testing.T) {
	hash, err := auth.HashPassword("chat1234")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("맞는 비밀번호는 통과한다", func(t *testing.T) {
		if !auth.CheckPassword(hash, "chat1234") {
			t.Error("통과해야 한다")
		}
	})
	t.Run("틀린 비밀번호는 막는다", func(t *testing.T) {
		if auth.CheckPassword(hash, "chat12345") {
			t.Error("막아야 한다")
		}
	})
	t.Run("해시가 평문을 담지 않는다", func(t *testing.T) {
		if hash == "chat1234" || len(hash) < 50 {
			t.Errorf("bcrypt 해시가 아니다: %q", hash)
		}
	})
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name, header, want string
		wantErr            bool
	}{
		{"표준 형식", "Bearer abc123", "abc123", false},
		{"대소문자를 가리지 않는다", "bearer abc123", "abc123", false},
		{"앞뒤 공백을 떼낸다", "Bearer  abc123 ", "abc123", false},
		{"빈 헤더는 오류다", "", "", true},
		{"접두어만 있으면 오류다", "Bearer ", "", true},
		{"다른 스킴은 오류다", "Basic abc123", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := auth.BearerToken(tc.header)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("오류여야 한다: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%q 가 아니라 %q", tc.want, got)
			}
		})
	}
}
