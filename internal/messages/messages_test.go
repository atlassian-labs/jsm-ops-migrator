package messages_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

func apiError(kind error, cause error) error {
	return fmt.Errorf("creating team: %w", &httperr.Error{Family: "jsm_ops", Method: "POST", Path: "/v1/teams", Kind: kind, Cause: cause})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		read, write messages.Class
	}{
		{"unauthorized", apiError(httperr.ErrUnauthorized, nil), messages.Blocker, messages.Blocker},
		{"forbidden", apiError(httperr.ErrForbidden, nil), messages.Blocker, messages.Blocker},
		{"refused", apiError(httperr.ErrRefused, nil), messages.Blocker, messages.Blocker},
		{"not in plan", apiError(httperr.ErrNotInPlan, nil), messages.Information, messages.Blocker},
		{"not found", apiError(httperr.ErrNotFound, nil), messages.Blocker, messages.Warning},
		{"conflict", apiError(httperr.ErrConflict, nil), messages.Blocker, messages.Warning},
		{"invalid", apiError(httperr.ErrInvalid, nil), messages.Blocker, messages.Warning},
		{"unavailable", apiError(httperr.ErrUnavailable, nil), messages.Blocker, messages.Warning},
		{"bad response", apiError(httperr.ErrBadResponse, nil), messages.Blocker, messages.Warning},
		{"partial", fmt.Errorf("adding members: %w", &httperr.Partial{Done: []int{0}, Failed: []httperr.ItemError{{Index: 1, Err: httperr.ErrConflict}}}),
			messages.Blocker, messages.Warning},
		{"unknown outcome", apiError(httperr.ErrUnknownOutcome, errors.New("timeout")), messages.Blocker, messages.ManualTask},
		// The cause under an unknown outcome must not decide the class: the
		// write may have taken effect.
		{"unknown outcome over unavailable", fmt.Errorf("%w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable),
			messages.Blocker, messages.ManualTask},
		// An error without a declaration: the list is no more complete on a
		// read, and the item failed on a write, as when JSM reports that
		// enabling Operations failed.
		{"JSM reports a failure", errors.New("JSM reported that enabling Operations failed"), messages.Blocker, messages.Warning},
		{"not from an API", errors.New("the step could not build its body"), messages.Blocker, messages.Warning},
		{"canceled", context.Canceled, messages.Blocker, messages.Warning},
		{"partial position without a declaration", fmt.Errorf("member 3: %w", errors.New("no such user")), messages.Blocker, messages.Warning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := messages.Classify(c.err, messages.Read); got != c.read {
				t.Errorf("read: got %q, want %q", got, c.read)
			}
			if got := messages.Classify(c.err, messages.Write); got != c.write {
				t.Errorf("write: got %q, want %q", got, c.write)
			}
		})
	}
	if got := messages.Classify(nil, messages.Write); got != "" {
		t.Errorf("nil error: got %q, want no class", got)
	}
}

// The class of each declaration on a write, as the taxonomy gives it.
var writeClasses = map[string]struct {
	err   error
	class messages.Class
}{
	"ErrUnauthorized":   {httperr.ErrUnauthorized, messages.Blocker},
	"ErrForbidden":      {httperr.ErrForbidden, messages.Blocker},
	"ErrRefused":        {httperr.ErrRefused, messages.Blocker},
	"ErrNotInPlan":      {httperr.ErrNotInPlan, messages.Blocker},
	"ErrNotFound":       {httperr.ErrNotFound, messages.Warning},
	"ErrConflict":       {httperr.ErrConflict, messages.Warning},
	"ErrInvalid":        {httperr.ErrInvalid, messages.Warning},
	"ErrUnavailable":    {httperr.ErrUnavailable, messages.Warning},
	"ErrBadResponse":    {httperr.ErrBadResponse, messages.Warning},
	"ErrPartial":        {httperr.ErrPartial, messages.Warning},
	"ErrUnknownOutcome": {httperr.ErrUnknownOutcome, messages.ManualTask},
}

// Every declaration in internal/httpx/httperr has a row above, read from the
// package's source, so a new declaration fails here until it gets one rather
// than being classed by the fallback.
func TestClassifyHasARowForEveryDeclaration(t *testing.T) {
	dir := filepath.Join("..", "httpx", "httperr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var declared []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					if strings.HasPrefix(n.Name, "Err") {
						declared = append(declared, n.Name)
					}
				}
			}
		}
	}
	if len(declared) != len(writeClasses) {
		t.Errorf("httperr declares %d errors, the table has %d rows: %v", len(declared), len(writeClasses), declared)
	}
	for _, name := range declared {
		row, ok := writeClasses[name]
		if !ok {
			t.Errorf("httperr.%s has no row", name)
			continue
		}
		if got := messages.Classify(row.err, messages.Write); got != row.class {
			t.Errorf("%s on a write: got %q, want %q", name, got, row.class)
		}
	}
}

