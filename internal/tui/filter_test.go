package tui

import (
	"bytes"
	"os"
	"testing"
)

func TestKeyboardProtocolFilter(t *testing.T) {
	in := "\x1b[>4;2m\x1b[>1u\x1b[?u\x1b[?25l┃ Plan\x1b[1mbold\x1b[0m\x1b[?2026$p\x1b[<1u\x1b[>4m\x1b[?25h"
	want := "\x1b[?25l┃ Plan\x1b[1mbold\x1b[0m\x1b[?25h"
	// Every split point, as bubbletea may flush a sequence in two writes.
	for cut := 0; cut <= len(in); cut++ {
		var out bytes.Buffer
		f := &keyboardProtocolFilter{w: &out}
		if _, err := f.Write([]byte(in[:cut])); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(in[cut:])); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("split at %d:\n got  %q\n want %q", cut, out.String(), want)
		}
	}
}

func TestFilteredKeepsTheTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, ok := filtered(f).(terminalFile); !ok {
		t.Error("a file must stay a terminalFile, or bubbletea draws nothing")
	}
	if _, ok := filtered(&bytes.Buffer{}).(terminalFile); ok {
		t.Error("a buffer must not pretend to be a terminal")
	}
	if _, err := filtered(f).Write([]byte("a\x1b[<1ub")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.Name())
	if string(data) != "ab" {
		t.Errorf("written %q, want %q", data, "ab")
	}
}

func TestFilterDropsModeQueries(t *testing.T) {
	var out bytes.Buffer
	f := &keyboardProtocolFilter{w: &out}
	_, _ = f.Write([]byte("a\x1b[?2026$pb\x1b[?2027$"))
	_, _ = f.Write([]byte("pc\x1b[?25hd"))
	if got := out.String(); got != "abc\x1b[?25hd" {
		t.Errorf("got %q", got)
	}
}

func TestTeeKeepsTheTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var copied bytes.Buffer
	w := Tee(f, &copied)
	if _, ok := w.(interface{ Fd() uintptr }); !ok {
		t.Error("a tee of a terminal must keep its file, or commands lose their prompts")
	}
	_, _ = w.Write([]byte("hello"))
	if copied.String() != "hello" {
		t.Errorf("copy = %q", copied.String())
	}
	if _, ok := Tee(&bytes.Buffer{}, &copied).(interface{ Fd() uintptr }); ok {
		t.Error("a tee of a buffer must not pretend to be a terminal")
	}
}

func TestLastLines(t *testing.T) {
	in := "\x1b[1mfirst\x1b[0m\n\nReading  10%\rReading 100%\nError: plan: not implemented yet\n"
	got := LastLines(in, 2)
	if len(got) != 2 || got[0] != "Reading 100%" || got[1] != "Error: plan: not implemented yet" {
		t.Errorf("got %q", got)
	}
	if got := LastLines(in, 10); len(got) != 3 || got[0] != "first" {
		t.Errorf("got %q", got)
	}
}

func TestFullScreenSequences(t *testing.T) {
	var out bytes.Buffer
	restore := FullScreen(&out)
	ClearScreen(&out)
	restore()
	if got := out.String(); got != "\x1b[?1049h\x1b[H\x1b[2J\x1b[H\x1b[2J\x1b[?1049l" {
		t.Errorf("got %q", got)
	}
}
