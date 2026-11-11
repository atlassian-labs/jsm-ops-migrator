package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

var loginNow = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// loginSetup is one test's machine: a keychain and the APIs, shared by every
// command the test runs, and the operator's answers to prompts.
type loginSetup struct {
	t        *testing.T
	dir      string
	kc       *testkit.FakeKeychain
	apis     *testkit.FakeAPIs
	terminal bool
	answers  [][]string // one slice per Ask, in order
	choices  []string   // one per Choose, in order
	asked    []string   // the headings and titles shown
	asks     [][]tui.Question
	stdin    string
	// pd, when set, is the PagerDuty account, read with the real client in
	// the region the command chose; otherwise apis answers for PagerDuty too.
	pd *pdfake.Fake
	// sites holds the site each built set of clients was for.
	sites []string
}

func newLoginSetup(t *testing.T) *loginSetup {
	return &loginSetup{t: t, dir: inTempDir(t), kc: testkit.NewFakeKeychain(), apis: testkit.HealthyAPIs(loginNow)}
}

func (s *loginSetup) ask(_ context.Context, _ io.Reader, _ io.Writer, _ bool, heading string, qs []tui.Question) ([]string, error) {
	s.asked = append(s.asked, heading)
	s.asks = append(s.asks, qs)
	if len(s.answers) == 0 {
		return nil, errors.New("unexpected prompt: " + heading)
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	if len(a) != len(qs) {
		s.t.Errorf("%s: %d questions, the test has %d answers", heading, len(qs), len(a))
	}
	return a, nil
}

func (s *loginSetup) choose(_ context.Context, _ io.Reader, _ io.Writer, _ bool, title, _ string, options []tui.Option, _ string) (string, error) {
	s.asked = append(s.asked, title)
	if len(s.choices) == 0 {
		return "", errors.New("unexpected choice: " + title)
	}
	c := s.choices[0]
	s.choices = s.choices[1:]
	if !slices.ContainsFunc(options, func(o tui.Option) bool { return o.Key == c }) {
		s.t.Errorf("%s: %q is not an option", title, c)
	}
	return c, nil
}

func (s *loginSetup) run(args ...string) result {
	s.t.Helper()
	var out, errOut bytes.Buffer
	opts := Options{
		Keychain:   s.kc,
		NewClients: s.clients,
		Now:        func() time.Time { return loginNow },
	}
	h := hooks{
		isTerminal: func(any) bool { return s.terminal },
		menu:       tui.Menu,
		accessible: func() bool { return false },
		ask:        s.ask,
		choose:     s.choose,
	}
	code := executeWith(context.Background(), args, strings.NewReader(s.stdin), &out, &errOut, opts, defaultBodies(), h)
	return result{code, out.String(), errOut.String()}
}

func (s *loginSetup) clients(env *Env, c credentials.Set) (preflight.Clients, error) {
	s.sites = append(s.sites, env.Config.Site)
	clients := s.apis.ClientsFor(c)
	if s.pd != nil {
		cfg := s.pd.Config(httpx.Region(env.Config.PagerDutyRegion))
		cfg.Credentials = c
		h, err := httpx.New(cfg)
		if err != nil {
			return preflight.Clients{}, err
		}
		clients.PagerDuty = pd.New(h)
	}
	return clients, nil
}

// stored reads what the workspace's vault holds.
func (s *loginSetup) stored() credentials.Set {
	s.t.Helper()
	ws, err := store.OpenWorkspace(s.dir, false)
	if err != nil {
		s.t.Fatal(err)
	}
	info, err := store.ReadInfo(context.Background(), ws)
	if err != nil {
		s.t.Fatal(err)
	}
	set, err := credentials.NewKeychainVault(s.kc, info.WorkspaceID).Load()
	if err != nil {
		s.t.Fatal(err)
	}
	return set
}

func (s *loginSetup) config() Config {
	s.t.Helper()
	ws, err := store.OpenWorkspace(s.dir, false)
	if err != nil {
		s.t.Fatal(err)
	}
	c, err := LoadConfigFile(ws)
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// setCredentialsInEnv sets the given credentials in the environment and
// clears the others.
func setCredentialsInEnv(t *testing.T, _ credentials.Set, kinds ...credentials.Kind) {
	t.Helper()
	values := map[credentials.Kind]string{
		credentials.PagerDutyKey:   "pd-canary-0123456789",
		credentials.AtlassianEmail: "operator@example.com",
		credentials.AtlassianToken: "atl-canary-0123456789",
		credentials.OrgAPIKey:      "org-canary-0123456789",
	}
	for _, k := range credentials.Kinds {
		v := ""
		if slices.Contains(kinds, k) {
			v = values[k]
		}
		t.Setenv(k.EnvVar(), v)
	}
}

// noLeaks fails if a credential value is in the output or in any workspace
// file. The keychain holds them; nothing else may.
// testPassphrase is in every passphrase the tests choose, so noLeaks can
// look for it.
const testPassphrase = "horse battery"

// noLeaks fails when the output names a credential or the passphrase, or a
// file in the workspace holds one of them.
func noLeaks(t *testing.T, dir string, r result) {
	t.Helper()
	for _, out := range []string{r.stdout, r.stderr} {
		for _, secret := range []string{"canary", "operator@example.com", testPassphrase} {
			if strings.Contains(out, secret) {
				t.Errorf("output leaks %q:\n%s", secret, out)
			}
		}
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := os.ReadFile(p)
		for _, secret := range []string{"canary", testPassphrase} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %q", p, secret)
			}
		}
		return nil
	})
}

