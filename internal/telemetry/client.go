package telemetry

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Transport 는 바깥으로 나가는 HTTP 호출(S3·Ollama)에 클라이언트 스팬을 여는 RoundTripper 다. base 가 nil 이면 기본 전송이다.
//
// 스팬 이름은 "<peer> <메서드>"(예: s3 PUT)다. 주소는 이름에 넣지 않는다 — S3 주소의 경로는 저장소 키이고 쿼리는 서명이다.
// otelhttp 가 표준대로 붙이는 url.full 은 내보내기 직전에 출처만 남긴다(redact.go). traceparent 는 싣지 않는다(NoPropagation).
func Transport(peer string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(TracerProvider()),
		otelhttp.WithMeterProvider(MeterProvider()),
		otelhttp.WithPropagators(NoPropagation()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return peer + " " + r.Method }),
	)
}

// ExpectedReply 는 계측이 4xx 라서 오류로 적은 바깥 호출의 응답이 이 호출에서는 정상인 답이라고 고친다(예: 버킷 만들기의
// 409 BucketAlreadyOwnedByYou). ctx 는 그 응답의 요청 ctx(res.Request.Context())다. 스팬이 이미 끝났으면 아무 일도 없다.
func ExpectedReply(ctx context.Context) {
	trace.SpanFromContext(ctx).SetStatus(codes.Ok, "")
}
