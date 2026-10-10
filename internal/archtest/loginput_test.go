package archtest

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// D30(감사 23): 로그에는 클라이언트가 정한 글자를 그대로 싣지 않는다. 길이 제한이 없으면 프레임 하나로 로그 한 줄이 수십 KB 가 되고
// (60KB type 의 프레임 500 개에 로그 30MB), 글자 제한이 없으면 이메일·이름을 id 자리에 넣어 로그에 남길 수 있다.
// 오류 원문 검사(logerr_test.go)와 같은 장치로, 입력이 나오는 자리(요청의 메서드·주소·헤더·경로 값, 요청 본문과 프레임의 필드)에서
// 로그 인자까지 같은 패키지 안의 대입을 따라간다. 거르개(아는 값으로 바꾸거나 id 로 정규화하는 함수)를 지나면 멈춘다.
// 못 잡는 것(함수 매개변수를 거친 흐름)은 오류 원문과 같다 — docs/design.md D30 "검사의 한계".

const (
	modulePath = "github.com/logcat-io/go-chat-system"
	logctxPath = modulePath + "/internal/logctx"
)

// inputRules 는 입력이 나오는 자리와 거르개다.
type inputRules struct {
	// pkgs 는 요청 본문·프레임을 읽는 구조체가 사는 패키지다. 이 패키지의 구조체 필드 중 json 태그가 붙은 글자 필드가 입력이다.
	pkgs map[string]bool
	// types 는 json 태그 없이도 입력을 담는 구조체(유스케이스의 매개변수)다. 글자 필드가 입력이다.
	types map[string]bool
	// sanitizers 는 입력을 받아 아는 값이나 정규화한 id 를 돌려주는 함수다. 결과는 입력이 아니다.
	sanitizers map[string]bool
}

// requestFields 는 *http.Request 에서 클라이언트가 정한 필드다(Pattern 은 mux 의 라우트라 아니다).
var requestFields = map[string]bool{
	"Method": true, "URL": true, "Host": true, "RequestURI": true, "RemoteAddr": true, "Header": true,
	"Form": true, "PostForm": true, "MultipartForm": true, "Trailer": true, "Proto": true,
}

// requestMethods 는 *http.Request 의 메서드 중 클라이언트가 정한 글자를 돌려주는 것이다.
var requestMethods = map[string]bool{
	"PathValue": true, "FormValue": true, "PostFormValue": true, "UserAgent": true, "Referer": true,
}

func productionInputRules() *inputRules {
	return &inputRules{
		pkgs: map[string]bool{
			modulePath + "/internal/adapter/httpapi":  true,
			modulePath + "/internal/adapter/realtime": true,
		},
		types: map[string]bool{
			modulePath + "/internal/app.SendParams":    true,
			modulePath + "/internal/app.UploadRequest": true,
			// 회원 토큰의 클레임(member_id·이름·이메일·전화)은 고객사 서버가 정한 글자다. json 태그가 있지만 app 패키지라 pkgs 로는 안 잡힌다(감사 24 A6).
			modulePath + "/internal/app.MemberClaims": true,
		},
		sanitizers: map[string]bool{
			modulePath + "/internal/id.Normalize":                true,
			modulePath + "/internal/adapter/realtime.frameKind":  true,
			modulePath + "/internal/adapter/httpapi.knownMethod": true,
			modulePath + "/internal/app.LoggableClientMsgID":     true,
			modulePath + "/internal/app.declaredClass":           true,
		},
	}
}

func funcName(f *types.Func) string {
	if f.Pkg() == nil {
		return f.Name()
	}
	if sig, ok := f.Type().(*types.Signature); ok && sig.Recv() != nil {
		return f.FullName()
	}
	return f.Pkg().Path() + "." + f.Name()
}

// source 는 e 가 입력이 나오는 자리인지 본다.
func (r *inputRules) source(info *types.Info, e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		sel := info.Selections[x]
		if sel == nil || sel.Kind() != types.FieldVal {
			return false
		}
		field, _ := sel.Obj().(*types.Var)
		if field == nil {
			return false
		}
		if isNamed(sel.Recv(), "net/http", "Request") {
			return requestFields[field.Name()]
		}
		if !isText(field.Type()) && !isAny(field.Type()) {
			return false // 수·참거짓은 줄을 부풀리지도 사람을 가리키지도 않는다
		}
		owner, tag := fieldOwner(sel)
		if owner == nil {
			return false
		}
		if n, ok := owner.(*types.Named); ok && n.Obj().Pkg() != nil && r.types[n.Obj().Pkg().Path()+"."+n.Obj().Name()] {
			return true
		}
		_, hasJSON := reflect.StructTag(tag).Lookup("json")
		return hasJSON && field.Pkg() != nil && r.pkgs[field.Pkg().Path()]
	case *ast.CallExpr:
		fn, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr)
		if !ok {
			return false
		}
		sel := info.Selections[fn]
		if sel == nil || sel.Kind() != types.MethodVal {
			return false
		}
		return isNamed(sel.Recv(), "net/http", "Request") && requestMethods[sel.Obj().Name()]
	}
	return false
}

