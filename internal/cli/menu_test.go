package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// scriptedMenu plays the operator: it returns the choices in order and keeps
// what the menu was shown each time.
type scriptedMenu struct {
	choices  []string
	shown    [][]string
	selected []string
}

func (m *scriptedMenu) menu(_ context.Context, _ io.Reader, _ io.Writer, _ bool, title string, status []string, items []tui.MenuItem, selected string) (string, error) {
	m.shown = append(m.shown, status)
	m.selected = append(m.selected, selected)
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if title != "JSM Ops Migrator" || strings.Join(keys, ",") != "plan,apply,resume,pause,login" {
		return "", errors.New("unexpected menu: " + title + " " + strings.Join(keys, ","))
	}
	if len(m.choices) == 0 {
		return tui.QuitKey, nil
	}
	c := m.choices[0]
	m.choices = m.choices[1:]
	return c, nil
}

func runMenuSession(t *testing.T, b bodies, opts Options, m *scriptedMenu, args ...string) result {
	t.Helper()
	return runMenuSessionWith(t, b, opts, m, false, args...)
}

func runMenuSessionWith(t *testing.T, b bodies, opts Options, m *scriptedMenu, accessible bool, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.NewKeyProvider = func(context.Context, *Env) (store.KeyProvider, error) { return testkit.NewMemoryKeyProvider(), nil }
	if opts.Keychain == nil {
		opts.Keychain = testkit.NewFakeKeychain()
	}
	h := hooks{isTerminal: func(any) bool { return true }, menu: m.menu, accessible: func() bool { return accessible }}
	code := executeWith(context.Background(), args, strings.NewReader(""), &out, &errOut, opts, b, h)
	return result{code, out.String(), errOut.String()}
}

const enterFullScreen, leaveFullScreen = "\x1b[?1049h", "\x1b[?1049l"

func TestMenuShowsTheLastResult(t *testing.T) {
	inTempDir(t)
	b := stubBodies()
	b.login = func(_ context.Context, env *Env, _ LoginFlags) error {
		_, _ = io.WriteString(env.Stdout, "Stored the credentials\n")
		return nil
	}
	m := &scriptedMenu{choices: []string{"plan", "login"}}
	r := runMenuSession(t, b, Options{}, m)
	if r.code != ExitOK || len(m.shown) != 3 {
		t.Fatalf("exit %d, menu shown %d times", r.code, len(m.shown))
	}
	if got := strings.Join(m.shown[0], "\n"); strings.Contains(got, "Last:") {
		t.Errorf("no result before the first action: %q", got)
	}
	if got := strings.Join(m.shown[1], "\n"); !strings.Contains(got, "\n\nLast: Plan failed\n  Error: plan: not implemented yet") {
		t.Errorf("after plan: %q", got)
	}
	if got := strings.Join(m.shown[2], "\n"); !strings.Contains(got, "Last: Login finished\n  Stored the credentials") || strings.Contains(got, "Plan failed") {
		t.Errorf("after login: %q", got)
	}
}

// A command's summary replaces its output, which was written for a terminal
// of its own.
func TestMenuShowsTheSummaryInsteadOfTheOutput(t *testing.T) {
	inTempDir(t)
	b := stubBodies()
	b.login = func(_ context.Context, env *Env, _ LoginFlags) error {
		_, _ = io.WriteString(env.Stdout, "Login checks\n  a long table\n")
		env.summarize([]string{"All checks passed", "Stored the PagerDuty API key"})
		return nil
	}
	m := &scriptedMenu{choices: []string{"login"}}
	if r := runMenuSession(t, b, Options{}, m); r.code != ExitOK || len(m.shown) != 2 {
		t.Fatalf("exit %d, menu shown %d times", r.code, len(m.shown))
	}
	got := strings.Join(m.shown[1], "\n")
	if !strings.HasSuffix(got, "\n\nLast: Login finished\n  All checks passed\n  Stored the PagerDuty API key") || strings.Contains(got, "a long table") {
		t.Errorf("after login: %q", got)
	}
}

