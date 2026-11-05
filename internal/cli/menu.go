package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

const menuTitle = "PagerDuty Migration CLI"

var menuItems = []tui.MenuItem{
	{Key: "plan", Label: "Plan", Description: "read PagerDuty and JSM and build a new plan"},
	{Key: "apply", Label: "Apply", Description: "write the confirmed plan to JSM Operations"},
	{Key: "resume", Label: "Resume", Description: "continue an apply that was paused or interrupted"},
	{Key: "pause", Label: "Pause", Description: "stop an apply running in another window after its current write"},
	{Key: "login", Label: "Login", Description: "store or change the credentials, site and organization"},
}

// MenuState is what the menu shows about the workspace. Foundation fills Dir,
// Exists, Site and OrgID. Login and pre-flight sets CredentialsStored, and the
// Run engine sets Plan and Run, through Options.DescribeWorkspace.
type MenuState struct {
	Dir               string
	Exists            bool
	Site, OrgID       string
	CredentialsStored *bool // nil until Login and pre-flight can tell
	Plan              *PlanStatus
	Run               *RunStatus
	Problem           string // set when the workspace cannot be read
}

// PlanStatus describes the latest plan version.
type PlanStatus struct {
	Version, Changes, Blockers, Warnings int
	Dir                                  string // its review files
}

// RunState is the state of the latest apply.
type RunState string

const (
	RunRunning     RunState = "running"
	RunPaused      RunState = "paused"
	RunInterrupted RunState = "interrupted"
	RunCompleted   RunState = "completed"
)

// RunStatus describes the latest apply.
type RunStatus struct {
	PlanVersion            int
	State                  RunState
	Written, Total, Failed int
	Record                 string // the run record file
}

// runMenu is what jsm-ops-migrator does without a command in a terminal. The
// whole session runs on the terminal's full screen: the menu, then the chosen
// action exactly as if it had been typed with the same global flags, then the
// menu again with that action's result in it, until the operator quits. On
// quit the terminal is left as it was.
func runMenu(ctx context.Context, args []string, r *runner, stdin io.Reader, stdout, stderr io.Writer, opts Options, b bodies, h hooks) int {
	accessible := h.accessible()
	if !accessible {
		restore := tui.FullScreen(stdout)
		defer restore()
		// A forced exit skips deferred calls, so it restores the screen first.
		if force := opts.ForceExit; force != nil {
			opts.ForceExit = func() { restore(); force() }
		}
	}
	var last []string
	for {
		status, selected := describeMenu(r.menuState(ctx))
		if len(last) > 0 {
			status = append(append(status, ""), last...)
		}
		if !accessible {
			tui.ClearScreen(stdout)
		}
		choice, err := h.menu(ctx, stdin, stdout, r.color(), menuTitle, status, menuItems, selected)
		if err != nil {
			opts.RenderError(stdout, stderr, false, err)
			return ExitError
		}
		if choice == tui.QuitKey || ctx.Err() != nil {
			return ExitOK
		}
		drainInterrupts(opts.Interrupts)
		// The command draws on the screen as usual, so it can still prompt
		// and show progress; a copy of its output becomes the menu's result.
		var captured bytes.Buffer
		code, _ := executeOnce(ctx, append(slices.Clone(args), choice), stdin,
			tui.Tee(stdout, &captured), tui.Tee(stderr, &captured), opts, b, h)
		last = lastResult(choice, code, captured.String())
	}
}

// lastResult is the block the menu shows about the action that just ran.
func lastResult(choice string, code int, output string) []string {
	head := "Last: " + menuLabel(choice) + " finished"
	if code != ExitOK {
		head = "Last: " + menuLabel(choice) + " failed"
	}
	lines := []string{head}
	for _, line := range tui.LastLines(output, maxResultLines) {
		lines = append(lines, "  "+line)
	}
	return lines
}

// maxResultLines keeps the menu on one screen whatever a command printed;
// everything it printed is also in its log file.
const maxResultLines = 8

