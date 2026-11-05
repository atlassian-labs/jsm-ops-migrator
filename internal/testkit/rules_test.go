package testkit_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

const mod = testkit.Module

func under(pkg, base string) bool { return pkg == base || strings.HasPrefix(pkg, base+"/") }

type importRule struct {
	name    string
	imports func(imp string) bool
	allowed func(pkg string) bool
}

// importRules are the repository's network and dependency boundaries. They
// mirror the depguard rules in .golangci.yml; this test runs without
// golangci-lint, so the rules hold on every machine.
var importRules = []importRule{
	{
		name: "only internal/httpx (and testkit fakes) imports the network stack",
		imports: func(imp string) bool {
			return imp == "net" || under(imp, "net/http") || under(imp, "net/rpc") || imp == "net/smtp" ||
				imp == "net/textproto" || imp == "crypto/tls" || under(imp, "golang.org/x/net") ||
				imp == "github.com/hashicorp/go-retryablehttp"
		},
		allowed: func(pkg string) bool { return pkg == mod+"/internal/httpx" || under(pkg, mod+"/internal/testkit") },
	},
	{
		name:    "only pd, jsm, testkit and cmd import internal/httpx itself",
		imports: func(imp string) bool { return imp == mod+"/internal/httpx" },
		allowed: func(pkg string) bool {
			return under(pkg, mod+"/internal/pd") || under(pkg, mod+"/internal/jsm") ||
				under(pkg, mod+"/internal/testkit") || pkg == mod+"/cmd/jsm-ops-migrator"
		},
	},
	{
		name:    "only cmd/jsm-ops-migrator imports internal/cli",
		imports: func(imp string) bool { return imp == mod+"/internal/cli" },
		allowed: func(pkg string) bool { return pkg == mod+"/cmd/jsm-ops-migrator" },
	},
	{
		name: "only internal/cli imports cobra, pflag and viper",
		imports: func(imp string) bool {
			return under(imp, "github.com/spf13/cobra") || under(imp, "github.com/spf13/pflag") || under(imp, "github.com/spf13/viper")
		},
		allowed: func(pkg string) bool { return under(pkg, mod+"/internal/cli") },
	},
	{
		name:    "only internal/credentials imports go-keyring",
		imports: func(imp string) bool { return under(imp, "github.com/zalando/go-keyring") },
		allowed: func(pkg string) bool { return pkg == mod+"/internal/credentials" },
	},
	{
		name:    "only internal/store imports the SQLite driver",
		imports: func(imp string) bool { return under(imp, "modernc.org/sqlite") },
		allowed: func(pkg string) bool { return pkg == mod+"/internal/store" },
	},
	{
		// testkit may read secrets and holds fakes, so it must never reach
		// the binary: only test files import it.
		name:    "only test files import internal/testkit",
		imports: func(imp string) bool { return under(imp, mod+"/internal/testkit") },
		allowed: func(pkg string) bool { return under(pkg, mod+"/internal/testkit") },
	},
	{
		name: "go-pagerduty and the Opsgenie SDK are references, not dependencies",
		imports: func(imp string) bool {
			return under(imp, "github.com/PagerDuty/go-pagerduty") || under(imp, "github.com/opsgenie/opsgenie-go-sdk-v2")
		},
		allowed: func(string) bool { return false },
	},
}

// forbiddenCalls mirror the forbidigo rules in .golangci.yml.
var forbiddenCalls = map[string]map[string]string{
	"net/http": {
		"DefaultClient": "use the configured httpx client", "DefaultTransport": "use the configured httpx client",
		"Get": "use the configured httpx client", "Head": "use the configured httpx client",
		"Post": "use the configured httpx client", "PostForm": "use the configured httpx client",
	},
	"fmt": {
		"Print": "write to the command's writers or the logger", "Printf": "write to the command's writers or the logger",
		"Println": "write to the command's writers or the logger",
	},
	"log": {
		"Print": "use the redacting slog logger", "Printf": "use the redacting slog logger", "Println": "use the redacting slog logger",
		"Fatal": "use the redacting slog logger", "Fatalf": "use the redacting slog logger", "Fatalln": "use the redacting slog logger",
		"Panic": "use the redacting slog logger", "Panicf": "use the redacting slog logger", "Panicln": "use the redacting slog logger",
	},
}

type goFile struct {
	path string // relative to the repository root
	pkg  string // import path
	ast  *ast.File
	fset *token.FileSet
}

// nonTestFiles parses every non-test Go file of the CLI module. tools/pdseed
// is a separate module with its own rules, so directories with their own
// go.mod are skipped, as are testdata folders.
func nonTestFiles(t *testing.T) []goFile {
	t.Helper()
	root, err := testkit.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	var files []goFile
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || d.Name() == "testdata" || fileExists(filepath.Join(p, "go.mod"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, goFile{
			path: filepath.ToSlash(rel),
			pkg:  path.Join(mod, filepath.ToSlash(filepath.Dir(rel))),
			ast:  f,
			fset: fset,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestImportRules(t *testing.T) {
	for _, f := range nonTestFiles(t) {
		for _, spec := range f.ast.Imports {
			imp, _ := strconv.Unquote(spec.Path.Value)
			for _, r := range importRules {
				if r.imports(imp) && !r.allowed(f.pkg) {
					t.Errorf("%s:%d imports %s: %s", f.path, f.fset.Position(spec.Pos()).Line, imp, r.name)
				}
			}
		}
	}
}

func TestForbiddenCalls(t *testing.T) {
	for _, f := range nonTestFiles(t) {
		for _, v := range forbiddenCallViolations(f.pkg, f.ast, f.fset) {
			t.Errorf("%s:%s", f.path, v)
		}
	}
}

// forbiddenCallViolations returns "line: problem" for each forbidden call in a
// file of package pkg.
func forbiddenCallViolations(pkg string, file *ast.File, fset *token.FileSet) []string {
	var out []string
	report := func(n ast.Node, format string, args ...any) {
		out = append(out, fmt.Sprintf("%d: ", fset.Position(n.Pos()).Line)+fmt.Sprintf(format, args...))
	}
	names := importNames(file)
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				imp := names[id.Name]
				if why, bad := forbiddenCalls[imp][x.Sel.Name]; bad {
					report(x, "uses %s.%s: %s", imp, x.Sel.Name, why)
				}
				if imp == "os" && x.Sel.Name == "Exit" && pkg != mod+"/cmd/jsm-ops-migrator" {
					report(x, "calls os.Exit: only cmd/jsm-ops-migrator may exit")
				}
			}
			// Secret.Reveal is the only way to read a credential; only the
			// keychain code and the transport may hand one over, plus the
			// fakes in testkit, which no binary code imports.
			if x.Sel.Name == "Reveal" && !under(pkg, mod+"/internal/credentials") &&
				pkg != mod+"/internal/httpx" && !under(pkg, mod+"/internal/testkit") {
				report(x, "calls Reveal: only internal/credentials and internal/httpx may read a Secret")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				report(x, "calls the builtin %s", id.Name)
			}
		}
		return true
	})
	return out
}

// TestForbiddenCallRulesFire proves each call rule catches what it should
// and leaves the allowed places alone.
func TestForbiddenCallRulesFire(t *testing.T) {
	cases := []struct {
		pkg, body string
		want      string // part of the violation, or "" for none
	}{
		{"internal/pd", `_, _ = http.Get("x")`, "net/http.Get"},
		{"internal/pd", `_ = http.DefaultClient`, "net/http.DefaultClient"},
		{"internal/httpx", `_ = http.DefaultTransport`, "net/http.DefaultTransport"},
		{"internal/run", `fmt.Println("x")`, "fmt.Println"},
		{"internal/run", `log.Fatal("x")`, "log.Fatal"},
		{"internal/run", `println("x")`, "builtin println"},
		{"internal/run", `os.Exit(1)`, "os.Exit"},
		{"cmd/jsm-ops-migrator", `os.Exit(1)`, ""},
		{"internal/run", `_ = s.Reveal()`, "Reveal"},
		{"internal/httpx", `_ = s.Reveal()`, ""},
		{"internal/httpx/httperr", `_ = s.Reveal()`, "Reveal"},
		{"internal/credentials", `_ = s.Reveal()`, ""},
		{"internal/run", `_, _ = fmt.Fprintln(w, "x")`, ""},
	}
	for _, c := range cases {
		src := "package p\nimport (\"fmt\"; \"log\"; \"net/http\"; \"os\")\n" +
			"var _, _, _, _ = fmt.Sprint, log.Print, http.Get, os.Getpid\nfunc f(s interface{ Reveal() string }, w interface{ Write([]byte) (int, error) }) {\n" + c.body + "\n}\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		// The helper references above are not violations themselves.
		var got []string
		for _, v := range forbiddenCallViolations(mod+"/"+c.pkg, file, fset) {
			if !strings.HasPrefix(v, "3: ") {
				got = append(got, v)
			}
		}
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s in %s: unexpected %v", c.body, c.pkg, got)
		case c.want != "" && (len(got) == 0 || !strings.Contains(strings.Join(got, ";"), c.want)):
			t.Errorf("%s in %s: want a violation naming %q, got %v", c.body, c.pkg, c.want, got)
		}
	}
}

// importNames maps each name a file uses for an import to its path.
func importNames(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		imp, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(imp)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		out[name] = imp
	}
	return out
}

// TestRulesCatchViolations proves each rule fires, so a broken rule cannot
// pass silently.
func TestRulesCatchViolations(t *testing.T) {
	cases := []struct {
		pkg, imp string
		wantRule bool
	}{
		{mod + "/internal/pd", "net/http", true},
		{mod + "/internal/httpx/httperr", "net/http", true},
		{mod + "/internal/httpx", "net/http", false},
		{mod + "/internal/pd", "net/url", false},
		{mod + "/internal/pd", "net/smtp", true},
		{mod + "/internal/pd", "net/rpc/jsonrpc", true},
		{mod + "/internal/run", mod + "/internal/httpx", true},
		{mod + "/internal/run", mod + "/internal/httpx/httperr", false},
		{mod + "/internal/store", mod + "/internal/cli", true},
		{mod + "/internal/store", "github.com/spf13/viper", true},
		{mod + "/internal/cli", "github.com/zalando/go-keyring", true},
		{mod + "/internal/run", "modernc.org/sqlite", true},
		{mod + "/internal/pd", "github.com/PagerDuty/go-pagerduty", true},
		{mod + "/internal/store", mod + "/internal/testkit", true},
		{mod + "/internal/testkit/pdfake", mod + "/internal/testkit", false},
	}
	for _, c := range cases {
		var hit bool
		for _, r := range importRules {
			if r.imports(c.imp) && !r.allowed(c.pkg) {
				hit = true
			}
		}
		if hit != c.wantRule {
			t.Errorf("%s importing %s: rule hit = %v, want %v", c.pkg, c.imp, hit, c.wantRule)
		}
	}
}
