package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, b bodies, opts Options, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	if opts.NewKeyProvider == nil {
		opts.NewKeyProvider = func(context.Context, *Env) (store.KeyProvider, error) { return testkit.NewMemoryKeyProvider(), nil }
	}
	if opts.Keychain == nil {
		opts.Keychain = testkit.NewFakeKeychain()
	}
	code := execute(context.Background(), args, strings.NewReader(""), &out, &errOut, opts, b)
	return result{code, out.String(), errOut.String()}
}

// inTempDir points the workspace and the home folder at fresh temporary
// directories, so no test ever touches the real ~/jsm-ops-migrator. It returns
// the workspace directory, which exists but holds no workspace yet.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("JSM_OPS_MIGRATOR_WORKSPACE", dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return dir
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestHelpListsTheFiveCommands(t *testing.T) {
	dir := inTempDir(t)
	r := run(t, defaultBodies(), Options{})
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr %s", r.code, r.stderr)
	}
	section := r.stdout[strings.Index(r.stdout, "Available Commands:"):]
	section = section[:strings.Index(section, "\n\n")]
	var listed []string
	for _, line := range strings.Split(section, "\n")[1:] {
		listed = append(listed, strings.Fields(line)[0])
	}
	slices.Sort(listed)
	// cobra's template always lists its help command.
	if want := []string{"apply", "help", "login", "pause", "plan", "resume"}; !slices.Equal(listed, want) {
		t.Errorf("commands = %v, want %v", listed, want)
	}
	if len(entries(t, dir)) != 0 {
		t.Errorf("help created files: %v", entries(t, dir))
	}
}

func TestUsageErrors(t *testing.T) {
	dir := inTempDir(t)
	for _, args := range [][]string{
		{"frobnicate"},
		{"plan", "--frob"},
		{"plan", "--output", "xml"},
		{"plan", "--log-level", "loud"},
		{"plan", "--only", "both"},
		{"login", "extra"},
		{"apply", "--clear"},
	} {
		r := run(t, defaultBodies(), Options{}, args...)
		if r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, r.code, ExitUsage)
		}
		if r.stdout != "" || !strings.HasPrefix(r.stderr, "Error: ") || !strings.Contains(r.stderr, "--help' for usage.") {
			t.Errorf("%v: stdout %q, stderr %q", args, r.stdout, r.stderr)
		}
	}
	if len(entries(t, dir)) != 0 {
		t.Errorf("usage errors created files: %v", entries(t, dir))
	}
}

// stubBodies replace login and plan with stubs, for tests about the command
// machinery rather than what the commands do.
func stubBodies() bodies {
	b := defaultBodies()
	b.login = func(context.Context, *Env, LoginFlags) error { return notImplemented("login") }
	b.plan = func(context.Context, *Env, PlanFlags) error { return notImplemented("plan") }
	return b
}

func TestStubBodiesAreNotImplemented(t *testing.T) {
	inTempDir(t)
	for _, c := range []string{"login", "plan", "apply", "pause", "resume"} {
		r := run(t, stubBodies(), Options{}, c)
		if r.code != ExitError || !strings.Contains(r.stderr, c+": not implemented yet") {
			t.Errorf("%s: exit %d, stderr %q", c, r.code, r.stderr)
		}
	}
}