func TestLoginStoresCheckedCredentials(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "ACME", "--org-id", "org-1")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := s.stored().Missing(); len(got) != 0 {
		t.Errorf("not stored: %v", got)
	}
	if c := s.config(); c.Site != "acme.atlassian.net" || c.CloudID != "cloud-1" || c.OrgID != "org-1" {
		t.Errorf("config %+v", c)
	}
	for _, want := range []string{"PagerDuty API key", "ok", "Stored in the OS keychain:", "cloudId cloud-1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout misses %q:\n%s", want, r.stdout)
		}
	}
	noLeaks(t, s.dir, r)
}

func TestLoginDoesNotStoreARejectedCredential(t *testing.T) {
	s := newLoginSetup(t)
	s.apis.PagerDutyKey = "the-only-valid-key"
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitError || !strings.Contains(r.stderr, "login: 1 check failed: PagerDuty API key") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if got := s.stored(); got.Has(credentials.PagerDutyKey) || !got.Has(credentials.OrgAPIKey) {
		t.Errorf("stored %v missing; want only the PagerDuty key left out", got.Missing())
	}
	if !strings.Contains(r.stdout, "Not stored, because the check failed: PagerDuty API key.") {
		t.Errorf("stdout %s", r.stdout)
	}
	noLeaks(t, s.dir, r)
}

// A credential whose check could not run is not stored, and login carries
// on. A stored one, checked when it was stored, stays.
func TestLoginDoesNotStoreAnUncheckedCredential(t *testing.T) {
	s := newLoginSetup(t)
	s.apis.AbilitiesErr = fmt.Errorf("GET /abilities: %w", httperr.ErrUnavailable)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if got := s.stored(); got.Has(credentials.PagerDutyKey) || !got.Has(credentials.OrgAPIKey) {
		t.Errorf("stored %v missing; want only the PagerDuty key left out", got.Missing())
	}
	if !strings.Contains(r.stdout, "Not stored, because the check could not run yet: PagerDuty API key.") {
		t.Errorf("stdout %s", r.stdout)
	}
	noLeaks(t, s.dir, r)

	s.apis.AbilitiesErr = nil
	if r := s.run("login", "--site", "acme", "--org-id", "org-1"); r.code != ExitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	t.Setenv(credentials.PagerDutyKey.EnvVar(), "pd-canary-a-newer-key")
	s.apis.AbilitiesErr = fmt.Errorf("GET /abilities: %w", httperr.ErrUnavailable)
	r = s.run("--output", "json", "login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"unchecked": [`) {
		t.Fatalf("exit %d, stdout %s", r.code, r.stdout)
	}
	if got := s.stored().PagerDutyKey.Reveal(); got != "pd-canary-0123456789" {
		t.Errorf("stored PagerDuty key %q, want the checked one kept", got)
	}
	noLeaks(t, s.dir, r)
}

