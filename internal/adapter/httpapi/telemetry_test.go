package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/logcat-io/go-chat-system/internal/id"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
	"github.com/logcat-io/go-chat-system/internal/testsupport"
)

// 관측(D33)의 정책. 스팬은 메모리 익스포터로, 지표는 수동 리더로 받는다. 운영과 같은 telemetry.NewTracerProvider 를 지난다 —
// D30 거르기가 그 안에 있다. 서버(하네스)는 설치한 뒤에 띄운다. otelhttp 의 지표 계기가 만들 때의 프로바이더를 쓴다.

type observed struct {
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
}

func observe(t *testing.T) *observed {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	restore := telemetry.Install(telemetry.NewTracerProvider(exp, false), telemetry.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(restore)
	return &observed{spans: exp, reader: reader}
}

// awaitSpan 은 조건에 맞는 스팬이 끝날 때까지 기다린다. 서버 스팬은 응답을 다 쓴 뒤에 끝난다.
func (o *observed) awaitSpan(t *testing.T, pred func(tracetest.SpanStub) bool) tracetest.SpanStub {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range o.spans.GetSpans() {
			if pred(s) {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var names []string
	for _, s := range o.spans.GetSpans() {
		names = append(names, s.Name)
	}
	t.Fatalf("기다린 스팬이 없다. 있는 것: %v", names)
	return tracetest.SpanStub{}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

func named(name string) func(tracetest.SpanStub) bool {
	return func(s tracetest.SpanStub) bool { return s.Name == name }
}

// spanDump 는 스팬이 내보내는 글자 전부다: 이름, 속성 키와 값, 이벤트, 상태 설명, 링크 속성.
func spanDump(s tracetest.SpanStub) string {
	var b strings.Builder
	b.WriteString(s.Name + "\n" + s.Status.Description + "\n")
	write := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			fmt.Fprintf(&b, "%s=%s\n", kv.Key, kv.Value.String())
		}
	}
	write(s.Attributes)
	for _, e := range s.Events {
		b.WriteString(e.Name + "\n")
		write(e.Attributes)
	}
	for _, l := range s.Links {
		write(l.Attributes)
	}
	return b.String()
}

func TestTelemetry_요청하나의HTTP스팬과그안의SQL스팬은같은트레이스이고로그의trace_id와같다(t *testing.T) {
	o := observe(t)
	h, logs := newLoggedHarness(t)
	cust, convID := h.customerSession()

	res := h.roundTrip(http.MethodPost, "/api/conversations/"+convID+"/messages", cust,
		map[string]any{"client_msg_id": id.New(), "body": "트레이스 확인"}, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("보내기 %d", res.StatusCode)
	}
	traceID := res.Header.Get("X-Trace-Id")

	server := o.awaitSpan(t, named("POST /api/conversations/{id}/messages"))
	if got := server.SpanContext.TraceID().String(); got != traceID {
		t.Fatalf("HTTP 스팬의 트레이스(%s)가 응답의 X-Trace-Id(%s)와 다르다", got, traceID)
	}
	if attr(server, "http.route") != "/api/conversations/{id}/messages" {
		t.Errorf("http.route: %q", attr(server, "http.route"))
	}
	if strings.Contains(server.Name, convID) {
		t.Errorf("스팬 이름에 경로의 id 가 들어갔다: %s", server.Name)
	}

	// 그 요청 안의 SQL 은 같은 트레이스이고, 부모를 따라 올라가면 HTTP 스팬에 닿는다.
	byID := map[string]tracetest.SpanStub{}
	for _, s := range o.spans.GetSpans() {
		byID[s.SpanContext.SpanID().String()] = s
	}
	sqlSpans := 0
	for _, s := range o.spans.GetSpans() {
		if attr(s, "db.query.text") == "" || s.SpanContext.TraceID().String() != traceID {
			continue
		}
		sqlSpans++
		cur, hops := s, 0
		for cur.SpanContext.SpanID() != server.SpanContext.SpanID() && hops < 10 {
			parent, ok := byID[cur.Parent.SpanID().String()]
			if !ok {
				t.Fatalf("SQL 스팬 %q 의 부모를 따라가다 끊겼다", s.Name)
			}
			cur, hops = parent, hops+1
		}
	}
	if sqlSpans == 0 {
		t.Fatal("요청 안의 SQL 스팬이 없다")
	}

	// 같은 요청의 로그는 그 트레이스 id 와, 그 트레이스 안의 스팬 id 를 단다.
	appended := linesOf(t, logs, res, "message.appended")
	if len(appended) != 1 {
		t.Fatalf("message.appended 가 trace_id=%s 로 한 줄이어야 한다:\n%s", traceID, logs.String())
	}
	span, ok := byID[appended[0].str("span_id")]
	if !ok || span.SpanContext.TraceID().String() != traceID {
		t.Errorf("로그의 span_id(%s)가 그 트레이스의 스팬이 아니다", appended[0].str("span_id"))
	}
	request := linesOf(t, logs, res, "http.request")
	if len(request) != 1 || request[0].str("span_id") != server.SpanContext.SpanID().String() {
		t.Errorf("http.request 의 span_id 가 HTTP 스팬이 아니다: %v", request)
	}
}

// D33(감사 24): 공개 입구는 바깥 추적 헤더를 믿지 않는다. 클라이언트가 trace id 를 정하면 문의 번호(D47)를 남의 요청과 같게 만들 수 있고,
// tracestate 원문은 스팬의 트레이스 상태로 그대로 복사되어 나간다. 요청마다 서버가 새 루트 트레이스를 만든다.
func TestTelemetry_들어온traceparent와tracestate와baggage는이어받지않고요청마다새루트트레이스다(t *testing.T) {
	o := observe(t)
	h, logs := newLoggedHarness(t)
	const incomingTrace, incomingSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	const mark = "tsmark-hong-example-com"

	res := h.roundTrip(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "추적고객"},
		map[string]string{
			"traceparent": "00-" + incomingTrace + "-" + incomingSpan + "-01",
			"tracestate":  "vendor=" + mark,
			"baggage":     "user=" + mark,
		})
	res.Body.Close()

	server := o.awaitSpan(t, named("POST /api/customer/session"))
	got := server.SpanContext.TraceID().String()
	if got == incomingTrace || server.Parent.IsValid() {
		t.Errorf("들어온 트레이스를 이어받았다: trace %s, 부모 %v", got, server.Parent)
	}
	if res.Header.Get("X-Trace-Id") != got {
		t.Errorf("X-Trace-Id(%s)가 새 루트 스팬의 트레이스(%s)가 아니다", res.Header.Get("X-Trace-Id"), got)
	}
	for _, s := range o.spans.GetSpans() {
		if s.SpanContext.TraceID().String() == incomingTrace || s.SpanContext.TraceState().Len() != 0 ||
			strings.Contains(spanDump(s)+s.SpanContext.TraceState().String(), mark) {
			t.Errorf("스팬 %q 에 들어온 트레이스·tracestate·baggage 가 남았다: %s", s.Name, s.SpanContext.TraceState().String())
		}
	}
	if lines := logs.where(t, "trace_id", incomingTrace); len(lines) != 0 {
		t.Errorf("들어온 trace_id 로 남은 로그가 %d 줄이다", len(lines))
	}
	if lines := logs.where(t, "trace_id", got); !hasMsg(lines, "session.created") {
		t.Errorf("session.created 가 새 trace_id 로 남지 않았다:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), mark) {
		t.Error("로그에 tracestate·baggage 원문이 남았다")
	}
}

// D30·D33(감사 24): Host 헤더는 클라이언트가 정한 글자다. 지표는 View 로 server.address 를 걸러도 exemplar 의 filtered_attributes 에
// 거른 값이 그대로 실려 OTLP 로 나갔다(Host 2만 자). 이제 otelhttp 에 설정의 서버 이름을 줘 기록 단계에서 Host 를 읽지 않는다.
func TestTelemetry_Host헤더는지표의속성에도exemplar에도스팬에도남지않는다(t *testing.T) {
	o := observe(t)
	h, _ := newLoggedHarness(t)
	const mark = "hostmark"
	host := mark + strings.Repeat("h", 20000) + ".example.com:4567"

	res := h.roundTrip(http.MethodPost, "/api/customer/session", "", map[string]any{"display_name": "호스트"},
		map[string]string{"Host": host})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("세션 %d", res.StatusCode)
	}
	server := o.awaitSpan(t, named("POST /api/customer/session"))
	if strings.Contains(spanDump(server), mark) || strings.Contains(spanDump(server), "4567") {
		t.Errorf("스팬에 Host 가 남았다:\n%.300s", spanDump(server))
	}

	rm := collect(t, o.reader)
	m, ok := findMetric(rm, "http.server.request.duration")
	if !ok {
		t.Fatal("HTTP 서버 지표가 없다 — 대상을 만들지 못했다")
	}
	exemplars := 0
	for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			if strings.Contains(kv.Value.String(), mark) || (kv.Key == "server.port" && kv.Value.AsInt64() == 4567) {
				t.Errorf("지표 속성 %s 에 Host 가 있다", kv.Key)
			}
		}
		for _, ex := range dp.Exemplars {
			exemplars++
			for _, kv := range ex.FilteredAttributes {
				if strings.Contains(kv.Value.String(), mark) || (kv.Key == "server.port" && kv.Value.AsInt64() == 4567) {
					t.Errorf("exemplar 의 filtered_attributes %s 에 Host 가 있다", kv.Key)
				}
			}
		}
	}
	if exemplars == 0 {
		t.Fatal("exemplar 가 하나도 없다 — 샘플링한 스팬 안에서 기록하는 길을 만들지 못했다")
	}
	// 남은 지표 전부(속성·exemplar)를 글자로 훑는다. 다른 계기가 Host 를 읽어도 잡는다.
	if dump := fmt.Sprintf("%+v", rm.ScopeMetrics); strings.Contains(dump, mark) {
		t.Error("지표 어딘가에 Host 가 남았다")
	}
}

