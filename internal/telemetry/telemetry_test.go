package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/logctx"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

func TestTelemetry_엔드포인트가없으면아무것도내보내지않고로그의trace_id는그대로나온다(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_SDK_DISABLED"} {
		t.Setenv(k, "")
	}
	if traces, metrics := telemetry.Enabled(); traces || metrics {
		t.Fatalf("엔드포인트가 없는데 켜졌다: traces=%v metrics=%v", traces, metrics)
	}
	tel, err := telemetry.Setup(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// 스팬은 열어도 기록되지 않고 id 도 없다. 내보낼 것이 생기지 않는다.
	ctx, span := telemetry.Tracer().Start(logctx.NewTrace(context.Background()), "x")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Errorf("no-op 이 아니다: recording=%v valid=%v", span.IsRecording(), span.SpanContext().IsValid())
	}
	span.End()

	// 로그의 trace_id 는 지금처럼 logctx 가 만든 값이다.
	var buf bytes.Buffer
	log := slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, nil)))
	log.InfoContext(ctx, "x")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] == "" || line["trace_id"] != logctx.TraceID(ctx) || len(logctx.TraceID(ctx)) != 32 {
		t.Errorf("trace_id 가 그대로 나오지 않았다: %v (ctx %q)", line, logctx.TraceID(ctx))
	}

	start := time.Now()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("종료: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("내보내지 않는 설정의 종료가 %s 걸렸다", d)
	}
}

func TestTelemetry_엔드포인트가있으면켜고OTEL_SDK_DISABLED면끈다(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	if traces, metrics := telemetry.Enabled(); !traces || metrics {
		t.Errorf("신호별 엔드포인트: traces=%v metrics=%v", traces, metrics)
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if traces, metrics := telemetry.Enabled(); traces || metrics {
		t.Errorf("OTEL_SDK_DISABLED=true 인데 켜졌다")
	}
}

// 걸러 내기는 내보내기 직전 한 곳이다. 어느 계측이 붙였든 주소의 경로·쿼리는 남지 않는다.
func TestTelemetry_내보내는스팬에는주소의경로와쿼리가남지않는다(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	restore := telemetry.Install(telemetry.NewTracerProvider(exp, false), metricnoop.NewMeterProvider())
	defer restore()

	signed := "https://bucket.s3.ap-northeast-2.amazonaws.com/ws/w1/c1/a1/tok?X-Amz-Signature=abc&X-Amz-Credential=AKIA"
	_, span := telemetry.Tracer().Start(context.Background(), "GET "+signed, trace.WithAttributes(
		attribute.String("url.full", signed),
		attribute.String("url.path", "/api/admin/invites/someone@example.com"),
		attribute.String("url.query", "token=secret"),
		attribute.String("http.target", "/files/get?token=secret"),
		attribute.String("db.query.parameter.0", "본문"),
		attribute.String("error.message", `Get "`+signed+`": dial tcp: refused`),
		attribute.StringSlice("chat.urls", []string{signed}),
		attribute.String("chat.conversation_id", "c-1"),
	))
	span.AddEvent("retry", trace.WithAttributes(attribute.String("target", signed)))
	span.SetStatus(codes.Error, `Put "`+signed+`": EOF`)
	span.End()

	got := exp.GetSpans()
	if len(got) != 1 {
		t.Fatalf("스팬 %d개", len(got))
	}
	s := got[0]
	var dump strings.Builder
	dump.WriteString(s.Name + "\n" + s.Status.Description + "\n")
	keys := map[string]string{}
	for _, kv := range s.Attributes {
		keys[string(kv.Key)] = kv.Value.String()
		dump.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
	}
	for _, e := range s.Events {
		for _, kv := range e.Attributes {
			dump.WriteString(kv.Value.String() + "\n")
		}
	}
	for _, f := range []string{"X-Amz", "tok?", "/ws/w1", "someone@example.com", "token=secret", "본문", "AKIA"} {
		if strings.Contains(dump.String(), f) {
			t.Errorf("%q 가 남았다:\n%s", f, dump.String())
		}
	}
	for _, k := range []string{"url.path", "url.query", "http.target", "db.query.parameter.0"} {
		if _, ok := keys[k]; ok {
			t.Errorf("%s 가 남았다", k)
		}
	}
	if keys["url.full"] != "https://bucket.s3.ap-northeast-2.amazonaws.com" {
		t.Errorf("url.full 은 출처만: %q", keys["url.full"])
	}
	if keys["chat.conversation_id"] != "c-1" {
		t.Errorf("id 는 남아야 한다: %v", keys)
	}
}

// 사람을 가리키는 값(클라이언트 IP·포트, 브라우저 식별 문자열)은 스팬에도 지표에도 싣지 않는다(D30·D33).
func TestTelemetry_스팬과지표에는클라이언트IP와UserAgent가없다(t *testing.T) {
	personal := []attribute.KeyValue{
		attribute.String("client.address", "203.0.113.7"),
		attribute.String("network.peer.address", "203.0.113.7"),
		attribute.Int("network.peer.port", 51234),
		attribute.String("user_agent.original", "Mozilla/5.0 (Macintosh)"),
		// 클라이언트가 정한 글자다(감사 23): 모르는 메서드의 원문과 Host 헤더. 지표에 붙으면 값의 수가 끝없이 는다.
		attribute.String("http.request.method_original", "HONGEXAMPLECOM"),
		attribute.String("server.address", "hong.example.com"),
	}
	keep := attribute.String("http.route", "/api/agent/login")

	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	restore := telemetry.Install(telemetry.NewTracerProvider(exp, false), telemetry.NewMeterProvider(sdkmetric.WithReader(reader)))
	defer restore()

	_, span := telemetry.Tracer().Start(context.Background(), "POST /api/agent/login",
		trace.WithAttributes(append([]attribute.KeyValue{keep}, personal...)...))
	span.End()
	counter, _ := telemetry.MeterProvider().Meter("test").Int64Counter("test.requests")
	counter.Add(context.Background(), 1, metric.WithAttributes(append([]attribute.KeyValue{keep}, personal...)...))

	t.Run("스팬", func(t *testing.T) {
		got := exp.GetSpans()
		if len(got) != 1 {
			t.Fatalf("스팬 %d개", len(got))
		}
		keys := map[attribute.Key]bool{}
		for _, kv := range got[0].Attributes {
			keys[kv.Key] = true
		}
		for _, kv := range personal {
			if keys[kv.Key] {
				t.Errorf("%s 가 남았다", kv.Key)
			}
		}
		if !keys[keep.Key] {
			t.Error("라우트는 남아야 한다")
		}
	})

	t.Run("지표", func(t *testing.T) {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		var points int
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					points++
					for _, kv := range personal {
						if _, ok := dp.Attributes.Value(kv.Key); ok {
							t.Errorf("%s 가 남았다", kv.Key)
						}
					}
					if _, ok := dp.Attributes.Value(keep.Key); !ok {
						t.Error("라우트는 남아야 한다")
					}
				}
			}
		}
		if points != 1 {
			t.Errorf("데이터 점 %d개", points)
		}
	})
}

