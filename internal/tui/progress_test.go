package tui

import (
	"bytes"
	"strings"
	"testing"
)

// The bar of a task is redrawn in place, a line printed meanwhile goes above
// it, and the next task starts a line of its own. What is left on the screen
// is one line per task and the printed lines, as LastLines reads it.
func TestProgressRedrawsItsLineInPlace(t *testing.T) {
	t.Setenv("ACCESSIBLE", "")
	var out bytes.Buffer
	p := NewProgress(&out)
	p.Update("teams", 1, 4)
	p.Update("teams", 2, 4)
	p.Println("  Warning  teams/T2/call_failed  The create call failed")
	p.Update("teams", 4, 4)
	p.Update("users", 0, 0)
	p.Close()
	p.Close()

	got := LastLines(out.String(), 10)
	want := []string{
		"  Warning  teams/T2/call_failed  The create call failed",
		"teams                    [##############################] 4/4",
		"users                    [##############################] 0/0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Count(out.String(), "\n") != 3 {
		t.Errorf("%d line ends, want 3: %q", strings.Count(out.String(), "\n"), out.String())
	}
	if bar := LastLines("\r\x1b[2Kteams [#####-----] 1/2", 1); len(bar) != 1 || bar[0] != "teams [#####-----] 1/2" {
		t.Errorf("a half bar: %q", bar)
	}
}

func TestAccessibleProgressUsesPlainLines(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var out bytes.Buffer
	p := NewProgress(&out)
	p.Update("teams", 1, 2)
	p.Println("Warning about a team")
	p.Update("teams", 2, 2)
	p.Update("users", 1, 1)
	p.Close()
	if want := "Warning about a team\nteams: 2/2\nusers: 1/1\n"; out.String() != want {
		t.Fatalf("accessible progress = %q, want %q", out.String(), want)
	}
}
