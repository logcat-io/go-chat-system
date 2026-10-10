package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// withSpan 은 요청마다 서버 스팬을 연다(D33). 맨 바깥에 둔다 — withTrace 가 그 스팬의 id 로 응답 헤더와 로그를 쓴다.
//
// 들어온 추적 헤더(traceparent·tracestate·baggage)는 읽지 않는다 — 요청마다 이 서버가 새 루트 트레이스를 만든다(D33, 감사 24).
// 공개 인터넷의 클라이언트가 trace id 를 정하면 문의 번호(D47)를 남의 요청과 같게 만들 수 있고, tracestate 원문은 스팬에 그대로 복사된다.
// 스팬 이름은 라우트 패턴(GET /api/conversations/{id})이고 경로의 id 를 이름에 넣지 않는다.
// 라우팅 전에는 패턴을 몰라 메서드만으로 열고, 라우팅이 끝나면 withTrace 가 패턴으로 바꾼다(nameSpan).
// 상태 확인은 로드밸런서가 몇 초마다 부르므로 스팬을 열지 않는다. 지표는 otelhttp 의 기본(http.server.*)이다.
//
// serverName 은 설정의 공개 주소에서 온 서버 이름이다. otelhttp 는 이 값이 없으면 server.address·server.port 를 요청의 Host 헤더에서
// 읽는데, Host 는 클라이언트가 정한 글자다. 지표는 View 로 속성을 걸러도 exemplar 의 filtered_attributes 에 거른 값이 그대로 실려
// 나간다(감사 24 — Host 2만 자가 OTLP 로 나갔다). 그래서 거르지 않고 처음부터 설정 값으로 싣는다.
func withSpan(serverName string, next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http",
		otelhttp.WithTracerProvider(telemetry.TracerProvider()),
		otelhttp.WithMeterProvider(telemetry.MeterProvider()),
		otelhttp.WithPropagators(telemetry.NoPropagation()),
		otelhttp.WithServerName(serverName),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return knownMethod(r.Method) // 라우팅 전(없는 메서드 405 등)이다. 클라이언트가 정한 메서드 글자를 이름에 쓰지 않는다
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
	)
}

// nameSpan 은 라우팅이 끝난 뒤 스팬 이름과 http.route 를 라우트 패턴으로 맞춘다.
// otelhttp 는 바깥 요청의 Pattern 을 보는데, 중간 미들웨어가 요청을 복사해서 ServeMux 가 적은 패턴이 거기까지 오지 않는다.
// 지표(http.server.request.duration)에도 같은 라우트를 싣는다 — 패턴이라 값이 라우트 수만큼뿐이다.
func nameSpan(ctx context.Context, route string) {
	if route == "" {
		return
	}
	// 패턴은 "GET /api/…" 꼴이다. 스팬 이름은 그대로 쓰고(표준의 "{method} {route}"), http.route 는 경로 부분만 싣는다.
	path := route
	if _, p, ok := strings.Cut(route, " "); ok {
		path = p
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetName(route)
		span.SetAttributes(semconv.HTTPRoute(path))
	}
	if labeler, ok := otelhttp.LabelerFromContext(ctx); ok {
		labeler.Add(semconv.HTTPRoute(path))
	}
}

// spanServerName 은 otelhttp 에 넘길 서버 이름(host:port)이다. 공개 주소(CHAT_PUBLIC_URL)의 호스트를 쓴다.
// 포트는 공개 주소에 적힌 것이고, 없으면 0 이다 — otelhttp 는 포트가 없는 이름이면 Host 헤더의 포트를 대신 읽으므로
// 늘 포트를 붙여 둔다(0 이면 server.port 를 싣지 않는다). 주소를 읽지 못하면 서비스 이름을 쓴다.
func spanServerName(publicURL string) string {
	host, port := telemetry.DefaultServiceName, "0"
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
		if p := u.Port(); p != "" {
			port = p
		}
	}
	return net.JoinHostPort(host, port)
}
