package archtest

import (
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/types"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// D47(감사 24): 오류율 알람은 한 오류를 한 번 센다. 요청 안의 원인 줄(ERROR)은 같은 요청의 http.request(ERROR)가 세므로
// OtherErrorCount 의 필터에서 이름으로 뺀다. 감사 23 에서 그 목록이 코드와 어긋나 두 번 셌고(agent.login_failed·files.save_failed),
// 필터 식은 deploy.md 의 셸 조각에만 있어 테스트가 대조할 수 없었다. 이제 필터는 deploy/cloudwatch/log-metric-filters.json 에 있고,
// 이 테스트가 코드에서 ERROR 를 낼 수 있는 사건을 모아 errorEvents 표와, 표의 요청 안 사건을 필터의 제외 목록과 맞춘다.
// 새 ERROR 사건을 더하면 표에 한 줄을 더해야 통과하고, 요청 안이라고 적으면 필터에도 더해야 통과한다.

const metricFiltersFile = "deploy/cloudwatch/log-metric-filters.json"

// errorEvents 는 ERROR 로 남을 수 있는 사건과 그 실패를 무엇이 세는지다(docs/deploy.md 15.3 의 표와 같다).
//
//	request  요청 안의 원인 줄. 같은 요청의 http.request(ERROR)가 센다 → OtherErrorCount 에서 뺀다
//	self     http.request 자신. ErrorCount 가 센다 → OtherErrorCount 에서 뺀다
//	panic    panic.recovered. PanicCount 가 센다 → OtherErrorCount 에서 뺀다
//	outside  요청이 세지 못하는 실패(WebSocket·기동·응답이 나간 뒤의 실패) → OtherErrorCount 가 센다
var errorEvents = map[string]string{
	"http.request":       "self",
	"http.failed":        "request",
	"panic.recovered":    "panic",
	"message.rejected":   "request", // WebSocket 전송이면 ws.failed 가 센다
	"upload.rejected":    "request",
	"rating.rejected":    "request",
	"agent.login_failed": "request",
	"files.save_failed":  "request",
	"export.failed":      "outside", // 본문이 200 으로 나간 뒤라 http.request 는 ERROR 가 아니다
	"ws.failed":          "outside",
	"server.exit":        "outside",
	"config.invalid":     "outside",
}

type metricFilter struct {
	FilterName            string `json:"filterName"`
	FilterPattern         string `json:"filterPattern"`
	MetricTransformations []struct {
		MetricName string `json:"metricName"`
	} `json:"metricTransformations"`
}

func TestAlarm_ERROR사건은코드와필터의제외목록이같아한오류를한번센다(t *testing.T) {
	found := errorEventsInCode(t)
	var events []string
	for e := range found {
		events = append(events, e)
	}
	sort.Strings(events)
	if len(events) < 8 {
		t.Fatalf("ERROR 사건을 %d 개만 찾았다 — 검사가 로그 호출을 알아보지 못한다: %v", len(events), events)
	}
	for _, e := range events {
		if _, ok := errorEvents[e]; !ok {
			t.Errorf("%s 가 ERROR 로 남을 수 있는데 errorEvents 표에 없다(%s). 무엇이 세는지 정하고 표·필터·deploy.md 15.3 을 고친다", e, found[e])
		}
	}
	for e := range errorEvents {
		if _, ok := found[e]; !ok {
			t.Errorf("errorEvents 의 %s 를 코드에서 ERROR 로 남기는 곳이 없다 — 표에서 빼고 필터를 고친다", e)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", metricFiltersFile))
	if err != nil {
		t.Fatal(err)
	}
	var filters []metricFilter
	if err := json.Unmarshal(raw, &filters); err != nil {
		t.Fatalf("%s: %v", metricFiltersFile, err)
	}
	byMetric := map[string]string{}
	for _, f := range filters {
		if len(f.MetricTransformations) != 1 {
			t.Fatalf("%s 의 지표 변환이 하나가 아니다", f.FilterName)
		}
		byMetric[f.MetricTransformations[0].MetricName] = f.FilterPattern
	}
	if got := byMetric["ErrorCount"]; got != `{ $.msg = "http.request" && $.level = "ERROR" }` {
		t.Errorf("ErrorCount 는 http.request 의 ERROR 만 센다: %q", got)
	}
	if got := byMetric["PanicCount"]; got != `{ $.msg = "panic.recovered" }` {
		t.Errorf("PanicCount 는 panic.recovered 를 센다: %q", got)
	}
	other := byMetric["OtherErrorCount"]
	if !strings.HasPrefix(other, `{ $.level = "ERROR" && `) {
		t.Fatalf("OtherErrorCount 는 ERROR 에서 이름으로 뺀 나머지다: %q", other)
	}
	// 필터 식은 `$.msg != "이름"` 의 && 묶음이어야 한다. 다른 꼴이 섞이면 아래 대조가 뜻을 잃는다.
	rest := strings.TrimSuffix(strings.TrimPrefix(other, `{ $.level = "ERROR" && `), " }")
	var excluded []string
	for _, part := range strings.Split(rest, " && ") {
		m := excludePattern.FindStringSubmatch(part)
		if m == nil {
			t.Fatalf("OtherErrorCount 의 조건 %q 가 `$.msg != \"이름\"` 꼴이 아니다", part)
		}
		excluded = append(excluded, m[1])
	}
	var want []string
	for e, class := range errorEvents {
		if class != "outside" {
			want = append(want, e)
		}
	}
	sort.Strings(excluded)
	sort.Strings(want)
	if !slices.Equal(excluded, want) {
		t.Errorf("OtherErrorCount 가 빼는 사건 %v 가 요청 안·http.request·패닉 사건 %v 와 다르다 — 한 오류를 두 번 세거나 놓친다", excluded, want)
	}
}

var excludePattern = regexp.MustCompile(`^\$\.msg != "([a-z0-9_.]+)"$`)

// errorEventsInCode 는 리포의 비테스트 코드에서 ERROR 로 남을 수 있는 사건 이름과 그 자리다.
//
//   - slog 의 Error·ErrorContext(패키지 함수와 *slog.Logger 메서드)의 사건 이름
//   - slog 의 Log·LogAttrs 중 수준이 ERROR 일 수 있는 것: 상수면 그 값, 같은 함수 안에서 상수만 대입한 변수면 그 값들의 최댓값,
//     그 밖(함수가 정하는 수준 — requestLevel·rejectLevel)은 ERROR 일 수 있다고 본다
//   - 사건 이름이 함수 매개변수인 로그 도우미(app.logRejected)는 그 도우미를 부르는 곳의 상수 이름
func errorEventsInCode(t *testing.T) map[string]string {
	t.Helper()
	l := loadPackages(t)
	type pkgInfo struct {
		files []*ast.File
		info  *types.Info
	}
	var pkgs []pkgInfo
	for _, p := range l.ours {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(l.fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: l.imp}).Check(p.ImportPath, l.fset, files, info); err != nil {
			t.Fatalf("%s 타입 검사: %v", p.ImportPath, err)
		}
		pkgs = append(pkgs, pkgInfo{files, info})
	}

	found := map[string]string{}
	wrappers := map[string]int{} // 도우미 함수 이름 → 사건 이름 매개변수의 자리
	for _, p := range pkgs {
		for _, f := range p.files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					msg, isErr := errorLogCall(p.info, fd, call)
					if msg == nil || !isErr {
						return true
					}
					if tv, ok := p.info.Types[msg]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
						found[constant.StringVal(tv.Value)] = l.fset.Position(call.Pos()).String()
						return true
					}
					if idx := paramIndex(p.info, fd, msg); idx >= 0 {
						if fn, ok := p.info.Defs[fd.Name].(*types.Func); ok {
							wrappers[funcName(fn)] = idx
						}
						return true
					}
					t.Errorf("%s: ERROR 일 수 있는 로그의 사건 이름이 상수도 매개변수도 아니다 — 검사가 알아볼 수 있게 상수로 쓴다", l.fset.Position(call.Pos()))
					return true
				})
			}
		}
	}
	for _, p := range pkgs {
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := calleeOf(p.info, call)
				if fn == nil {
					return true
				}
				idx, ok := wrappers[funcName(fn)]
				if !ok || idx >= len(call.Args) {
					return true
				}
				if tv, ok := p.info.Types[call.Args[idx]]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
					found[constant.StringVal(tv.Value)] = l.fset.Position(call.Pos()).String()
				} else {
					t.Errorf("%s: 로그 도우미 %s 의 사건 이름이 상수가 아니다", l.fset.Position(call.Pos()), fn.Name())
				}
				return true
			})
		}
	}
	return found
}

