// Package archtest 는 의존 방향을 테스트로 고정한다. 문서의 그림은 틀려도 통과하지만 이건 빌드가 막는다.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	root   = "github.com/logcat-io/go-chat-system/"
	module = root + "internal/"
)

// deps 는 디렉토리와 그 아래 모든 하위 패키지의 비테스트 파일이 가져오는 것들이다.
// internal 은 모듈 접두사를 뗀 내부 경로, external 은 표준 라이브러리가 아닌 바깥 모듈이다.
func deps(t *testing.T, dir string) (internal, external map[string][]string) {
	t.Helper()
	internal, external = map[string][]string{}, map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			switch {
			case strings.HasPrefix(p, module):
				internal[strings.TrimPrefix(p, module)] = append(internal[strings.TrimPrefix(p, module)], path)
			case strings.HasPrefix(p, root):
				key := "/" + strings.TrimPrefix(p, root) // internal 밖의 우리 패키지. 예: /web (내장 정적 파일)
				internal[key] = append(internal[key], path)
			case strings.Contains(strings.SplitN(p, "/", 2)[0], "."): // 첫 마디에 점이 있으면 바깥 모듈이다
				external[p] = append(external[p], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return internal, external
}

type rule struct {
	name     string
	dir      string   // internal 기준 경로
	internal []string // 가져와도 되는 내부 패키지 (자기 자신과 하위는 늘 된다)
	external []string // 가져와도 되는 바깥 모듈 접두사
}

// 의존은 한 방향이다: adapter → app → core. 어댑터끼리는 서로를 모른다.
// 바깥 라이브러리는 어댑터에만 들어온다 — core 와 app 이 드라이버나 웹소켓을 알면 갈아 끼울 수 없다.
// 관측(OpenTelemetry, D33)은 어댑터와 cmd/server, 잎 패키지 telemetry 에만 들어온다.
const otel = "go.opentelemetry.io/"

var rules = []rule{
	{"core 는 표준 라이브러리만 쓴다", "core", nil, nil},
	// logctx 는 로그 키를 ctx 에 싣는 잎 패키지다. 모든 계층이 가져다 쓰므로 무엇에도 기대면 안 된다. core 는 logctx 를 모른다.
	// 스팬의 id 는 telemetry 가 logctx.SetSpanSource 로 꽂아 준다. logctx 는 OTel 을 모른다.
	{"logctx 는 표준 라이브러리만 쓴다", "logctx", nil, nil},
	// telemetry 는 OTel 의 시작·종료와 공용 계기를 둔다. 도메인을 모른다.
	{"telemetry 는 OTel 과 logctx 만 안다", "telemetry", []string{"logctx"}, []string{otel}},
	{"app 은 core 와 id 만 알고 바깥 라이브러리를 모른다", "app", []string{"core", "id", "auth", "logctx"}, nil},
	{"pgstore 는 app 을 몰라도 포트를 충족한다", "adapter/pgstore", []string{"core", "id", "auth", "logctx", "telemetry"}, []string{"github.com/jackc/", otel}},
	{"httpapi 는 다른 어댑터를 모른다", "adapter/httpapi", []string{"app", "core", "id", "auth", "config", "/web", "logctx", "telemetry"}, []string{otel}},
	{"realtime 은 다른 어댑터를 모른다", "adapter/realtime", []string{"app", "core", "id", "logctx", "telemetry"}, []string{"github.com/coder/websocket", otel}},
	{"ollama 는 app 의 포트 타입만 쓴다", "adapter/ollama", []string{"app", "core", "telemetry"}, nil},
	{"filestore 는 core 만 안다", "adapter/filestore", []string{"core", "id"}, nil},
	{"s3store 는 core 만 알고 SDK 를 쓰지 않는다", "adapter/s3store", []string{"core", "telemetry"}, nil},
}

// observabilityFree 는 관측을 몰라야 하는 계층이다. 위 rules 가 바깥 라이브러리를 다 막지만, 허용 목록을 넓히다
// OTel 이 새어 드는 것을 따로 막는다. 유스케이스가 스팬·계기를 알면 백엔드를 바꿀 때 도메인 코드가 따라 바뀐다.
var observabilityFree = []string{"core", "app"}

// otelImports 는 dir 아래 비테스트 파일이 가져오는 OTel 과 telemetry 다.
func otelImports(t *testing.T, dir string) []string {
	t.Helper()
	in, ex := deps(t, dir)
	var out []string
	for dep, files := range ex {
		if strings.HasPrefix(dep, otel) {
			out = append(out, dep+" ← "+files[0])
		}
	}
	for dep, files := range in {
		if dep == "telemetry" || strings.HasPrefix(dep, "telemetry/") {
			out = append(out, "internal/"+dep+" ← "+files[0])
		}
	}
	sort.Strings(out)
	return out
}

func TestCoreAndAppDoNotKnowTelemetry(t *testing.T) {
	for _, dir := range observabilityFree {
		t.Run(dir+" 는 OTel 과 telemetry 를 가져오지 않는다", func(t *testing.T) {
			for _, v := range otelImports(t, filepath.Join("..", dir)) {
				t.Errorf("%s 가 관측을 안다: %s", dir, v)
			}
		})
	}
}

func violations(t *testing.T, r rule) []string {
	t.Helper()
	in, ex := deps(t, filepath.Join("..", filepath.FromSlash(r.dir)))
	var out []string
	for dep, files := range in {
		ok := dep == r.dir || strings.HasPrefix(dep, r.dir+"/")
		for _, a := range r.internal {
			ok = ok || dep == a || strings.HasPrefix(dep, a+"/")
		}
		if !ok {
			out = append(out, strings.TrimPrefix("internal/"+dep, "internal//")+" ← "+files[0])
		}
	}
	for dep, files := range ex {
		ok := false
		for _, a := range r.external {
			ok = ok || strings.HasPrefix(dep, a)
		}
		if !ok {
			out = append(out, dep+" ← "+files[0])
		}
	}
	sort.Strings(out)
	return out
}

func TestDependencyDirection(t *testing.T) {
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			for _, v := range violations(t, r) {
				t.Errorf("%s 가 가져오면 안 되는 것: %s", r.dir, v)
			}
		})
	}
}

