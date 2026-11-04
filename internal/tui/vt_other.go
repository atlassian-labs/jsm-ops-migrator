//go:build !windows

package tui

import "io"

// enableVirtualTerminal is needed only on Windows; other terminals always
// process escape sequences.
func enableVirtualTerminal(io.Writer) {}
