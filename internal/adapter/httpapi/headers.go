package httpapi

import (
	"net/http"
	"strings"
)

// withSecurityHeaders 는 모든 응답에 거는 보안 헤더다(D46). 핸들러가 같은 헤더를 다시 쓰면 그 값이 이긴다.
//
//	Referrer-Policy: no-referrer   우리 페이지가 다른 곳으로 Referer 를 보내지 않는다. 주소에 토큰이 실렸던 시절(초대 링크·/ws)의
//	                               값이 첨부 저장소나 바깥 링크로 새던 길을 닫는다. 우리 서버는 Referer 를 읽지 않아 잃는 것이 없다 —
//	                               위젯의 호스트 오리진은 호스트가 보낸 postMessage 의 origin 과 호스트 쪽 정책의 referrer 로 안다.
//	                               strict-origin 보다 좁다. 오리진만 보내도 얻을 것이 없다.
//	X-Content-Type-Options: nosniff 브라우저가 Content-Type 을 무시하고 내용을 추측해 스크립트로 돌리지 않는다.
//	Cache-Control: no-store         API·WebSocket·상태 확인 응답은 어디에도 남기지 않는다. 세션·티켓·고객 정보가 응답 본문에 있다.
//	                               정적 자산(페이지·JS·CSS)은 revalidated 가 no-cache + ETag 를 건다. 서명된 파일 주소(/files/get)는
//	                               그 핸들러가 private, max-age=300 을 건다.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		if noStorePath(r.URL.Path) {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// noStorePath 는 캐시에 남기면 안 되는 응답의 경로다.
func noStorePath(path string) bool {
	switch path {
	case "/ws", "/healthz", "/readyz":
		return true
	}
	return apiPath(path)
}

// apiPath 는 API 주소인지다. 슬래시 없는 "/api" 도 API 다 — 정적 서버로 가면 no-store 가 빠진다(감사 21).
func apiPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}
