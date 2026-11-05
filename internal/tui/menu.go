package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// MenuItem is one action in the menu.
type MenuItem struct {
	Key         string // returned when the item is chosen, for example "plan"
	Label       string // shown in the list, for example "Plan"
	Description string
}

// QuitKey is what Menu returns when the operator quits.
const QuitKey = "quit"

// Accessible reports whether the ACCESSIBLE environment variable asks for the
// screen-reader mode: numbered prompts that never redraw or clear the screen.
func Accessible() bool { return os.Getenv("ACCESSIBLE") != "" }

// Menu shows a heading, status lines and the actions, and returns the Key of
// the chosen item, or QuitKey when the operator picks Quit or presses q, Esc
// or Ctrl+C. It draws where the cursor is; the caller puts the session on a
// full screen with FullScreen. In accessible mode it asks for a number.
func Menu(ctx context.Context, in io.Reader, out io.Writer, color bool, title string, status []string, items []MenuItem, selected string) (string, error) {
	width := 0
	for _, it := range items {
		width = max(width, len(it.Label))
	}
	options := make([]huh.Option[string], 0, len(items)+1)
	for _, it := range items {
		options = append(options, huh.NewOption(fmt.Sprintf("%-*s  %s", width, it.Label, it.Description), it.Key))
	}
	options = append(options, huh.NewOption("Quit", QuitKey))

	choice := selected
	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc", "q"), key.WithHelp("q", "quit"))
	sel := huh.NewSelect[string]().Options(options...).Value(&choice)
	form := huh.NewForm(huh.NewGroup(sel)).WithInput(in).WithKeyMap(keys)
	if !color {
		form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}

	if Accessible() {
		// Accessible mode reads out neither a select's title nor its
		// description, so they are printed first.
		if _, err := fmt.Fprintln(out, heading(title, color)); err != nil {
			return "", err
		}
		for _, line := range status {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return "", err
			}
		}
		form = form.WithOutput(out).WithAccessible(true)
	} else {
		// A terminal cannot show a larger font, so the heading has a rule
		// under it and a blank line before the status.
		sel.Title(heading(title, false)).Description("\n" + strings.Join(status, "\n") + "\n")
		form = form.WithOutput(filtered(out))
	}

	err := form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) {
		return QuitKey, nil
	}
	if err != nil {
		return "", err
	}
	return choice, nil
}

// heading is the title with a rule under it. In the full-screen menu the
// theme styles it, so bold is added only for the accessible prompt.
func heading(title string, bold bool) string {
	rule := strings.Repeat("─", lipgloss.Width(title))
	if bold {
		title = lipgloss.NewStyle().Bold(true).Render(title)
	}
	return title + "\n" + rule
}

// terminalFile is what bubbletea looks for to treat its output as a terminal:
// with it, bubbletea switches the terminal to raw mode and reads its size.
type terminalFile interface {
	io.ReadWriteCloser
	Fd() uintptr
}

// filteredTerminal keeps the terminal's file for bubbletea and passes every
// write through the filter.
type filteredTerminal struct {
	terminalFile
	filter *keyboardProtocolFilter
}

func (t *filteredTerminal) Write(p []byte) (int, error) { return t.filter.Write(p) }

// filtered wraps out in the keyboard-protocol filter, keeping it recognizable
// as a terminal when it is one; otherwise bubbletea would draw nothing.
func filtered(out io.Writer) io.Writer {
	f := &keyboardProtocolFilter{w: out}
	if tf, ok := out.(terminalFile); ok {
		return &filteredTerminal{terminalFile: tf, filter: f}
	}
	return f
}

// keyboardProtocolFilter drops the sequences that switch the terminal's
// enhanced keyboard reporting on and off: Kitty's push (CSI > n u), pop
// (CSI < n u) and query (CSI ? u), and xterm's modifyOtherKeys (CSI > 4 ; n m).
// It also drops mode queries (CSI ? n $ p). bubbletea always sends them, and
// terminals that do not know them, such as the one in IntelliJ, print a stray
// '<' or '$'. Without an answer, bubbletea assumes the optional modes are off,
// and the menu needs only plain keys.
type keyboardProtocolFilter struct {
	w       io.Writer
	pending []byte // an escape sequence split across writes
}

func (f *keyboardProtocolFilter) Write(p []byte) (int, error) {
	data := append(f.pending, p...)
	f.pending = nil
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] != 0x1b {
			out = append(out, data[i])
			continue
		}
		n, complete := keyboardSequenceLen(data[i:])
		if !complete {
			f.pending = append([]byte(nil), data[i:]...)
			break
		}
		if n > 0 {
			i += n - 1
			continue
		}
		out = append(out, data[i])
	}
	if _, err := f.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// keyboardSequenceLen returns the length of a keyboard-protocol sequence at
// the start of b, 0 if b starts with any other escape, and complete=false if
// b ends before it can tell.
func keyboardSequenceLen(b []byte) (n int, complete bool) {
	if len(b) < 3 {
		return 0, len(b) >= 2 && b[1] != '['
	}
	if b[1] != '[' || (b[2] != '>' && b[2] != '<' && b[2] != '?') {
		return 0, true
	}
	for j := 3; j < len(b); j++ {
		c := b[j]
		switch {
		case c >= '0' && c <= '9', c == ';':
			continue
		case c == 'u':
			return j + 1, true
		case c == '$' && b[2] == '?':
			if j+1 == len(b) {
				return 0, false
			}
			if b[j+1] == 'p' {
				return j + 2, true
			}
			return 0, true
		case c == 'm' && b[2] == '>' && strings.HasPrefix(string(b[3:j]), "4"):
			return j + 1, true
		default:
			return 0, true
		}
	}
	return 0, false
}
