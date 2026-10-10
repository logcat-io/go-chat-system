package archtest

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/types"
	"path/filepath"
	"regexp"
	"testing"
)

// D47(감사 24): 입력 거절(app.InvalidError)은 모두 고정 사유 코드를 싣는다. 감사 24 가 reason 없이 남는 400 invalid 를 15종 넘게 셌다 —
// http.request 한 줄로는 어느 필드가 틀렸는지 몰랐다. 코드는 로그에 그대로 실리므로 원문·입력값이 섞이면 안 된다. 그래서 app.invalid 의
// 코드 인자는 소문자 리터럴이거나, 그 코드를 매개변수로 넘기는 도우미(trimmedRunes)를 부르는 곳의 리터럴이어야 한다.

const appPath = modulePath + "/internal/app"

var reasonCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestInput_입력거절의사유코드는소문자리터럴이다(t *testing.T) {
	l := loadPackages(t)
	var files []*ast.File
	var info *types.Info
	for _, p := range l.ours {
		if p.ImportPath != appPath {
			continue
		}
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(l.fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info = &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: l.imp}).Check(appPath, l.fset, files, info); err != nil {
			t.Fatalf("%s 타입 검사: %v", appPath, err)
		}
	}
	if info == nil {
		t.Fatal("app 패키지를 찾지 못했다")
	}

	literal := func(e ast.Expr) (string, bool) {
		tv, ok := info.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
			return "", false
		}
		_, isLit := ast.Unparen(e).(*ast.BasicLit)
		return constant.StringVal(tv.Value), isLit
	}
	// 1단계: invalid 를 부르는 곳. 코드가 리터럴이 아니면 매개변수여야 하고, 그 함수는 도우미로 적어 둔다.
	wrappers := map[types.Object]int{}
	codes := map[string]int{}
	calls := 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name == "invalid" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := calleeOf(info, call)
				if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != appPath || fn.Name() != "invalid" || len(call.Args) < 2 {
					return true
				}
				calls++
				if code, ok := literal(call.Args[0]); ok {
					if !reasonCode.MatchString(code) {
						t.Errorf("%s: 사유 코드 %q 가 소문자·숫자·_ 가 아니다", l.fset.Position(call.Pos()), code)
					}
					codes[code]++
					return true
				}
				if idx := paramIndex(info, fd, call.Args[0]); idx >= 0 {
					wrappers[info.Defs[fd.Name]] = idx
					return true
				}
				t.Errorf("%s: 사유 코드가 리터럴이 아니다 — 원문이나 입력값이 로그에 실릴 수 있다", l.fset.Position(call.Pos()))
				return true
			})
		}
	}
	// 2단계: 도우미를 부르는 곳의 코드도 리터럴이어야 한다.
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := calleeOf(info, call)
			if fn == nil {
				return true
			}
			idx, ok := wrappers[fn]
			if !ok || idx >= len(call.Args) {
				return true
			}
			calls++
			code, lit := literal(call.Args[idx])
			if !lit || !reasonCode.MatchString(code) {
				t.Errorf("%s: %s 에 넘긴 사유 코드가 소문자 리터럴이 아니다", l.fset.Position(call.Pos()), fn.Name())
			}
			codes[code]++
			return true
		})
	}
	if calls < 80 {
		t.Fatalf("입력 거절을 %d 곳만 찾았다 — 검사가 invalid 호출을 알아보지 못한다", calls)
	}
	if len(codes) < 40 {
		t.Errorf("사유 코드가 %d 종뿐이다 — 필드 수준으로 갈리지 않는다", len(codes))
	}
}
