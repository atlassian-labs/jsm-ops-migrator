package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/mattn/go-isatty"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// OutputFormat is the --output value.
type OutputFormat string

const (
	OutputText OutputFormat = "text"
	OutputJSON OutputFormat = "json"
)

// Env is what a command body gets: the opened and, for most commands, locked
// workspace, the resolved config, a redacting logger and the terminal facts.
type Env struct {
	Command   string
	Workspace *store.Workspace
	Config    Config
	Logger    *slog.Logger

	// Interactive is false with --non-interactive, or when stdin or stdout is
	// not a terminal.
	Interactive bool
	Output      OutputFormat
	// Color is false with --no-color, with --output json, or when stdout is
	// not a terminal.
	Color bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Interrupted is closed at the first Ctrl+C. apply and resume stop after
	// their current write; the second Ctrl+C cancels the context.
	Interrupted <-chan struct{}
	// RequestInterrupt does what a Ctrl+C signal does. A terminal UI in raw
	// mode, which receives Ctrl+C as a key, calls it.
	RequestInterrupt func()

	newKeyProvider func(*Env) (store.KeyProvider, error)
	keyOnce        sync.Once
	key            store.KeyProvider
	keyErr         error
}

// OpenStore opens state.db with the configured key provider. The provider is
// built once per command, so a passphrase provider asks only once.
func (e *Env) OpenStore(ctx context.Context, mode store.OpenMode) (*store.Store, error) {
	e.keyOnce.Do(func() {
		if e.newKeyProvider == nil {
			e.keyErr = errors.New("no key provider configured")
			return
		}
		e.key, e.keyErr = e.newKeyProvider(e)
	})
	if e.keyErr != nil {
		return nil, e.keyErr
	}
	return store.Open(ctx, e.Workspace, e.key, mode)
}

// isTerminal checks the file behind a stream. A stream without one, such as a
// test buffer, is not a terminal; the menu's capturing tee keeps the file of
// the terminal it writes to.
func isTerminal(x any) bool {
	f, ok := x.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
