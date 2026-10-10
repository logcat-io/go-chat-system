package archtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// D30: 로그에는 오류 원문을 싣지 않는다. DB 접속 오류 문구에는 user=·database=·호스트:포트가, 저장소 오류에는 주소가 들어간다.
// 감사 15 는 고친 뒤에도 `"err", err` 가 21곳 남았음을 셌다(감사 16 이 24곳으로 바로잡았다. 그중 DB 오류가 닿는 12곳). 사람이 한 곳씩 고치면 새 줄이 또 생긴다.
// 여기서는 리포의 모든 비테스트 코드를 타입까지 검사해, 로그로 가는 인자에 오류 값이나 그 원문이 들어가면 실패한다.
// 종류는 logctx.ErrKind(err) 로 싣는다 — 그 호출은 오류 값을 받아 종류만 돌려주므로 보지 않는다.
//
// 감사 16 은 첫 검사를 피해 가는 꼴 아홉 가지를 보였다(원문을 변수에 담기, 구조체로 감싸기, strings 함수에 넘기기, 인자 펼치기,
// slog.Attr 리터럴, 오류 슬라이스·맵, &err, 표준 출력). 지금 검사는 그중 정적으로 가를 수 있는 것을 다 잡는다(testdata/logleak).
// 못 잡는 것은 docs/design.md D30 의 "검사의 한계" 에 적었다.

// slogFuncs 는 인자가 로그 레코드로 가는 slog 의 함수·메서드다. Any·Group·String 은 속성을 만들어 그 안의 값이 로그로 간다.
var slogFuncs = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"With": true, "Any": true, "Group": true, "String": true,
}

// logWrappers 는 받은 인자를 그대로 slog 로 넘기는 우리 도우미와, 로그로 가는 인자가 몇 번째부터인지다.
var logWrappers = map[string]int{
	"github.com/logcat-io/go-chat-system/internal/app.logRejected": 3, // (ctx, event, err, args...)
}

// stdlogFuncs 는 표준 log 패키지의 출력 함수다.
var stdlogFuncs = map[string]bool{
	"Print": true, "Printf": true, "Println": true, "Fatal": true, "Fatalf": true, "Fatalln": true,
	"Panic": true, "Panicf": true, "Panicln": true, "Output": true,
}

// fmtPrint 는 표준 출력으로 바로 쓰는 fmt 함수다. 컨테이너의 표준 출력이 곧 로그다(D33).
var fmtPrint = map[string]bool{"Print": true, "Printf": true, "Println": true}

// fmtFprint 는 쓰는 곳을 받는 fmt 함수다. 쓰는 곳이 os.Stdout·os.Stderr 일 때만 로그다.
var fmtFprint = map[string]bool{"Fprint": true, "Fprintf": true, "Fprintln": true}

// fmtText 는 인자를 그대로 글자로 옮기는 fmt 함수다. 로그 인자 안에서 쓰면 그 결과가 로그로 간다.
var fmtText = map[string]bool{"Sprint": true, "Sprintf": true, "Sprintln": true, "Errorf": true,
	"Append": true, "Appendf": true, "Appendln": true}

type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Module     *struct{ Main bool }
}

// loaded 는 go list 로 읽은 우리 패키지와, 타입 검사에 쓰는 내보내기 자료다.
type loaded struct {
	fset *token.FileSet
	imp  types.Importer
	ours []listedPkg
	// input 이 있으면 checkDir 가 오류 대신 클라이언트 입력을 찾는다.
	input *inputRules
}

func loadPackages(t *testing.T) loaded {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export,Module", "./...")
	cmd.Dir = filepath.Join("..", "..")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var ours []listedPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Main {
			ours = append(ours, p)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f := exports[path]
		if f == "" {
			return nil, fmt.Errorf("내보내기 자료 없음: %s", path)
		}
		return os.Open(f)
	})
	return loaded{fset: fset, imp: imp, ours: ours}
}