// fieldOwner 는 고른 필드를 직접 담은 구조체 타입(이름이 있으면 이름 있는 타입)과 그 필드의 태그다.
func fieldOwner(sel *types.Selection) (types.Type, string) {
	t := sel.Recv()
	idx := sel.Index()
	for i, n := range idx {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok || n >= st.NumFields() {
			return nil, ""
		}
		if i == len(idx)-1 {
			return t, st.Tag(n)
		}
		t = st.Field(n).Type()
	}
	return nil, ""
}

// isNamed 는 t(포인터면 그 안)가 pkg.name 인지 본다.
func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

func TestLog_로그호출에는클라이언트가정한글자를그대로넘기지않는다(t *testing.T) {
	l := loadPackages(t)
	l.input = productionInputRules()
	var bad []string
	checked := 0
	for _, p := range l.ours {
		b, n := l.checkDir(t, p.ImportPath, p.Dir, p.GoFiles)
		bad, checked = append(bad, b...), checked+n
	}
	if checked < 50 {
		t.Fatalf("로그 호출을 %d 개만 찾았다 — 검사가 로그 호출을 알아보지 못한다", checked)
	}
	if len(bad) > 0 {
		t.Errorf("로그 호출에 클라이언트가 정한 글자가 그대로 들어갔다(D30 — 아는 값이나 정규화한 id 로 바꿔 싣는다):\n  %s", strings.Join(bad, "\n  "))
	}
}

// testdata/loginput 의 "잡는다:" 줄을 다 잡고 표시 없는 줄은 잡지 않는지 본다.
func TestLog_입력을변수에담거나이어붙이거나메서드로꺼내도로그로가면잡는다(t *testing.T) {
	l := loadPackages(t)
	const pkg = modulePath + "/internal/archtest/testdata/loginput"
	l.input = productionInputRules()
	l.input.pkgs[pkg] = true
	l.input.types[pkg+".params"] = true
	l.input.sanitizers[pkg+".knownSanitized"] = true
	dir := filepath.Join("testdata", "loginput")
	bad, checked := l.checkDir(t, pkg, dir, []string{"leak.go"})
	if checked < 10 {
		t.Fatalf("testdata 의 로그 호출을 %d 개만 찾았다", checked)
	}
	got := map[int]bool{}
	for _, b := range bad {
		parts := strings.SplitN(b, ":", 4)
		var line int
		if _, err := fmt.Sscan(parts[1], &line); err != nil {
			t.Fatalf("줄 번호를 못 읽었다: %q", b)
		}
		got[line] = true
	}
	want := map[int]string{}
	f, err := os.Open(filepath.Join(dir, "leak.go"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if i := strings.Index(sc.Text(), "// 잡는다:"); i >= 0 {
			want[n] = strings.TrimSpace(sc.Text()[i+len("// 잡는다:"):])
		}
	}
	if len(want) < 10 {
		t.Fatalf("testdata 에 잡아야 할 줄이 %d 개뿐이다", len(want))
	}
	for line, what := range want {
		if !got[line] {
			t.Errorf("loginput/leak.go:%d (%s) 를 잡지 못했다", line, what)
		}
	}
	for line := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("loginput/leak.go:%d 는 입력을 싣지 않는데 잡았다", line)
		}
	}
}

// D47(감사 23): 패닉 경로를 시험하는 주입 지점(realtime 의 frameHook·faultHook, cmd/server 의 faultHook)은 운영에서 늘 nil 이어야 한다.
// 소문자 패키지 변수라 다른 패키지는 닿지 못한다. 같은 패키지의 비테스트 코드가 설정하는 줄이 생기지 않게 여기서 막는다.
// 감사 24(C3): 설정 메서드 이름만 보면 별칭(`p := &faultHook; p.Store(…)`)으로 피해 간다. 그래서 선언과 읽기(`.Load()`) 말고는
// 이름이 나오는 것 자체를 막는다. 남은 한계 — 이름을 쓰지 않고 닿는 길(reflect·unsafe·linkname)은 보지 않는다(D30 "검사의 한계").
func TestHook_테스트전용주입지점은운영코드가설정하지않는다(t *testing.T) {
	l := loadPackages(t)
	hooks := []string{"frameHook", "faultHook"}
	scanned := 0
	for _, p := range l.ours {
		for _, name := range p.GoFiles {
			raw, err := os.ReadFile(filepath.Join(p.Dir, name))
			if err != nil {
				t.Fatal(err)
			}
			scanned++
			for n, line := range strings.Split(string(raw), "\n") {
				code, _, _ := strings.Cut(line, "//")
				for _, h := range hooks {
					rest := strings.ReplaceAll(strings.ReplaceAll(code, "var "+h+" ", ""), h+".Load()", "")
					if strings.Contains(rest, h) {
						t.Errorf("%s/%s:%d 가 테스트 전용 주입 지점을 읽기 말고 다른 데 쓴다(설정·별칭): %s", p.ImportPath, name, n+1, strings.TrimSpace(line))
					}
				}
			}
		}
	}
	if scanned < 50 {
		t.Fatalf("비테스트 파일을 %d 개만 봤다", scanned)
	}
}