func TestTelemetry_스팬어디에도본문이메일토큰파일이름저장소키서명쿼리가없고SQL스팬에는문장만있고인자값이없다(t *testing.T) {
	o := observe(t)
	h, _ := newLoggedHarness(t)
	ctx := testCtx(t, 15*time.Second)
	secret := strings.ToLower(id.New()[24:])

	// 고객: 본문이 든 메시지를 REST 와 WebSocket 으로 보낸다. WebSocket 토큰은 주소의 쿼리에 실린다.
	cust, convID := h.customerSession()
	restBody := "REST본문-" + secret
	h.send(convID, cust, restBody, false)
	wsBody := "WS본문-" + secret
	c := h.dial(ctx, cust)
	c.subscribe(ctx, convID, 0)
	c.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": wsBody})
	if ack := c.await(ctx, func(f frame) bool { return f.Type == "ack" || f.Type == "error" }); ack.Type != "ack" {
		t.Fatalf("ack 가 아니다: %+v", ack)
	}

	// 첨부: 파일 이름, 서명된 주소(양식·내려받기의 token), 저장소 키.
	filename := "비밀파일-" + secret + ".txt"
	att := h.upload(convID, cust, filename, []byte("hello"))
	attID, _ := att["attachment"].(map[string]any)["id"].(string)
	started := h.startUpload(convID, cust, "두번째-"+secret+".txt", "text/plain", 3)
	link, _ := h.get("/api/attachments/"+attID+"/url", cust)["url"].(string)
	ref, err := h.store.AttachmentFile(context.Background(), attID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)

	// 상담원: 로그인(이메일·비밀번호가 본문에), 초대와 초대 취소(이메일이 경로에).
	admin := h.adminToken()
	agent := testsupport.Agent(t, h.store)
	login := h.post("/api/agent/login", "", map[string]any{"email": agent.Email, "password": "chat1234"})
	agentTok, _ := login["token"].(string)
	inviteEmail := "invitee-" + secret + "@example.com"
	h.post("/api/admin/invites", admin, map[string]any{"email": inviteEmail, "name": "초대-" + secret, "role": "STAFF"})
	h.status(http.MethodDelete, "/api/admin/invites/"+url.PathEscape(inviteEmail), admin, nil)

	c.ws.CloseNow()
	o.awaitSpan(t, named("GET /ws"))
	o.awaitSpan(t, named("DELETE /api/admin/invites/{email}"))

	forbidden := map[string]string{
		"REST 본문": restBody, "WS 본문": wsBody, "본문 꼬리": secret,
		"고객 토큰": cust, "상담원 토큰": agentTok, "관리자 토큰": admin,
		"상담원 이메일": agent.Email, "초대 이메일": inviteEmail, "비밀번호": "chat1234",
		"파일 이름": filename, "저장소 키": ref.StoragePath,
		"내려받기 서명": u.Query().Get("token"), "올리기 서명": started.Upload.Fields["token"],
	}
	spans := o.spans.GetSpans()
	sqlSpans := 0
	for _, s := range spans {
		dump := spanDump(s)
		for what, v := range forbidden {
			if v != "" && strings.Contains(dump, v) {
				t.Errorf("스팬 %q 에 %s(%q)가 있다:\n%s", s.Name, what, v, dump)
			}
		}
		for _, kv := range s.Attributes {
			if strings.HasPrefix(string(kv.Key), "db.query.parameter") || kv.Key == "url.path" || kv.Key == "url.query" {
				t.Errorf("스팬 %q 에 %s 속성이 있다", s.Name, kv.Key)
			}
			// 사람을 가리키는 값(IP·브라우저 식별 문자열)도 싣지 않는다(D30·D33).
			if personalKeys[string(kv.Key)] {
				t.Errorf("스팬 %q 에 %s 속성이 있다: %s", s.Name, kv.Key, kv.Value.String())
			}
		}
		if attr(s, "db.query.text") != "" {
			sqlSpans++
		}
	}
	// SQL 스팬에는 문장이 있다. 본문을 넣는 INSERT 도 자리표시자뿐이다.
	if sqlSpans < 10 {
		t.Errorf("SQL 스팬이 너무 적다(%d) — 걸러 낼 대상을 만들지 못했다", sqlSpans)
	}
	// 지표 속성에도 없다. otelhttp 의 http.server.* 지표가 대상이다.
	rm := collect(t, o.reader)
	if _, ok := findMetric(rm, "http.server.request.duration"); !ok {
		t.Error("HTTP 서버 지표가 없다 — 걸러 낼 대상을 만들지 못했다")
	}
	for _, key := range metricAttrKeys(rm) {
		if personalKeys[key] {
			t.Errorf("지표 속성에 %s 가 있다", key)
		}
	}

	insert := o.awaitSpan(t, func(s tracetest.SpanStub) bool {
		return strings.Contains(attr(s, "db.query.text"), "INSERT INTO messages")
	})
	if !strings.Contains(attr(insert, "db.query.text"), "$1") {
		t.Errorf("메시지 INSERT 문장이 자리표시자가 아니다: %s", attr(insert, "db.query.text"))
	}
}

