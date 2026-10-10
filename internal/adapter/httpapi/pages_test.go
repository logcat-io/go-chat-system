package httpapi_test

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logcat-io/go-chat-system/web"
)

// 정적 페이지는 리다이렉트 없이 바로 나와야 한다.
// "/" 를 "/index.html" 로 바꾸면 파일 서버의 정규화 리다이렉트가 "/" 로 되돌려 루프가 생긴다.
func TestPages_리다이렉트없이열린다(t *testing.T) {
	h := newHarness(t, time.Second, false)

	// 리다이렉트를 따라가지 않는 클라이언트로 상태 코드를 그대로 본다.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	cases := []struct{ path, wantType string }{
		{"/", "text/html"},
		{"/widget", "text/html"},
		{"/desk", "text/html"},
		{"/app.css", "text/css"},
		{"/chat.js", "text/javascript"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			res, err := client.Get(h.srv.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("200 이어야 한다: %d (Location=%q)", res.StatusCode, res.Header.Get("Location"))
			}
			if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
				t.Errorf("Content-Type 이 %s 가 아니다: %s", tc.wantType, ct)
			}
		})
	}
}

// 배포 뒤에 옛 chat.js 가 브라우저·엣지에 남으면 새 서버와 옛 클라이언트가 섞인다.
// 정적 자산과 페이지는 매번 검증하고(no-cache), 바뀌지 않았으면 304 로 본문 없이 재사용한다.
func TestPages_정적자산은매번검증하고바뀌지않았으면304다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(t *testing.T, path, etag string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	for _, path := range []string{"/", "/widget", "/desk", "/app.css", "/chat.js", "/ui.js", "/desk.js", "/widget.js", "/embed.js"} {
		t.Run(path, func(t *testing.T) {
			res := get(t, path, "")
			if res.StatusCode != http.StatusOK {
				t.Fatalf("200 이어야 한다: %d", res.StatusCode)
			}
			if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control 이 no-cache 여야 한다: %q", cc)
			}
			etag := res.Header.Get("ETag")
			if etag == "" {
				t.Fatal("ETag 가 있어야 한다")
			}
			if again := get(t, path, etag); again.StatusCode != http.StatusNotModified {
				t.Errorf("같은 ETag 로 물으면 304 여야 한다: %d", again.StatusCode)
			}
			if stale := get(t, path, `"옛-판"`); stale.StatusCode != http.StatusOK {
				t.Errorf("다른 ETag 면 200 이어야 한다: %d", stale.StatusCode)
			}
		})
	}
}

// 첫 화면은 공개 설치에서도 그대로 나간다. 시드 계정(기본 이메일·비밀번호)을 화면에 적으면 그 설치의 로그인 정보를 광고하는 셈이다.
// 로컬 안내는 README 가 맡는다. 페이지와 데스크·위젯 화면 어디에도 없다.
// 화면은 JS 가 그리므로 HTML 만 보면 스크립트가 적어 넣는 문구를 놓친다. 임베드된 정적 파일을 전부 받아 본다.
func TestPages_첫화면과데스크와위젯화면에는시드계정의이메일과비밀번호가없다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	paths := []string{"/", "/desk", "/widget"}
	if err := fs.WalkDir(web.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, "/"+name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/app.css", "/chat.js", "/ui.js", "/desk.js", "/widget.js", "/embed.js"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("임베드 목록에 %s 가 없다: %v", want, paths)
		}
	}
	for _, path := range paths {
		res, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
		for _, seed := range []string{"agent@example.com", "chat1234"} {
			if strings.Contains(string(body), seed) {
				t.Errorf("%s 응답에 시드 계정의 %q 가 있다", path, seed)
			}
		}
	}
}