// errorLogCall 은 call 이 slog 로그 호출이면 사건 이름 식과 ERROR 일 수 있는지를 돌려준다. 로그 호출이 아니면 nil 이다.
func errorLogCall(info *types.Info, fd *ast.FuncDecl, call *ast.CallExpr) (ast.Expr, bool) {
	fn := calleeOf(info, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return nil, false
	}
	arg := func(i int) ast.Expr {
		if i < len(call.Args) {
			return call.Args[i]
		}
		return nil
	}
	switch fn.Name() {
	case "Error":
		return arg(0), true
	case "ErrorContext":
		return arg(1), true
	case "Log", "LogAttrs":
		lv := arg(1)
		if lv == nil {
			return nil, false
		}
		return arg(2), maxLevel(info, fd, lv) >= slog.LevelError
	}
	return nil, false
}

// maxLevel 은 수준 식이 가질 수 있는 가장 높은 값이다. 상수가 아니고 상수만 대입한 지역 변수도 아니면 ERROR 로 본다.
func maxLevel(info *types.Info, fd *ast.FuncDecl, e ast.Expr) slog.Level {
	if v, ok := constLevel(info, e); ok {
		return v
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return slog.LevelError
	}
	obj := info.Uses[id]
	if obj == nil {
		return slog.LevelError
	}
	top, seen := slog.Level(-1000), false
	unknown := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			lid, ok := lhs.(*ast.Ident)
			if !ok || (info.Defs[lid] != obj && info.Uses[lid] != obj) {
				continue
			}
			seen = true
			if len(as.Rhs) != len(as.Lhs) {
				unknown = true
				continue
			}
			if v, ok := constLevel(info, as.Rhs[i]); ok {
				top = max(top, v)
			} else {
				unknown = true
			}
		}
		return true
	})
	if !seen || unknown {
		return slog.LevelError
	}
	return top
}

