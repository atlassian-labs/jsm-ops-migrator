package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// teamStep plans two teams, one with a warning, and makes each call as told.
type teamStep struct {
	jsmReadErr error
	fail       map[string]error
	calls      []string
	// during runs inside each call; an error it returns is the call's.
	during func(ctx context.Context, key string) error
	// found is what a lookup finds; an item without one is unknown.
	found map[string]engine.Lookup
}

func (s *teamStep) Name() string { return "teams" }

func (s *teamStep) Read(_ context.Context, side review.Side, _ *discover.Writer) error {
	if side == review.JSM {
		return s.jsmReadErr
	}
	return nil
}

// Plan maps a team an earlier run made, as a real step does: planning it
// again would make it twice.
func (s *teamStep) Plan(_ context.Context, in engine.PlanInput) (engine.Planned, error) {
	var p engine.Planned
	for _, t := range []struct{ id, name string }{{"PT1", "Platform"}, {"PT2", "Payments"}} {
		pd := review.Ref{Type: "team", ID: t.id}
		it := review.Item{Key: "teams/" + t.id, Action: review.Create, Summary: "Create " + t.name, PagerDuty: &pd}
		if jsm, ok := in.Mappings.Find(pd, "team"); ok {
			it.Action, it.JSM, it.Summary = review.Map, &jsm, "Map "+t.name
		}
		p.Items = append(p.Items, it)
	}
	if p.Items[0].Action == review.Create {
		p.Messages = []messages.Message{{Class: messages.Warning, Code: "member_dropped", Item: "teams/PT1", Text: "Casey is not migrated."}}
	}
	return p, nil
}

func (s *teamStep) Apply(ctx context.Context, it review.Item, _ engine.Results) (engine.Outcome, error) {
	s.calls = append(s.calls, it.Key)
	if s.during != nil {
		if err := s.during(ctx, it.Key); err != nil {
			return engine.Outcome{}, err
		}
	}
	if err := s.fail[it.Key]; err != nil {
		return engine.Outcome{}, err
	}
	return engine.Outcome{JSM: &review.Ref{Type: "team", ID: "jsm-" + it.PagerDuty.ID}, Status: 201}, nil
}

func (s *teamStep) Lookup(_ context.Context, it review.Item, _ engine.Results, _ *discover.Snapshot) (engine.Lookup, error) {
	return s.found[it.Key], nil
}

// planned logs in and plans with the team step.
func planned(t *testing.T) (*loginSetup, *teamStep) {
	t.Helper()
	s := newLoginSetup(t)
	step := &teamStep{fail: map[string]error{}, found: map[string]engine.Lookup{}}
	s.steps = []engine.Step{step}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	if r := s.run("login", "--site", "acme", "--org-id", "org-1"); r.code != ExitOK {
		t.Fatalf("login: exit %d, %s", r.code, r.stderr)
	}
	if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitOK {
		t.Fatalf("plan: exit %d, %s\n%s", r.code, r.stderr, r.stdout)
	}
	return s, step
}

func (s *loginSetup) artifact(parts ...string) string {
	return filepath.Join(append([]string{s.dir, "artifacts"}, parts...)...)
}

// acceptanceFile fills in v1's template with a reason for each warning.
func (s *loginSetup) acceptanceFile(reason string) string {
	s.t.Helper()
	return s.acceptanceFileFor(1, reason)
}

func (s *loginSetup) acceptanceFileFor(version int, reason string) string {
	s.t.Helper()
	data, err := os.ReadFile(s.artifact("plans", fmt.Sprintf("v%d", version), review.AcceptanceTemplateFile))
	if err != nil {
		s.t.Fatal(err)
	}
	path := filepath.Join(s.t.TempDir(), "accept.yaml")
	filled := bytes.ReplaceAll(data, []byte(`reason: ""`), []byte(fmt.Sprintf("reason: %q", reason)))
	if err := os.WriteFile(path, filled, 0o600); err != nil {
		s.t.Fatal(err)
	}
	return path
}