func menuLabel(key string) string {
	for _, it := range menuItems {
		if it.Key == key {
			return it.Label
		}
	}
	return key
}

// drainInterrupts drops a Ctrl+C that arrived while no command was running, so
// it cannot stop the next command the moment it starts.
func drainInterrupts(ch <-chan os.Signal) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// menuState reads the workspace without creating or locking anything.
func (r *runner) menuState(ctx context.Context) MenuState {
	dir, err := r.workspaceDir()
	if err != nil {
		return MenuState{Problem: credentials.RedactText(err.Error())}
	}
	s := MenuState{Dir: dir}
	ws, err := store.OpenWorkspace(dir, false)
	switch {
	case errors.Is(err, store.ErrWorkspaceNotFound):
		return s
	case err != nil:
		s.Problem = credentials.RedactText(err.Error())
		return s
	}
	s.Exists = true
	cfg, err := loadConfig(ws, r.rootFlags)
	if err != nil {
		s.Problem = credentials.RedactText(err.Error())
		return s
	}
	s.Site, s.OrgID = cfg.Site, cfg.OrgID
	if r.opts.DescribeWorkspace != nil {
		r.opts.DescribeWorkspace(ctx, ws, cfg, &s)
	}
	return s
}

// describeMenu turns the state into the lines above the menu and the action
// the cursor starts on: always the next step.
func describeMenu(s MenuState) (lines []string, selected string) {
	if s.Dir != "" {
		lines = append(lines, "Your files: "+displayPath(s.Dir))
	}
	switch {
	case s.Problem != "":
		return append(lines, s.Problem), "login"
	case !s.Exists:
		return append(lines, "Nothing set up yet: choose Login to start"), "login"
	case s.Site == "":
		return append(lines, "Not logged in yet: choose Login to start"), "login"
	}
	login := "Logged in to " + s.Site
	if s.OrgID != "" {
		login += ", organization " + s.OrgID
	}
	lines = append(lines, login)

	p, run := s.Plan, s.Run
	switch {
	case s.CredentialsStored != nil && !*s.CredentialsStored:
		return append(lines, "Your credentials are missing: choose Login to store them again"), "login"
	case run != nil && run.State == RunRunning:
		return append(lines,
			fmt.Sprintf("Apply of plan v%d is running in another window: %d of %d written", run.PlanVersion, run.Written, run.Total),
			"Choose Pause to stop it after its current write"), "pause"
	case run != nil && (run.State == RunPaused || run.State == RunInterrupted):
		return append(lines,
			fmt.Sprintf("Apply of plan v%d was %s after %d of %d writes", run.PlanVersion, run.State, run.Written, run.Total),
			"Choose Resume to continue where it stopped"), "resume"
	case p == nil:
		return append(lines, "Next: choose Plan to read PagerDuty and JSM"), "plan"
	case p.Blockers > 0:
		return append(lines,
			fmt.Sprintf("Plan v%d has %s to fix first", p.Version, count(p.Blockers, "blocker")),
			"See "+displayPath(filepath.Join(p.Dir, "risk-queue.md"))+", then choose Plan again"), "plan"
	case run != nil && run.State == RunCompleted && run.PlanVersion == p.Version:
		return append(lines,
			fmt.Sprintf("Plan v%d applied: %d written, %d failed", p.Version, run.Written-run.Failed, run.Failed),
			"See "+displayPath(run.Record)), "plan"
	default:
		return append(lines,
			fmt.Sprintf("Plan v%d is ready: %s, %s to accept", p.Version, count(p.Changes, "change"), count(p.Warnings, "warning")),
			"Review "+displayPath(filepath.Join(p.Dir, "plan.md"))+", then choose Apply"), "apply"
	}
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// displayPath shows a path as absolute and clean, with the home directory
// shortened to ~.
func displayPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Join("~", rel)
	}
	return path
}