// checkDir 는 dir 의 files 를 한 패키지로 타입 검사하고 로그로 새는 자리를 돌려준다. checked 는 찾은 로그 호출 수다.
func (l loaded) checkDir(t *testing.T, path, dir string, names []string) (bad []string, checked int) {
	t.Helper()
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(l.fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: l.imp}
	if _, err := conf.Check(path, l.fset, files, info); err != nil {
		t.Fatalf("%s 타입 검사: %v", path, err)
	}
	c := newLeakChecker(info)
	c.input = l.input
	c.collect(files)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			args, ok := c.logArgs(call)
			if !ok {
				return true
			}
			checked++
			for _, arg := range args {
				if where := c.leak(arg); where != nil {
					bad = append(bad, fmt.Sprintf("%s: %s", l.fset.Position(where.Pos()), types.ExprString(where)))
				}
			}
			return true
		})
	}
	sort.Strings(bad)
	return slices.Compact(bad), checked // slog.String 처럼 그 자체가 로그 함수인 인자는 두 번 잡힌다
}

func TestLog_로그호출에는오류값도오류원문도넘기지않는다(t *testing.T) {
	l := loadPackages(t)
	if len(l.ours) < 10 {
		t.Fatalf("검사할 우리 패키지가 %d 개뿐이다 — go list 가 잘못 돌았다", len(l.ours))
	}
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
		t.Errorf("로그 호출에 오류 값이나 원문이 들어갔다(D30 — 종류만 logctx.ErrKind(err) 로 싣는다):\n  %s", strings.Join(bad, "\n  "))
	}
}

// 감사 16 이 보인 우회 꼴을 testdata/logleak 에 모아 두고, 검사가 "잡는다" 표시한 줄을 다 잡고 표시 없는 줄은 잡지 않는지 본다.
// 검사를 고치다 한 꼴이라도 놓치면 여기서 실패한다.
func TestLog_원문을변수에담거나감싸거나펼치거나다른함수에넘겨도로그로가면잡는다(t *testing.T) {
	l := loadPackages(t)
	dir := filepath.Join("testdata", "logleak")
	bad, checked := l.checkDir(t, "github.com/logcat-io/go-chat-system/internal/archtest/testdata/logleak", dir, []string{"leak.go"})
	if checked < 15 {
		t.Fatalf("testdata 의 로그 호출을 %d 개만 찾았다", checked)
	}
	got := map[int]bool{}
	for _, b := range bad {
		// 꼴: testdata/logleak/leak.go:줄:칸: 식
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
	if len(want) < 15 {
		t.Fatalf("testdata 에 잡아야 할 줄이 %d 개뿐이다", len(want))
	}
	for line, what := range want {
		if !got[line] {
			t.Errorf("leak.go:%d (%s) 를 잡지 못했다", line, what)
		}
	}
	for line := range got {
		if _, ok := want[line]; !ok {
			t.Errorf("leak.go:%d 는 원문을 싣지 않는데 잡았다", line)
		}
	}
}

// leakChecker 는 한 패키지 안에서 오류 값·원문이 로그 인자로 흘러가는지 본다.
// tainted 는 오류 값이나 원문을 담은 적이 있는 변수·필드다(같은 패키지 안의 대입만 따라간다).
type leakChecker struct {
	info    *types.Info
	errType *types.Interface
	tainted map[types.Object]bool
	// input 이 있으면 오류 대신 클라이언트가 정한 글자를 찾는다(loginput_test.go, 감사 23).
	input *inputRules
}

func newLeakChecker(info *types.Info) *leakChecker {
	return &leakChecker{info: info, errType: types.Universe.Lookup("error").Type().Underlying().(*types.Interface),
		tainted: map[types.Object]bool{}}
}

// collect 는 대입을 따라 tainted 를 채운다. 새로 물드는 변수가 없을 때까지 되풀이한다(a := err.Error(); b := a + "x" 처럼 이어진 것).
func (c *leakChecker) collect(files []*ast.File) {
	for changed := true; changed; {
		changed = false
		mark := func(lhs ast.Expr) {
			if o := c.target(lhs); o != nil && !c.tainted[o] {
				c.tainted[o] = true
				changed = true
			}
		}
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if len(s.Lhs) == len(s.Rhs) {
						for i := range s.Rhs {
							if c.leak(s.Rhs[i]) != nil {
								mark(s.Lhs[i])
							}
						}
					} else if len(s.Rhs) == 1 && c.leak(s.Rhs[0]) != nil {
						// b, _ := json.Marshal(w) 처럼 여러 값을 돌려주는 호출이면 글자를 받는 쪽만 물든다.
						for _, lhs := range s.Lhs {
							if tv, ok := c.info.Types[lhs]; ok && (isText(tv.Type) || isAny(tv.Type)) {
								mark(lhs)
							} else if id, ok := lhs.(*ast.Ident); ok {
								if o := c.info.Defs[id]; o != nil && (isText(o.Type()) || isAny(o.Type())) {
									mark(lhs)
								}
							}
						}
					}
				case *ast.ValueSpec:
					if len(s.Names) == len(s.Values) {
						for i := range s.Values {
							if c.leak(s.Values[i]) != nil {
								mark(s.Names[i])
							}
						}
					}
				case *ast.RangeStmt:
					if c.leak(s.X) != nil {
						if s.Key != nil {
							mark(s.Key)
						}
						if s.Value != nil {
							mark(s.Value)
						}
					}
				}
				return true
			})
		}
	}
}

