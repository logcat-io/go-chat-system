package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// 패닉을 되살리는 곳(D47). 고루틴 하나의 패닉은 recover 가 없으면 프로세스 전체를 끝낸다 — 연결 수천 개와 진행 중인 저장이 같이 끊긴다.
// 그래서 서버가 띄우는 고루틴(HTTP 핸들러, WebSocket 읽기·ping·전달, 알림 리스너, 안전망, 주기 작업)은 모두 여기를 지난다.
// 패닉 하나는 그 요청·연결·작업 한 번만 끝내고, ERROR 한 줄(panic.recovered)과 지표(chat.panics)를 남긴다.

// Recovered 는 되살린 패닉 하나를 남긴다. recover() 가 돌려준 값을 넘긴다. defer 안에서 불러야 스택에 패닉 자리가 남는다.
//
// panic 값은 타입 이름만 싣는다. 값에는 입력·오류 원문(접속 정보)이 들어 있을 수 있다(D30). 스택은 함수 이름·파일·줄과
// 인자의 기계 값(포인터·정수)뿐이라 싣는다 — 스택이 없으면 어디서 났는지 다시 일으키기 전에는 모른다.
func Recovered(ctx context.Context, log *slog.Logger, where string, v any) {
	log.ErrorContext(ctx, "panic.recovered", "where", where, "panic_type", fmt.Sprintf("%T", v), "stack", string(debug.Stack()))
	Metrics().Panics.Add(context.WithoutCancel(ctx), 1, metric.WithAttributes(attribute.String("where", where)))
}

// Guard 는 fn 을 부르고 패닉을 되살린다. 패닉이었으면 Recovered 로 남기고 true 다.
func Guard(ctx context.Context, log *slog.Logger, where string, fn func(context.Context)) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			Recovered(ctx, log, where, v)
			panicked = true
		}
	}()
	fn(ctx)
	return false
}