// Like a credential, the site and organization are saved only when their
// check passed. Giving the same one again keeps it; a different one that
// could not be checked removes it, so plan cannot use the old one.
func TestLoginSavesOnlyACheckedSiteAndOrganization(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.apis.TenantErr = fmt.Errorf("GET /_edge/tenant_info: %w", httperr.ErrUnavailable)
	s.apis.Orgs = nil // the key manages no organization, so org-1 fails
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if !strings.Contains(r.stdout, "Not saved, because the check did not pass: site acme.atlassian.net, organization org-1.") {
		t.Errorf("stdout %s", r.stdout)
	}
	if c := s.config(); c.Site != "" || c.OrgID != "" {
		t.Errorf("config %+v, want nothing saved", c)
	}

	s.apis.TenantErr, s.apis.Orgs = nil, []preflight.Org{{ID: "org-1", Name: "Acme"}}
	if r := s.run("login", "--site", "acme", "--org-id", "org-1"); r.code != ExitOK {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	s.apis.TenantErr = fmt.Errorf("GET /_edge/tenant_info: %w", httperr.ErrUnavailable)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	if c := s.config(); c.Site != "acme.atlassian.net" || c.CloudID != "cloud-1" || c.OrgID != "org-1" {
		t.Errorf("config %+v, want the checked site kept", c)
	}

	r = s.run("login", "--site", "other", "--org-id", "org-2")
	if c := s.config(); c.Site != "" || c.CloudID != "" || c.OrgID != "" {
		t.Errorf("config %+v, want the replaced site and organization removed", c)
	}
	for _, want := range []string{
		"Not saved, because the check did not pass: site other.atlassian.net, organization org-2.",
		"Removed the site and organization saved before, so they are not used by mistake.",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
}

// A site saved without its cloudId was never checked, so it counts as not
// saved: plan does not use it, the menu does not show it, and the next save
// leaves it out. A --site for one run still works.
func TestAnUncheckedSavedSiteIsNotUsed(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	ws, err := store.OpenWorkspace(s.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.ConfigPath(), []byte("site: typo.atlassian.netl\norg_id: org-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "missing: Site") || strings.Contains(r.stdout, "typo") {
		t.Fatalf("exit %d, stdout %s, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := s.run("plan", "--jsm-plan", "premium", "--site", "acme"); !strings.Contains(r.stdout, "acme.atlassian.net, cloudId cloud-1") {
		t.Errorf("--site was not used: %s", r.stdout)
	}
	if c := s.config(); c.Site != "" || c.OrgID != "org-1" {
		t.Errorf("config %+v", c)
	}
}

// A missing credential exits 4, and the ones given are still stored.
// The email and API token are checked together, so one given without the
// other was never sent to the site and is not stored.
func TestLoginDoesNotStoreHalfOfTheAtlassianLogin(t *testing.T) {
	for _, given := range []credentials.Kind{credentials.AtlassianToken, credentials.AtlassianEmail} {
		t.Run(string(given), func(t *testing.T) {
			s := newLoginSetup(t)
			// The site would reject the token, but it is never asked.
			s.apis.AtlassianToken = "another"
			setCredentialsInEnv(t, credentials.Set{}, credentials.PagerDutyKey, credentials.OrgAPIKey, given)
			r := s.run("login", "--site", "acme", "--org-id", "org-1")
			if r.code != ExitAwaitingInput {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}
			got := s.stored()
			if got.Has(given) {
				t.Errorf("stored the %s without the other half", given.Label())
			}
			if !got.Has(credentials.PagerDutyKey) || !got.Has(credentials.OrgAPIKey) {
				t.Errorf("stored %v missing; want the checked keys stored", got.Missing())
			}
			if want := "Not stored, because the check could not run yet: " + given.Label() + "."; !strings.Contains(r.stdout, want) {
				t.Errorf("stdout %s, want %q", r.stdout, want)
			}
			noLeaks(t, s.dir, r)
		})
	}
}

// An org API key that manages several organizations works even before one is
// chosen, so it is stored while login waits for the organization.
func TestLoginStoresAWorkingOrgKeyWithoutAnOrganization(t *testing.T) {
	s := newLoginSetup(t)
	s.apis.Orgs = []preflight.Org{{ID: "org-1", Name: "Acme"}, {ID: "org-2", Name: "Other"}}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "Org API key") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if !s.stored().Has(credentials.OrgAPIKey) {
		t.Error("the working org API key was not stored")
	}
	if c := s.config(); c.OrgID != "" {
		t.Errorf("saved organization %q, want none", c.OrgID)
	}
}

func TestLoginWithAMissingCredentialWaitsForInput(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.PagerDutyKey)
	r := s.run("login", "--site", "acme")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "login: missing: Atlassian email and API token, Org API key") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if !s.stored().Has(credentials.PagerDutyKey) {
		t.Error("the given key was not stored")
	}
}

func TestLoginInTheTerminal(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{})
	s.terminal = true
	s.apis.Orgs = []preflight.Org{{ID: "org-1", Name: "Acme"}, {ID: "org-2", Name: "Other"}}
	// PagerDuty key, site, email, token, org key and organization on one
	// page. The organization is left empty and the key manages two, so the
	// operator chooses one.
	s.answers = [][]string{{"pd-canary-0123456789", "acme", "operator@example.com", "atl-canary-0123456789", "org-canary-0123456789", ""}}
	s.choices = []string{"org-1"}
	if r := s.run("login"); r.code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", r.code, r.stderr, r.stdout)
	}
	if c := s.config(); c.OrgID != "org-1" || c.Site != "acme.atlassian.net" {
		t.Errorf("config %+v", c)
	}
	if len(s.asked) != 2 || s.asked[1] != "Organization" {
		t.Errorf("asked %q, want the login page and then the organization choice", s.asked)
	}

	// Again, changing only the token: empty answers keep the stored ones, and
	// the saved organization, filled in, is used without choosing again.
	s.answers = [][]string{{"", "acme.atlassian.net", "", "atl-canary-new-0123456789", "", "org-1"}}
	s.asked = nil
	r := s.run("login")
	if r.code != ExitOK {
		t.Fatalf("second login: exit %d: %s", r.code, r.stderr)
	}
	if len(s.asked) != 1 {
		t.Errorf("asked %q, want only the login page", s.asked)
	}
	if q := s.asks[len(s.asks)-1]; q[len(q)-1].Title != "Organization ID" || q[len(q)-1].Value != "org-1" {
		t.Errorf("the last question %+v, want the saved organization filled in", q[len(q)-1])
	}
	got := s.stored()
	if !got.PagerDutyKey.Equal(credentials.NewSecret("pd-canary-0123456789")) ||
		!got.AtlassianToken.Equal(credentials.NewSecret("atl-canary-new-0123456789")) {
		t.Error("an empty answer must keep the stored credential, and a new one replace it")
	}
	noLeaks(t, s.dir, r)
}

// A credential set in the environment is not asked for.
func TestLoginInTheTerminalSkipsWhatTheEnvironmentGives(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.PagerDutyKey, credentials.OrgAPIKey)
	s.terminal = true
	s.answers = [][]string{{"acme", "operator@example.com", "atl-canary-0123456789", ""}}
	if r := s.run("login"); r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if c := s.config(); c.OrgID != "org-1" {
		t.Errorf("the key's only organization must be taken: %+v", c)
	}
}

func TestLoginFromStdin(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{})
	s.stdin = "JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY=pd-canary-0123456789\n" +
		"JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL=operator@example.com\n" +
		"JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN=atl-canary-0123456789\n" +
		"JSM_OPS_MIGRATOR_ORG_API_KEY=org-canary-0123456789\n"
	r := s.run("login", "--stdin", "--site", "acme")
	if r.code != ExitOK || len(s.stored().Missing()) != 0 {
		t.Fatalf("exit %d, missing %v: %s", r.code, s.stored().Missing(), r.stderr)
	}
	if !strings.Contains(r.stdout, "from standard input") {
		t.Errorf("the source must be shown: %s", r.stdout)
	}
	if r := s.run("login", "--stdin", "--clear"); r.code != ExitUsage {
		t.Errorf("--stdin with --clear: exit %d", r.code)
	}
}

func TestLoginJSON(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--output", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"stored": [`) || !strings.Contains(r.stdout, `"pagerduty_api_key"`) {
		t.Fatalf("exit %d, stdout %s", r.code, r.stdout)
	}
	noLeaks(t, s.dir, r)
}

func TestLoginClear(t *testing.T) {
	s := newLoginSetup(t)
	if r := s.run("login", "--clear"); r.code != ExitOK || !strings.Contains(r.stdout, "No credentials are stored") {
		t.Fatalf("nothing to clear: exit %d, %q", r.code, r.stdout)
	}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme")
	r := s.run("login", "--clear")
	if r.code != ExitOK || !s.stored().IsZero() {
		t.Fatalf("exit %d, still stored %v", r.code, s.stored().Missing())
	}
	if !strings.Contains(r.stdout, "Still set in the environment: JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY") {
		t.Errorf("stdout %q", r.stdout)
	}
}

