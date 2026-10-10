package pgstore_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// Postgres 오류 문구에는 입력값이 그대로 든다(invalid input syntax for type uuid: "…"). SQL 스팬은 실패를 SQLSTATE 로만 적는다.
// 운영과 같은 telemetry.NewTracerProvider(D30 거르기 포함)를 지나 내보낸 것을 본다.
func TestQueryTracer_SQL오류스팬의상태설명은SQLSTATE뿐이고오류문구의입력값이없다(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	t.Cleanup(telemetry.Install(telemetry.NewTracerProvider(exp, false), sdkmetric.NewMeterProvider()))
	store := testsupport.Store(t)

	secret := "not-a-uuid-" + strings.ReplaceAll(id.New()[24:], "-", "")
	ctx, parent := telemetry.Tracer().Start(context.Background(), "test parent")
	_, err := store.Pool().Exec(ctx, "SELECT $1::text::uuid", secret)
	parent.End()
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("오류 문구에 입력값이 든 실패를 만들지 못했다: %v", err)
	}

	var sqlSpan *tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if strings.HasPrefix(s.Name, "pg ") {
			sqlSpan = &s
		}
		if strings.Contains(s.Name+s.Status.Description, secret) {
			t.Errorf("스팬 %q 의 이름·상태 설명에 입력값이 있다: %q", s.Name, s.Status.Description)
		}
		for _, kv := range s.Attributes {
			if strings.Contains(kv.Value.String(), secret) {
				t.Errorf("스팬 %q 의 %s 에 입력값이 있다", s.Name, kv.Key)
			}
		}
		for _, e := range s.Events {
			for _, kv := range e.Attributes {
				if strings.Contains(kv.Value.String(), secret) {
					t.Errorf("스팬 %q 의 이벤트 %s 에 입력값이 있다", s.Name, e.Name)
				}
			}
		}
	}
	if sqlSpan == nil {
		t.Fatal("SQL 스팬이 없다")
	}
	if sqlSpan.Status.Code != codes.Error || sqlSpan.Status.Description != "22P02" {
		t.Errorf("SQL 스팬 상태가 %v %q 다. Error 와 SQLSTATE 22P02 여야 한다", sqlSpan.Status.Code, sqlSpan.Status.Description)
	}
}
