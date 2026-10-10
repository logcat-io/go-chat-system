package logctx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// capture 는 JSON 한 줄씩 쓰는 로거와 그 출력을 돌려준다.
func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(logctx.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))), &buf
}

// lines 는 출력을 줄마다 해석한다.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("JSON 이 아니다: %v (%s)", err, line)
		}
		out = append(out, m)
	}
	return out
}

func only(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	all := lines(t, buf)
	if len(all) != 1 {
		t.Fatalf("한 줄이어야 하는데 %d줄이다: %s", len(all), buf.String())
	}
	return all[0]
}

func TestContextKeys(t *testing.T) {
	t.Run("ctx 에 얹은 키가 로그에 붙는다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "conversation_id", "c-1", "workspace_id", "w-1")
		log.InfoContext(ctx, "message.appended", "seq", 3)

		got := only(t, buf)
		if got["conversation_id"] != "c-1" || got["workspace_id"] != "w-1" || got["seq"] != float64(3) {
			t.Errorf("ctx 키가 안 붙었다: %v", got)
		}
	})

	t.Run("같은 키를 다시 얹으면 마지막 값 하나만 나온다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "conversation_id", "old")
		ctx = logctx.With(ctx, "conversation_id", "new")
		log.InfoContext(ctx, "x")

		raw := buf.String()
		if strings.Count(raw, `"conversation_id"`) != 1 {
			t.Errorf("같은 키가 두 번 나왔다: %s", raw)
		}
		if got := only(t, buf); got["conversation_id"] != "new" {
			t.Errorf("마지막 값이 아니다: %v", got)
		}
	})

	t.Run("호출부 속성과 겹치면 한 번만 나오고 호출부 값이 이긴다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "conversation_id", "from-ctx")
		log.InfoContext(ctx, "x", "conversation_id", "from-call")

		raw := buf.String()
		if strings.Count(raw, `"conversation_id"`) != 1 {
			t.Errorf("같은 키가 두 번 나왔다: %s", raw)
		}
		if got := only(t, buf); got["conversation_id"] != "from-call" {
			t.Errorf("호출부 값이 아니다: %v", got)
		}
	})

	t.Run("로거에 미리 붙인 속성과 겹쳐도 한 번만 나온다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "conn_id", "from-ctx")
		log.With("conn_id", "from-logger").InfoContext(ctx, "x")

		if raw := buf.String(); strings.Count(raw, `"conn_id"`) != 1 {
			t.Errorf("같은 키가 두 번 나왔다: %s", raw)
		}
	})

	t.Run("갈라진 ctx 는 서로의 키를 덮어쓰지 않는다", func(t *testing.T) {
		base := logctx.With(context.Background(), "a", "1")
		left := logctx.With(base, "b", "left")
		right := logctx.With(base, "b", "right")
		if logctx.Value(left, "b") != "left" || logctx.Value(right, "b") != "right" || logctx.Value(base, "b") != "" {
			t.Error("부모 ctx 의 키가 바뀌었다")
		}
	})

	t.Run("WithAttrs 뒤에도 ctx 키가 붙는다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "trace_id", "t-1")
		log.With("component", "realtime").InfoContext(ctx, "x")

		got := only(t, buf)
		if got["trace_id"] != "t-1" || got["component"] != "realtime" {
			t.Errorf("속성이 빠졌다: %v", got)
		}
	})

	t.Run("WithGroup 뒤에도 ctx 키는 그룹 밖 맨 위에 붙는다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.With(context.Background(), "conversation_id", "c-9")
		log.WithGroup("req").With("method", "GET").InfoContext(ctx, "x", "status", 200)

		got := only(t, buf)
		if got["conversation_id"] != "c-9" {
			t.Fatalf("ctx 키가 맨 위에 없다: %v", got)
		}
		req, _ := got["req"].(map[string]any)
		if req["method"] != "GET" || req["status"] != float64(200) {
			t.Errorf("그룹 안의 속성이 어긋났다: %v", got)
		}
	})

	t.Run("ctx 키가 없으면 안쪽 핸들러가 받은 그대로 쓴다", func(t *testing.T) {
		log, buf := capture()
		log.InfoContext(context.Background(), "x", "k", "v")
		if got := only(t, buf); got["k"] != "v" || got["msg"] != "x" {
			t.Errorf("레코드가 바뀌었다: %v", got)
		}
	})
}