// target 은 대입의 왼쪽이 가리키는 변수·필드다. args[0] = … 처럼 원소에 넣으면 담은 변수 전체가 물든다.
func (c *leakChecker) target(e ast.Expr) types.Object {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		if o := c.info.Defs[x]; o != nil {
			return o
		}
		return c.info.Uses[x]
	case *ast.SelectorExpr:
		return c.info.Uses[x.Sel]
	case *ast.IndexExpr:
		return c.target(x.X)
	case *ast.StarExpr:
		return c.target(x.X)
	}
	return nil
}

// logArgs 는 call 이 로그로 쓰는 호출이면 로그로 가는 인자를 돌려준다.
func (c *leakChecker) logArgs(call *ast.CallExpr) ([]ast.Expr, bool) {
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		if b, ok := c.info.Uses[id].(*types.Builtin); ok && (b.Name() == "print" || b.Name() == "println") {
			return call.Args, true
		}
	}
	f := calleeOf(c.info, call)
	if f == nil || f.Pkg() == nil {
		return nil, false
	}
	// 우리 코드의 로그 도우미. 뒤쪽 인자를 그대로 slog 에 넘긴다 — 함수 경계를 넘는 흐름이라 이름으로 잇는다.
	if from, ok := logWrappers[funcName(f)]; ok && len(call.Args) >= from {
		return call.Args[from:], true
	}
	switch f.Pkg().Path() {
	case logctxPath:
		// logctx.With 로 ctx 에 얹은 키는 그 뒤의 모든 줄에 붙는다. 첫 인자(ctx) 뒤가 로그로 간다.
		if f.Name() == "With" && len(call.Args) > 0 {
			return call.Args[1:], true
		}
	case "log/slog":
		return call.Args, slogFuncs[f.Name()]
	case "log":
		return call.Args, stdlogFuncs[f.Name()]
	case "fmt":
		if fmtPrint[f.Name()] {
			return call.Args, true
		}
		if fmtFprint[f.Name()] && len(call.Args) > 0 && isStdStream(c.info, call.Args[0]) {
			return call.Args[1:], true
		}
	}
	return nil, false
}

// isStdStream 은 e 가 os.Stdout 이나 os.Stderr 인지 본다.
func isStdStream(info *types.Info, e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	v, ok := info.Uses[sel.Sel].(*types.Var)
	return ok && v.Pkg() != nil && v.Pkg().Path() == "os" && (v.Name() == "Stdout" || v.Name() == "Stderr")
}