func TestPlanWritesTheReviewFiles(t *testing.T) {
	s, _ := planned(t)
	for _, f := range []string{review.PlanFile, review.PlanMarkdownFile, review.DiffFile, review.RiskQueueFile, review.AcceptanceTemplateFile} {
		if _, err := os.Stat(s.artifact("plans", "v1", f)); err != nil {
			t.Errorf("v1: %v", err)
		}
	}
	r := s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Plan v1 is unchanged") {
		t.Errorf("a second plan: exit %d\n%s", r.code, r.stdout)
	}
	// --only reads one side again and builds on the other's latest read.
	r = s.run("plan", "--jsm-plan", "premium", "--only", "jsm")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Using the PagerDuty read of") || !strings.Contains(r.stdout, "Read JSM: complete") {
		t.Errorf("plan --only jsm: exit %d, %s\n%s", r.code, r.stderr, r.stdout)
	}
	noLeaks(t, s.dir, r)
}

func TestApplyNeedsAConfirmationWithoutATerminal(t *testing.T) {
	s, step := planned(t)
	r := s.run("apply")
	if r.code != ExitAwaitingInput || !strings.Contains(r.stderr, "--accept-file") ||
		!strings.Contains(r.stderr, filepath.Join("plans", "v1", review.AcceptanceTemplateFile)) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if len(step.calls) != 0 {
		t.Errorf("calls without a confirmation: %v", step.calls)
	}
}

func TestApplyWithAnAcceptanceFile(t *testing.T) {
	s, step := planned(t)
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("Casey left the company"))
	if r.code != ExitOK || !strings.Contains(r.stdout, "of plan v1: 2 of 2 items done") {
		t.Fatalf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("calls %v", step.calls)
	}
	conf, err := os.ReadFile(s.artifact("plans", "v1", review.ConfirmationFile))
	if err != nil || !bytes.Contains(conf, []byte("account-of-org-1")) || !bytes.Contains(conf, []byte("Casey left the company")) {
		t.Errorf("confirmation.md: %v\n%s", err, conf)
	}
	runs, _ := os.ReadDir(s.artifact("runs"))
	if len(runs) != 1 {
		t.Fatalf("runs %v", runs)
	}
	for _, f := range []string{review.RunFile, review.RunMarkdownFile, review.MappingFile} {
		if _, err := os.Stat(s.artifact("runs", runs[0].Name(), f)); err != nil {
			t.Error(err)
		}
	}
	noLeaks(t, s.dir, r)

	// A version is applied once.
	r = s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("again"))
	if r.code != ExitError || !strings.Contains(r.stderr, "plan v1 was applied by run") {
		t.Errorf("a second apply: exit %d, %q", r.code, r.stderr)
	}
	if len(step.calls) != 2 {
		t.Errorf("calls %v", step.calls)
	}
	// The next plan maps what the run made; once that is applied too,
	// there is nothing new.
	r = s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Plan v2: 0 calls") || !strings.Contains(r.stdout, "teams: 2 map") {
		t.Fatalf("plan after the run: exit %d\n%s", r.code, r.stdout)
	}
	if r = s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFileFor(2, "")); r.code != ExitOK {
		t.Fatalf("apply v2: exit %d, %s", r.code, r.stderr)
	}
	r = s.run("plan", "--jsm-plan", "premium")
	if r.code != ExitOK || !strings.Contains(r.stdout, "there is nothing new to apply") {
		t.Errorf("plan after the second run: exit %d\n%s", r.code, r.stdout)
	}
	if len(step.calls) != 2 {
		t.Errorf("calls %v", step.calls)
	}
}

// Second review: without a terminal or --accept-file, apply asked for the
// file before it checked anything else, so a blocked, applied or edited
// plan exited 4 with an invitation to fill in a file for it.
func TestApplyRefusesBeforeAskingForAConfirmation(t *testing.T) {
	s, _ := planned(t)
	path := s.artifact("plans", "v1", review.PlanFile)
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte("Create Payments"), []byte("Create Paymints"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := s.run("apply", "--jsm-plan", "premium"); r.code != messages.ExitValidationFailed {
		t.Errorf("an edited plan.json: exit %d, %q", r.code, r.stderr)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("ok")); r.code != ExitOK {
		t.Fatalf("apply: exit %d, %q", r.code, r.stderr)
	}
	if r := s.run("apply", "--jsm-plan", "premium"); r.code != ExitError || !strings.Contains(r.stderr, "was applied by run") {
		t.Errorf("an applied version: exit %d, %q", r.code, r.stderr)
	}

	blocked := newLoginSetup(t)
	blocked.steps = []engine.Step{&teamStep{jsmReadErr: fmt.Errorf("GET /v1/teams: 403: %w", httperr.ErrForbidden)}}
	blocked.run("login", "--site", "acme", "--org-id", "org-1")
	blocked.run("plan", "--jsm-plan", "premium")
	if r := blocked.run("apply", "--jsm-plan", "premium"); r.code != messages.ExitBlocked {
		t.Errorf("a blocked plan: exit %d, %q", r.code, r.stderr)
	}
}

func TestApplyRefusesAnAcceptanceFileThatDoesNotAcceptThePlan(t *testing.T) {
	s, step := planned(t)
	cases := map[string]struct {
		edit func([]byte) []byte
		want string
	}{
		"no reason": {func(b []byte) []byte { return b }, "accepted without a reason"},
		"another version": {func(b []byte) []byte {
			return bytes.Replace(b, []byte("plan_version: 1"), []byte("plan_version: 7"), 1)
		}, "not v1"},
		"a typo": {func(b []byte) []byte { return append(b, []byte("plan_hsah: x\n")...) }, "plan_hsah"},
	}
	for name, c := range cases {
		path := s.acceptanceFile("")
		data, _ := os.ReadFile(path)
		if err := os.WriteFile(path, c.edit(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", path); r.code != messages.ExitValidationFailed || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s: exit %d, stderr %q", name, r.code, r.stderr)
		}
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", filepath.Join(t.TempDir(), "missing.yaml")); r.code != ExitError {
		t.Errorf("a missing file: exit %d, %q", r.code, r.stderr)
	}
	if len(step.calls) != 0 {
		t.Errorf("calls %v", step.calls)
	}
}

func TestApplyRefusesAnEditedPlan(t *testing.T) {
	s, step := planned(t)
	path := s.artifact("plans", "v1", review.PlanFile)
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(data, []byte("Create Payments"), []byte("Create Paymints"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("ok"))
	if r.code != messages.ExitValidationFailed || !strings.Contains(r.stderr, "was changed after plan wrote it") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if len(step.calls) != 0 {
		t.Errorf("calls %v", step.calls)
	}
}

func TestABlockedPlanIsNotApplied(t *testing.T) {
	s := newLoginSetup(t)
	step := &teamStep{jsmReadErr: fmt.Errorf("GET /v1/teams: 403: %w", httperr.ErrForbidden)}
	s.steps = []engine.Step{step}
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	s.run("login", "--site", "acme", "--org-id", "org-1")
	r := s.run("plan", "--jsm-plan", "premium")
	if r.code != messages.ExitBlocked || !strings.Contains(r.stderr, "plan v1 has 1 blocker to fix first") ||
		!strings.Contains(r.stdout, "To fix before apply can run:") || !strings.Contains(r.stdout, "teams_jsm_read") {
		t.Fatalf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if _, err := os.Stat(s.artifact("plans", "v1", review.RiskQueueFile)); err != nil {
		t.Errorf("no risk queue: %v", err)
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("ok")); r.code != messages.ExitBlocked {
		t.Errorf("apply: exit %d, stderr %q", r.code, r.stderr)
	}
	if len(step.calls) != 0 {
		t.Errorf("calls %v", step.calls)
	}
}

func TestApplyExitsPartialWhenAnItemFails(t *testing.T) {
	s, step := planned(t)
	step.fail["teams/PT2"] = fmt.Errorf("POST /v1/teams: 409: %w", httperr.ErrConflict)
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("ok"))
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "1 of 2 items are not done (1 failed)") ||
		!strings.Contains(r.stdout, "Not done:") || !strings.Contains(r.stdout, "teams/PT2/call_failed") {
		t.Errorf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
}

func TestApplyAsksInATerminal(t *testing.T) {
	t.Setenv("ACCESSIBLE", "")
	s, step := planned(t)
	s.terminal = true
	s.answers = [][]string{{"Casey left the company"}}
	s.choices = []string{"no"}
	if r := s.run("apply", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "nothing was written") {
		t.Fatalf("no: exit %d, %q\n%s", r.code, r.stderr, r.stdout)
	}
	if len(step.calls) != 0 {
		t.Fatalf("calls after no: %v", step.calls)
	}
	if q := s.asks[0]; len(q) != 1 || q[0].Title != "teams/PT1/member_dropped" || q[0].Validate("  ") == nil {
		t.Errorf("questions %+v", q)
	}

	s.answers = [][]string{{"Casey left the company"}}
	s.choices = []string{"yes"}
	r := s.run("apply", "--jsm-plan", "premium")
	if r.code != ExitOK || len(step.calls) != 2 {
		t.Fatalf("yes: exit %d, %q, calls %v", r.code, r.stderr, step.calls)
	}
	conf, _ := os.ReadFile(s.artifact("plans", "v1", review.ConfirmationFile))
	if !bytes.Contains(conf, []byte("Casey left the company")) || !bytes.Contains(conf, []byte(review.ViaPrompt)) {
		t.Errorf("confirmation.md:\n%s", conf)
	}
	// In a terminal the run draws a bar per step as it goes.
	if !strings.Contains(r.stdout, "\r\x1b[2Kteams") || !slices.Contains(tui.LastLines(r.stdout, 20),
		"teams                    [##############################] 2/2") {
		t.Errorf("no progress bar:\n%q", r.stdout)
	}
}

// With --output json, plan and apply each print one document, at the end.
func TestPlanAndApplyPrintOneJSONDocument(t *testing.T) {
	s, _ := planned(t)
	r := s.run("plan", "--jsm-plan", "premium", "--output", "json")
	var plan struct {
		Preflight map[string]any `json:"preflight"`
		Reads     []map[string]any
		Plan      struct {
			Version   int  `json:"version"`
			Unchanged bool `json:"unchanged"`
			Messages  []messages.Message
		} `json:"plan"`
	}
	if err := decodeOne(r.stdout, &plan); err != nil || r.code != ExitOK {
		t.Fatalf("plan: exit %d, %v\n%s", r.code, err, r.stdout)
	}
	if plan.Plan.Version != 1 || !plan.Plan.Unchanged || len(plan.Reads) != 2 || plan.Preflight == nil || len(plan.Plan.Messages) != 1 {
		t.Errorf("plan: %+v", plan)
	}
	r = s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("ok"), "--output", "json")
	var apply struct {
		Run struct {
			State  string         `json:"state"`
			Counts map[string]int `json:"counts"`
		} `json:"run"`
	}
	if err := decodeOne(r.stdout, &apply); err != nil || r.code != ExitOK {
		t.Fatalf("apply: exit %d, %v\n%s", r.code, err, r.stdout)
	}
	if apply.Run.State != "completed" || apply.Run.Counts["done"] != 2 {
		t.Errorf("apply: %+v", apply)
	}
}

// Second review: answering no in a terminal with --output json printed text,
// and errors after the pre-flight checks printed no document.
func TestEveryPathAfterTheChecksPrintsOneJSONDocument(t *testing.T) {
	s, step := planned(t)
	s.terminal = true
	s.answers = [][]string{{"Casey left the company"}}
	s.choices = []string{"no"}
	r := s.run("apply", "--jsm-plan", "premium", "--output", "json")
	var doc struct {
		Preflight    map[string]any `json:"preflight"`
		NotConfirmed bool           `json:"not_confirmed"`
	}
	if err := decodeOne(r.stdout, &doc); err != nil || r.code != ExitOK || !doc.NotConfirmed || doc.Preflight == nil {
		t.Errorf("no: exit %d, %v, %+v\n%s", r.code, err, doc, r.stdout)
	}
	if len(step.calls) != 0 {
		t.Errorf("calls %v", step.calls)
	}

	fresh := newLoginSetup(t)
	setCredentialsInEnv(t, credentials.Set{}, credentials.PagerDutyKey)
	r = fresh.run("plan", "--only", "pagerduty", "--output", "json")
	if err := decodeOne(r.stdout, &doc); err != nil || r.code != ExitError || doc.Preflight == nil {
		t.Errorf("plan --only without a JSM read: exit %d, %v\n%s", r.code, err, r.stdout)
	}
}

func decodeOne(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("more than one JSON document")
	}
	return nil
}

func TestWorkspaceAccountBinding(t *testing.T) {
	for _, change := range []string{"site", "organization", "PagerDuty account"} {
		t.Run(change, func(t *testing.T) {
			s, step := planned(t)
			file := s.acceptanceFile("accepted")
			var flags []string
			switch change {
			case "site":
				s.apis.Sites["other.atlassian.net"] = preflight.Tenant{CloudID: "cloud-2", ServerTime: loginNow}
				flags = []string{"--site", "other"}
			case "organization":
				s.apis.Orgs = append(s.apis.Orgs, preflight.Org{ID: "org-2"})
				s.apis.Members["org-2"] = []string{"operator@example.com"}
				flags = []string{"--org-id", "org-2"}
			case "PagerDuty account":
				s.apis.PagerDutyAccount = "other.pagerduty.com"
			}
			for _, args := range [][]string{{"login"}, {"plan", "--jsm-plan", "premium"}, {"apply", "--jsm-plan", "premium", "--accept-file", file}} {
				r := s.run(append(args, flags...)...)
				if r.code != messages.ExitValidationFailed {
					t.Fatalf("%s: exit %d: %s", args[0], r.code, r.stderr)
				}
			}
			if len(step.calls) != 0 || s.config().CloudID != "cloud-1" || s.config().OrgID != "org-1" {
				t.Fatal("changing accounts altered the original migration")
			}
		})
	}
}

