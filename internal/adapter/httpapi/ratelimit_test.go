package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 입구 한도는 "잠깐 몰리는 건 받고, 계속 두드리는 건 막는다" 다.
func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)

	t.Run("폭주 허용량까지는 연달아 받는다", func(t *testing.T) {
		l := newRateLimiter(60, 5, 0)
		for i := 0; i < 5; i++ {
			if !l.allow("1.2.3.4", now) {
				t.Fatalf("%d번째가 막혔다", i+1)
			}
		}
		if l.allow("1.2.3.4", now) {
			t.Error("허용량을 넘기면 막아야 한다")
		}
	})

	t.Run("시간이 지나면 다시 받는다", func(t *testing.T) {
		l := newRateLimiter(60, 1, 0) // 초당 1개 채워진다
		if !l.allow("k", now) || l.allow("k", now) {
			t.Fatal("첫 번째만 통과해야 한다")
		}
		if !l.allow("k", now.Add(time.Second)) {
			t.Error("1초 뒤에는 하나가 채워져야 한다")
		}
	})

	t.Run("IP 마다 따로 센다", func(t *testing.T) {
		l := newRateLimiter(60, 1, 0)
		l.allow("a", now)
		if !l.allow("b", now) {
			t.Error("다른 IP 는 영향받지 않는다")
		}
	})

	t.Run("설정이 0 이어도 입구가 막히지 않는다", func(t *testing.T) {
		l := newRateLimiter(0, 0, 0)
		if !l.allow("x", now) {
			t.Error("바닥값이 있어야 한다")
		}
	})
}

// 한도의 키는 공격자가 고를 수 없어야 한다. 헤더 한 줄로 바꿀 수 있으면 한도가 없는 것과 같다.
func TestClientIP(t *testing.T) {
	req := func(xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/agent/login", nil)
		r.RemoteAddr = "10.0.0.9:4321"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	t.Run("프록시를 신뢰하지 않으면 X-Forwarded-For 를 무시한다", func(t *testing.T) {
		l := newRateLimiter(60, 1, 0)
		if got := l.clientIP(req("203.0.113.7")); got != "10.0.0.9" {
			t.Errorf("소켓 주소여야 한다: %s", got)
		}
	})

	t.Run("프록시 하나를 신뢰하면 오른쪽에서 하나 건너뛴 값을 쓴다", func(t *testing.T) {
		// 프록시는 자기가 받은 연결의 주소를 맨 뒤에 붙인다. 왼쪽은 클라이언트가 미리 써 둔 것일 수 있다.
		l := newRateLimiter(60, 1, 1)
		if got := l.clientIP(req("1.1.1.1, 203.0.113.7")); got != "203.0.113.7" {
			t.Errorf("클라이언트가 붙인 왼쪽 값은 버려야 한다: %s", got)
		}
	})

	t.Run("헤더가 두 줄로 와도 프록시가 붙인 줄의 값을 쓴다", func(t *testing.T) {
		l := newRateLimiter(60, 1, 1)
		r := req("1.1.1.1")                            // 클라이언트가 미리 써 둔 줄
		r.Header.Add("X-Forwarded-For", "203.0.113.7") // 프록시가 새 줄로 덧붙인 것
		if got := l.clientIP(r); got != "203.0.113.7" {
			t.Errorf("마지막 줄의 값이어야 한다: %s", got)
		}
	})

	t.Run("헤더가 신뢰 수보다 짧으면 소켓 주소로 돌아간다", func(t *testing.T) {
		l := newRateLimiter(60, 1, 2)
		if got := l.clientIP(req("203.0.113.7")); got != "10.0.0.9" {
			t.Errorf("소켓 주소여야 한다: %s", got)
		}
	})

	t.Run("헤더를 바꿔 가며 두드려도 같은 키로 센다", func(t *testing.T) {
		l := newRateLimiter(60, 2, 0)
		now := time.Now()
		for i := 0; i < 2; i++ {
			l.allow(l.clientIP(req("203.0.113."+string(rune('1'+i)))), now)
		}
		if l.allow(l.clientIP(req("203.0.113.9")), now) {
			t.Error("헤더만 바꾼 세 번째는 막혀야 한다")
		}
	})
}
