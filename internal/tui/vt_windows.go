package tui

import (
	"io"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on escape-sequence processing for a Windows
// console, which the full-screen switch needs before bubbletea runs.
func enableVirtualTerminal(out io.Writer) {
	f, ok := out.(interface{ Fd() uintptr })
	if !ok {
		return
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}
