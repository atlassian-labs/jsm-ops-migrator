package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// runPlan runs the pre-flight checks, reads each side whose checks found
// every credential, and builds a new plan version from both reads. A missing
// credential exits with ExitAwaitingInput after the other side has been read
// and saved, so the next plan needs only the missing side; with --only, the
// other side's latest read is used again. A plan with a blocker exits with
// ExitBlocked once its review files are written, so the operator sees every
// problem in one run.
func runPlan(ctx context.Context, env *Env, f PlanFlags) error {
	st, err := env.OpenStore(ctx, store.OpenCreate)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	e := env.newEngine(st)
	if r, err := e.Unfinished(ctx); err != nil {
		return err
	} else if r != nil {
		return fmt.Errorf("plan: %w", r.UnfinishedError())
	}
	creds, err := env.credentials(st)
	if err != nil {
		return err
	}

	checks := preflight.All
	switch f.Only {
	case "pagerduty":
		checks = preflight.PagerDutySide
	case "jsm":
		checks = preflight.JSMSide
	}
	report, err := runChecks(ctx, env, creds, checks, f.Only != "pagerduty")
	if err != nil {
		return err
	}
	out := planOutput{Preflight: report}
	if env.Output == OutputText {
		if err := writeChecks(env.Stdout, "Pre-flight checks", report.Checks); err != nil {
			return err
		}
	}
	summary := checksSummary(report.Checks)
	if len(report.With(preflight.Failed)) > 0 {
		env.summarize(summary)
		return out.emit(env, checksError("plan", report))
	}
	if report.CloudID != "" {
		env.Config.CloudID = report.CloudID
	}
	if e.Steps, err = env.Steps(creds.set); err != nil {
		return out.emit(env, err)
	}

	e.Accounts = migrationAccounts(report, env.Config.OrgID)
	reads := map[review.Side]*discover.Snapshot{}
	for _, side := range []review.Side{review.PagerDuty, review.JSM} {
		if (f.Only != "" && f.Only != string(side)) || sideMissing(report, side) || !e.Accounts[side].Valid(side) {
			continue
		}
		s, err := e.Read(ctx, side)
		if err != nil {
			return out.emit(env, fmt.Errorf("reading %s: %w", side.Label(), err))
		}
		reads[side] = s
		out.Reads = append(out.Reads, readResult(s, false))
	}
	// A side that could not be read for a missing credential is saved for
	// the next plan: the operator gives the credential and plans again.
	if err := checksError("plan", report); err != nil {
		if err := out.writeReads(env); err != nil {
			return err
		}
		env.summarize(append(summary, out.readLines()...))
		return out.emit(env, err)
	}
	for _, side := range []review.Side{review.PagerDuty, review.JSM} {
		if reads[side] != nil {
			continue
		}
		if f.Only == "" || f.Only == string(side) {
			return out.emit(env, fmt.Errorf("plan: could not verify the %s migration account; check the pre-flight report", side.Label()))
		}
		s, err := e.LatestRead(ctx, side)
		if err != nil {
			return out.emit(env, err)
		}
		if s == nil {
			return out.emit(env, fmt.Errorf("plan --only %s: there is no %s read to build on yet, as only one made by this build's steps "+
				"can be; run 'jsm-ops-migrator plan' without --only first", f.Only, side.Label()))
		}
		reads[side] = s
		out.Reads = append(out.Reads, readResult(s, true))
	}
	if err := out.writeReads(env); err != nil {
		return err
	}

	res, err := e.Plan(ctx, reads[review.PagerDuty], reads[review.JSM])
	if err != nil {
		return out.emit(env, fmt.Errorf("plan: %w", err))
	}
	out.Plan = planResult(env.Workspace, res)
	if res.Unchanged {
		applied, err := e.RunOf(ctx, res.Version.Version)
		if err != nil {
			return out.emit(env, err)
		}
		if applied != nil {
			out.Plan.AppliedBy = applied.ID
		}
	}
	if env.Output == OutputText {
		if err := writePlan(env.Stdout, out.Plan); err != nil {
			return err
		}
	}
	env.summarize(planSummary(out.Plan))
	if res.Version.Blockers > 0 {
		return out.emit(env, fmt.Errorf("plan: %w", res.Version.Blocked()))
	}
	return out.emit(env, nil)
}

// sideChecks are the checks only one side needs; the secret storage check
// is in both.
var sideChecks = map[review.Side][]preflight.ID{
	review.PagerDuty: {preflight.PagerDutyKey},
	review.JSM: {preflight.Site, preflight.AtlassianToken, preflight.OrgKey, preflight.OrgAdmin, preflight.JSMPlan,
		preflight.Clock, preflight.Services, preflight.UserNotifications, preflight.GlobalEscalations},
}

// sideMissing reports whether a check of side is missing a credential or an
// answer, so the side cannot be read.
func sideMissing(report preflight.Report, side review.Side) bool {
	for _, id := range sideChecks[side] {
		if c, ok := report.Find(id); ok && c.Status == preflight.Missing {
			return true
		}
	}
	return false
}

