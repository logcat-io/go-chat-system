package logctx_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/internal/logctx"
)

// netTimeout 은 네트워크 계층의 시한 초과 대역이다(net.Error 의 Timeout). context.DeadlineExceeded 로는 보이지 않는다.
type netTimeout struct{}

func (netTimeout) Error() string   { return "i/o timeout to 10.0.0.5:5432" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

// 감사 15: 연결 수준 실패(거절·이름 풀이·끊김)가 모두 error 였다. 원문을 빼면서 운영 진단도 함께 잃었다.
// 종류만으로 "DB 가 내려갔다(conn_refused)" 와 "주소가 틀렸다(dns)" 와 "도중에 끊겼다(reset)" 를 가른다. 원문은 어디에도 남지 않는다.
func TestErrKind_연결수준실패는거절과이름풀이와끊김과TLS와시한초과를가르고취소는canceled다(t *testing.T) {
	wrap := func(err error) error {
		return fmt.Errorf("failed to connect to `user=chat database=chat`: %w", &net.OpError{Op: "dial", Net: "tcp", Err: err})
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"거절", wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), "conn_refused"},
		{"이름 풀이", wrap(&net.DNSError{Err: "no such host", Name: "db.internal", IsNotFound: true}), "dns"},
		{"상대가 끊음(RST)", wrap(syscall.ECONNRESET), "reset"},
		{"끊긴 파이프", wrap(syscall.EPIPE), "reset"},
		{"예기치 않은 EOF", fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), "reset"},
		{"상대가 닫음(EOF)", fmt.Errorf("receive message: %w", io.EOF), "reset"},
		{"네트워크 시한 초과", wrap(netTimeout{}), "timeout"},
		{"인증서 이름 불일치", fmt.Errorf("tls error: %w", x509.HostnameError{Host: "db.internal"}), "tls"},
		{"모르는 CA", fmt.Errorf("tls error: %w", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), "tls"},
		{"ctx 시한 초과", fmt.Errorf("query: %w", context.DeadlineExceeded), "timeout"},
		{"취소", fmt.Errorf("query: %w", context.Canceled), "canceled"},
		{"그 밖", errors.New("unexpected user=chat"), "error"},
	}
	for _, tc := range cases {
		if got := logctx.ErrKind(tc.err); got != tc.want {
			t.Errorf("%s: 종류가 %q 다. %q 여야 한다", tc.name, got, tc.want)
		}
	}
	if logctx.ErrKind(nil) != "" {
		t.Error("오류가 없으면 빈 종류다")
	}
}

// 실제 다이얼이 내는 오류로도 같다. 닫힌 포트는 conn_refused 다(DB 가 내려간 장애 조치 순간의 모양).
func TestErrKind_닫힌포트에붙으면conn_refused다(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = (&net.Dialer{Timeout: 2 * time.Second}).Dial("tcp", addr)
	if err == nil {
		t.Fatal("닫은 포트에 다시 붙었다 — 다른 프로세스가 잡았다. 다시 돌려라")
	}
	if got := logctx.ErrKind(err); got != "conn_refused" {
		t.Errorf("닫힌 포트의 종류가 %q 다(%v). conn_refused 여야 한다", got, err)
	}
	if strings.Contains(logctx.ErrKind(err), "127.0.0.1") {
		t.Error("종류에 주소가 들어갔다")
	}
}

// 맺은 연결을 상대가 먼저 닫으면(장애 조치로 DB 가 연결을 놓았을 때의 모양) 읽기가 io.EOF 를 받는다. 이것도 reset 이다 —
// error 로 뭉치면 "도중에 끊겼다" 와 "질의가 틀렸다" 를 가르지 못한다(감사 16 X11).
func TestErrKind_맺은연결을상대가닫아읽기가EOF를받으면reset이다(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("닫힌 연결 읽기가 %v 다. io.EOF 여야 이 테스트가 뜻이 있다", err)
	}
	if got := logctx.ErrKind(fmt.Errorf("receive message: %w", err)); got != "reset" {
		t.Errorf("상대가 닫은 연결의 종류가 %q 다. reset 이어야 한다", got)
	}
}

// pgLike 는 SQLSTATE 를 가진 DB 오류 대역이다.
type pgLike struct{}

func (pgLike) Error() string    { return "ERROR: deadlock detected (SQLSTATE 40P01)" }
func (pgLike) SQLState() string { return "40P01" }

// coded 는 고정 코드를 가진 오류 대역이다(core.CodedError 와 같은 메서드). logctx 는 core 를 모른다.
type coded struct {
	code  string
	inner error
}

func (c coded) Error() string   { return "업무 오류: " + c.code }
func (c coded) ErrCode() string { return c.code }
func (c coded) Unwrap() error   { return c.inner }

// D47: 업무 오류는 고정 코드로 남는다. 전에는 lease_lost·too_many_retries·object_exists 가 모두 error 였다.
// 고정 코드가 있으면 감싼 DB 오류나 시한 초과보다 먼저 본다 — 코드가 더 좁은 뜻이다.
func TestErrKind_고정코드가있으면SQLSTATE와연결종류보다먼저그코드다(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"고정 코드", coded{code: "lease_lost"}, "lease_lost"},
		{"감싼 고정 코드", fmt.Errorf("완료: %w", coded{code: "too_many_retries"}), "too_many_retries"},
		{"DB 오류를 감싼 고정 코드", coded{code: "invalid_reference", inner: pgLike{}}, "invalid_reference"},
		{"시한 초과를 감싼 고정 코드", coded{code: "storage_timeout", inner: context.DeadlineExceeded}, "storage_timeout"},
		{"빈 코드는 무시한다", coded{code: "", inner: pgLike{}}, "40P01"},
		{"코드 없는 DB 오류", fmt.Errorf("저장: %w", pgLike{}), "40P01"},
	}
	for _, tc := range cases {
		if got := logctx.ErrKind(tc.err); got != tc.want {
			t.Errorf("%s: %q, %q 여야 한다", tc.name, got, tc.want)
		}
	}
}
