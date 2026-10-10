package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// HTML 페이지는 CSP script-src 'self' 로 뜬다(D46). 인라인 스크립트·이벤트 속성·javascript: 주소가 하나라도 있으면 그 화면이 조용히 깨진다.
// 페이지를 고치는 사람이 그 줄을 넣으면 여기서 막는다. 스크립트는 우리 출처의 파일(src="/…")만 쓴다.
func TestHTML페이지에는인라인스크립트도이벤트속성도없다_CSP스크립트는우리출처의파일만이다(t *testing.T) {
	inlineScript := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	pages := 0
	err := fs.WalkDir(FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".html") {
			return err
		}
		pages++
		raw, err := fs.ReadFile(FS(), name)
		if err != nil {
			return err
		}
		html := string(raw)
		for _, m := range inlineScript.FindAllStringSubmatch(html, -1) {
			attrs, body := m[1], strings.TrimSpace(m[2])
			if body != "" {
				t.Errorf("%s: 인라인 스크립트가 있다: %.60q", name, body)
			}
			src := regexp.MustCompile(`src="([^"]+)"`).FindStringSubmatch(attrs)
			if src == nil || !strings.HasPrefix(src[1], "/") || strings.HasPrefix(src[1], "//") {
				t.Errorf("%s: 우리 출처 밖의 스크립트다: %q", name, attrs)
			}
		}
		if loc := handler.FindStringIndex(html); loc != nil {
			t.Errorf("%s: 이벤트 속성이 있다: %q", name, html[loc[0]:min(loc[1]+20, len(html))])
		}
		if strings.Contains(strings.ToLower(html), "javascript:") {
			t.Errorf("%s: javascript: 주소가 있다", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages < 3 {
		t.Fatalf("페이지 %d개 — index·desk·widget 이 있어야 한다", pages)
	}
}
