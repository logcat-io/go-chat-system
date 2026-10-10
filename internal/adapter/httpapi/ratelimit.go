package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 요청 한도. 토큰 없이 들어오는 입구(세션 생성·로그인·초대 수락)를 지킨다.
//
// 공개로 띄우는 순간 세션 생성 한 번에 고객·세션 행이 생기므로, 한도가 없으면 DB 가
// 무의미한 행으로 찬다. 토큰 버킷을 입구(라우트)와 IP 의 짝마다 두고, 한 프로세스 메모리에 둔다.
// 입구마다 따로 세는 이유: 사내 NAT 뒤에서는 상담원과 테스트 고객이 같은 IP 로 들어온다. 버킷을 같이 쓰면
// 고객 세션 생성이 한도를 다 쓴 순간 상담원 로그인까지 429 가 된다. 한도 값은 "입구 하나당" 이다.
// 서버를 여러 대로 늘리면 대수만큼 한도가 느슨해진다 — 그때는 역프록시에서 거는 게 맞다.

type bucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // 초당 채워지는 토큰
	burst     float64
	lastSweep time.Time
	// trustedProxies 만큼 X-Forwarded-For 의 오른쪽을 건너뛴다. 0 이면 헤더를 안 본다.
	trustedProxies int
}

// defaultEntryRate 는 설정이 비었을 때의 바닥값이다. 0 을 그대로 쓰면 입구가 통째로 막힌다.
const defaultEntryRate = 30

// maxBuckets 를 넘으면 조용한 키를 당겨서 치운다. 키는 공격자가 고르는 값이라 상한이 있어야 한다.
const maxBuckets = 50000

func newRateLimiter(perMinute, burst, trustedProxies int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = defaultEntryRate
	}
	if burst <= 0 {
		burst = perMinute
	}
	if trustedProxies < 0 {
		trustedProxies = 0
	}
	return &rateLimiter{
		buckets:        map[string]*bucket{},
		rate:           float64(perMinute) / 60,
		burst:          float64(burst),
		lastSweep:      time.Now(),
		trustedProxies: trustedProxies,
	}
}

// allow 는 토큰 하나를 쓴다. 없으면 거절한다.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 오래 조용한 키는 치운다. 안 치우면 IP 수만큼 메모리가 는다.
	// 상한에 닿으면 기다리지 않고 1분 넘게 조용한 키부터 치운다.
	idle := 10 * time.Minute
	if len(l.buckets) >= maxBuckets {
		idle = time.Minute
	}
	if now.Sub(l.lastSweep) > 10*time.Minute || len(l.buckets) >= maxBuckets {
		for k, b := range l.buckets {
			if now.Sub(b.last) > idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limited 는 핸들러 앞에 한도를 건다. 키는 라우트 패턴(r.Pattern)과 클라이언트 IP 의 짝이다.
// 패턴은 ServeMux 가 정하므로 클라이언트가 바꿀 수 없고, 라우트를 더해도 이름을 따로 붙일 일이 없다.
func (l *rateLimiter) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(r.Pattern+"|"+l.clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "too_many", "요청이 너무 잦습니다. 잠시 후 다시 시도해 주세요")
			return
		}
		next(w, r)
	}
}

// clientIP 는 신뢰하는 프록시 수만큼 X-Forwarded-For 의 오른쪽을 건너뛴 값을 쓴다.
// 왼쪽 값은 클라이언트가 붙일 수 있어 믿지 않는다. 프록시가 없으면(0) 소켓 주소다.
func (l *rateLimiter) clientIP(r *http.Request) string {
	if l.trustedProxies > 0 {
		// 같은 이름의 헤더가 여러 줄로 올 수 있다(HAProxy 기본). Get 은 첫 줄만 주므로 전부 합친다.
		if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
			parts := strings.Split(xff, ",")
			if i := len(parts) - l.trustedProxies; i >= 0 {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
					return ip
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