// personalKeys 는 사람(클라이언트)을 가리키는 표준 속성이다. 내보내는 스팬과 지표 어디에도 없어야 한다.
var personalKeys = map[string]bool{
	"client.address": true, "network.peer.address": true, "network.peer.port": true, "user_agent.original": true,
}

// metricAttrKeys 는 모든 지표의 모든 데이터 점에 붙은 속성 키다.
func metricAttrKeys(rm metricdata.ResourceMetrics) []string {
	var keys []string
	add := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			keys = append(keys, string(kv.Key))
		}
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					add(dp.Attributes)
				}
			}
		}
	}
	return keys
}

// collect 는 수동 리더로 지금까지의 지표를 모은다.
func collect(t *testing.T, r *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func connections(t *testing.T, r *sdkmetric.ManualReader, role string) int64 {
	t.Helper()
	m, ok := findMetric(collect(t, r), "chat.ws.connections")
	if !ok {
		return 0
	}
	var n int64
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		if v, _ := dp.Attributes.Value("role"); v.AsString() == role {
			n += dp.Value
		}
	}
	return n
}

func TestTelemetry_메시지를보내면ack지연히스토그램에하나가쌓이고WebSocket연결수가늘었다준다(t *testing.T) {
	o := observe(t)
	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 10*time.Second)
	cust, convID := h.customerSession()

	if n := connections(t, o.reader, "customer"); n != 0 {
		t.Fatalf("붙기 전 연결 수가 %d 다", n)
	}
	c := h.dial(ctx, cust)
	c.subscribe(ctx, convID, 0)
	if n := connections(t, o.reader, "customer"); n != 1 {
		t.Fatalf("붙은 뒤 연결 수가 1이 아니라 %d 다", n)
	}
	c.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": "지표"})
	if ack := c.await(ctx, func(f frame) bool { return f.Type == "ack" || f.Type == "error" }); ack.Type != "ack" {
		t.Fatalf("ack 가 아니다: %+v", ack)
	}

	// ack 를 다 쓴 뒤에 기록하므로 잠깐 기다린다.
	var count uint64
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && count == 0 {
		if m, ok := findMetric(collect(t, o.reader), "chat.message.ack.duration"); ok {
			if m.Unit != "ms" {
				t.Errorf("단위: %q", m.Unit)
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if v, _ := dp.Attributes.Value("role"); v.AsString() != "customer" || dp.Attributes.Len() != 1 {
					t.Errorf("속성은 role 하나여야 한다: %v", dp.Attributes.ToSlice())
				}
				count += dp.Count
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count != 1 {
		t.Fatalf("ack 지연이 %d개 쌓였다. 1개여야 한다", count)
	}

	_ = c.ws.CloseNow()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && connections(t, o.reader, "customer") != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := connections(t, o.reader, "customer"); n != 0 {
		t.Errorf("끊긴 뒤 연결 수가 0이 아니라 %d 다", n)
	}
}

func TestTelemetry_컬렉터가없어도메시지저장과ack가된다(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	_ = ln.Close() // 닫힌 포트: 컬렉터가 죽어 있는 상태다
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", closed)
	// 테스트 시간 안에 내보내기를 여러 번 시도하게 주기를 줄인다.
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "50")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")

	tel, err := telemetry.Setup(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if traces, metrics := telemetry.Enabled(); !traces || !metrics {
		t.Fatal("엔드포인트를 줬는데 내보내기가 꺼져 있다")
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	h := newHarness(t, time.Second, false)
	ctx := testCtx(t, 20*time.Second)
	cust, convID := h.customerSession()
	c := h.dial(ctx, cust)
	c.subscribe(ctx, convID, 0)
	before := len(h.messagesOf(convID, cust)) // 자동 메시지가 있을 수 있다
	const n = 20
	for i := 0; i < n; i++ {
		start := time.Now()
		c.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": id.New(), "body": fmt.Sprintf("컬렉터 없음 %d", i)})
		ack := c.await(ctx, func(f frame) bool { return f.Type == "ack" || f.Type == "error" })
		if ack.Type != "ack" {
			t.Fatalf("%d번째가 ack 가 아니다: %+v", i, ack)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%d번째 ack 가 %s 걸렸다", i, d)
		}
		time.Sleep(10 * time.Millisecond) // 배치 내보내기가 사이사이 실패하게 둔다
	}
	h.send(convID, cust, "REST 도 된다", false)
	if got := len(h.messagesOf(convID, cust)) - before; got != n+1 {
		t.Errorf("새로 저장된 메시지가 %d건이다. %d건이어야 한다", got, n+1)
	}

	// 종료 때 내보내기는 시한 안에서 포기한다.
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_ = tel.Shutdown(sctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("내보내기 종료가 시한(1초)을 넘겨 %s 걸렸다", d)
	}
}

func TestTelemetry_WebSocket프레임은새트레이스로연결스팬과링크되고연결스팬아래에는전달조회가쌓이지않는다(t *testing.T) {
	o := observe(t)
	h := newHarness(t, time.Second, true)
	ctx := testCtx(t, 10*time.Second)
	cust, convID := h.customerSession()
	c := h.dial(ctx, cust)
	c.subscribe(ctx, convID, 0)
	clientMsgID := id.New()
	c.send(ctx, map[string]any{"type": "send", "conversation_id": convID, "client_msg_id": clientMsgID, "body": "링크"})
	c.await(ctx, func(f frame) bool { return f.Type == "message" && f.ClientMsgID == clientMsgID }) // 전달 루프가 조회해 내려보냈다
	_ = c.ws.CloseNow()

	conn := o.awaitSpan(t, named("GET /ws"))
	send := o.awaitSpan(t, named("ws send"))
	if send.SpanContext.TraceID() == conn.SpanContext.TraceID() || send.Parent.IsValid() {
		t.Errorf("프레임 스팬이 연결 트레이스 안에 있다")
	}
	if len(send.Links) != 1 || send.Links[0].SpanContext.SpanID() != conn.SpanContext.SpanID() {
		t.Errorf("프레임 스팬이 연결 스팬과 링크되지 않았다: %v", send.Links)
	}
	if attr(send, "chat.conversation_id") != convID || attr(send, "chat.ws.frame") != "send" {
		t.Errorf("프레임 속성: %v", send.Attributes)
	}
	insert := false
	for _, s := range o.spans.GetSpans() {
		if s.Parent.SpanID() == send.SpanContext.SpanID() && strings.Contains(attr(s, "db.query.text"), "INSERT INTO messages") {
			insert = true
		}
		// 업그레이드 전의 인증 조회는 연결 스팬 아래가 맞다. 업그레이드 뒤(첫 프레임 이후)에는 아무것도 붙지 않아야 한다.
		if s.SpanContext.TraceID() == conn.SpanContext.TraceID() && s.StartTime.After(send.StartTime) {
			t.Errorf("연결 트레이스 아래에 업그레이드 뒤의 %q 가 쌓였다", s.Name)
		}
	}
	if !insert {
		t.Error("send 프레임 스팬 아래에 메시지 INSERT 가 없다")
	}
}

// D47(감사 24, DEF-144 의 남은 갈래): 트레이스를 내보내도 구독이 풀리는 not_found 프레임의 trace_id 는 그 풀림 하나를 가리킨다.
// 연결 ctx 에는 업그레이드 요청의 스팬이 있어, 새 logctx id 를 얹어도 로그와 프레임에는 연결 내내 같은 trace 가 실렸다.
// 트레이스를 끈 경우는 realtime 의 TestConn_구독이풀리는not_found프레임도… 가 본다.
func TestTelemetry_트레이스를내보내도구독이풀리는not_found프레임의trace_id는그풀림의새루트트레이스다(t *testing.T) {
	o := observe(t)
	h, logs := newCapturedHarness(t, time.Second, true)
	ctx := testCtx(t, 15*time.Second)
	cust, convID := h.customerSession()
	c := h.dial(ctx, cust)
	c.subscribe(ctx, convID, 0)

	agent := testsupport.Agent(t, h.store)
	if err := h.store.DeleteConversation(ctx, convID, agent.ID, "지운 상담"); err != nil {
		t.Fatal(err)
	}
	f := c.await(ctx, func(f frame) bool { return f.Type == "error" })
	if f.Code != "not_found" || len(f.TraceID) != 32 {
		t.Fatalf("프레임 %+v — not_found 와 32자리 trace_id 여야 한다", f)
	}
	_ = c.ws.CloseNow()
	conn := o.awaitSpan(t, named("GET /ws"))
	if f.TraceID == conn.SpanContext.TraceID().String() {
		t.Fatal("not_found 프레임의 trace_id 가 연결(업그레이드 요청)의 트레이스다 — 그 풀림 하나를 가리키지 않는다")
	}
	unsub := o.awaitSpan(t, named("ws unsubscribe"))
	if unsub.SpanContext.TraceID().String() != f.TraceID || unsub.Parent.IsValid() {
		t.Errorf("ws unsubscribe 스팬(%s, 부모 %v)이 프레임의 trace_id(%s)의 루트가 아니다", unsub.SpanContext.TraceID(), unsub.Parent, f.TraceID)
	}
	if len(unsub.Links) != 1 || unsub.Links[0].SpanContext.SpanID() != conn.SpanContext.SpanID() {
		t.Errorf("ws unsubscribe 스팬이 연결 스팬과 링크되지 않았다: %v", unsub.Links)
	}
	lines := logs.where(t, "trace_id", f.TraceID)
	if len(lines) != 1 || lines[0].str("msg") != "ws.unsubscribed" || lines[0].str("reason") != "not_found" {
		t.Errorf("trace_id=%s 의 로그는 ws.unsubscribed reason=not_found 한 줄이어야 한다: %v", f.TraceID, msgs(lines))
	}
}
