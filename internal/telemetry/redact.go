package telemetry

import (
	"context"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// D30 은 스팬에도 걸린다: 본문, 이름, 이메일, 전화, 토큰, 파일 이름, 서명된 주소, 저장소 키를 싣지 않는다.
// 대화·메시지·상담원 id 는 실어도 된다(로그와 같은 규칙).
//
// 우리가 여는 스팬(프레임·SQL)은 처음부터 id 만 싣는다. 걸러야 하는 것은 otelhttp 가 표준(semconv)대로 붙이는 주소다.
//   - 들어오는 요청의 url.path: 경로에 이메일이 있는 라우트가 있다(DELETE /api/admin/invites/{email}). 라우트 패턴(http.route)만 남긴다.
//   - 나가는 요청(S3)의 url.full: 경로가 저장소 키이고 쿼리가 서명(X-Amz-Signature…)이다. 출처(scheme://host)만 남긴다.
//   - 나가는 요청이 실패하면 스팬 상태 설명이 오류 문구인데, net/http 의 오류 문구에는 주소 전체가 들어 있다.
//
//   - 들어오는 요청의 client.address·network.peer.*·user_agent.original: 사람을 가리키는 값이다. 버린다.
//
// 지표는 사정이 다르다. View 의 AttributeFilter 로 속성을 걸러도 SDK 는 걸러 낸 속성을 exemplar 의 filtered_attributes 로
// 내보낸다(감사 24 — Host 2만 자가 그렇게 나갔다). 그래서 지표에 대해서는 클라이언트가 정하는 값을 기록 단계에서 싣지 않는 것이 규칙이고
// (httpapi.withSpan 이 설정의 서버 이름을 줘 Host 를 읽지 않는다), 아래 metricView 는 새 계측이 표준 속성을 더할 때의 둘째 방어다.
//
// 걸러 내기는 내보내기 직전(익스포터 앞)에 한다. 요청 경로에서 일하지 않고, 어느 계측이 붙였든 한 곳에서 막는다.
// 운영(Setup)과 테스트가 같은 NewTracerProvider 를 지나므로 테스트가 이 길을 그대로 확인한다.

// dropKeys 는 통째로 버리는 속성이다.
var dropKeys = map[attribute.Key]bool{
	"url.path":    true,
	"url.query":   true,
	"http.target": true,
	// 사람(클라이언트)을 가리키는 값이다. otelhttp 가 표준대로 요청마다 붙인다. 고객의 IP 와 브라우저 식별 문자열은
	// 로그에 적지 않는 것(D30)과 같은 이유로 스팬·지표에도 싣지 않는다. 남용 추적은 ALB·CDN 의 접근 로그가 맡는다.
	"client.address":       true,
	"network.peer.address": true,
	"network.peer.port":    true,
	"user_agent.original":  true,
	// 클라이언트가 정한 글자다(감사 23). 모르는 메서드의 원문(http.request.method 는 _OTHER 로 실린다)과 Host 헤더.
	// Host 는 감사 24 부터 처음부터 읽지 않는다(서버 이름이 설정 값이다). 여기 남긴 것은 다른 계측이 Host 를 실을 때의 둘째 방어다.
	"http.request.method_original": true,
	"server.address":               true,
}

// dropped 는 통째로 버릴 속성인지다. 스팬과 지표가 같은 판정을 쓴다.
func dropped(k attribute.Key) bool {
	return dropKeys[k] || strings.HasPrefix(string(k), "db.query.parameter.")
}

// metricView 는 지표의 모든 계기에서 버릴 속성을 뺀다. 지표는 내보내기 앞에서 고칠 수 없어(모은 뒤라 같은 점이 합쳐져 있다)
// 모으는 단계(View)에서 거른다. 걸러 낸 속성은 exemplar 의 filtered_attributes 로는 나간다 — 위 설명처럼 둘째 방어일 뿐이다.
var metricView = sdkmetric.NewView(
	sdkmetric.Instrument{Name: "*"},
	sdkmetric.Stream{AttributeFilter: func(kv attribute.KeyValue) bool { return !dropped(kv.Key) }},
)

// originKeys 는 출처만 남기는 속성이다.
var originKeys = map[attribute.Key]bool{
	"url.full": true,
	"http.url": true,
}

// urlPattern 은 문자열 안의 주소다. 출처(scheme://host[:port]) 뒤의 경로·쿼리를 잘라 낸다.
var urlPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://[^/\s?#"'<>]+)[^\s"'<>]*`)

// scrubString 은 문자열 속의 주소를 출처만 남긴다.
func scrubString(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlPattern.ReplaceAllString(s, "$1")
}

func scrubAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if dropped(kv.Key) {
			continue
		}
		switch kv.Value.Type() {
		case attribute.STRING:
			v := kv.Value.AsString()
			if originKeys[kv.Key] {
				v = origin(v)
			}
			out = append(out, kv.Key.String(scrubString(v)))
		case attribute.STRINGSLICE:
			vs := kv.Value.AsStringSlice()
			for i := range vs {
				vs[i] = scrubString(vs[i])
			}
			out = append(out, kv.Key.StringSlice(vs))
		default:
			out = append(out, kv)
		}
	}
	return out
}

// origin 은 주소의 scheme://host 만 남긴다. 주소가 아니면 비운다.
func origin(u string) string {
	if m := urlPattern.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

// redactedSpan 은 내보낼 스팬의 이름·속성·이벤트·상태 설명을 걸러 낸 모습이다. 나머지는 원래 스팬을 그대로 읽는다.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Name() string { return scrubString(s.ReadOnlySpan.Name()) }

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return scrubAttrs(s.ReadOnlySpan.Attributes())
}

func (s redactedSpan) Events() []sdktrace.Event {
	events := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(events))
	for i, e := range events {
		e.Name = scrubString(e.Name)
		e.Attributes = scrubAttrs(e.Attributes)
		out[i] = e
	}
	return out
}

func (s redactedSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = scrubString(st.Description)
	return st
}

// redactingExporter 는 익스포터 앞에서 스팬을 걸러 낸다.
type redactingExporter struct {
	next sdktrace.SpanExporter
}

func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redactedSpan{s}
	}
	return e.next.ExportSpans(ctx, out)
}

func (e redactingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }
