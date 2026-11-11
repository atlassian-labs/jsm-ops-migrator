package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
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
	// Now is the clock.
	Now func() time.Time

	// given are the settings set for this run with a flag or an environment
	// variable.
	given map[string]bool

	keychain       credentials.Keychain
	newKeyProvider func(context.Context, *Env) (store.KeyProvider, error)
	newClients     func(*Env, credentials.Set) (preflight.Clients, error)
	ask            func(ctx context.Context, in io.Reader, out io.Writer, color bool, heading string, qs []tui.Question) ([]string, error)
	choose         func(ctx context.Context, in io.Reader, out io.Writer, color bool, title, description string, options []tui.Option, selected string) (string, error)
	setSummary     func([]string)

	keyOnce      sync.Once
	key          store.KeyProvider
	keyErr       error
	passPrompted bool // the passphrase came from a prompt, so a wrong one can be asked again
}

// maxPassphraseTries is how often an interactive command asks again after a
// wrong passphrase.
const maxPassphraseTries = 3

// OpenStore opens state.db with the workspace's key provider. The provider is
// built once per command, so a passphrase is asked for only once.
func (e *Env) OpenStore(ctx context.Context, mode store.OpenMode) (*store.Store, error) {
	e.keyOnce.Do(func() {
		if e.newKeyProvider != nil {
			e.key, e.keyErr = e.newKeyProvider(ctx, e)
			return
		}
		e.key, e.keyErr = e.defaultKeyProvider(ctx)
	})
	if e.keyErr != nil {
		return nil, e.keyErr
	}
	for try := 1; ; try++ {
		st, err := store.Open(ctx, e.Workspace, e.key, mode)
		// The passphrase provider forgets a wrong passphrase, so opening
		// again asks for it again.
		if errors.Is(err, credentials.ErrWrongPassphrase) && e.passphrasePrompted() && try < maxPassphraseTries {
			_, _ = io.WriteString(e.Stderr, "The passphrase is wrong. Try again.\n")
			continue
		}
		return st, err
	}
}

// Ask shows questions in the terminal. Callers check Interactive first.
func (e *Env) Ask(ctx context.Context, heading string, qs []tui.Question) ([]string, error) {
	if !e.Interactive || e.ask == nil {
		return nil, errors.New("cannot ask: not running in a terminal")
	}
	return e.ask(ctx, e.Stdin, e.Stdout, e.Color, heading, qs)
}

// Choose asks for one of the options in the terminal. Callers check
// Interactive first.
func (e *Env) Choose(ctx context.Context, title, description string, options []tui.Option, selected string) (string, error) {
	if !e.Interactive || e.choose == nil {
		return "", errors.New("cannot ask: not running in a terminal")
	}
	return e.choose(ctx, e.Stdin, e.Stdout, e.Color, title, description, options, selected)
}

// Clients builds the API clients for the given credentials.
func (e *Env) Clients(creds credentials.Set) (preflight.Clients, error) {
	if e.newClients == nil {
		return preflight.Placeholders(), nil
	}
	return e.newClients(e, creds)
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

// summarize gives the menu a short result to show instead of the command's
// output, which was written for a terminal of its own. A command calls it
// once it has nothing left that could fail.
func (e *Env) summarize(lines []string) {
	if e.setSummary != nil {
		e.setSummary(lines)
	}
}