func TestExitCodes(t *testing.T) {
	base := errors.New("plan: 1 blocker")
	cases := []struct {
		wrap func(error) error
		code int
	}{
		{messages.Blocked, messages.ExitBlocked},
		{messages.AwaitingInput, messages.ExitAwaitingInput},
		{messages.ValidationFailed, messages.ExitValidationFailed},
		{messages.PartiallyComplete, messages.ExitPartial},
	}
	for _, c := range cases {
		err := fmt.Errorf("apply: %w", c.wrap(base))
		if got, ok := messages.ExitCodeOf(err); !ok || got != c.code {
			t.Errorf("code %d: ExitCodeOf = %d, %v", c.code, got, ok)
		}
		if !errors.Is(err, base) || err.Error() != "apply: plan: 1 blocker" {
			t.Errorf("code %d: the wrapped error must keep its text and identity: %q", c.code, err)
		}
		if c.wrap(nil) != nil {
			t.Errorf("code %d: a nil error must stay nil", c.code)
		}
	}
	if _, ok := messages.ExitCodeOf(fmt.Errorf("keychain: %w", foreignCode{5})); ok {
		t.Errorf("an error with its own ExitCode method chose the exit code")
	}
	codes := map[int]bool{}
	for _, c := range []int{messages.ExitOK, messages.ExitError, messages.ExitUsage, messages.ExitBlocked,
		messages.ExitAwaitingInput, messages.ExitValidationFailed, messages.ExitPartial} {
		if codes[c] {
			t.Errorf("exit code %d is used twice", c)
		}
		codes[c] = true
	}
}

// foreignCode has an ExitCode method, as an *exec.ExitError does.
type foreignCode struct{ code int }

func (e foreignCode) Error() string { return "exit status" }
func (e foreignCode) ExitCode() int { return e.code }

func TestMessageIDAndCheck(t *testing.T) {
	m := messages.Message{Class: messages.Warning, Code: "member_dropped", Item: "teams/P1", Text: "x"}
	if got := m.ID(); got != "teams/P1/member_dropped" {
		t.Errorf("ID = %q", got)
	}
	if err := m.Check(); err != nil {
		t.Errorf("Check: %v", err)
	}
	if got := (messages.Message{Class: messages.Blocker, Code: "pagerduty_incomplete"}).ID(); got != "pagerduty_incomplete" {
		t.Errorf("ID without an item = %q", got)
	}
	for _, bad := range []messages.Message{
		{Class: "error", Code: "x"},
		{Class: messages.Warning, Code: ""},
		{Class: messages.Warning, Code: "Member-Dropped"},
		{Class: messages.Warning, Code: "a/b"},
	} {
		if bad.Check() == nil {
			t.Errorf("Check(%+v) = nil, want an error", bad)
		}
	}
}

func TestCounts(t *testing.T) {
	c := messages.Count([]messages.Message{
		{Class: messages.Warning}, {Class: messages.Blocker}, {Class: messages.Warning}, {Class: messages.Information},
	})
	if want := (messages.Counts{Blockers: 1, Warnings: 2, Information: 1}); c != want {
		t.Errorf("Count = %+v, want %+v", c, want)
	}
	if got, want := c.String(), "1 blocker, 2 warnings, 0 manual tasks, 1 information"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestWriteText(t *testing.T) {
	msgs := []messages.Message{
		{Class: messages.Information, Code: "no_services", Text: "The account has no services."},
		{Class: messages.Warning, Code: "member_dropped", Item: "teams/P1", Text: "Call +1 555 010 0199\nfirst."},
		{Class: messages.Blocker, Code: "pagerduty_incomplete", Text: "The PagerDuty snapshot is incomplete."},
	}
	var b bytes.Buffer
	if err := messages.WriteText(&b, msgs); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"  blocker      pagerduty_incomplete     The PagerDuty snapshot is incomplete.\n" +
		"  warning      teams/P1/member_dropped  Call [redacted] first.\n" +
		"  information  no_services              The account has no services.\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	if strings.Contains(b.String(), "555") {
		t.Errorf("the phone number reached the output")
	}
}

func TestOneLine(t *testing.T) {
	cases := map[string]string{
		"two\nlines\r\n":            "two lines",
		"tab\tand  spaces ":         "tab and spaces",
		"\x1b[2Jcleared\x1b]0;t\a":  "[2Jcleared ]0;t",
		"nul\x00byte":               "nul byte",
		"c1\u0085next":              "c1 next",
		"bad \xff utf-8":            "bad \ufffd utf-8",
		"non\uffffcharacter\ufffe!": "non character !",
	}
	for in, want := range cases {
		if got := messages.OneLine(in); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOneLineRemovesFormatCharacters(t *testing.T) {
	cases := map[string]string{
		// A right-to-left override would show "warning" as "gninraw".
		"plain \u202egninraw\u202c text": "plain gninraw text",
		"zero\u200bwidth":                "zerowidth",
		"\ufeffbyte order mark":          "byte order mark",
	}
	for in, want := range cases {
		if got := messages.OneLine(in); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		// A line break hides the number from the redactor until OneLine
		// joins it, so the line is made first.
		"Call +1 415 555\n0123 now": "Call [redacted] now",
		// Removing the number makes the URL after it match: Clean goes on
		// until nothing more changes.
		"Line +1 415 555 0123https://acme.pagerduty.com/private/feed123/x.ics": "Line [redacted][redacted]",
	}
	for in, want := range cases {
		got := messages.Clean(in)
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
		if again := messages.Clean(got); again != got {
			t.Errorf("Clean is not stable: %q, then %q", got, again)
		}
	}
}

func TestWriteTextKeepsEveryMessage(t *testing.T) {
	msgs := []messages.Message{
		{Class: "error", Code: "x", Text: "an unknown class"},
		{Class: messages.Warning, Code: "bad_id", Item: "teams/P\x1b[2J1", Text: "ok"},
	}
	var b bytes.Buffer
	if err := messages.WriteText(&b, msgs); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"  warning  teams/P [2J1/bad_id  ok\n" +
		"  error    x                    an unknown class\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}
