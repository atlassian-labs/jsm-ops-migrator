package tui_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

var items = []tui.MenuItem{
	{Key: "plan", Label: "Plan", Description: "read both systems"},
	{Key: "login", Label: "Login", Description: "store the credentials"},
}

// Accessible mode reads a number per line instead of drawing in the terminal,
// which is how screen-reader users drive it and how it can be tested.
func TestMenuInAccessibleMode(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	for input, want := range map[string]string{"2\n": "login", "1\n": "plan", "3\n": tui.QuitKey} {
		var out bytes.Buffer
		got, err := tui.Menu(context.Background(), strings.NewReader(input), &out, false,
			"JSM Ops Migrator", []string{"Workspace: ~/acme"}, items, "plan")
		if err != nil {
			t.Fatalf("input %q: %v", input, err)
		}
		if got != want {
			t.Errorf("input %q: got %q, want %q", input, got, want)
		}
		for _, s := range []string{"JSM Ops Migrator", "Workspace: ~/acme", "Plan", "Login", "Quit"} {
			if !strings.Contains(out.String(), s) {
				t.Errorf("input %q: output misses %q:\n%s", input, s, out.String())
			}
		}
	}
}
