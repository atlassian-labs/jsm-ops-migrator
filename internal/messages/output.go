package messages

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Counts is how many messages a plan or run has of each class.
type Counts struct {
	Blockers    int `json:"blockers"`
	Warnings    int `json:"warnings"`
	ManualTasks int `json:"manual_tasks"`
	Information int `json:"information"`
}

// Count counts msgs by class.
func Count(msgs []Message) Counts {
	var c Counts
	for _, m := range msgs {
		switch m.Class {
		case Blocker:
			c.Blockers++
		case Warning:
			c.Warnings++
		case ManualTask:
			c.ManualTasks++
		case Information:
			c.Information++
		}
	}
	return c
}

// Of returns the count of one class.
func (c Counts) Of(class Class) int {
	switch class {
	case Blocker:
		return c.Blockers
	case Warning:
		return c.Warnings
	case ManualTask:
		return c.ManualTasks
	case Information:
		return c.Information
	}
	return 0
}

// String gives every count in words, the most urgent first: "1 blocker, 2
// warnings, 0 manual tasks, 0 information".
func (c Counts) String() string {
	parts := make([]string, len(Classes))
	for i, class := range Classes {
		parts[i] = Quantity(c.Of(class), class)
	}
	return strings.Join(parts, ", ")
}

// Quantity is a count with its class in words: "1 blocker", "2 warnings".
func Quantity(n int, class Class) string {
	if n == 1 {
		return "1 " + class.Label()
	}
	return fmt.Sprintf("%d %s", n, class.Plural())
}

// ByClass returns msgs ordered by class, the most urgent first, keeping their
// order within each class. Messages of an unknown class come last, so none
// is lost.
func ByClass(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, class := range Classes {
		for _, m := range msgs {
			if m.Class == class {
				out = append(out, m)
			}
		}
	}
	for _, m := range msgs {
		if !m.Class.Known() {
			out = append(out, m)
		}
	}
	return out
}

// WriteText prints msgs one per line, the most urgent first: the class in
// words, so a screen reader reads it out, then the ID and the text, all
// cleaned (see Clean).
func WriteText(w io.Writer, msgs []Message) error {
	type line struct{ class, id, text string }
	lines := make([]line, 0, len(msgs))
	classWidth, idWidth := 0, 0
	for _, m := range ByClass(msgs) {
		l := line{m.Class.Label(), Clean(m.ID()), Clean(m.Text)}
		classWidth = max(classWidth, len(l.class))
		idWidth = max(idWidth, len(l.id))
		lines = append(lines, l)
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "  %-*s  %-*s  %s\n", classWidth, l.class, idWidth, l.id, l.text)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// OneLine makes s fit on one line of a terminal or a review file: control
// characters become spaces and runs of spaces become one. A line break could
// otherwise make the next lines look like messages of their own, and an
// escape sequence would reach the terminal of whoever prints the text.
// Invisible format characters, such as the ones that reverse the direction
// of the text after them, are removed, so the text reads as it is. Invalid
// UTF-8 becomes U+FFFD, and U+FFFE and U+FFFF become spaces, because YAML
// files refuse them.
func OneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cf, r):
			return -1
		case unicode.IsControl(r) || r == 0xFFFE || r == 0xFFFF:
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