// calleeOf 는 호출되는 함수(패키지 함수 또는 메서드)다.
func calleeOf(info *types.Info, call *ast.CallExpr) *types.Func {
	switch fn := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		f, _ := info.Uses[fn].(*types.Func)
		return f
	case *ast.SelectorExpr:
		if sel := info.Selections[fn]; sel != nil {
			f, _ := sel.Obj().(*types.Func)
			return f
		}
		f, _ := info.Uses[fn.Sel].(*types.Func)
		return f
	}
	return nil
}

// leak 은 로그로 가는 식에서 오류 값이나 원문을 찾는다. 없으면 nil 이다.
//
//   - 타입이 오류이거나 오류를 필드·원소로 품는 값(구조체, 슬라이스, 맵, 포인터)
//   - 오류 값·원문이 대입된 변수와 필드(collect)
//   - x.Error(), 그리고 그 글자를 옮기는 식: 문자열 더하기, 변환, 글자를 받아 글자를 돌려주는 호출(strings.TrimSpace 등)
//   - 인자를 그대로 옮기는 호출(slog 의 모든 함수, fmt.Sprint 류, json.Marshal, append)과 리터럴·&·인덱스·슬라이스·단언의 안
//
// 오류 값을 받아 다른 것을 돌려주는 호출(logctx.ErrKind(err), errors.Is)은 결과만 로그로 가므로 안을 보지 않는다.
func (c *leakChecker) leak(e ast.Expr) ast.Expr {
	e = ast.Unparen(e)
	if c.input != nil {
		if c.input.source(c.info, e) {
			return e
		}
	} else if tv, ok := c.info.Types[e]; ok && tv.Type != nil && !tv.IsType() && tv.Type != types.Typ[types.UntypedNil] {
		if types.Implements(types.NewPointer(tv.Type), c.errType) && isAddressable(e) && !types.Implements(tv.Type, c.errType) {
			return e
		}
		if c.holdsError(tv.Type, map[types.Type]bool{}) {
			return e
		}
	}
	switch x := e.(type) {
	case *ast.Ident:
		if o := c.info.Uses[x]; o != nil && c.tainted[o] {
			return e
		}
	case *ast.SelectorExpr:
		if o := c.info.Uses[x.Sel]; o != nil && c.tainted[o] {
			return e
		}
		// 입력을 찾을 때는 필드의 바깥도 본다 — r.URL.Path 는 r.URL(요청의 주소)에서 온다.
		if sel := c.info.Selections[x]; c.input != nil && sel != nil && sel.Kind() == types.FieldVal {
			return c.leak(x.X)
		}
	case *ast.CallExpr:
		return c.leakCall(x)
	case *ast.CompositeLit:
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if w := c.leak(kv.Key); w != nil {
					return w
				}
				elt = kv.Value
			}
			if w := c.leak(elt); w != nil {
				return w
			}
		}
	case *ast.UnaryExpr:
		return c.leak(x.X)
	case *ast.StarExpr:
		return c.leak(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			if w := c.leak(x.X); w != nil {
				return w
			}
			return c.leak(x.Y)
		}
	case *ast.IndexExpr:
		return c.leak(x.X)
	case *ast.SliceExpr:
		return c.leak(x.X)
	case *ast.TypeAssertExpr:
		return c.leak(x.X)
	}
	return nil
}

