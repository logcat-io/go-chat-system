package pgstore

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// queryTracer 는 pgx 의 QueryTracer 다. 쿼리 하나가 스팬 하나다(D33).
//
// SQL 문장은 싣고 인자 값은 싣지 않는다. 인자에는 메시지 본문, 이메일, 토큰 해시, 저장소 키가 들어간다(D30).
// 문장은 전부 $n 자리표시자를 쓴다 — 값을 문자열로 이어 붙인 쿼리는 열 이름 같은 상수뿐이다.
// 실패하면 상태 설명에 SQLSTATE 만 적는다. Postgres 오류 문구에는 입력값이 그대로 들어가기도 한다
// (예: invalid input syntax for type uuid: "…").
//
// 기록 중인 부모 스팬이 없으면 스팬을 열지 않는다. 안전망(ChangeWatcher)·리스너의 주기 조회와 WebSocket 전달 루프의 조회가
// 요청과 무관한 트레이스로 쌓이지 않게 한다. 내보내기를 끈 설정(no-op)이나 샘플링에서 빠진 요청도 여기서 끝난다.
type queryTracer struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}
	ctx, _ = telemetry.Tracer().Start(ctx, "pg "+operation(data.SQL),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", operation(data.SQL)),
			attribute.String("db.query.text", data.SQL),
		))
	return ctx
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	if data.Err != nil {
		code := "error"
		var pgErr *pgconn.PgError
		switch {
		case errors.As(data.Err, &pgErr):
			code = pgErr.Code
		case errors.Is(data.Err, context.Canceled) || errors.Is(data.Err, context.DeadlineExceeded):
			code = "canceled"
		case errors.Is(data.Err, pgx.ErrNoRows):
			code = ""
		}
		if code != "" {
			span.SetAttributes(attribute.String("db.response.status_code", code))
			span.SetStatus(codes.Error, code)
		}
	}
	span.End()
}

// operation 은 문장의 첫 낱말이다(SELECT·INSERT·UPDATE·WITH …). 스팬 이름에 쓴다 — 문장 전체를 이름으로 쓰면 이름이 끝없이 늘어난다.
func operation(sql string) string {
	sql = strings.TrimSpace(sql)
	for i, r := range sql {
		if r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ';' {
			sql = sql[:i]
			break
		}
	}
	if sql == "" {
		return "query"
	}
	return strings.ToUpper(sql)
}

// observePool 은 커넥션 풀 상태를 관측 게이지로 낸다: chat.db.pool.acquired·idle·total·max.
// 지표를 모을 때만 Stat 을 읽는다. 돌려준 함수가 등록을 푼다.
func observePool(pool *pgxpool.Pool, mp metric.MeterProvider) func() {
	m := mp.Meter(telemetry.Scope)
	unit := metric.WithUnit("{connection}")
	acquired, err1 := m.Int64ObservableGauge("chat.db.pool.acquired", unit, metric.WithDescription("빌려 간 커넥션"))
	idle, err2 := m.Int64ObservableGauge("chat.db.pool.idle", unit, metric.WithDescription("놀고 있는 커넥션"))
	total, err3 := m.Int64ObservableGauge("chat.db.pool.total", unit, metric.WithDescription("풀의 커넥션(생성 중 포함)"))
	maxConns, err4 := m.Int64ObservableGauge("chat.db.pool.max", unit, metric.WithDescription("풀의 상한"))
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return func() {}
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := pool.Stat()
		o.ObserveInt64(acquired, int64(st.AcquiredConns()))
		o.ObserveInt64(idle, int64(st.IdleConns()))
		o.ObserveInt64(total, int64(st.TotalConns()))
		o.ObserveInt64(maxConns, int64(st.MaxConns()))
		return nil
	}, acquired, idle, total, maxConns)
	if err != nil {
		return func() {}
	}
	return func() { _ = reg.Unregister() }
}
