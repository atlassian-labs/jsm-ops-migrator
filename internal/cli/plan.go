package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// runPlan runs the pre-flight checks. Reading both systems and building the
// plan come with the Run engine. A missing credential exits with
// ExitAwaitingInput after the other side's checks have run.
func runPlan(ctx context.Context, env *Env, f PlanFlags) error {
	st, err := env.OpenStore(ctx, store.OpenCreate)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	vault := env.vault(st)
	creds, sources, err := resolveCredentials(vault)
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
	jsmPlan := ""
	if f.Only != "pagerduty" {
		if jsmPlan, err = statedJSMPlan(ctx, env); err != nil {
			return err
		}
	}
	site, err := normalizeSite(env.Config.Site)
	if err != nil {
		return fmt.Errorf("site %q: %w", env.Config.Site, err)
	}
	// The API clients choose the Atlassian API host by the site's name.
	env.Config.Site = site
	cloudID, err := cloudIDFor(env, site)
	if err != nil {
		return err
	}

	if env.Config.PagerDutyRegion, err = pagerDutyRegion(env.Config.PagerDutyRegion); err != nil {
		return err
	}
	clients, err := env.Clients(creds)
	if err != nil {
		return err
	}
	report, err := preflight.Run(ctx, clients, preflight.Input{
		Credentials:   creds,
		Sources:       sources,
		SecretStorage: vault.Where(),
		Site:          site,
		CloudID:       cloudID,
		OrgID:         env.Config.OrgID,
		JSMPlan:       jsmPlan,
		Checks:        checks,
		Now:           env.Now,
	})
	if err != nil {
		return err
	}
	if err := writeReport(env, "Pre-flight checks", report); err != nil {
		return err
	}
	summary := checksSummary(report.Checks)
	if err := checksError("plan", report); err != nil {
		env.summarize(summary)
		return err
	}
	env.summarize(append(summary, "Reading PagerDuty and JSM is not implemented yet"))
	return fmt.Errorf("plan: reading PagerDuty and JSM is %w", ErrNotImplemented)
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