func TestSeparateSitesNeedSeparateAcceptanceFiles(t *testing.T) {
	first, _ := planned(t)
	fileA := first.acceptanceFile("approved site A")
	second := newLoginSetup(t)
	step := &teamStep{}
	second.steps = []engine.Step{step}
	second.apis.Sites["other.atlassian.net"] = preflight.Tenant{CloudID: "cloud-2", ServerTime: loginNow}
	for _, args := range [][]string{{"login", "--site", "other", "--org-id", "org-1"}, {"plan", "--jsm-plan", "premium"}} {
		if r := second.run(args...); r.code != ExitOK {
			t.Fatal(r.stderr)
		}
	}
	if r := second.run("apply", "--jsm-plan", "premium", "--accept-file", fileA); r.code != messages.ExitValidationFailed || len(step.calls) != 0 {
		t.Fatalf("accepted another site's v1: %d, %s", r.code, r.stderr)
	}
	if r := second.run("apply", "--jsm-plan", "premium", "--accept-file", second.acceptanceFile("approved site B")); r.code != ExitOK || len(step.calls) != 2 {
		t.Fatalf("own v1: %d, %s", r.code, r.stderr)
	}
}

func TestPlanCannotReuseAnotherSitesMappings(t *testing.T) {
	s, _ := planned(t)
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("accepted")); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
	s.apis.Sites["other.atlassian.net"] = preflight.Tenant{CloudID: "cloud-2", ServerTime: loginNow}
	r := s.run("plan", "--only", "jsm", "--jsm-plan", "premium", "--site", "other")
	if r.code != messages.ExitValidationFailed {
		t.Fatalf("exit %d: %s", r.code, r.stdout+r.stderr)
	}
}

func TestReplanReplacesAnUnusableVersion(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			s, _ := planned(t)
			old := s.acceptanceFile("v1")
			path := s.artifact("plans", "v1", review.PlanFile)
			var err error
			if missing {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("edited"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "Plan v2:") {
				t.Fatalf("replan: %s", r.stdout+r.stderr)
			}
			if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", old); r.code != messages.ExitValidationFailed {
				t.Fatalf("old acceptance: %d", r.code)
			}
			if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFileFor(2, "v2")); r.code != ExitOK {
				t.Fatal(r.stderr)
			}
		})
	}
}

func TestInteractiveJSONUsesASeparatePromptStream(t *testing.T) {
	s, step := planned(t)
	t.Setenv("ACCESSIBLE", "1")
	var out, errOut bytes.Buffer
	hooks := defaultHooks()
	hooks.isTerminal = func(any) bool { return true }
	opts := Options{Keychain: s.kc, NewClients: s.clients, Now: func() time.Time { return loginNow },
		NewSteps: func(*Env, credentials.Set) ([]engine.Step, error) { return s.steps, nil }}
	code := executeWith(context.Background(), []string{"apply", "--output", "json", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("accepted")}, strings.NewReader("2\n"), &out, &errOut, opts, defaultBodies(), hooks)
	if code != ExitOK || !json.Valid(out.Bytes()) || !strings.Contains(errOut.String(), "Apply plan v1") || len(step.calls) != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestPlanRequiresFreshIdentityForRequestedRead(t *testing.T) {
	s, _ := planned(t)
	s.apis.AbilitiesErr = httperr.ErrUnavailable
	r := s.run("plan", "--only", "pagerduty")
	if r.code == ExitOK || !strings.Contains(r.stderr, "could not verify the PagerDuty migration account") {
		t.Fatalf("unverified read reused: %d: %s", r.code, r.stdout+r.stderr)
	}
}

func TestCredentialRotationKeepsTheWorkspaceBinding(t *testing.T) {
	s, _ := planned(t)
	t.Setenv(credentials.PagerDutyKey.EnvVar(), "pd-rotated-canary")
	s.apis.PagerDutyKey = "pd-rotated-canary"
	if r := s.run("login"); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("accepted")); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
}
