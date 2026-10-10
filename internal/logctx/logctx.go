// Package logctx 는 로그 키를 context.Context 에 실어 나른다. 자바의 MDC 가 하는 일이다.
//
// Go 에는 스레드 로컬이 없어서 요청 하나의 키(trace_id, conversation_id …)를 함수마다 인자로 넘기거나
// 로거를 들고 다녀야 한다. 둘 다 계층마다 로깅이 새어 든다. 대신 이미 모든 계층을 지나가는 ctx 에 키를 얹고,
// slog 핸들러가 레코드를 쓸 때 ctx 에서 꺼내 붙인다. 호출부는 slog.InfoContext(ctx, "사건", 자기 속성) 만 쓴다.
//
// 표준 라이브러리만 쓴다. core 처럼 아무것도 모르는 잎 패키지라 어느 계층이든 가져다 쓸 수 있다.
package logctx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"syscall"
)

type ctxKey struct{}

// fields 는 ctx 에 얹힌 키들이다. 얹은 순서를 지킨다. 한 번 만든 값은 고치지 않는다 —
// 같은 부모 ctx 에서 갈라진 두 요청이 서로의 키를 덮어쓰면 안 된다.
type fields []slog.Attr

// With 는 키·값 쌍을 얹은 새 ctx 를 돌려준다. 같은 키가 이미 있으면 마지막 값만 남긴다.
// 인자는 slog 와 같은 모양이다: "key", value, "key2", value2 … 또는 slog.Attr.
func With(ctx context.Context, args ...any) context.Context {
	add := argsToAttrs(args)
	if len(add) == 0 {
		return ctx
	}
	old, _ := ctx.Value(ctxKey{}).(fields)
	next := make(fields, 0, len(old)+len(add))
	next = append(next, old...)
	for _, a := range add {
		if i := slices.IndexFunc(next, func(b slog.Attr) bool { return b.Key == a.Key }); i >= 0 {
			next[i] = a
			continue
		}
		next = append(next, a)
	}
	return context.WithValue(ctx, ctxKey{}, next)
}

// Value 는 ctx 에 얹힌 키의 값이다. 없으면 빈 문자열이다.
func Value(ctx context.Context, key string) string {
	for _, a := range attrs(ctx) {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

// attrs 는 ctx 에 얹힌 키다. 스팬이 있으면 trace_id·span_id 는 그 스팬의 값으로 바꿔 낸다 —
// 로그의 trace_id 와 트레이스 저장소의 트레이스 id 가 하나여야 로그 한 줄에서 트레이스로 건너간다(D33).
func attrs(ctx context.Context) fields {
	if ctx == nil {
		return nil
	}
	f, _ := ctx.Value(ctxKey{}).(fields)
	s, ok := currentSpan(ctx)
	if !ok {
		return f
	}
	out := make(fields, 0, len(f)+2)
	trace, span := slog.String(KeyTrace, s.TraceID), slog.String(KeySpan, s.SpanID)
	var hasTrace, hasSpan bool
	for _, a := range f {
		switch a.Key {
		case KeyTrace:
			a, hasTrace = trace, true
		case KeySpan:
			a, hasSpan = span, true
		}
		out = append(out, a)
	}
	if !hasTrace {
		out = append(out, trace)
	}
	if !hasSpan {
		out = append(out, span)
	}
	return out
}

// argsToAttrs 는 slog 의 인자 규칙을 따른다. 짝이 안 맞는 마지막 값은 버린다 — 로그 한 줄 때문에 요청이 죽으면 안 된다.
func argsToAttrs(args []any) []slog.Attr {
	var out []slog.Attr
	for len(args) > 0 {
		switch v := args[0].(type) {
		case slog.Attr:
			out = append(out, v)
			args = args[1:]
		case string:
			if len(args) < 2 {
				return out
			}
			out = append(out, slog.Any(v, args[1]))
			args = args[2:]
		default:
			args = args[1:]
		}
	}
	return out
}

// ErrKind 는 로그에 싣는 오류의 종류다(D30). 오류 원문은 싣지 않는다 — DB 연결 오류 문구에는 접속 정보(user=·database=·호스트:포트)가,
// 저장소 오류에는 주소가 들어갈 수 있다. 원문 대신 운영자가 무엇을 볼지 가를 만큼만 나눈다.
//
//	고정 코드          오류 사슬에 ErrCode() 를 가진 오류가 있으면 그 코드다(D47). 업무 오류(lease_lost·not_found …), 저장소의
//	                   HTTP 실패(s3_403 …), 모델 실패(ollama_http_503 …). 코드는 리포에 고정된 이름이라 원문이 아니다. 이 패키지는 core 를 모른다
//	SQLSTATE(다섯 글자) DB 가 답한 오류. SQLState() 를 가진 오류(pgx 의 PgError)에서 꺼낸다. 이 패키지는 드라이버를 모른다
//	timeout            시한 초과(context.DeadlineExceeded, 네트워크 Timeout)
//	canceled           취소(요청이 끊겼거나 내려가는 중)
//	dns                이름을 풀지 못했다(*net.DNSError)
//	conn_refused       상대가 연결을 거절했다(포트가 닫혔다 — DB 가 내려갔거나 주소가 틀렸다)
//	reset              맺은 연결이 도중에 끊겼다(RST·EPIPE·예기치 않은 EOF)
//	tls                인증서 확인이나 TLS 협상이 실패했다(RDS 엔드포인트 이름·CA 번들)
//	error              그 밖
//
// 연결 수준 실패(dns·conn_refused·reset·tls)를 error 로 뭉치면 "DB 가 내려갔다" 와 "주소가 틀렸다" 와 "질의가 틀렸다" 를 로그로 가를 수 없다(감사 15).
func ErrKind(err error) string {
	if err == nil {
		return ""
	}
	var coded interface{ ErrCode() string }
	if errors.As(err, &coded) && coded.ErrCode() != "" {
		return coded.ErrCode()
	}
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) && pg.SQLState() != "" {
		return pg.SQLState()
	}
	var timeout interface{ Timeout() bool }
	var dns *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conn_refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "reset"
	case errors.As(err, &unknownCA), errors.As(err, &hostname), errors.As(err, &invalidCert), errors.As(err, &verify),
		errors.As(err, &record), errors.As(err, &alert):
		return "tls"
	}
	return "error"
}