func TestCommandsWithoutAWorkspace(t *testing.T) {
	dir := inTempDir(t)
	for _, c := range []string{"apply", "pause", "resume"} {
		r := run(t, defaultBodies(), Options{}, c)
		if r.code != ExitError || !strings.Contains(r.stderr, "workspace not found") || !strings.Contains(r.stderr, "jsm-ops-migrator login") {
			t.Errorf("%s: exit %d, stderr %q", c, r.code, r.stderr)
		}
	}
	if len(entries(t, dir)) != 0 {
		t.Errorf("created files: %v", entries(t, dir))
	}
	if r := run(t, defaultBodies(), Options{}, "login"); r.code != ExitAwaitingInput {
		t.Fatalf("login without credentials: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, store.ArtifactsDir)); err != nil {
		t.Errorf("login must create the workspace: %v", err)
	}
}

func TestWorkspaceThatIsAFile(t *testing.T) {
	inTempDir(t)
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, defaultBodies(), Options{}, "login", "--workspace", file)
	if r.code != ExitError || !strings.Contains(r.stderr, "is not a directory") || strings.Contains(r.stderr, "login' first") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestWorkspaceLocation(t *testing.T) {
	inTempDir(t)
	var got string
	b := defaultBodies()
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error { got = env.Workspace.Dir(); return nil }

	t.Setenv("JSM_OPS_MIGRATOR_WORKSPACE", "")
	home, _ := os.UserHomeDir()
	if r := run(t, b, Options{}, "plan"); r.code != ExitOK || got != filepath.Join(home, "jsm-ops-migrator") {
		t.Errorf("default: exit %d, workspace %s, want ~/jsm-ops-migrator", r.code, got)
	}
	fromEnv := t.TempDir()
	t.Setenv("JSM_OPS_MIGRATOR_WORKSPACE", fromEnv)
	if r := run(t, b, Options{}, "plan"); r.code != ExitOK || got != fromEnv {
		t.Errorf("env: exit %d, workspace %s", r.code, got)
	}
	fromFlag := t.TempDir()
	if r := run(t, b, Options{}, "plan", "--workspace", fromFlag); r.code != ExitOK || got != fromFlag {
		t.Errorf("flag: exit %d, workspace %s", r.code, got)
	}
}

func TestLockIsHeldExceptForPause(t *testing.T) {
	dir := inTempDir(t)
	ws, err := store.OpenWorkspace(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Unlock() }()

	var paused bool
	b := stubBodies()
	b.pause = func(context.Context, *Env, PauseFlags) error { paused = true; return nil }
	for _, c := range []string{"login", "plan", "apply", "resume"} {
		if r := run(t, b, Options{}, c); r.code != ExitError || !strings.Contains(r.stderr, "another jsm-ops-migrator command") {
			t.Errorf("%s while locked: exit %d, stderr %q", c, r.code, r.stderr)
		}
	}
	if r := run(t, b, Options{}, "pause"); r.code != ExitOK || !paused {
		t.Errorf("pause must run while an apply holds the lock: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestConfigPrecedence(t *testing.T) {
	dir := inTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, store.ConfigFile),
		[]byte("site: file.atlassian.net\ncloud_id: c-file\norg_id: o-file\npagerduty_subdomain: p-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got Config
	b := defaultBodies()
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error { got = env.Config; return nil }

	t.Setenv("JSM_OPS_MIGRATOR_SITE", "")                // empty counts as unset
	t.Setenv("JSM_OPS_MIGRATOR_CLOUD_ID", "c-env")       // env beats the file
	t.Setenv("JSM_OPS_MIGRATOR_ORG_ID", "o-env")         // the flag beats env
	t.Setenv("JSM_OPS_MIGRATOR_PAGERDUTY_SUBDOMAIN", "") // empty counts as unset
	if r := run(t, b, Options{}, "plan", "--org-id", "o-flag"); r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := Config{Site: "file.atlassian.net", CloudID: "c-env", OrgID: "o-flag", PagerDutySubdomain: "p-file"}
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}
}

func TestConfigRejectsUnknownKeys(t *testing.T) {
	dir := inTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, store.ConfigFile), []byte("sitee: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, defaultBodies(), Options{}, "plan")
	if r.code != ExitError || !strings.Contains(r.stderr, "sitee") || !strings.Contains(r.stderr, store.ConfigFile) {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestSaveConfigWritesOnlyTheFileValues(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Site: "acme.atlassian.net", CloudID: "c1"}
	if err := SaveConfig(ws, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfigFile(ws)
	if err != nil || got != c {
		t.Fatalf("LoadConfigFile = %+v, %v", got, err)
	}
	t.Setenv("JSM_OPS_MIGRATOR_ORG_ID", "o-env")
	loaded, err := loadConfig(ws, pflag.NewFlagSet("x", pflag.ContinueOnError))
	if err != nil || loaded.OrgID != "o-env" {
		t.Fatalf("loadConfig = %+v, %v", loaded, err)
	}
	if err := SaveConfig(ws, got); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ws.ConfigPath())
	if strings.Contains(string(data), "o-env") {
		t.Errorf("an environment override was saved: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(ws.ConfigPath())
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config.yaml mode %v", info.Mode().Perm())
		}
	}
}

func TestConfigHoldsNoSecret(t *testing.T) {
	secret := reflect.TypeOf(credentials.Secret{})
	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		if typ.Field(i).Type == secret {
			t.Errorf("Config.%s is a Secret: credentials never go in config.yaml", typ.Field(i).Name)
		}
	}
}

// TestNoSecretFlags enforces that no flag can carry a credential: login takes
// secrets from prompts, environment variables or stdin only.
func TestNoSecretFlags(t *testing.T) {
	r := &runner{bodies: defaultBodies(), hooks: defaultHooks(), interrupts: newInterrupter(func() {}, nil)}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				for _, word := range []string{"token", "key", "secret", "password", "passphrase"} {
					if strings.Contains(strings.ToLower(f.Name), word) {
						t.Errorf("%s --%s looks like it takes a secret", c.CommandPath(), f.Name)
					}
				}
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(r.newRootCommand())
}

func TestInterruptSteps(t *testing.T) {
	for _, tc := range []struct {
		graceful               bool
		closedAt, cancelAt, at int // interrupt numbers; at is when ForceExit runs
	}{{false, 1, 1, 2}, {true, 1, 2, 3}} {
		ctx, cancel := context.WithCancel(context.Background())
		var forced atomic.Int32
		ir := newInterrupter(cancel, func() { forced.Add(1) })
		ir.setGraceful(tc.graceful)
		for n := 1; n <= 3; n++ {
			ir.interrupt()
			closed := isClosed(ir.ch)
			if closed != (n >= tc.closedAt) || (ctx.Err() != nil) != (n >= tc.cancelAt) || (forced.Load() > 0) != (n >= tc.at) {
				t.Errorf("graceful=%v after %d interrupts: closed=%v canceled=%v forced=%d",
					tc.graceful, n, closed, ctx.Err() != nil, forced.Load())
			}
		}
		cancel()
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestInterruptThroughSignalsAndRequest(t *testing.T) {
	inTempDir(t)
	if _, err := store.OpenWorkspace(".", true); err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 2)
	b := defaultBodies()
	b.apply = func(ctx context.Context, env *Env, _ ApplyFlags) error {
		sigs <- os.Interrupt
		<-env.Interrupted
		select {
		case <-ctx.Done():
			return errors.New("the first interrupt must not cancel apply")
		case <-time.After(50 * time.Millisecond):
		}
		env.RequestInterrupt()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the second interrupt did not cancel apply")
		}
	}
	if r := run(t, b, Options{Interrupts: sigs}, "apply"); r.code != ExitOK {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// codedError has an ExitCode method of its own, as an *exec.ExitError from a
// keychain helper does. Its code must not become the CLI's.
type codedError struct{ code int }

func (e codedError) Error() string { return fmt.Sprintf("coded %d", e.code) }
func (e codedError) ExitCode() int { return e.code }

func TestExitCodes(t *testing.T) {
	inTempDir(t)
	for _, tc := range []struct {
		err  error
		want int
	}{
		{codedError{5}, ExitError},
		{fmt.Errorf("opening the workspace: keychain unavailable: %w", codedError{6}), ExitError},
		{errors.New("plain"), ExitError},
		{messages.Blocked(errors.New("plan: 1 blocker")), messages.ExitBlocked},
		{fmt.Errorf("plan: %w", messages.AwaitingInput(errors.New("missing"))), ExitAwaitingInput},
		{messages.ValidationFailed(errors.New("plan.json changed")), messages.ExitValidationFailed},
		{fmt.Errorf("apply: %w", messages.PartiallyComplete(errors.New("1 failed"))), messages.ExitPartial},
		{errors.Join(errors.New("a"), messages.Blocked(errors.New("b"))), messages.ExitBlocked},
		{messages.Blocked(codedError{6}), messages.ExitBlocked},
	} {
		b := defaultBodies()
		b.plan = func(context.Context, *Env, PlanFlags) error { return tc.err }
		if r := run(t, b, Options{}, "plan"); r.code != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.err, r.code, tc.want)
		}
	}
}

func logFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, store.ArtifactsDir, "logs", "*.log"))
	return files
}