// traceparentShape 는 이 서버가 내보내는 traceparent 의 모양이다(version 00, 소문자 hex, 전부 0 이 아닌 id).
var traceparentShape = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

func validTraceparent(v string) bool {
	return traceparentShape.MatchString(v) && !strings.HasPrefix(v, "00-00000000000000000000000000000000-") &&
		!strings.Contains(v, "-0000000000000000-")
}

func TestTraceparent(t *testing.T) {
	t.Run("새로 만든 trace 는 W3C 형식이고 매번 다르다", func(t *testing.T) {
		a := logctx.NewTrace(context.Background())
		b := logctx.NewTrace(context.Background())
		for _, ctx := range []context.Context{a, b} {
			tp := logctx.Traceparent(ctx)
			if !validTraceparent(tp) || !strings.HasSuffix(tp, "-01") {
				t.Errorf("형식이 틀렸다: %q", tp)
			}
		}
		if logctx.TraceID(a) == logctx.TraceID(b) {
			t.Error("두 trace_id 가 같다")
		}
	})

	t.Run("trace 를 다시 얹으면 이전 trace_id 를 대신한다", func(t *testing.T) {
		log, buf := capture()
		ctx := logctx.NewTrace(context.Background())
		first := logctx.TraceID(ctx)
		ctx = logctx.NewTrace(ctx)
		log.InfoContext(ctx, "frame")
		raw := buf.String()
		if strings.Count(raw, `"trace_id"`) != 1 || strings.Contains(raw, first) {
			t.Errorf("이전 trace_id 가 남았다: %s", raw)
		}
	})
}

func TestSpanSource(t *testing.T) {
	type spanKey struct{}
	// 바깥 트레이서 흉내: ctx 에 스팬을 두면 그 id 를 돌려준다.
	install := func(t *testing.T) {
		t.Cleanup(func() { logctx.SetSpanSource(nil) })
		logctx.SetSpanSource(func(ctx context.Context) (logctx.Span, bool) {
			s, ok := ctx.Value(spanKey{}).(logctx.Span)
			return s, ok
		})
	}
	span := logctx.Span{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331", Sampled: true}

	t.Run("스팬이 있으면 로그의 trace_id 와 span_id 는 그 스팬의 값이다", func(t *testing.T) {
		install(t)
		log, buf := capture()
		ctx := logctx.NewTrace(context.Background())
		ctx = context.WithValue(ctx, spanKey{}, span)
		log.InfoContext(ctx, "x")

		got := only(t, buf)
		if got["trace_id"] != span.TraceID || got["span_id"] != span.SpanID {
			t.Errorf("스팬의 id 가 아니다: %v", got)
		}
		if raw := buf.String(); strings.Count(raw, `"trace_id"`) != 1 || strings.Count(raw, `"span_id"`) != 1 {
			t.Errorf("키가 두 번 나왔다: %s", raw)
		}
		if logctx.TraceID(ctx) != span.TraceID || logctx.Traceparent(ctx) != "00-"+span.TraceID+"-"+span.SpanID+"-01" {
			t.Errorf("헤더 값이 스팬과 다르다: %s %s", logctx.TraceID(ctx), logctx.Traceparent(ctx))
		}
	})

	t.Run("logctx 로 얹은 키가 없어도 스팬의 id 가 붙는다", func(t *testing.T) {
		install(t)
		log, buf := capture()
		log.InfoContext(context.WithValue(context.Background(), spanKey{}, span), "x")
		if got := only(t, buf); got["trace_id"] != span.TraceID || got["span_id"] != span.SpanID {
			t.Errorf("스팬 id 가 안 붙었다: %v", got)
		}
	})

	t.Run("샘플링하지 않은 스팬의 traceparent 는 flags 가 00 이다", func(t *testing.T) {
		install(t)
		unsampled := span
		unsampled.Sampled = false
		ctx := context.WithValue(context.Background(), spanKey{}, unsampled)
		if got := logctx.Traceparent(ctx); !strings.HasSuffix(got, "-00") {
			t.Errorf("flags: %q", got)
		}
	})

	t.Run("스팬이 없으면 직접 만든 trace_id 를 그대로 쓴다", func(t *testing.T) {
		install(t)
		log, buf := capture()
		ctx := logctx.NewTrace(context.Background())
		log.InfoContext(ctx, "x")
		if got := only(t, buf); got["trace_id"] != logctx.TraceID(ctx) || got["trace_id"] == "" {
			t.Errorf("trace_id: %v", got)
		}
	})
}