func migrationAccounts(report preflight.Report, orgID string) map[review.Side]review.MigrationAccount {
	if check, _ := report.Find(preflight.OrgKey); check.Status != preflight.OK {
		orgID = ""
	}
	return map[review.Side]review.MigrationAccount{
		review.PagerDuty: {ID: report.PagerDutyAccount},
		review.JSM:       {ID: report.CloudID, OrgID: orgID},
	}
}

// resolved are the credentials a command runs with, and where each came
// from.
type resolved struct {
	set     credentials.Set
	sources map[credentials.Kind]string
	where   string
}

func (e *Env) credentials(st *store.Store) (resolved, error) {
	vault := e.vault(st)
	set, sources, err := resolveCredentials(vault)
	if err != nil {
		return resolved{}, err
	}
	return resolved{set: set, sources: sources, where: vault.Where()}, nil
}

// runChecks runs the pre-flight checks: plan runs them before it reads
// anything, and apply again before it writes anything, because a credential
// can expire between the two.
func runChecks(ctx context.Context, env *Env, creds resolved, checks []preflight.ID, needsJSMPlan bool) (preflight.Report, error) {
	jsmPlan := ""
	var err error
	if needsJSMPlan {
		if jsmPlan, err = statedJSMPlan(ctx, env); err != nil {
			return preflight.Report{}, err
		}
	}
	site, err := normalizeSite(env.Config.Site)
	if err != nil {
		return preflight.Report{}, fmt.Errorf("site %q: %w", env.Config.Site, err)
	}
	// The API clients choose the Atlassian API host by the site's name.
	env.Config.Site = site
	cloudID, err := cloudIDFor(env, site)
	if err != nil {
		return preflight.Report{}, err
	}
	if env.Config.PagerDutyRegion, err = pagerDutyRegion(env.Config.PagerDutyRegion); err != nil {
		return preflight.Report{}, err
	}
	clients, err := env.Clients(creds.set)
	if err != nil {
		return preflight.Report{}, err
	}
	return preflight.Run(ctx, clients, preflight.Input{
		Credentials:   creds.set,
		Sources:       creds.sources,
		SecretStorage: creds.where,
		Site:          site,
		CloudID:       cloudID,
		OrgID:         env.Config.OrgID,
		JSMPlan:       jsmPlan,
		Checks:        checks,
		Now:           env.Now,
	})
}

// planOutput is what plan reports: in JSON, one document at the end.
type planOutput struct {
	Preflight preflight.Report `json:"preflight"`
	Reads     []readOutput     `json:"reads,omitempty"`
	Plan      *planVersion     `json:"plan,omitempty"`
}

type readOutput struct {
	Side     review.Side `json:"side"`
	At       time.Time   `json:"at"`
	Complete bool        `json:"complete"`
	// Reused means the read is from an earlier plan: plan --only read the
	// other side again.
	Reused bool `json:"reused,omitempty"`
}

func readResult(s *discover.Snapshot, reused bool) readOutput {
	return readOutput{Side: s.Side(), At: s.At(), Complete: s.Complete(), Reused: reused}
}

// planVersion is a plan version as plan and apply report it. Its messages
// are cleaned, as in the review files.
type planVersion struct {
	Version   int  `json:"version"`
	Unchanged bool `json:"unchanged"`
	// AppliedBy is the run that applied an unchanged version: nothing has
	// changed since, so there is nothing new to apply.
	AppliedBy string             `json:"applied_by,omitempty"`
	Dir       string             `json:"dir"`
	Hash      string             `json:"hash"`
	Summary   review.Summary     `json:"summary"`
	Messages  []messages.Message `json:"messages"`
}

func planResult(ws *store.Workspace, res engine.PlanResult) *planVersion {
	v := res.Version
	msgs := make([]messages.Message, len(v.Plan.Messages))
	for i, m := range messages.ByClass(v.Plan.Messages) {
		msgs[i] = m.Redacted()
		msgs[i].Item = messages.Clean(m.Item)
	}
	return &planVersion{
		Version: v.Version, Unchanged: res.Unchanged, Dir: filepath.Join(ws.ArtifactsDir(), v.Dir), Hash: v.Hash,
		Summary: review.Summarize(v.Plan), Messages: msgs,
	}
}

