package httpapi_test

import (
	"net/http"
	"testing"
	"time"
)

// 설치 코드의 서버 주소. 관리자가 데스크를 사내 주소나 localhost 로 열어도 고객 사이트에는 공개 주소가 들어가야 한다.

func TestE2E_설치코드주소(t *testing.T) {
	h := newHarness(t, time.Second, false)
	tn := h.newTenant("설치 주소 상점")

	t.Run("워크스페이스 설정 응답의 공개 주소는 서버 설정의 CHAT_PUBLIC_URL 이다", func(t *testing.T) {
		// 하네스의 PublicURL 은 http://test.local 이다. 요청이 들어온 주소(httptest 의 127.0.0.1)가 아니다.
		status, body := h.call(http.MethodGet, "/api/admin/workspace", tn.token, nil)
		if status != http.StatusOK {
			t.Fatalf("워크스페이스 설정을 못 읽었다: %d %+v", status, body)
		}
		if got, _ := body["public_url"].(string); got != "http://test.local" {
			t.Errorf("public_url = %q, CHAT_PUBLIC_URL(http://test.local) 이어야 한다", got)
		}
	})
}