// The Atlassian side is still checked without a PagerDuty key, and plan
// waits for input.
func TestPlanChecksTheOtherSideWhenACredentialIsMissing(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.AtlassianEmail, credentials.AtlassianToken, credentials.OrgAPIKey)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	r := s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "plan: missing: PagerDuty API key") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if !slices.Contains(s.apis.Calls(), "ops teams cloud-1") {
		t.Errorf("JSM was not checked: %v", s.apis.Calls())
	}
	if !regexp.MustCompile(`Atlassian email and API token +ok`).MatchString(r.stdout) {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestPlanRunsThePreflightChecks(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	setCredentialsInEnv(t, credentials.Set{})
	r := s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitError || !strings.Contains(r.stderr, "plan: reading PagerDuty and JSM is not implemented yet") {
		t.Fatalf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	for _, want := range []string{"Pre-flight checks", "Secret storage", "kept in the OS keychain", "Org admin", "warning"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout misses %q:\n%s", want, r.stdout)
		}
	}

	// A site recreated under another cloudId fails the check.
	s.apis.Sites["acme.atlassian.net"] = preflight.Tenant{CloudID: "cloud-2", ServerTime: loginNow}
	if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitError || !strings.Contains(r.stderr, "plan: 1 check failed: Site") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	noLeaks(t, s.dir, r)
}

// The clients choose the Atlassian API host by the site, so they are built
// for the site being checked, in the form the checks use, however it was
// typed.
func TestClientsAreBuiltForTheCheckedSite(t *testing.T) {
	s := newLoginSetup(t)
	s.apis.Sites["acme.jira-dev.com"] = preflight.Tenant{CloudID: "cloud-stg", ServerTime: loginNow}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "https://ACME.jira-dev.com/jira", "--org-id", "org-1")
	s.run("plan", "--jsm-plan", "premium", "--site", "acme")
	if len(s.sites) != 2 || s.sites[0] != "acme.jira-dev.com" || s.sites[1] != "acme.atlassian.net" {
		t.Fatalf("clients built for %q, want login's acme.jira-dev.com, then plan's acme.atlassian.net", s.sites)
	}
}

// The saved cloudId belongs to the saved site. Another site given with --site
// is checked on its own, unless --cloud-id gives the cloudId to expect.
func TestPlanOnAnotherSiteDoesNotUseTheSavedCloudID(t *testing.T) {
	s := newLoginSetup(t)
	s.apis.Sites["other.atlassian.net"] = preflight.Tenant{CloudID: "cloud-9", ServerTime: loginNow}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	if r := s.run("login", "--site", "acme", "--org-id", "org-1"); r.code != ExitOK {
		t.Fatalf("login: exit %d, stderr %q", r.code, r.stderr)
	}
	r := s.run("plan", "--jsm-plan", "premium", "--site", "other")
	if strings.Contains(r.stderr, "check failed") || !strings.Contains(r.stdout, "other.atlassian.net, cloudId cloud-9") {
		t.Errorf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	r = s.run("plan", "--jsm-plan", "premium", "--site", "other", "--cloud-id", "cloud-old")
	if !strings.Contains(r.stderr, "plan: 1 check failed: Site") {
		t.Errorf("--cloud-id: exit %d, stderr %q", r.code, r.stderr)
	}
	if c := s.config(); c.Site != "acme.atlassian.net" || c.CloudID != "cloud-1" {
		t.Errorf("config %+v, want the saved site kept", c)
	}
}

func TestPlanAsksForTheJSMPlanOnceAndSavesIt(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	s.terminal = true
	s.choices = []string{"standard"}
	s.run("plan")
	if c := s.config(); c.JSMPlan != "standard" {
		t.Fatalf("config %+v", c)
	}
	s.run("plan") // no choice left: asking again would fail the test
	if strings.Count(strings.Join(s.asked, "\n"), "JSM plan") != 1 {
		t.Errorf("asked %v", s.asked)
	}
}

func TestPlanOnlyPagerDutyNeedsNoJSMCredentials(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.PagerDutyKey)
	r := s.run("plan", "--only", "pagerduty")
	if r.code != ExitError || !strings.Contains(r.stderr, "not implemented yet") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if strings.Contains(r.stdout, "Site") || slices.ContainsFunc(s.apis.Calls(), func(c string) bool { return !strings.HasPrefix(c, "pagerduty") }) {
		t.Errorf("checked the JSM side: %v\n%s", s.apis.Calls(), r.stdout)
	}
}

