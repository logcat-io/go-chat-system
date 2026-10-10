package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// DefaultServiceName 은 OTEL_SERVICE_NAME 이 비었을 때의 service.name 이다.
const DefaultServiceName = "go-chat-system"

// Telemetry 는 Setup 이 띄운 것이다. Shutdown 으로 남은 것을 내보내고 닫는다.
type Telemetry struct {
	tp      *sdktrace.TracerProvider
	mp      *sdkmetric.MeterProvider
	restore func()
}

// Enabled 는 트레이스·지표를 내보내는지다. 표준 환경 변수만 본다.
//
// 엔드포인트(OTEL_EXPORTER_OTLP_ENDPOINT, 또는 신호별 _TRACES_/_METRICS_ENDPOINT)가 비면 내보내지 않는다.
// 로컬 개발과 테스트는 아무것도 안 띄워도 지금처럼 돈다. OTEL_SDK_DISABLED=true 면 엔드포인트가 있어도 끈다.
func Enabled() (traces, metrics bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false, false
	}
	all := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	return all || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "",
		all || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// Setup 은 환경 변수대로 트레이스·지표 내보내기를 띄우고 지금 칸에 꽂는다.
//
// 읽는 것은 OTel 표준 환경 변수다: OTEL_EXPORTER_OTLP_ENDPOINT(·_TRACES_·_METRICS_), OTEL_EXPORTER_OTLP_HEADERS,
// OTEL_SERVICE_NAME(기본 go-chat-system), OTEL_RESOURCE_ATTRIBUTES, OTEL_TRACES_SAMPLER·_ARG,
// OTEL_BSP_*(배치), OTEL_METRIC_EXPORT_INTERVAL. 프로토콜은 OTLP/HTTP(protobuf, 기본 포트 4318)다.
//
// 내보내기는 비동기다. 스팬은 배치 큐에 넣고 끝나며 큐가 차면 버린다. 지표는 주기마다 모아 보낸다.
// 그래서 컬렉터가 죽어 있어도 기동과 요청은 막히지 않는다. 내보내기 실패는 버리고 1분에 한 줄만 남긴다.
func Setup(ctx context.Context, log *slog.Logger) (*Telemetry, error) {
	traces, metrics := Enabled()
	if !traces && !metrics {
		// 지금 칸은 기본(no-op) 그대로다. 로그의 trace_id 는 logctx 가 만든다.
		return &Telemetry{restore: func() {}}, nil
	}

	res, err := resource.New(ctx,
		// 기본값을 먼저 두고 환경 변수가 덮는다.
		resource.WithAttributes(attribute.String("service.name", DefaultServiceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && res == nil {
		return nil, err
	}

	otel.SetErrorHandler(newThrottledHandler(log, time.Minute))

	t := &Telemetry{}
	var tp trace.TracerProvider = tracenoop.NewTracerProvider()
	var mp metric.MeterProvider = metricnoop.NewMeterProvider()
	if traces {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		t.tp = NewTracerProvider(exp, true, sdktrace.WithResource(res))
		tp = t.tp
	}
	if metrics {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		t.mp = NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
		mp = t.mp
	}
	// 계측 라이브러리가 전역을 볼 때도 같은 것을 보게 맞춘다.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(NoPropagation())
	t.restore = Install(tp, mp)
	log.Info("telemetry.enabled", "traces", traces, "metrics", metrics)
	return t, nil
}

// NewTracerProvider 는 익스포터 앞에 D30 거르기를 끼운 프로바이더다. 운영(Setup)과 테스트가 같은 길을 쓴다.
// batch 가 false 면 스팬이 끝날 때 바로 내보낸다(테스트용 메모리 익스포터).
// 샘플러를 주지 않으면 OTEL_TRACES_SAMPLER(·_ARG)를 따르고, 그것도 없으면 parentbased_always_on 이다.
func NewTracerProvider(exp sdktrace.SpanExporter, batch bool, opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	var sp sdktrace.SpanProcessor
	if batch {
		sp = sdktrace.NewBatchSpanProcessor(redactingExporter{exp})
	} else {
		sp = sdktrace.NewSimpleSpanProcessor(redactingExporter{exp})
	}
	return sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(sp)}, opts...)...)
}

// NewMeterProvider 는 D30 거르기(사람을 가리키는 속성 빼기)를 끼운 지표 프로바이더다. 운영(Setup)과 테스트가 같은 길을 쓴다.
func NewMeterProvider(opts ...sdkmetric.Option) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(append([]sdkmetric.Option{sdkmetric.WithView(metricView)}, opts...)...)
}

// Shutdown 은 남은 스팬·지표를 ctx 시한 안에서 내보내고 닫는다. 종료 순서(D32)의 맨 끝, HTTP 정리 뒤에 부른다.
// 컬렉터가 없으면 시한까지 기다리다 버린다. 내보내지 않던 설정이면 바로 돌아온다.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.tp != nil {
		errs = append(errs, t.tp.Shutdown(ctx))
	}
	if t.mp != nil {
		errs = append(errs, t.mp.Shutdown(ctx))
	}
	if t.restore != nil {
		t.restore()
	}
	return errors.Join(errs...)
}

// throttledHandler 는 OTel 내부 오류(주로 내보내기 실패)를 period 에 한 줄만 남긴다.
// 컬렉터가 죽어 있으면 배치마다 실패하는데, 그걸 다 남기면 운영 로그가 그 소리로 덮인다.
// 오류 문구에는 컬렉터 주소만 들어 있다(D30 에 걸리는 값이 없다).
type throttledHandler struct {
	log    *slog.Logger
	period time.Duration

	mu      sync.Mutex
	last    time.Time
	dropped int
}

func newThrottledHandler(log *slog.Logger, period time.Duration) *throttledHandler {
	return &throttledHandler{log: log, period: period}
}

func (h *throttledHandler) Handle(err error) {
	h.mu.Lock()
	now := time.Now()
	if now.Sub(h.last) < h.period {
		h.dropped++
		h.mu.Unlock()
		return
	}
	dropped := h.dropped
	h.last, h.dropped = now, 0
	h.mu.Unlock()
	h.log.Warn("telemetry.export_failed", "err_kind", logctx.ErrKind(err), "suppressed", dropped)
}
