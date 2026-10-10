package s3store_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/logcat-io/go-chat-system/internal/adapter/s3store"
	"github.com/logcat-io/go-chat-system/internal/telemetry"
)

// spanText 는 스팬 하나가 내보내는 글자 전부다: 이름, 속성, 이벤트, 상태 설명.
func spanText(s tracetest.SpanStub) string {
	var b strings.Builder
	b.WriteString(s.Name + "\n" + s.Status.Description + "\n")
	for _, kv := range s.Attributes {
		fmt.Fprintf(&b, "%s=%s\n", kv.Key, kv.Value.String())
	}
	for _, e := range s.Events {
		b.WriteString(e.Name + "\n")
		for _, kv := range e.Attributes {
			fmt.Fprintf(&b, "%s=%s\n", kv.Key, kv.Value.String())
		}
	}
	return b.String()
}

// 스팬에도 D30 이 걸린다. S3 주소의 경로는 저장소 키이고 쿼리는 서명이다. 스팬에는 출처(scheme://host)만 남는다.
func TestS3_S3호출의스팬에는서명쿼리와저장소키가없다(t *testing.T) {
	const bucket, access = "span-bucket", "SPANACCESSKEY"
	key, final := "tmp/span-attachment", "ws/span-ws/span-conv/span-att/span-token"
	forbidden := []string{"X-Amz-", "Signature", "Credential", bucket, access, "span-attachment", "span-token", "?"}

	run := func(t *testing.T, endpoint string) []tracetest.SpanStub {
		t.Helper()
		exp := tracetest.NewInMemoryExporter()
		restore := telemetry.Install(telemetry.NewTracerProvider(exp, false), noop.NewMeterProvider())
		t.Cleanup(restore)
		store, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: bucket, AccessKey: access, SecretKey: "secret", PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		_, _ = store.Stat(ctx, key)
		_ = store.Remove(ctx, final)
		_ = store.Promote(ctx, key, final)
		spans := exp.GetSpans()
		if len(spans) < 3 {
			t.Fatalf("S3 호출 스팬이 모자란다: %d", len(spans))
		}
		for _, s := range spans {
			text := spanText(s)
			for _, f := range forbidden {
				if strings.Contains(text, f) {
					t.Errorf("스팬에 %q 가 있다:\n%s", f, text)
				}
			}
			if !strings.HasPrefix(s.Name, "s3 ") {
				t.Errorf("스팬 이름: %q", s.Name)
			}
		}
		return spans
	}

	t.Run("응답한 호출의 url.full 은 출처만 남는다", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		for _, s := range run(t, srv.URL) {
			for _, kv := range s.Attributes {
				if kv.Key == "url.full" && kv.Value.AsString() != srv.URL {
					t.Errorf("url.full = %q, 출처(%s)만 남아야 한다", kv.Value.AsString(), srv.URL)
				}
			}
		}
	})

	t.Run("닿지 않는 저장소의 오류 스팬 상태 설명에도 주소가 없다", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		failed := 0
		for _, s := range run(t, "http://"+addr) {
			if s.Status.Description != "" {
				failed++
			}
		}
		if failed == 0 {
			t.Error("실패한 호출의 상태 설명이 없다 — 걸러 낼 대상을 만들지 못했다")
		}
	})
}

// 기동 때 버킷을 만들면(CHAT_S3_CREATE_BUCKET) 두 번째 기동부터는 늘 409 BucketAlreadyOwnedByYou 다. 정상인 답이 기동마다
// 오류 스팬으로 쌓이면 진짜 오류를 가린다. 남의 버킷(BucketAlreadyExists)이나 다른 거절은 그대로 오류다.
func TestS3_버킷이이미내것으로있으면기동때만들기스팬은오류가아니고다른거절은오류다(t *testing.T) {
	ensure := func(t *testing.T, store *s3store.Store, exp *tracetest.InMemoryExporter) tracetest.SpanStub {
		t.Helper()
		exp.Reset()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.EnsureBucket(ctx); err != nil {
			t.Fatalf("버킷 만들기: %v", err)
		}
		spans := exp.GetSpans()
		if len(spans) != 1 || spans[0].Name != "s3 PUT" {
			t.Fatalf("스팬이 s3 PUT 하나가 아니다: %d", len(spans))
		}
		return spans[0]
	}
	install := func(t *testing.T) *tracetest.InMemoryExporter {
		exp := tracetest.NewInMemoryExporter()
		t.Cleanup(telemetry.Install(telemetry.NewTracerProvider(exp, false), noop.NewMeterProvider()))
		return exp
	}
	conflict := func(code string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>m</Message><BucketName>b</BucketName></Error>", code)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	fake := func(t *testing.T, endpoint string) *s3store.Store {
		store, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("실제 저장소에서 이미 있는 내 버킷", func(t *testing.T) {
		exp := install(t)
		store := open(t) // 여기서 한 번 만들거나 이미 있다
		if s := ensure(t, store, exp); s.Status.Code == codes.Error {
			t.Errorf("이미 있는 내 버킷의 만들기 스팬이 오류다: %q", s.Status.Description)
		}
	})
	t.Run("409 BucketAlreadyOwnedByYou", func(t *testing.T) {
		exp := install(t)
		if s := ensure(t, fake(t, conflict("BucketAlreadyOwnedByYou")), exp); s.Status.Code == codes.Error {
			t.Errorf("스팬이 오류다: %q", s.Status.Description)
		}
	})
	t.Run("409 BucketAlreadyExists 는 오류다", func(t *testing.T) {
		exp := install(t)
		if s := ensure(t, fake(t, conflict("BucketAlreadyExists")), exp); s.Status.Code != codes.Error {
			t.Errorf("남의 버킷인데 스팬 상태가 %v 다", s.Status.Code)
		}
	})
}
