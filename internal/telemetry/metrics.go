package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Instruments 는 여러 어댑터가 함께 쓰는 계기다. 이름은 chat.* 이고 단위를 적는다.
//
// 속성에는 id 를 넣지 않는다. 대화·연결·상담원 id 를 넣으면 시계열이 그 수만큼 늘어난다(카디널리티).
// 역할·결과·이유처럼 값이 몇 개뿐인 열거값만 넣는다. 어느 대화인지는 로그와 트레이스가 맡는다.
type Instruments struct {
	// AckDuration 은 WebSocket send 프레임을 받은 때부터 ack 를 다 쓴 때까지다. 속성: role.
	AckDuration metric.Float64Histogram
	// WSConnections 는 열린 WebSocket 수다. 붙으면 +1, 끊기면 -1. 속성: role.
	WSConnections metric.Int64UpDownCounter
	// NotifyFailed 는 커밋 뒤 알림이 실패하거나 시한을 넘긴 횟수다(D4). 속성: reason(timeout·error).
	NotifyFailed metric.Int64Counter
	// ListenerReconnects 는 알림 리스너가 끊겨 다시 붙으러 간 횟수다.
	ListenerReconnects metric.Int64Counter
	// Resyncs 는 알림을 놓쳤을 수 있어 전원을 깨운 횟수다(리스너가 붙을 때마다 한 번).
	Resyncs metric.Int64Counter
	// Panics 는 되살린 패닉 수다(D47). 속성: where(http·ws.read·ws.ping·ws.deliver·listener·watcher·job.<이름>).
	Panics metric.Int64Counter
}

// ackBuckets 는 ack 지연 히스토그램의 경계(ms)다. 부하 시험의 기준(1,000 msg/s 에서 p99 48ms)이 가운데 오게 잡았다.
var ackBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

func newInstruments(m metric.Meter) *Instruments {
	// 계기를 못 만들면 OTel 은 no-op 계기와 오류를 함께 돌려준다. 관측 때문에 서버가 뜨지 못하면 안 되므로 오류는 버린다.
	ack, _ := m.Float64Histogram("chat.message.ack.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("WebSocket send 프레임 수신부터 ack 전송까지"),
		metric.WithExplicitBucketBoundaries(ackBuckets...))
	conns, _ := m.Int64UpDownCounter("chat.ws.connections",
		metric.WithUnit("{connection}"),
		metric.WithDescription("열린 WebSocket 연결 수"))
	notify, _ := m.Int64Counter("chat.notify.failed",
		metric.WithUnit("{notification}"),
		metric.WithDescription("커밋 뒤 알림 실패. 저장은 성공이고 안전망이 전달한다"))
	reconnects, _ := m.Int64Counter("chat.listener.reconnects",
		metric.WithUnit("{reconnect}"),
		metric.WithDescription("알림 리스너 재접속"))
	resyncs, _ := m.Int64Counter("chat.resync.count",
		metric.WithUnit("{resync}"),
		metric.WithDescription("알림을 놓쳤을 수 있어 전원을 깨운 횟수"))
	panics, _ := m.Int64Counter("chat.panics",
		metric.WithUnit("{panic}"),
		metric.WithDescription("되살린 패닉. 그 요청·연결·작업 한 번만 끝나고 프로세스는 산다"))
	return &Instruments{
		AckDuration: ack, WSConnections: conns, NotifyFailed: notify,
		ListenerReconnects: reconnects, Resyncs: resyncs, Panics: panics,
	}
}

// 속성 값. 지표 속성은 이 열거값만 쓴다.
var (
	RoleCustomer = attribute.String("role", "customer")
	RoleAgent    = attribute.String("role", "agent")
)

// Reason 은 실패 이유 속성이다. 값은 호출부의 작은 열거값(timeout·error)이다.
func Reason(r string) attribute.KeyValue { return attribute.String("reason", r) }
