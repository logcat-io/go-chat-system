package logctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync/atomic"
)

// W3C Trace Context (https://www.w3.org/TR/trace-context/) 의 traceparent 모양으로 trace 를 만들어 응답에 싣는다. 들어온 것은 읽지 않는다.
//
// 이 패키지는 OpenTelemetry 를 모른다(표준 라이브러리만 쓰는 잎이다). 스팬이 있으면 그 id 를 로그에 쓰도록
// 바깥(internal/telemetry)이 SetSpanSource 로 "ctx 에서 지금 스팬의 id 를 읽는 함수" 를 꽂는다(D33).
// 꽂힌 함수가 스팬을 찾으면 trace_id·span_id 는 그 스팬의 값이고, 못 찾으면(내보내기를 끈 no-op) 여기서 만든 값이다.
// 어느 쪽이든 형식은 W3C Trace Context 라 로그의 trace_id 로 트레이스 저장소를 바로 찾는다.

const (
	// KeyTrace 는 요청 하나(HTTP 요청, WebSocket 프레임, 백그라운드 작업 한 번)의 키다.
	KeyTrace = "trace_id"
	// KeySpan 은 이 서버 안의 한 걸음이다.
	KeySpan = "span_id"
)

// NewTrace 는 새 trace_id 와 span_id 를 얹는다. HTTP 요청·WebSocket 프레임·백그라운드 작업 한 번마다 부른다.
// 들어온 traceparent 를 이어받는 길은 두지 않는다 — 공개 입구의 클라이언트가 trace id 를 정하게 되기 때문이다(D33, 감사 24).
func NewTrace(ctx context.Context) context.Context {
	return With(ctx, KeyTrace, randomHex(16), KeySpan, randomHex(8))
}

// TraceID 는 ctx 의 trace_id 다. 없으면 빈 문자열이다.
func TraceID(ctx context.Context) string { return Value(ctx, KeyTrace) }

// Traceparent 는 ctx 의 trace 를 traceparent 헤더 값으로 만든다. 응답과 다음 호출에 싣는다. 없으면 빈 문자열이다.
// 스팬이 있으면 그 스팬의 id 와 샘플링 여부를 싣는다.
func Traceparent(ctx context.Context) string {
	flags := "01"
	if s, ok := currentSpan(ctx); ok && !s.Sampled {
		flags = "00"
	}
	traceID, spanID := Value(ctx, KeyTrace), Value(ctx, KeySpan)
	if traceID == "" || spanID == "" {
		return ""
	}
	return "00-" + traceID + "-" + spanID + "-" + flags
}

// Span 은 바깥 트레이서가 ctx 에 둔 지금 스팬의 id 다. 둘 다 소문자 hex(32자리·16자리)다.
type Span struct {
	TraceID string
	SpanID  string
	Sampled bool
}

// SpanSource 는 ctx 에서 지금 스팬을 찾는다. 이 서버가 만든(원격이 아닌) 올바른 스팬이 있을 때만 ok 다.
type SpanSource func(context.Context) (Span, bool)

var spanSource atomic.Pointer[SpanSource]

// SetSpanSource 는 스팬을 찾는 함수를 꽂는다. nil 이면 뺀다. 서버가 뜰 때 한 번 부른다(internal/telemetry).
func SetSpanSource(f SpanSource) {
	if f == nil {
		spanSource.Store(nil)
		return
	}
	spanSource.Store(&f)
}

func currentSpan(ctx context.Context) (Span, bool) {
	if ctx == nil {
		return Span{}, false
	}
	f := spanSource.Load()
	if f == nil {
		return Span{}, false
	}
	return (*f)(ctx)
}

func allZero(s string) bool {
	return strings.Trim(s, "0") == ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	for {
		// crypto/rand.Read 는 실패하지 않는다(Go 1.24+ 는 실패하면 프로세스를 멈춘다).
		_, _ = rand.Read(b)
		s := hex.EncodeToString(b)
		if !allZero(s) {
			return s
		}
	}
}
