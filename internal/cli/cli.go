package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// Exit codes. ExitAwaitingInput means a credential or an answer is missing:
// the command did what it could without it, and runs fully once it is given.
// The Run engine adds the codes for Blocked, Validation failed and Partially
// complete as error types that implement ExitCoder.
const (
	ExitOK            = 0
	ExitError         = 1
	ExitUsage         = 2
	ExitAwaitingInput = 4
)

// ExitCoder is an error that chooses the process exit code.
type ExitCoder interface {
	error
	ExitCode() int
}

// ErrNotImplemented is returned by command bodies that later tasks replace.
var ErrNotImplemented = errors.New("not implemented yet")

// awaitingInputError exits with ExitAwaitingInput.
type awaitingInputError struct{ err error }

func (e awaitingInputError) Error() string { return e.err.Error() }
func (e awaitingInputError) Unwrap() error { return e.err }
func (e awaitingInputError) ExitCode() int { return ExitAwaitingInput }

func awaitingInput(err error) error { return awaitingInputError{err} }

// Options are the seams cmd/jsm-ops-migrator wires and tests replace.
type Options struct {
	// Keychain is the OS keychain, which holds each workspace's key and
	// credentials. main passes credentials.OSKeychain; nil means there is
	// none, as on a machine without a Secret Service.
	Keychain credentials.Keychain
	// NewKeyProvider overrides how state.db's data key is protected. Without
	// it, a workspace uses the provider recorded in state.db, and a new one
	// uses the keychain when it works and otherwise a passphrase.
	NewKeyProvider func(ctx context.Context, env *Env) (store.KeyProvider, error)
	// NewClients builds the API clients the checks use from the credentials
	// being checked. Nil means preflight.Placeholders.
	NewClients func(env *Env, creds credentials.Set) (preflight.Clients, error)
	// RenderError prints an error that stopped a command. The default writes
	// "Error: <message>" to stderr, so stdout carries only what a command
	// renders.
	RenderError func(stdout, stderr io.Writer, jsonOutput bool, err error)
	// Interrupts delivers Ctrl+C. main passes os.Interrupt; nil disables it.
	Interrupts <-chan os.Signal
	// ForceExit ends the process. It runs on the Ctrl+C that comes after the
	// context was canceled, so a command that does not stop can always be
	// left. main passes a function that exits with code 130.
	ForceExit func()
	// DescribeWorkspace adds what the Run engine knows to the menu's status:
	// the latest plan and apply.
	DescribeWorkspace func(ctx context.Context, ws *store.Workspace, cfg Config, s *MenuState)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Execute runs the CLI with args (without the program name) and returns the
// process exit code.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options) int {
	return execute(ctx, args, stdin, stdout, stderr, opts, defaultBodies())
}

// hooks are the seams tests replace: whether a stream is a terminal, the
// interactive menu and prompts, which need one, and whether the accessible
// mode is on.
type hooks struct {
	isTerminal func(any) bool
	menu       func(ctx context.Context, in io.Reader, out io.Writer, color bool, title string, status []string, items []tui.MenuItem, selected string) (string, error)
	accessible func() bool
	ask        func(ctx context.Context, in io.Reader, out io.Writer, color bool, heading string, qs []tui.Question) ([]string, error)
	choose     func(ctx context.Context, in io.Reader, out io.Writer, color bool, title, description string, options []tui.Option, selected string) (string, error)
}

func defaultHooks() hooks {
	return hooks{isTerminal: isTerminal, menu: tui.Menu, accessible: tui.Accessible, ask: tui.Ask, choose: tui.Choose}
}

func execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options, b bodies) int {
	return executeWith(ctx, args, stdin, stdout, stderr, opts, b, defaultHooks())
}

func executeWith(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options, b bodies, h hooks) int {
	if opts.RenderError == nil {
		opts.RenderError = renderError
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Keychain == nil {
		opts.Keychain = noKeychain{}
	}
	code, r := executeOnce(ctx, args, stdin, stdout, stderr, opts, b, h)
	if !r.wantMenu {
		return code
	}
	return runMenu(ctx, args, r, stdin, stdout, stderr, opts, b, h)
}

// executeOnce runs one command line. Each run has its own context and Ctrl+C
// handling, so stopping a command started from the menu does not end the
// session.
func executeOnce(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, opts Options, b bodies, h hooks) (int, *runner) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ir := newInterrupter(cancel, opts.ForceExit)
	if opts.Interrupts != nil {
		// The watcher runs until the command returns, not until its context
		// is canceled: the Ctrl+C after the cancel must still reach it.
		done := make(chan struct{})
		defer close(done)
		go ir.watch(done, opts.Interrupts)
	}

	r := &runner{opts: opts, bodies: b, hooks: h, stdin: stdin, stdout: stdout, stderr: stderr, interrupts: ir}
	root := r.newRootCommand()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return ExitOK, r
	}
	if !r.started {
		// Anything that fails before a command body starts is a usage error:
		// an unknown command or flag, a bad flag value or an extra argument.
		_, _ = fmt.Fprintf(stderr, "Error: %s\n", credentials.RedactText(err.Error()))
		_, _ = fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return ExitUsage, r
	}
	if !r.rendered {
		opts.RenderError(stdout, stderr, r.global.output == string(OutputJSON), err)
	}
	return exitCode(err), r
}

func renderError(_, stderr io.Writer, _ bool, err error) {
	_, _ = fmt.Fprintf(stderr, "Error: %s\n", credentials.RedactText(err.Error()))
}

func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ec ExitCoder
	if errors.As(err, &ec) {
		if c := ec.ExitCode(); c != ExitOK {
			return c
		}
	}
	// A failure never exits 0, even if an ExitCoder says so.
	return ExitError
}

// interrupter turns Ctrl+C into the steps apply and resume need: the first
// interrupt closes Env.Interrupted so the command can stop after its current
// write, and the second cancels the context, leaving the write in flight for
// resume to look up. Commands without a write to finish cancel at the first
// interrupt. Any interrupt after the cancel calls forceExit, because the
// handler took over Ctrl+C from the OS and must not leave the operator stuck.
type interrupter struct {
	cancel    context.CancelFunc
	forceExit func()
	mu        sync.Mutex
	count     int
	graceful  bool
	ch        chan struct{}
}

func newInterrupter(cancel context.CancelFunc, forceExit func()) *interrupter {
	return &interrupter{cancel: cancel, forceExit: forceExit, ch: make(chan struct{})}
}

func (i *interrupter) setGraceful(g bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.graceful = g
}

func (i *interrupter) interrupt() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.count++
	cancelAt := 1
	if i.graceful {
		cancelAt = 2
	}
	switch {
	case i.count == 1 && i.graceful:
		close(i.ch)
	case i.count == cancelAt:
		if i.count == 1 {
			close(i.ch)
		}
		i.cancel()
	case i.forceExit != nil:
		i.forceExit()
	}
}

func (i *interrupter) watch(done <-chan struct{}, sigs <-chan os.Signal) {
	for {
		select {
		case <-done:
			return
		case _, ok := <-sigs:
			if !ok {
				return
			}
			i.interrupt()
		}
	}
}

// runner holds one Execute call's state, so in-process tests never share a
// logger, a viper instance or flag values.
type runner struct {
	opts       Options
	bodies     bodies
	hooks      hooks
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	interrupts *interrupter
	started    bool
	rendered   bool // the wrapper already printed the command's error
	summary    []string

	// Set when jsm-ops-migrator runs without a command in a terminal.
	wantMenu  bool
	rootFlags *pflag.FlagSet

	global globalFlags
	login  LoginFlags
	plan   PlanFlags
	apply  ApplyFlags
	pause  PauseFlags
	resume ResumeFlags
}

// commandSpec says how the wrapper prepares the workspace for a command.
type commandSpec struct {
	name     string
	create   bool // create the workspace when it is missing
	lock     bool // take the workspace lock for the whole command
	graceful bool // the first Ctrl+C asks the command to stop, the second cancels
	// absent, when set, is printed instead of an error when the workspace
	// does not exist, and the command succeeds without running.
	absent string
}