// D47: 되살린 패닉은 로그 한 줄(panic.recovered)과 지표(chat.panics, where 속성)로 남는다. 패닉은 밖으로 새지 않는다.
func TestTelemetry_Guard는패닉을되살려panic_recovered와chat_panics를남긴다(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	restore := telemetry.Install(telemetry.NewTracerProvider(tracetest.NewInMemoryExporter(), false), telemetry.NewMeterProvider(sdkmetric.WithReader(reader)))
	defer restore()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	if !telemetry.Guard(context.Background(), log, "ws.read", func(context.Context) { panic("원문") }) {
		t.Fatal("패닉인데 false 다")
	}
	if telemetry.Guard(context.Background(), log, "ws.read", func(context.Context) {}) {
		t.Fatal("패닉이 아닌데 true 다")
	}
	var l map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &l); err != nil || l["msg"] != "panic.recovered" || l["where"] != "ws.read" ||
		l["panic_type"] != "string" || l["level"] != "ERROR" || !strings.Contains(l["stack"].(string), "TestTelemetry_Guard") {
		t.Fatalf("panic.recovered 한 줄이어야 한다: %s", buf.String())
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "chat.panics" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if v, _ := dp.Attributes.Value("where"); v.AsString() == "ws.read" {
					total += dp.Value
				}
			}
		}
	}
	if total != 1 {
		t.Errorf("chat.panics{where=ws.read} 가 %d 다. 1 이어야 한다", total)
	}
}