func TestStderrIsSavedAndRedacted(t *testing.T) {
	dir := inTempDir(t)
	phone := "+90 555 " + "123 45 67"
	b := defaultBodies()
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error {
		env.Logger.Warn("calling "+phone, "token", "tok-abc", "secret", credentials.NewSecret("s3cr3t-xyz"))
		_, _ = fmt.Fprintln(env.Stderr, "progress line")
		return errors.New("request failed: Authorization: Bearer abcdefghijkl")
	}
	r := run(t, b, Options{}, "plan", "--log-level", "info")
	logs := logFiles(t, dir)
	if len(logs) != 1 {
		t.Fatalf("log files = %v", logs)
	}
	saved, _ := os.ReadFile(logs[0])
	for _, out := range []string{r.stderr, string(saved)} {
		for _, leak := range []string{"555 123", "tok-abc", "s3cr3t-xyz", "abcdefghijkl"} {
			if strings.Contains(out, leak) {
				t.Errorf("leaked %q in %q", leak, out)
			}
		}
		for _, want := range []string{"calling", "progress line", "Error: request failed"} {
			if !strings.Contains(out, want) {
				t.Errorf("%q missing from %q", want, out)
			}
		}
	}
	if strings.Count(r.stderr, "Error: request failed") != 1 {
		t.Errorf("the error must be printed once: %q", r.stderr)
	}

	b.plan = func(context.Context, *Env, PlanFlags) error { return nil }
	run(t, b, Options{}, "plan")
	if len(logFiles(t, dir)) != 1 {
		t.Errorf("a command that prints nothing to stderr must leave no log file: %v", logFiles(t, dir))
	}
}

