package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Accessible mode reads one answer per line instead of drawing, which is how
// screen-reader users answer and how prompts can be tested.
func TestAskInAccessibleMode(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var out bytes.Buffer
	answers, err := Ask(context.Background(), strings.NewReader("acme.atlassian.net\n\n"), &out, false, "Log in", []Question{
		{Title: "Atlassian site", Description: "For example acme.atlassian.net."},
		{Title: "Organization ID", Value: "org-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answers[0] != "acme.atlassian.net" || answers[1] != "org-1" {
		t.Errorf("answers %q: an empty line must keep the starting value", answers)
	}
	for _, s := range []string{"Log in", "Atlassian site", "Organization ID"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output misses %q:\n%s", s, out.String())
		}
	}
}

func TestAskValidatesInAccessibleMode(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var out bytes.Buffer
	answers, err := Ask(context.Background(), strings.NewReader("bad\ngood\n"), &out, false, "", []Question{{
		Title: "Email",
		Validate: func(v string) error {
			if v != "good" {
				return errors.New("not good")
			}
			return nil
		},
	}})
	if err != nil || answers[0] != "good" || !strings.Contains(out.String(), "not good") {
		t.Errorf("answers %q, err %v, output %q", answers, err, out.String())
	}
}

func TestChooseInAccessibleMode(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var out bytes.Buffer
	got, err := Choose(context.Background(), strings.NewReader("2\n"), &out, false, "JSM plan", "No API reports it.",
		[]Option{{Key: "free", Label: "Free"}, {Key: "premium", Label: "Premium"}}, "")
	if err != nil || got != "premium" {
		t.Errorf("got %q, %v", got, err)
	}
}

// A prompt shown by a command that the menu runs draws on the terminal, not
// into the copy the menu keeps for its result.
func TestPromptsBypassTheMenusCopy(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var copied bytes.Buffer
	if _, err := Ask(context.Background(), strings.NewReader("x\n"), Tee(f, &copied), false, "Log in", []Question{{Title: "Site"}}); err != nil {
		t.Fatal(err)
	}
	if copied.Len() != 0 {
		t.Errorf("the menu's copy got the prompt: %q", copied.String())
	}
	if data, _ := os.ReadFile(f.Name()); !strings.Contains(string(data), "Site") {
		t.Errorf("the terminal did not get the prompt: %q", data)
	}
}