func (c *leakChecker) leakCall(call *ast.CallExpr) ast.Expr {
	// 변환(string(b), []byte(s), any(err))은 안을 그대로 옮긴다.
	if tv, ok := c.info.Types[call.Fun]; ok && tv.IsType() {
		for _, a := range call.Args {
			if w := c.leak(a); w != nil {
				return w
			}
		}
		return nil
	}
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		if b, ok := c.info.Uses[id].(*types.Builtin); ok {
			if b.Name() == "append" {
				for _, a := range call.Args {
					if w := c.leak(a); w != nil {
						return w
					}
				}
			}
			return nil
		}
	}
	f := calleeOf(c.info, call)
	if f == nil {
		return nil
	}
	sig, _ := f.Type().(*types.Signature)
	if c.input != nil {
		if c.input.sanitizers[funcName(f)] {
			return nil // 입력을 고정 값·정규화한 id 로 바꾸는 거르개다
		}
		// 입력 값의 메서드가 글자(나 글자 맵)를 돌려주면 그것도 입력이다(r.URL.String(), r.Header.Get("…"), r.URL.Query().Get("q")).
		if fn, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && sig != nil && sig.Recv() != nil && carriesText(sig.Results()) {
			if w := c.leak(fn.X); w != nil {
				return w
			}
		}
	} else if sig != nil && sig.Recv() != nil && f.Name() == "Error" && sig.Params().Len() == 0 && types.Implements(sig.Recv().Type(), c.errType) {
		return call
	}
	if f.Pkg() != nil {
		p := f.Pkg().Path()
		if p == "log/slog" || p == "fmt" && fmtText[f.Name()] || p == "encoding/json" && strings.HasPrefix(f.Name(), "Marshal") {
			for _, a := range call.Args {
				if w := c.leak(a); w != nil {
					return w
				}
			}
			return nil
		}
	}
	// 글자를 받아 글자를 돌려주는 호출은 원문을 옮긴다(strings.TrimSpace(err.Error()), strconv.Quote(msg)).
	if sig == nil || !producesText(sig.Results()) {
		return nil
	}
	for _, a := range call.Args {
		if tv, ok := c.info.Types[ast.Unparen(a)]; ok && isText(tv.Type) {
			if w := c.leak(a); w != nil {
				return w
			}
		}
	}
	return nil
}

// holdsError 는 t 가 오류이거나 오류를 필드·원소로 품는지 본다. any 처럼 실행 때에야 아는 것은 보지 않는다.
func (c *leakChecker) holdsError(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if types.Implements(t, c.errType) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Struct:
		for i := range u.NumFields() {
			if c.holdsError(u.Field(i).Type(), seen) {
				return true
			}
		}
	case *types.Pointer:
		return c.holdsError(u.Elem(), seen)
	case *types.Slice:
		return c.holdsError(u.Elem(), seen)
	case *types.Array:
		return c.holdsError(u.Elem(), seen)
	case *types.Map:
		return c.holdsError(u.Key(), seen) || c.holdsError(u.Elem(), seen)
	}
	return false
}

// isText 는 글자를 담는 타입(string, []byte, []string)이다.
func isText(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok {
			return b.Kind() == types.Byte || b.Info()&types.IsString != 0
		}
	}
	return false
}

// isAny 는 메서드 없는 인터페이스(any)다. 무엇이든 담을 수 있다.
func isAny(t types.Type) bool {
	i, ok := t.Underlying().(*types.Interface)
	return ok && i.NumMethods() == 0
}

// producesText 는 결과에 글자나 무엇이든 담을 수 있는 값(any)이 있는지 본다.
func producesText(res *types.Tuple) bool {
	for i := range res.Len() {
		if t := res.At(i).Type(); isAny(t) || isText(t) {
			return true
		}
	}
	return false
}

// isAddressable 은 포인터 수신자로 error 를 충족하는 값이 실제로 그렇게 쓰일 수 있는지 대략 본다. 변수와 필드만 그렇다.
func isAddressable(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	}
	return false
}

// carriesText 는 결과에 글자, any, 또는 글자를 값으로 담은 맵(url.Values·http.Header)이 있는지 본다.
func carriesText(res *types.Tuple) bool {
	if producesText(res) {
		return true
	}
	for i := range res.Len() {
		if m, ok := res.At(i).Type().Underlying().(*types.Map); ok && isText(m.Elem()) {
			return true
		}
	}
	return false
}