func TestLogFileNamesDoNotCollide(t *testing.T) {
	dir := inTempDir(t)
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	b := defaultBodies()
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error { env.Logger.Warn("hello"); return nil }
	for range 3 {
		run(t, b, Options{Now: func() time.Time { return now }}, "plan")
	}
	if got := logFiles(t, dir); len(got) != 3 {
		t.Errorf("log files = %v, want 3", got)
	}
}

func TestJSONOutputLogsJSON(t *testing.T) {
	inTempDir(t)
	b := defaultBodies()
	b.plan = func(_ context.Context, env *Env, _ PlanFlags) error {
		if env.Output != OutputJSON || env.Color || env.Interactive {
			return fmt.Errorf("env %+v", env)
		}
		env.Logger.Warn("hello")
		return nil
	}
	r := run(t, b, Options{}, "plan", "--output", "json")
	if r.code != ExitOK || !strings.HasPrefix(strings.TrimSpace(r.stderr), "{") || r.stdout != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestOpenStoreBuildsTheKeyProviderOnce(t *testing.T) {
	inTempDir(t)
	var built int
	kp := testkit.NewMemoryKeyProvider()
	opts := Options{NewKeyProvider: func(context.Context, *Env) (store.KeyProvider, error) { built++; return kp, nil }}
	b := defaultBodies()
	b.plan = func(ctx context.Context, env *Env, _ PlanFlags) error {
		for range 2 {
			st, err := env.OpenStore(ctx, store.OpenCreate)
			if err != nil {
				return err
			}
			if err := st.Close(); err != nil {
				return err
			}
		}
		return nil
	}
	if r := run(t, b, opts, "plan"); r.code != ExitOK || built != 1 {
		t.Errorf("exit %d, provider built %d times: %s", r.code, built, r.stderr)
	}
}

// A command that ignores its canceled context must still be leavable: the
// Ctrl+C after the cancel reaches ForceExit.
func TestCtrlCAfterTheCancelForcesTheExit(t *testing.T) {
	inTempDir(t)
	sigs := make(chan os.Signal, 2)
	var forced atomic.Bool
	b := defaultBodies()
	b.plan = func(ctx context.Context, _ *Env, _ PlanFlags) error {
		sigs <- os.Interrupt
		<-ctx.Done()
		sigs <- os.Interrupt
		deadline := time.Now().Add(5 * time.Second)
		for !forced.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	run(t, b, Options{Interrupts: sigs, ForceExit: func() { forced.Store(true) }}, "plan")
	if !forced.Load() {
		t.Error("the second Ctrl+C was swallowed")
	}
}
