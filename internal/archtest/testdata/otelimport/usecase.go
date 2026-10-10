// Package otelimport 는 archtest 가 규칙을 시험하는 데만 쓴다. testdata 아래라 빌드에 들어가지 않는다.
// app 의 유스케이스가 스팬을 직접 여는 모습이다. 이렇게 쓰면 안 된다.
package otelimport

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

func Send(ctx context.Context) {
	_, span := telemetry.Tracer().Start(ctx, "send")
	defer span.End()
	_ = trace.SpanFromContext(ctx)
}