// emit ends the command: in JSON, it writes the one document, then returns
// err either way.
func (o planOutput) emit(env *Env, err error) error {
	if env.Output != OutputJSON {
		return err
	}
	if werr := writeJSON(env.Stdout, o); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

func (o planOutput) readLines() []string {
	lines := make([]string, len(o.Reads))
	for i, r := range o.Reads {
		state := "complete"
		if !r.Complete {
			state = "incomplete"
		}
		if r.Reused {
			lines[i] = fmt.Sprintf("Using the %s read of %s: %s", r.Side.Label(), r.At.Format(time.RFC3339), state)
		} else {
			lines[i] = fmt.Sprintf("Read %s: %s", r.Side.Label(), state)
		}
	}
	return lines
}

func (o planOutput) writeReads(env *Env) error {
	if env.Output != OutputText || len(o.Reads) == 0 {
		return nil
	}
	_, err := io.WriteString(env.Stdout, "\n"+strings.Join(o.readLines(), "\n")+"\n")
	return err
}

// writePlan prints the plan: its counts per step, what needs the operator,
// and where its review files are. Every blocker is listed, because nothing
// can go on until each one is fixed.
func writePlan(w io.Writer, v *planVersion) error {
	var b strings.Builder
	switch {
	case v.AppliedBy != "":
		fmt.Fprintf(&b, "\nPlan v%d is unchanged since run %s applied it: there is nothing new to apply.\n", v.Version, v.AppliedBy)
	case v.Unchanged:
		fmt.Fprintf(&b, "\nPlan v%d is unchanged, so no new version was written.\n", v.Version)
	default:
		fmt.Fprintf(&b, "\nPlan v%d: %s.\n", v.Version, count(v.Summary.Calls, "call"))
	}
	for _, s := range v.Summary.Steps {
		var parts []string
		for _, a := range review.Actions {
			if n := s.Of(a); n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, a))
			}
		}
		fmt.Fprintf(&b, "  %s: %s\n", s.Step, strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "Messages: %s.\n", v.Summary.Messages)
	fmt.Fprintf(&b, "Review files: %s (plan.md lists every call, risk-queue.md every message).\n", displayPath(v.Dir))
	var blockers []messages.Message
	for _, m := range v.Messages {
		if m.Class == messages.Blocker {
			blockers = append(blockers, m)
		}
	}
	if len(blockers) > 0 {
		b.WriteString("\nTo fix before apply can run:\n")
		if err := messages.WriteText(&b, blockers); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func planSummary(v *planVersion) []string {
	first := fmt.Sprintf("Plan v%d: %s; %s", v.Version, count(v.Summary.Calls, "call"), v.Summary.Messages)
	if v.Unchanged {
		first = fmt.Sprintf("Plan v%d is unchanged: %s; %s", v.Version, count(v.Summary.Calls, "call"), v.Summary.Messages)
	}
	return []string{first, "Review files: " + displayPath(v.Dir)}
}

// cloudIDFor is the cloudId the site's answer is compared with: one given for
// this run with --cloud-id or the environment, or else the saved one, but only
// for the saved site. The saved cloudId belongs to that site, so checking
// another site against it would report a move that never happened.
func cloudIDFor(env *Env, site string) (string, error) {
	saved, err := LoadConfigFile(env.Workspace)
	if err != nil {
		return "", err
	}
	if env.Config.CloudID != saved.CloudID {
		return env.Config.CloudID, nil
	}
	if savedSite, err := normalizeSite(saved.Site); err == nil && savedSite == site {
		return saved.CloudID, nil
	}
	return "", nil
}

// statedJSMPlan is the plan from --jsm-plan, the environment or config.yaml.
// An interactive operator who has not stated one is asked, and the answer is
// saved for the next runs.
func statedJSMPlan(ctx context.Context, env *Env) (string, error) {
	plan := strings.ToLower(strings.TrimSpace(env.Config.JSMPlan))
	if plan != "" || !env.Interactive {
		return plan, nil
	}
	options := make([]tui.Option, len(preflight.JSMPlans))
	for i, p := range preflight.JSMPlans {
		options[i] = tui.Option{Key: p, Label: capitalize(p)}
	}
	plan, err := env.Choose(ctx, "Which JSM plan does the site have?",
		"No API reports it, so jsm-ops-migrator asks. The answer is saved in config.yaml.", options, "")
	if err != nil {
		return "", err
	}
	cfg, err := LoadConfigFile(env.Workspace)
	if err != nil {
		return "", err
	}
	cfg.JSMPlan = plan
	return plan, SaveConfig(env.Workspace, cfg)
}

// checksError is the command's error for the report: a failed check is an
// error, and a missing credential or answer exits with ExitAwaitingInput.
func checksError(command string, report preflight.Report) error {
	failedChecks, missingChecks := report.With(preflight.Failed), report.With(preflight.Missing)
	switch {
	case len(failedChecks) > 0:
		msg := fmt.Sprintf("%s: %s failed: %s", command, count(len(failedChecks), "check"), checkTitles(failedChecks))
		if len(missingChecks) > 0 {
			msg += "; missing: " + checkTitles(missingChecks)
		}
		return errors.New(msg)
	case len(missingChecks) > 0:
		return awaitingInput(fmt.Errorf("%s: missing: %s", command, checkTitles(missingChecks)))
	}
	return nil
}

func checkTitles(checks []preflight.Check) string {
	titles := make([]string, len(checks))
	for i, c := range checks {
		titles[i] = c.Title
	}
	return strings.Join(titles, ", ")
}

func (e *Env) newEngine(st *store.Store) *engine.Engine {
	return &engine.Engine{Store: st, Workspace: e.Workspace, Now: e.Now}
}
