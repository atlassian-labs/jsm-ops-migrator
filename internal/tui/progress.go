package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Progress draws a line per task in a terminal, with a bar that is redrawn
// in place as the task goes, and prints other lines above the one being
// drawn. It keeps the terminal as it is, so Ctrl+C still reaches the command
// as a signal. It is safe for use from more than one goroutine.
type Progress struct {
	mu    sync.Mutex
	out   io.Writer
	plain bool
	label string // the task whose line is being drawn, or ""
	last  [2]int // its done and total
}

// NewProgress starts drawing on out, which is a terminal.
func NewProgress(out io.Writer) *Progress {
	if Accessible() {
		return &Progress{out: out, plain: true}
	}
	enableVirtualTerminal(screen(out))
	return &Progress{out: out}
}

// barWidth and labelWidth keep the line within 80 columns for a step name
// of up to labelWidth characters.
const (
	barWidth   = 30
	labelWidth = 24
)

// Update redraws the line of the task label: done of total. A task other
// than the one being drawn ends that one's line first.
func (p *Progress) Update(label string, done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plain {
		// Announce a completed step once. Redrawing each item would
		// repeatedly interrupt a screen reader.
		if done == total {
			_, _ = fmt.Fprintf(p.out, "%s: %d/%d\n", label, done, total)
		}
		return
	}
	if p.label != "" && p.label != label {
		_, _ = io.WriteString(p.out, "\n")
	}
	p.label = label
	p.draw(done, total)
}

func (p *Progress) draw(done, total int) {
	filled := barWidth
	if total > 0 {
		filled = min(barWidth, barWidth*done/total)
	}
	_, _ = fmt.Fprintf(p.out, "\r\x1b[2K%-*s [%s%s] %d/%d", labelWidth, p.label,
		strings.Repeat("#", filled), strings.Repeat("-", barWidth-filled), done, total)
	p.last = [2]int{done, total}
}

// Println prints line above the line being drawn, which is drawn again
// under it.
func (p *Progress) Println(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.label == "" {
		_, _ = io.WriteString(p.out, line+"\n")
		return
	}
	_, _ = io.WriteString(p.out, "\r\x1b[2K"+line+"\n")
	p.draw(p.last[0], p.last[1])
}

// Close ends the line being drawn, so what is printed next starts on a line
// of its own.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.label != "" {
		_, _ = io.WriteString(p.out, "\n")
		p.label = ""
	}
}
