package tui

import (
	"io"
	"regexp"
	"strings"
)

// FullScreen switches out to the terminal's alternate screen, as full-screen
// programs such as vim do, and returns a function that switches back.
// Everything drawn in between, the menu and the commands it runs, disappears
// when it returns, so the operator's terminal is left exactly as it was.
func FullScreen(out io.Writer) (restore func()) {
	enableVirtualTerminal(out)
	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[H\x1b[2J")
	return func() { _, _ = io.WriteString(out, "\x1b[?1049l") }
}

// ClearScreen clears the screen and moves the cursor to the top left.
func ClearScreen(out io.Writer) { _, _ = io.WriteString(out, "\x1b[H\x1b[2J") }

// Tee writes everything to out and to copy. When out is a terminal it stays
// recognizable as one, so a command run from the menu can still prompt and
// draw progress while its output is captured.
func Tee(out, copy io.Writer) io.Writer {
	w := io.MultiWriter(out, copy)
	if tf, ok := out.(terminalFile); ok {
		return &teeTerminal{terminalFile: tf, w: w}
	}
	return w
}

type teeTerminal struct {
	terminalFile
	w io.Writer
}

func (t *teeTerminal) Write(p []byte) (int, error) { return t.w.Write(p) }

var escapeRe = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]`)

// LastLines returns up to n non-empty lines of captured terminal output, with
// escape sequences removed and a line redrawn with '\r' reduced to its final
// form, as a progress bar would be.
func LastLines(captured string, n int) []string {
	var lines []string
	for _, line := range strings.Split(escapeRe.ReplaceAllString(captured, ""), "\n") {
		if i := strings.LastIndex(strings.TrimRight(line, "\r"), "\r"); i >= 0 {
			line = line[i+1:]
		}
		if line = strings.TrimRight(line, " \r\t"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