func TestMenuSessionUsesTheFullScreen(t *testing.T) {
	inTempDir(t)
	m := &scriptedMenu{choices: []string{"plan"}}
	r := runMenuSession(t, stubBodies(), Options{}, m)
	enter, leave := strings.Index(r.stdout, enterFullScreen), strings.LastIndex(r.stdout, leaveFullScreen)
	if enter != 0 || leave < 0 || leave != len(r.stdout)-len(leaveFullScreen) {
		t.Errorf("the session must start and end the full screen: %q", r.stdout)
	}
	if strings.Count(r.stdout, enterFullScreen) != 1 {
		t.Errorf("one full screen for the whole session: %q", r.stdout)
	}
	accessible := runMenuSessionWith(t, stubBodies(), Options{}, &scriptedMenu{choices: []string{"plan"}}, true)
	if strings.Contains(accessible.stdout, enterFullScreen) || strings.Contains(accessible.stdout, "\x1b[2J") {
		t.Errorf("accessible mode must never switch or clear the screen: %q", accessible.stdout)
	}
}

// lockedBuffer is safe to write from the command and read from the Ctrl+C
// watcher at the same time, as a terminal is.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestForcedExitRestoresTheScreen(t *testing.T) {
	inTempDir(t)
	var out, errOut lockedBuffer
	sigs := make(chan os.Signal, 2)
	var restoredFirst atomic.Bool
	opts := Options{
		Interrupts:     sigs,
		NewKeyProvider: func(context.Context, *Env) (store.KeyProvider, error) { return testkit.NewMemoryKeyProvider(), nil },
		ForceExit:      func() { restoredFirst.Store(strings.HasSuffix(out.String(), leaveFullScreen)) },
	}
	b := stubBodies()
	b.plan = func(ctx context.Context, _ *Env, _ PlanFlags) error {
		sigs <- os.Interrupt // cancels plan
		<-ctx.Done()
		sigs <- os.Interrupt // the operator insists
		time.Sleep(100 * time.Millisecond)
		return ctx.Err()
	}
	m := &scriptedMenu{choices: []string{"plan"}}
	h := hooks{isTerminal: func(any) bool { return true }, menu: m.menu, accessible: func() bool { return false }}
	executeWith(context.Background(), nil, strings.NewReader(""), &out, &errOut, opts, b, h)
	if !restoredFirst.Load() {
		t.Error("a forced exit must leave the full screen before exiting")
	}
}

func TestMenuOnlyInATerminal(t *testing.T) {
	inTempDir(t)
	for _, args := range [][]string{{"--non-interactive"}, {"--output", "json"}} {
		m := &scriptedMenu{}
		r := runMenuSession(t, stubBodies(), Options{}, m, args...)
		if r.code != ExitOK || len(m.shown) != 0 || !strings.Contains(r.stdout, "Available Commands") {
			t.Errorf("%v: exit %d, menu shown %d times, stdout %q", args, r.code, len(m.shown), r.stdout)
		}
	}
	if r := run(t, stubBodies(), Options{}); !strings.Contains(r.stdout, "Available Commands") {
		t.Errorf("without a terminal jsm-ops-migrator must print the help: %q", r.stdout)
	}
}

// withCredentialsInEnv sets every credential in the environment, as a script
// would, so the menu counts them as available.
func withCredentialsInEnv(t *testing.T) {
	t.Helper()
	for _, k := range credentials.Kinds {
		t.Setenv(k.EnvVar(), "env-value-"+string(k))
	}
}