var (
	specLogin      = commandSpec{name: "login", create: true, lock: true}
	specLoginClear = commandSpec{name: "login", lock: true, absent: "No credentials are stored in %s."}
	specPlan       = commandSpec{name: "plan", create: true, lock: true}
	specApply      = commandSpec{name: "apply", lock: true, graceful: true}
	specPause      = commandSpec{name: "pause"} // never waits for a running apply's lock
	specResume     = commandSpec{name: "resume", lock: true, graceful: true}
)

// wrap prepares the workspace, logger, config and Env, then runs the body.
func (r *runner) wrap(spec commandSpec, body func(context.Context, *Env) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		r.started = true
		r.interrupts.setGraceful(spec.graceful)

		dir, err := r.workspaceDir()
		if err != nil {
			return err
		}
		ws, err := store.OpenWorkspace(dir, spec.create)
		if errors.Is(err, store.ErrWorkspaceNotFound) && spec.absent != "" {
			_, err := fmt.Fprintf(r.stdout, spec.absent+"\n", displayPath(dir))
			return err
		}
		if errors.Is(err, store.ErrWorkspaceNotFound) && !spec.create {
			return fmt.Errorf("%w; run 'jsm-ops-migrator login' first", err)
		}
		if err != nil {
			return err
		}
		if spec.lock {
			if err := ws.Lock(); err != nil {
				return err
			}
			defer func() { _ = ws.Unlock() }()
		}

		// Everything the command writes to stderr, its logs and its final
		// error included, is also saved in the log file.
		logFile := newLogFile(ws, spec.name, r.stderr, r.opts.Now())
		defer func() { _ = logFile.Close() }()
		stderr := io.MultiWriter(r.stderr, logFile)
		logger := newLogger(stderr, parseLevel(r.global.logLevel), r.global.output == string(OutputJSON))
		for _, p := range ws.Tightened() {
			logger.Warn("tightened permissions so only you can read this file", slog.String("path", p))
		}
		renderHere := func(err error) error {
			if err != nil {
				r.opts.RenderError(r.stdout, stderr, r.global.output == string(OutputJSON), err)
				r.rendered = true
			}
			return err
		}

		cfg, err := loadConfig(ws, cmd.Flags())
		if err != nil {
			return renderHere(err)
		}
		env := &Env{
			Command:          spec.name,
			Workspace:        ws,
			Config:           cfg,
			Logger:           logger,
			Interactive:      r.interactive(),
			Output:           OutputFormat(r.global.output),
			Color:            r.color(),
			Stdin:            r.stdin,
			Stdout:           r.stdout,
			Stderr:           stderr,
			Interrupted:      r.interrupts.ch,
			RequestInterrupt: r.interrupts.interrupt,
			Now:              r.opts.Now,
			keychain:         r.opts.Keychain,
			newKeyProvider:   r.opts.NewKeyProvider,
			newClients:       r.opts.NewClients,
			ask:              r.hooks.ask,
			choose:           r.hooks.choose,
			setSummary:       func(lines []string) { r.summary = lines },
		}
		return renderHere(body(cmd.Context(), env))
	}
}

// interactive is false with --non-interactive, or when stdin or stdout is not
// a terminal.
func (r *runner) interactive() bool {
	return !r.global.nonInteractive && r.hooks.isTerminal(r.stdin) && r.hooks.isTerminal(r.stdout)
}

// color is false with --no-color, with --output json, or when stdout is not a
// terminal.
func (r *runner) color() bool {
	return !r.global.noColor && r.global.output != string(OutputJSON) && r.hooks.isTerminal(r.stdout)
}

// workspaceDir is --workspace, then JSM_OPS_MIGRATOR_WORKSPACE, then
// ~/jsm-ops-migrator: one visible folder per user, so customers never have to
// choose one and the review files are easy to find. SEs who run several
// migrations give each its own --workspace.
func (r *runner) workspaceDir() (string, error) {
	if r.global.workspace != "" {
		return r.global.workspace, nil
	}
	if v := os.Getenv(envPrefix + "_WORKSPACE"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home folder for ~/jsm-ops-migrator (use --workspace): %w", err)
	}
	return filepath.Join(home, DefaultWorkspaceName), nil
}

// DefaultWorkspaceName is the folder in the home directory that holds the
// workspace when none is given.
const DefaultWorkspaceName = "jsm-ops-migrator"
