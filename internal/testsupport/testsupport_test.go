package testsupport

import (
	"net/url"
	"testing"

	"github.com/logcat-io/go-chat-system/internal/config"
)

func TestURL_테스트의기본DB는개발서버의DB와다르다(t *testing.T) {
	t.Setenv("CHAT_TEST_DATABASE_URL", "")
	t.Setenv("CHAT_DATABASE_URL", "")
	test, err := url.Parse(URL())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if test.Host != dev.Host {
		t.Fatalf("테스트 DB 는 개발 서버와 같은 Postgres 여야 한다(make up 하나로 돈다): %s ≠ %s", test.Host, dev.Host)
	}
	if test.Path == dev.Path {
		t.Fatalf("테스트가 개발 서버의 DB %s 를 쓴다", dev.Path)
	}
}