// Without a keychain, a new workspace needs the operator's agreement or
// JSM_OPS_MIGRATOR_PASSPHRASE, and then keeps everything in credentials.enc.
func TestNoKeychainNeedsAPassphrase(t *testing.T) {
	s := newLoginSetup(t)
	s.kc.SetUnavailable(true)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	t.Setenv(PassphraseEnvVar, "")
	r := s.run("login", "--site", "acme")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "set JSM_OPS_MIGRATOR_PASSPHRASE") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(s.dir, store.StateFile)); err == nil {
		t.Error("state.db was created without a key")
	}

	// In a terminal the operator agrees and chooses one; a mismatch asks again.
	s.terminal = true
	s.choices = []string{"passphrase"}
	s.answers = [][]string{
		{"correct horse battery staple", "correct horse battery stapler"},
		{"correct horse battery staple", "correct horse battery staple"},
		{"acme", ""}, // the site and organization; every credential comes from the environment
	}
	r = s.run("login")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Stored in credentials.enc") || !strings.Contains(r.stderr, "do not match") {
		t.Fatalf("exit %d, stdout %s, stderr %s", r.code, r.stdout, r.stderr)
	}
	noLeaks(t, s.dir, r)

	// Later commands ask once; a wrong answer is asked again.
	s.answers = [][]string{{"wrong passphrase!!"}, {"correct horse battery staple"}}
	s.choices = []string{"premium"}
	r = s.run("plan", "--jsm-plan", "premium")
	if !strings.Contains(r.stderr, "The passphrase is wrong. Try again.") || !strings.Contains(r.stdout, "kept in credentials.enc") {
		t.Errorf("exit %d, stdout %s, stderr %s", r.code, r.stdout, r.stderr)
	}

	// --clear needs no passphrase.
	if r := s.run("login", "--clear"); r.code != ExitOK {
		t.Errorf("clear: exit %d, %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(s.dir, credentials.CredentialsFile)); err == nil {
		t.Error("credentials.enc is still there")
	}
}

func TestMenuSeesStoredCredentials(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	setCredentialsInEnv(t, credentials.Set{})
	ws, _ := store.OpenWorkspace(s.dir, false)
	if got := credentialsStatus(context.Background(), ws, s.kc); got == nil || !*got {
		t.Errorf("status %v, want stored", got)
	}
	s.run("login", "--clear")
	if got := credentialsStatus(context.Background(), ws, s.kc); got == nil || *got {
		t.Errorf("status after clear %v, want missing", got)
	}
}

// The email question in the terminal checks what is typed, and an empty
// answer, which keeps the stored email, is fine.
func TestLoginInTheTerminalChecksTheEmail(t *testing.T) {
	s := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{})
	s.terminal = true
	s.answers = [][]string{{"pd-canary-0123456789", "acme", "operator@example.com", "atl-canary-0123456789", "org-canary-0123456789", ""}}
	if r := s.run("login"); r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	i := slices.IndexFunc(s.asks[0], func(q tui.Question) bool { return q.Title == "Atlassian account email" })
	if i < 0 || s.asks[0][i].Validate == nil {
		t.Fatalf("no email question with a check in %+v", s.asks[0])
	}
	if s.asks[0][i].Masked {
		t.Error("the email is masked; only secrets are")
	}
	validate := s.asks[0][i].Validate
	for _, bad := range []string{"operator", "operator@", "@example.com", "operator@example", "oper ator@example.com"} {
		if validate(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"operator@example.com", " operator@example.com ", ""} {
		if err := validate(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestNormalizeSite(t *testing.T) {
	for in, want := range map[string]string{
		"acme": "acme.atlassian.net", "ACME.atlassian.net": "acme.atlassian.net",
		"https://acme.atlassian.net/jira/ops": "acme.atlassian.net", " acme.atlassian.net ": "acme.atlassian.net", "": "",
	} {
		if got, err := normalizeSite(in); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://user:pw@acme.atlassian.net", "acme.atlassian.net:8443", "acme_site.example", "-acme.example"} {
		if _, err := normalizeSite(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