// 클라이언트 테스트(*.test.mjs)는 web/assets 에 같이 있지만 배포물이 아니다. 내부 구현과 테스트 픽스처를 공개로 내보내지 않는다.
func TestPages_클라이언트테스트파일은서빙하지않고자산은서빙한다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	status := func(path string) int {
		t.Helper()
		res, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, path := range []string{"/chat.test.mjs", "/ui.test.mjs", "/widget.test.mjs", "/widget-view.test.mjs", "/assets/chat.test.mjs"} {
		if code := status(path); code != http.StatusNotFound {
			t.Errorf("%s 가 %d 다. 404 여야 한다", path, code)
		}
	}
	for _, path := range []string{"/", "/desk", "/widget", "/app.css", "/widget.css", "/chat.js", "/ui.js", "/desk.js", "/widget.js",
		"/widget-view.js", "/embed.js", "/favicon.svg", "/icons/speech_balloon.png", "/icons/LICENSE"} {
		if code := status(path); code != http.StatusOK {
			t.Errorf("%s 가 %d 다. 200 이어야 한다", path, code)
		}
	}
	if err := fs.WalkDir(web.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(name, ".test.mjs") {
			t.Errorf("임베드 목록에 테스트 파일 %s 가 있다", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 위젯의 3D 아이콘(Fluent Emoji, MIT)은 바이너리에 들어 있고 같은 서버가 내준다(D38). 사내망과 CSP 때문에 바깥 주소에서 받지 않는다.
// 위젯 화면(html·css·js)이 가리키는 /icons/ 주소는 전부 이 서버에서 이미지로 나온다.
var iconRef = regexp.MustCompile(`/icons/[a-z0-9_-]+\.png`)

func TestPages_아이콘자산은같은서버에서서빙된다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	icons, err := fs.Glob(web.FS(), "icons/*.png")
	if err != nil || len(icons) == 0 {
		t.Fatalf("임베드된 아이콘이 없다: %v %v", icons, err)
	}
	for _, name := range icons {
		want, _ := fs.ReadFile(web.FS(), name)
		res, err := http.Get(h.srv.URL + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" || string(body) != string(want) {
			t.Errorf("/%s: %d %q (%d바이트) — 200 image/png 로 같은 파일이 나와야 한다", name, res.StatusCode, res.Header.Get("Content-Type"), len(body))
		}
	}
	for _, page := range []string{"widget.html", "widget.css", "widget.js", "widget-view.js"} {
		text, err := fs.ReadFile(web.FS(), page)
		if err != nil {
			t.Fatalf("%s 가 임베드되지 않았다: %v", page, err)
		}
		for _, ref := range iconRef.FindAllString(string(text), -1) {
			if !slices.Contains(icons, ref[1:]) {
				t.Errorf("%s 가 없는 아이콘 %s 를 가리킨다", page, ref)
			}
		}
		// 프로토콜을 뺀 주소(//cdn.example.com/…)도 바깥 주소다. 따옴표 종류와 공백, @import 도 본다.
		if found := outsideRef.FindString(string(text)); found != "" {
			t.Errorf("%s 가 바깥 주소에서 자산을 받는다(%s)", page, found)
		}
	}
}

// 제목 글꼴(Sora, OFL)은 바이너리에 들어 있고 같은 서버가 내준다. 바깥 글꼴 서버에서 받지 않는다.
// 화면 CSS 가 가리키는 /fonts/ 주소는 전부 이 서버에서 글꼴로 나오고, 라이선스 고지도 함께 나온다.
var fontRef = regexp.MustCompile(`/fonts/[a-z0-9_-]+\.woff2`)

func TestPages_글꼴자산은같은서버에서서빙되고CSS가가리키는글꼴은전부있다(t *testing.T) {
	h := newHarness(t, time.Second, false)
	refs := 0
	for _, page := range []string{"app.css", "widget.css"} {
		text, err := fs.ReadFile(web.FS(), page)
		if err != nil {
			t.Fatalf("%s 가 임베드되지 않았다: %v", page, err)
		}
		if found := outsideRef.FindString(string(text)); found != "" {
			t.Errorf("%s 가 바깥 주소에서 자산을 받는다(%s)", page, found)
		}
		for _, ref := range fontRef.FindAllString(string(text), -1) {
			refs++
			want, err := fs.ReadFile(web.FS(), ref[1:])
			if err != nil {
				t.Errorf("%s 가 가리키는 %s 가 임베드되지 않았다", page, ref)
				continue
			}
			res, err := http.Get(h.srv.URL + ref)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "font/woff2" || string(body) != string(want) {
				t.Errorf("%s: %d %q (%d바이트) — 200 font/woff2 로 같은 파일이 나와야 한다", ref, res.StatusCode, res.Header.Get("Content-Type"), len(body))
			}
		}
	}
	if refs == 0 {
		t.Fatal("화면 CSS 가 글꼴을 하나도 가리키지 않는다")
	}
	res, err := http.Get(h.srv.URL + "/fonts/LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/fonts/LICENSE: %d — 글꼴 라이선스 고지가 함께 나와야 한다", res.StatusCode)
	}
}

var outsideRef = regexp.MustCompile(`(?i)(src=|url\(|@import)\s*["']?(https?:)?//`)

// outsideRef 가 잡아야 하는 모양과 잡으면 안 되는 모양. 검사 자체가 빈틈이면 위 테스트는 늘 통과한다.
func TestPages_바깥주소검사는프로토콜을뺀주소도잡는다(t *testing.T) {
	for _, bad := range []string{`src="https://cdn.example.com/a.png"`, `url(http://x/a.png)`, `url("//cdn.example.com/a.png")`,
		`url( '//cdn.example.com/a.png')`, `src='//cdn.example.com/a.js'`, `SRC=//cdn.example.com/a.js`, `@import "//fonts.example.com/a.css"`} {
		if !outsideRef.MatchString(bad) {
			t.Errorf("%s 를 바깥 주소로 잡지 못한다", bad)
		}
	}
	for _, ok := range []string{`src="/icons/check.png"`, `url("/icons/page.png")`, `src="${ICON.balloon}"`, `// 주석`} {
		if outsideRef.MatchString(ok) {
			t.Errorf("%s 를 바깥 주소로 잘못 잡는다", ok)
		}
	}
}