func TestMenuRunsChosenCommandsUntilQuit(t *testing.T) {
	dir := filepath.Join(inTempDir(t), "migration")
	withCredentialsInEnv(t)
	var ran []string
	b := stubBodies()
	b.login = func(_ context.Context, env *Env, _ LoginFlags) error {
		ran = append(ran, "login:"+env.Workspace.Dir())
		return SaveConfig(env.Workspace, Config{Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1"})
	}
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error {
		ran = append(ran, "plan:"+env.Config.Site)
		return errors.New("plan failed")
	}
	m := &scriptedMenu{choices: []string{"login", "plan", "plan"}}
	r := runMenuSession(t, b, Options{}, m, "--workspace", dir)

	if r.code != ExitOK {
		t.Fatalf("quitting the menu must exit 0, got %d (stderr %q)", r.code, r.stderr)
	}
	if want := "login:" + dir + ",plan:acme.atlassian.net,plan:acme.atlassian.net"; strings.Join(ran, ",") != want {
		t.Errorf("ran %v", ran)
	}
	if strings.Count(r.stderr, "Error: plan failed") != 2 {
		t.Errorf("each failure must be shown and the menu must come back: %q", r.stderr)
	}
	if len(m.shown) != 4 {
		t.Fatalf("menu shown %d times, want 4", len(m.shown))
	}
	if !strings.Contains(strings.Join(m.shown[0], "\n"), "Nothing set up yet") || m.selected[0] != "login" {
		t.Errorf("first status %v, selected %q", m.shown[0], m.selected[0])
	}
	if got := strings.Join(m.shown[1], "\n"); !strings.Contains(got, "Logged in to acme.atlassian.net, organization org-1") || m.selected[1] != "plan" {
		t.Errorf("status after login %q, selected %q", got, m.selected[1])
	}
}

// Without the credentials the header shows the site, not "Logged in", and
// nothing else: the cursor on Login says what to do next.
func TestMenuWithoutCredentialsShowsOnlyTheSite(t *testing.T) {
	no := false
	lines, selected := describeMenu(MenuState{Dir: "/w", Exists: true, Site: "acme.atlassian.net", OrgID: "o1", CredentialsStored: &no})
	if len(lines) != 2 || lines[1] != "Site acme.atlassian.net, organization o1" || selected != "login" {
		t.Errorf("lines %q, selected %q", lines, selected)
	}
}

func TestMenuStatusAtEveryStep(t *testing.T) {
	no, yes := false, true
	base := MenuState{Dir: "/w", Exists: true, Site: "acme.atlassian.net", OrgID: "o1"}
	with := func(f func(*MenuState)) MenuState { s := base; f(&s); return s }
	ready := &PlanStatus{Version: 3, Changes: 228, Warnings: 6, Dir: "/w/artifacts/plan-v3"}
	cases := []struct {
		name     string
		state    MenuState
		want     string
		selected string
	}{
		{"nothing yet", MenuState{Dir: "/w"}, "Nothing set up yet: choose Login to start", "login"},
		{"workspace without login", MenuState{Dir: "/w", Exists: true}, "Not logged in yet: choose Login to start", "login"},
		{"after login", base, "Next: choose Plan to read PagerDuty and JSM", "plan"},
		{"credentials gone", with(func(s *MenuState) { s.CredentialsStored = &no }),
			"Site acme.atlassian.net, organization o1", "login"},
		{"credentials stored", with(func(s *MenuState) { s.CredentialsStored = &yes }), "Next: choose Plan", "plan"},
		{"plan with blockers", with(func(s *MenuState) {
			s.Plan = &PlanStatus{Version: 2, Blockers: 1, Dir: "/w/artifacts/plan-v2"}
		}), "Plan v2 has 1 blocker to fix first", "plan"},
		{"plan ready", with(func(s *MenuState) { s.Plan = ready }), "Plan v3 is ready: 228 changes, 6 warnings to accept", "apply"},
		{"apply running", with(func(s *MenuState) {
			s.Plan, s.Run = ready, &RunStatus{PlanVersion: 3, State: RunRunning, Written: 98, Total: 228}
		}), "Apply of plan v3 is running in another window: 98 of 228 written", "pause"},
		{"apply paused", with(func(s *MenuState) {
			s.Plan, s.Run = ready, &RunStatus{PlanVersion: 3, State: RunPaused, Written: 98, Total: 228}
		}), "Apply of plan v3 was paused after 98 of 228 writes", "resume"},
		{"apply interrupted", with(func(s *MenuState) {
			s.Plan, s.Run = ready, &RunStatus{PlanVersion: 3, State: RunInterrupted, Written: 98, Total: 228}
		}), "Choose Resume to continue where it stopped", "resume"},
		{"applied", with(func(s *MenuState) {
			s.Plan, s.Run = ready, &RunStatus{PlanVersion: 3, State: RunCompleted, Written: 228, Total: 228, Failed: 2, Record: "/w/artifacts/run-1/run-record.md"}
		}), "Plan v3 applied: 226 written, 2 failed", "plan"},
		{"new plan after an applied one", with(func(s *MenuState) {
			s.Plan = &PlanStatus{Version: 4, Changes: 1, Warnings: 1, Dir: "/w/artifacts/plan-v4"}
			s.Run = &RunStatus{PlanVersion: 3, State: RunCompleted}
		}), "Plan v4 is ready: 1 change, 1 warning to accept", "apply"},
	}
	for _, c := range cases {
		lines, selected := describeMenu(c.state)
		if got := strings.Join(lines, "\n"); !strings.Contains(got, c.want) || selected != c.selected || !strings.HasPrefix(got, "Your files: ") {
			t.Errorf("%s:\n got  %q, selected %q\n want %q, selected %q", c.name, got, selected, c.want, c.selected)
		}
	}
}

func TestMenuStatusComesFromLaterTasks(t *testing.T) {
	dir := inTempDir(t)
	withCredentialsInEnv(t)
	ws, _ := store.OpenWorkspace(dir, true)
	if err := SaveConfig(ws, Config{Site: "acme.atlassian.net", CloudID: "cloud-1"}); err != nil {
		t.Fatal(err)
	}
	opts := Options{DescribeWorkspace: func(_ context.Context, _ *store.Workspace, _ Config, s *MenuState) {
		s.Plan = &PlanStatus{Version: 1, Changes: 5, Dir: filepath.Join(dir, "artifacts", "plan-v1")}
	}}
	m := &scriptedMenu{}
	runMenuSession(t, stubBodies(), opts, m)
	if len(m.shown) != 1 || !strings.Contains(strings.Join(m.shown[0], "\n"), "Plan v1 is ready: 5 changes") || m.selected[0] != "apply" {
		t.Errorf("shown %v, selected %v", m.shown, m.selected)
	}
}

// The header shows only a saved, checked site: not one saved without its
// cloudId, and not an override for one run.
func TestMenuShowsOnlyACheckedSavedSite(t *testing.T) {
	dir := inTempDir(t)
	withCredentialsInEnv(t)
	t.Setenv(envPrefix+"_SITE", "override.atlassian.net")
	ws, _ := store.OpenWorkspace(dir, true)
	if err := SaveConfig(ws, Config{Site: "typo.atlassian.netl", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	m := &scriptedMenu{}
	runMenuSession(t, stubBodies(), Options{}, m)
	got := strings.Join(m.shown[0], "\n")
	if !strings.Contains(got, "Not logged in yet: choose Login to start") || strings.Contains(got, "typo") || strings.Contains(got, "override") {
		t.Errorf("shown %q", got)
	}
}

func TestMenuCtrlCStopsOnlyTheCommand(t *testing.T) {
	inTempDir(t)
	sigs := make(chan os.Signal, 2)
	var planRuns int
	b := stubBodies()
	b.plan = func(ctx context.Context, _ *Env, _ PlanFlags) error {
		planRuns++
		if planRuns == 1 {
			sigs <- os.Interrupt
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return errors.New("Ctrl+C did not stop the command")
			}
		}
		if ctx.Err() != nil {
			return errors.New("the next command started already canceled")
		}
		return nil
	}
	m := &scriptedMenu{choices: []string{"plan", "plan"}}
	r := runMenuSession(t, b, Options{Interrupts: sigs}, m)
	if r.code != ExitOK || planRuns != 2 || len(m.shown) != 3 || strings.Contains(r.stderr, "already canceled") {
		t.Errorf("exit %d, plan runs %d, menu shown %d, stderr %q", r.code, planRuns, len(m.shown), r.stderr)
	}
}

func TestMenuDropsAStaleCtrlC(t *testing.T) {
	inTempDir(t)
	sigs := make(chan os.Signal, 2)
	sigs <- os.Interrupt // arrived while the menu was not reading keys
	b := stubBodies()
	b.plan = func(ctx context.Context, _ *Env, _ PlanFlags) error {
		select {
		case <-ctx.Done():
			return errors.New("a stale Ctrl+C stopped the command")
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}
	m := &scriptedMenu{choices: []string{"plan"}}
	if r := runMenuSession(t, b, Options{Interrupts: sigs}, m); r.code != ExitOK || r.stderr != "" {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestDisplayPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := displayPath(filepath.Join(home, "jsm-ops-migrator") + string(filepath.Separator)); got != filepath.Join("~", "jsm-ops-migrator") {
		t.Errorf("got %q", got)
	}
	other := filepath.Join(filepath.Dir(home), "elsewhere")
	if got := displayPath(other); got != other {
		t.Errorf("a path outside the home folder must stay absolute: %q", got)
	}
}
