// Package telemetry 는 OpenTelemetry 트레이스와 지표의 시작·종료와 공용 계기를 둔다(D33).
//
// 앱은 OTLP 로만 내보낸다. 받는 쪽(운영은 같은 태스크의 ADOT 컬렉터 사이드카, 로컬은 compose 의 컬렉터)이
// 트레이스를 X-Ray 로, 지표를 CloudWatch 로 보낸다. 백엔드를 바꿔도 이 패키지와 계측 지점은 바뀌지 않는다.
//
// 계측은 어댑터(httpapi·realtime·pgstore·s3store·ollama)와 cmd/server 에만 있다. core 와 app 은 이 패키지도
// OTel 도 모른다(internal/archtest 가 막는다). 이 패키지는 OTel 과 logctx 만 아는 잎이다.
//
// 지금 쓰는 프로바이더는 전역 한 칸(current)에 있다. 기본은 아무것도 하지 않는 no-op 이고, 서버가 뜰 때 Setup 이,
// 테스트는 Install 이 바꾼다. 계측 지점은 스팬·계기를 쓸 때마다 이 칸을 읽으므로 조립 순서에 묶이지 않는다.
package telemetry

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// Scope 는 이 서버가 직접 만드는 스팬과 계기의 계측 범위 이름이다.
const Scope = "github.com/logcat-io/go-chat-system"

type state struct {
	tp      trace.TracerProvider
	mp      metric.MeterProvider
	metrics *Instruments
	// tracers 는 이름별 트레이서를 기억한다. SDK 의 Tracer() 는 잠금을 잡으므로 스팬마다 부르지 않는다.
	tracers sync.Map
}

func (s *state) tracer(name string, opts []trace.TracerOption) trace.Tracer {
	// tracers 에는 이 함수만 trace.Tracer 를 넣는다. 다른 값이면 고칠 곳이 여기라 패닉으로 드러나게 둔다.
	if t, ok := s.tracers.Load(name); ok {
		return t.(trace.Tracer) //nolint:forcetypeassert // 위 주석
	}
	t, _ := s.tracers.LoadOrStore(name, s.tp.Tracer(name, opts...))
	return t.(trace.Tracer) //nolint:forcetypeassert // 위 주석
}

var current atomic.Pointer[state]

func init() {
	current.Store(newState(tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider()))
}

func newState(tp trace.TracerProvider, mp metric.MeterProvider) *state {
	return &state{tp: tp, mp: mp, metrics: newInstruments(mp.Meter(Scope))}
}

// Install 은 지금 쓸 프로바이더를 바꾸고 이전 것으로 되돌리는 함수를 준다. Setup 과 테스트가 쓴다.
// 로그의 trace_id 를 스팬의 트레이스 id 로 맞추는 연결(logctx.SetSpanSource)도 여기서 꽂는다.
func Install(tp trace.TracerProvider, mp metric.MeterProvider) (restore func()) {
	prev := current.Swap(newState(tp, mp))
	logctx.SetSpanSource(spanOf)
	return func() { current.Store(prev) }
}

// spanOf 는 ctx 의 스팬 id 를 logctx 에 건넨다. 이 서버가 만든 스팬만 본다 — 원격 스팬은 로그의 span_id 로 쓰지 않는다
// (들어온 traceparent 는 읽지 않으므로 지금은 생기지 않는다, 감사 24). no-op 프로바이더의 스팬은 id 가 비어 있어 logctx 가 만든 값을 쓴다.
func spanOf(ctx context.Context) (logctx.Span, bool) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || sc.IsRemote() {
		return logctx.Span{}, false
	}
	return logctx.Span{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), Sampled: sc.IsSampled()}, true
}

// TracerProvider 는 지금 칸을 따라가는 프로바이더다. otelhttp 처럼 만들 때 프로바이더를 받는 계측에 넘긴다.
func TracerProvider() trace.TracerProvider { return dynamicProvider{} }

// MeterProvider 는 지금 칸의 미터 프로바이더다. 계기는 만들 때의 프로바이더에 묶이므로 Install 뒤에 부른다.
func MeterProvider() metric.MeterProvider { return current.Load().mp }

// Tracer 는 이 서버가 직접 여는 스팬(WebSocket 프레임·SQL·주기 작업)의 트레이서다.
func Tracer() trace.Tracer { return dynamicTracer{name: Scope} }

// Metrics 는 지금 칸의 공용 계기다.
func Metrics() *Instruments { return current.Load().metrics }

// NoPropagation 은 추적 헤더를 읽지도 싣지도 않는다. 들어오는 요청과 나가는 요청 모두에 쓴다(D33, 감사 24).
//
//   - 들어오는 요청(httpapi): 공개 인터넷의 클라이언트가 보낸 traceparent·tracestate·baggage 를 믿지 않는다. 받아들이면 클라이언트가
//     trace id 를 정해 문의 번호(D47)를 남의 요청과 같게 만들 수 있고, tracestate 원문이 스팬에 그대로 복사되어 나간다.
//     요청마다 이 서버가 새 루트 트레이스를 만든다. 서비스 사이 호출이 생기면(D33 "다시 볼 시점") 그 입구만 따로 연다.
//   - 나가는 요청(S3·Ollama): 서명한 요청에 헤더를 더하지 않고, 우리 트레이스 id 를 바깥에 흘리지 않는다.
func NoPropagation() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator()
}

type dynamicProvider struct{ embedded.TracerProvider }

func (dynamicProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return dynamicTracer{name: name, opts: opts}
}

type dynamicTracer struct {
	embedded.Tracer
	name string
	opts []trace.TracerOption
}

func (t dynamicTracer) Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return current.Load().tracer(t.name, t.opts).Start(ctx, spanName, opts...)
}