// 새 어댑터를 만들고 규칙을 안 적으면 그 어댑터는 무엇이든 가져올 수 있다. 등록을 강제한다.
func TestEveryAdapterHasARule(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, r := range rules {
		known[r.dir] = true
	}
	for _, e := range entries {
		if e.IsDir() && !known["adapter/"+e.Name()] {
			t.Errorf("internal/adapter/%s 에 의존 규칙이 없다. rules 에 추가한다", e.Name())
		}
	}
}

// 규칙이 실제로 잡는지 확인한다. 통과만 하는 검사는 검사가 아니다.
func TestRulesCatchViolations(t *testing.T) {
	t.Run("core 가 app 을 알면 걸린다", func(t *testing.T) {
		if len(violations(t, rule{dir: "app", internal: nil})) == 0 {
			t.Error("app 은 core 를 가져오는데 허용 목록이 비어도 통과했다")
		}
	})
	t.Run("app 이 otel 을 가져오면 걸린다", func(t *testing.T) {
		// testdata 는 빌드에 들어가지 않는다. app 에 OTel 을 들인 모습을 흉내 낸 파일이다.
		if len(otelImports(t, filepath.Join("testdata", "otelimport"))) != 2 {
			t.Error("OTel 과 telemetry 를 가져오는 파일을 잡지 못했다")
		}
		found := false
		for _, dir := range observabilityFree {
			found = found || dir == "app"
		}
		if !found {
			t.Error("app 이 관측을 몰라야 하는 계층 목록에 없다")
		}
		// app 의 의존 규칙을 그대로 testdata 에 대 본다. 허용 목록에 OTel 이 들어가면 여기서 걸린다.
		for _, r := range rules {
			if r.dir == "app" {
				r.dir = "archtest/testdata/otelimport"
				if len(violations(t, r)) < 2 {
					t.Errorf("app 의 의존 규칙이 OTel·telemetry 를 막지 않는다: %v", violations(t, r))
				}
			}
		}
	})
	t.Run("바깥 라이브러리를 허용하지 않으면 걸린다", func(t *testing.T) {
		if len(violations(t, rule{dir: "adapter/pgstore", internal: []string{"core", "id", "auth"}})) == 0 {
			t.Error("pgstore 는 pgx 를 가져오는데 허용 목록이 비어도 통과했다")
		}
	})
}
