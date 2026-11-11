package testkit_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestPagerDutyClientOnlyReads checks the source of internal/pd. The HTTP
// layer refuses a PagerDuty write before it is sent; this keeps the client
// from building one at all. The packages that read PagerDuty data cannot
// import the HTTP layer, so they can only read through the client.
func TestPagerDutyClientOnlyReads(t *testing.T) {
	for _, f := range nonTestFiles(t) {
		if under(f.pkg, mod+"/internal/pd") {
			for _, v := range pagerDutyWriteViolations(f.ast, f.fset) {
				t.Errorf("%s:%s", f.path, v)
			}
		}
	}
}

// requestWriteFields are the httpx.Request fields that make a request
// something other than a plain GET.
var requestWriteFields = map[string]bool{"Method": true, "Body": true, "Idempotent": true}

// pagerDutyWriteViolations returns "line: problem" for each way a file could
// build a request that writes.
func pagerDutyWriteViolations(file *ast.File, fset *token.FileSet) []string {
	var out []string
	report := func(n ast.Node, format string, args ...any) {
		out = append(out, fmt.Sprintf("%d: ", fset.Position(n.Pos()).Line)+fmt.Sprintf(format, args...))
	}
	httpx := ""
	for name, imp := range importNames(file) {
		if imp == mod+"/internal/httpx" {
			httpx = name
		}
	}
	isHTTPX := func(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && httpx != "" && id.Name == httpx }
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if !isHTTPX(x.X) {
				break
			}
			if strings.HasPrefix(x.Sel.Name, "Method") && x.Sel.Name != "MethodGet" {
				report(x, "names httpx.%s: the PagerDuty client sends only GET", x.Sel.Name)
			}
			if x.Sel.Name == "PageInfo" {
				report(x, "uses httpx.PageInfo, which pages with POST")
			}
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || !isHTTPX(sel.X) || sel.Sel.Name != "Request" {
				break
			}
			for _, e := range x.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && requestWriteFields[k.Name] {
						report(kv, "sets httpx.Request.%s: the PagerDuty client sends only GET", k.Name)
					}
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && requestWriteFields[sel.Sel.Name] {
					report(sel, "sets .%s: the PagerDuty client sends only GET", sel.Sel.Name)
				}
			}
		}
		return true
	})
	return out
}

// TestPagerDutyReadRuleFires proves the rule catches each way of writing and
// leaves a plain GET alone.
func TestPagerDutyReadRuleFires(t *testing.T) {
	cases := []struct{ body, want string }{
		{`_ = httpx.Request{Family: httpx.PagerDuty, Path: "/x"}`, ""},
		{`_ = httpx.Request{Method: httpx.MethodGet}`, "Request.Method"},
		{`_ = httpx.Request{Path: "/x", Body: 1}`, "Request.Body"},
		{`_ = httpx.Request{Idempotent: true}`, "Request.Idempotent"},
		{`_ = httpx.MethodPost`, "httpx.MethodPost"},
		{`_ = httpx.MethodDelete`, "httpx.MethodDelete"},
		{`var r httpx.Request; r.Method = "PUT"; _ = r`, ".Method"},
		{`var r httpx.Request; r.Body = 1; _ = r`, ".Body"},
		{`_ = httpx.PageInfo[int]`, "httpx.PageInfo"},
	}
	for _, c := range cases {
		src := "package p\nimport \"" + mod + "/internal/httpx\"\nfunc f() {\n" + c.body + "\n}\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		got := strings.Join(pagerDutyWriteViolations(file, fset), ";")
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: unexpected %s", c.body, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: want a violation naming %q, got %q", c.body, c.want, got)
		}
	}
}