func constLevel(info *types.Info, e ast.Expr) (slog.Level, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return 0, false
	}
	v, ok := constant.Int64Val(tv.Value)
	return slog.Level(v), ok
}

// paramIndex 는 e 가 fd 의 매개변수면 그 자리, 아니면 -1 이다.
func paramIndex(info *types.Info, fd *ast.FuncDecl, e ast.Expr) int {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return -1
	}
	obj := info.Uses[id]
	i := 0
	for _, field := range fd.Type.Params.List {
		for _, name := range field.Names {
			if info.Defs[name] == obj {
				return i
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	return -1
}

// D49(감사 31): 스키마가 뒤처진 서버가 트래픽을 받는 경우(손으로 update-service, 옛 백업 복원)는 /readyz 503 만으로는 아무도 모른다 —
// ALB 헬스체크는 /healthz 다. 서버가 남기는 schema.behind(WARN, 1분에 한 줄)를 로그 지표 SchemaBehind 로 세고 알람을 건다
// (docs/deploy.md 15.4). 사건 이름을 바꾸면 필터가 조용히 0 이 되므로, 코드의 이름과 필터 식을 맞춰 본다.
func TestAlarm_스키마가뒤처진서버의schema_behind를세는로그지표필터가있다(t *testing.T) {
	code, err := os.ReadFile(filepath.Join("..", "..", "cmd", "server", "commands.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(code), `log.Warn("schema.behind"`) {
		t.Fatal(`cmd/server/commands.go 가 schema.behind 를 남기지 않는다 — 필터와 이 테스트를 같이 고친다`)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", metricFiltersFile))
	if err != nil {
		t.Fatal(err)
	}
	var filters []metricFilter
	if err := json.Unmarshal(raw, &filters); err != nil {
		t.Fatal(err)
	}
	for _, f := range filters {
		if len(f.MetricTransformations) == 1 && f.MetricTransformations[0].MetricName == "SchemaBehind" {
			if f.FilterPattern != `{ $.msg = "schema.behind" }` {
				t.Errorf("SchemaBehind 의 필터 식 %q, schema.behind 를 세야 한다", f.FilterPattern)
			}
			return
		}
	}
	t.Errorf("%s 에 SchemaBehind 지표 필터가 없다", metricFiltersFile)
}
