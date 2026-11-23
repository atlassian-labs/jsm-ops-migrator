package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// runApply writes the latest plan version to JSM, once it is confirmed. A
// plan with a blocker exits with ExitBlocked, and a plan.json changed since
// plan wrote it, or an acceptance file that does not accept the plan, with
// ExitValidationFailed. Without a terminal, the acceptance file is the
// confirmation, and its absence exits with ExitAwaitingInput. The pre-flight
// checks run again first, because a credential can expire between plan and
// apply, and one of the checks apply writes with that could not run stops it
// before anything is written.
//
// A run whose items do not all end done exits with ExitPartial: the run
// carried on past each failure, and its record says what is left. So does a
// run that the first Ctrl+C, or pause in another window, stopped before its
// next item; resume finishes it.
func runApply(ctx context.Context, env *Env, f ApplyFlags) error {
	st, err := env.OpenStore(ctx, store.OpenExisting)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	e := env.newEngine(st)
	if r, err := e.Unfinished(ctx); err != nil {
		return err
	} else if r != nil {
		return fmt.Errorf("apply: %w", r.UnfinishedError())
	}
	v, err := e.Latest(ctx)
	if err != nil {
		return err
	}
	if v == nil {
		return errors.New("apply: there is no plan to apply yet; run 'jsm-ops-migrator plan' first")
	}
	// Every refusal comes before the confirmation is asked for, so nobody
	// fills in a file for a plan that can never be applied.
	creds, err := env.credentials(st)
	if err != nil {
		return err
	}
	e.Redactor = creds.set.Redactor()
	if err := e.CheckPlan(ctx, v); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	var accept *review.Acceptance
	switch {
	case f.AcceptFile != "":
		data, err := os.ReadFile(f.AcceptFile)
		if err != nil {
			return fmt.Errorf("apply: reading the acceptance file: %w", err)
		}
		a, err := review.ParseAcceptance(data)
		if err != nil {
			return messages.ValidationFailed(fmt.Errorf("apply: %w", err))
		}
		accept = &a
	case !env.Interactive:
		if e.Steps, err = env.Steps(creds.set); err != nil {
			return err
		}
		if err := e.CheckPlanSteps(v); err != nil {
			return err
		}
		template := filepath.Join(env.Workspace.ArtifactsDir(), v.Dir, review.AcceptanceTemplateFile)
		if _, err := os.Stat(template); errors.Is(err, os.ErrNotExist) {
			return messages.ValidationFailed(fmt.Errorf("apply: %s is missing; run plan again, which writes a new version with every file",
				displayPath(template)))
		} else if err != nil {
			return err
		}
		return awaitingInput(fmt.Errorf("apply: plan v%d needs a confirmation: fill in %s and give it with --accept-file",
			v.Version, displayPath(template)))
	}

	report, err := runChecks(ctx, env, creds, preflight.All, true)
	if err != nil {
		return err
	}
	out := applyOutput{Preflight: report}
	if env.Output == OutputText {
		if err := writeChecks(env.Stdout, "Pre-flight checks", report.Checks); err != nil {
			return err
		}
	}
	if err := writePreflightError("apply", report); err != nil {
		env.summarize(checksSummary(report.Checks))
		return out.emit(env, err)
	}
	// The API token runs every call, and it is personal, so its account is
	// who confirms. No API shows whether it is an org admin.
	if report.Operator == nil || report.Operator.AccountID == "" {
		return out.emit(env, errors.New("apply: the org admin check did not find your account, so the confirmation cannot name it"))
	}
	e.Accounts = migrationAccounts(report, env.Config.OrgID)
	if err := e.CheckAccounts(v); err != nil {
		return out.emit(env, err)
	}
	env.Config.CloudID = report.CloudID
	if e.Steps, err = env.Steps(creds.set); err != nil {
		return out.emit(env, err)
	}
	if err := e.CheckPlanSteps(v); err != nil {
		return out.emit(env, err)
	}
	pv := planResult(env.Workspace, engine.PlanResult{Version: v})
	if env.Output == OutputText {
		if err := writePlan(env.Stdout, pv); err != nil {
			return err
		}
	}

	// A Ctrl+C during the checks or the prompts stops apply before the run
	// exists, so nothing waits for resume; nobody is asked to confirm a plan
	// after asking to stop.
	stopped := func() error {
		return out.emit(env, fmt.Errorf("apply: stopped at Ctrl+C: plan v%d was not applied, and nothing was written", v.Version))
	}
	if interrupted(env) {
		return stopped()
	}
	c := review.Confirmation{PlanVersion: v.Version, PlanHash: v.Hash, AccountID: report.Operator.AccountID}
	if accept != nil {
		c.Via = review.ViaFile
		if c.Accepted, err = accept.Accept(v.Plan, v.Hash); err != nil {
			return out.emit(env, messages.ValidationFailed(fmt.Errorf("apply: %w", err)))
		}
	}
	if env.Interactive {
		if accept == nil {
			c.Via = review.ViaPrompt
			if c.Accepted, err = askReasons(ctx, env, v); err != nil {
				return out.emit(env, err)
			}
		}
		yes, err := askToApply(ctx, env, v, c.AccountID)
		if err != nil {
			return out.emit(env, err)
		}
		if !yes {
			out.NotConfirmed = true
			env.summarize([]string{fmt.Sprintf("Plan v%d was not applied: nothing was written", v.Version)})
			if env.Output == OutputText {
				if _, err := fmt.Fprintf(env.Stdout, "\nPlan v%d was not applied: nothing was written.\n", v.Version); err != nil {
					return err
				}
			}
			return out.emit(env, nil)
		}
	}
	c.At = env.Now()
	if interrupted(env) {
		return stopped()
	}

	view := startRunView(env, e)
	res, err := e.Apply(ctx, v, c)
	view.close()
	return finishRun(env, "apply", out, res, err)
}

// writeNeeds are the checks apply and resume write on: the API token makes every call,
// and the others identify the accounts the plan was made for and the
// operator the confirmation names. The checks of the plan, the clock and the
// APIs still being built keep their warnings: no call depends on them alone.
var writeNeeds = []preflight.ID{preflight.PagerDutyKey, preflight.Site, preflight.AtlassianToken, preflight.OrgKey, preflight.OrgAdmin}

// writePreflightError also rejects required checks that could not run.
func writePreflightError(command string, report preflight.Report) error {
	if err := checksError(command, report); err != nil {
		return err
	}
	var titles []string
	for _, id := range writeNeeds {
		if c, ok := report.Find(id); !ok || (c.Status != preflight.OK && c.Status != preflight.Warning) {
			titles = append(titles, id.Title())
		}
	}
	if len(titles) == 0 {
		return nil
	}
	again := "they can"
	if len(titles) == 1 {
		again = "it can"
	}
	return fmt.Errorf("%s: nothing was written, because %s could not run: %s; run %s again once %s",
		command, count(len(titles), "check"), strings.Join(titles, ", "), command, again)
}

// askReasons asks for a reason for each warning of the plan, as the
// acceptance file would give it.
func askReasons(ctx context.Context, env *Env, v *engine.PlanVersion) ([]review.Accepted, error) {
	var warnings []messages.Message
	for _, m := range v.Plan.Messages {
		if m.Class == messages.Warning {
			warnings = append(warnings, m)
		}
	}
	if len(warnings) == 0 {
		return nil, nil
	}
	qs := make([]tui.Question, len(warnings))
	for i, m := range warnings {
		qs[i] = tui.Question{
			Title:       messages.Clean(m.ID()),
			Description: messages.Clean(m.Text),
			Placeholder: "why this is acceptable",
			Validate: func(s string) error {
				if !review.IsReason(s) {
					return errors.New("give a reason")
				}
				return nil
			},
		}
	}
	heading := fmt.Sprintf("Accept the %s of plan v%d", messages.Quantity(len(warnings), messages.Warning), v.Version)
	answers, err := env.Ask(ctx, heading, qs)
	if err != nil {
		return nil, err
	}
	accepted := make([]review.Accepted, len(warnings))
	for i, m := range warnings {
		accepted[i] = review.Accepted{ID: m.ID(), Reason: answers[i]}
	}
	return accepted, nil
}

// askToApply is the last question before the first write.
func askToApply(ctx context.Context, env *Env, v *engine.PlanVersion, account string) (bool, error) {
	choice, err := env.Choose(ctx,
		fmt.Sprintf("Apply plan v%d to %s?", v.Version, env.Config.Site),
		fmt.Sprintf("It makes %s with the API token of account %s, which the confirmation records.",
			count(v.Calls, "call"), account),
		[]tui.Option{{Key: "yes", Label: "Yes, apply it"}, {Key: "no", Label: "No, stop here"}}, "no")
	return choice == "yes", err
}

// applyOutput is what apply reports: in JSON, one document at the end.
type applyOutput struct {
	Preflight preflight.Report `json:"preflight"`
	// NotConfirmed means the operator answered no at the prompt, so
	// nothing was written.
	NotConfirmed bool       `json:"not_confirmed,omitempty"`
	Run          *runOutput `json:"run,omitempty"`
}

func (o applyOutput) emit(env *Env, err error) error {
	if env.Output != OutputJSON {
		return err
	}
	if werr := writeJSON(env.Stdout, o); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

// runOutput is a run as apply reports it. Counts are null when the journal
// could not be read, so nothing is known about the items, and Record is left
// out when the run record could not be written. NotDone are the messages of
// the items that did not end done, cleaned as in the run record.
type runOutput struct {
	ID          string             `json:"id"`
	PlanVersion int                `json:"plan_version"`
	State       review.RunState    `json:"state"`
	Counts      *runCounts         `json:"counts"`
	Record      string             `json:"record,omitempty"`
	NotDone     []messages.Message `json:"not_done,omitempty"`
}

type runCounts struct {
	Total    int `json:"total"`
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Unknown  int `json:"unknown"`
	Declined int `json:"declined"`
	Pending  int `json:"pending"`
}

func runResult(ws *store.Workspace, res engine.RunResult) *runOutput {
	out := &runOutput{ID: res.Info.ID, PlanVersion: res.Info.PlanVersion, State: res.Info.State}
	if res.RecordWritten {
		out.Record = filepath.Join(ws.ArtifactsDir(), res.Info.Record)
	}
	if res.Record.ID == "" {
		return out
	}
	c := res.Record.Counts()
	out.Counts = &runCounts{Total: res.Info.Total, Done: c.Done, Failed: c.Failed, Unknown: c.Unknown,
		Declined: c.Declined, Pending: res.Info.Total - c.Done - c.Failed - c.Unknown - c.Declined}
	for _, e := range res.Record.Entries {
		if e.State == review.Done {
			continue
		}
		for _, m := range e.Messages {
			m = m.Redacted()
			m.Item = messages.Clean(m.Item)
			out.NotDone = append(out.NotDone, m)
		}
	}
	return out
}

// maxNotDone keeps the terminal readable after a run with many failures;
// run.md lists every one.
const maxNotDone = 20

func writeRun(w io.Writer, r *runOutput) error {
	var b strings.Builder
	b.WriteString("\n" + runLine(r) + ".\n")
	if len(r.NotDone) > 0 {
		b.WriteString("\nNot done:\n")
		if err := messages.WriteText(&b, r.NotDone[:min(len(r.NotDone), maxNotDone)]); err != nil {
			return err
		}
		if len(r.NotDone) > maxNotDone {
			fmt.Fprintf(&b, "  and %d more\n", len(r.NotDone)-maxNotDone)
		}
	}
	if r.Record == "" {
		b.WriteString("The run record could not be written; state.db keeps every result.\n")
	} else {
		fmt.Fprintf(&b, "Record: %s (with run.json and mapping.csv beside it).\n", displayPath(r.Record))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// runLine says how far the run got: "Run <id> of plan v1: 1 of 2 items done;
// 1 failed".
func runLine(r *runOutput) string {
	if r.Counts == nil {
		return fmt.Sprintf("Run %s of plan v%d: its journal could not be read, so what it did is not known", r.ID, r.PlanVersion)
	}
	line := fmt.Sprintf("Run %s of plan v%d: %d of %d items done", r.ID, r.PlanVersion, r.Counts.Done, r.Counts.Total)
	if rest := notDone(*r.Counts); rest != "" {
		line += "; " + rest
	}
	return line
}

// notDone says how many items are not done, by state: "2 failed, 1 unknown".
func notDone(c runCounts) string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{c.Failed, "failed"}, {c.Unknown, "unknown"}, {c.Declined, "declined"}, {c.Pending, "not taken yet"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	return strings.Join(parts, ", ")
}

func runSummary(r *runOutput) []string {
	if r.Record == "" {
		return []string{runLine(r), "The run record could not be written"}
	}
	return []string{runLine(r), "Record: " + displayPath(r.Record)}
}

func have(n int) string {
	if n == 1 {
		return " has"
	}
	return " have"
}

// runError is the command's error for a run: nil when every item is done,
// and otherwise one that exits with ExitPartial.
func runError(command string, r *runOutput) error {
	if r.Counts == nil {
		return messages.PartiallyComplete(fmt.Errorf("%s: the journal of run %s could not be read, so what it did is not known", command, r.ID))
	}
	switch {
	case r.State == review.Interrupted && r.Counts.Pending == 0 && r.Counts.Unknown > 0:
		return messages.PartiallyComplete(fmt.Errorf("%s: %s an unknown outcome, so run %s is unfinished; "+
			"run 'jsm-ops-migrator resume' to look them up before anything else",
			command, count(r.Counts.Unknown, "call")+have(r.Counts.Unknown), r.ID))
	case r.State != review.Completed:
		return messages.PartiallyComplete(fmt.Errorf("%s: run %s was %s after %d of %d items; run 'jsm-ops-migrator resume' to finish it",
			command, r.ID, r.State, r.Counts.Total-r.Counts.Pending, r.Counts.Total))
	case r.Counts.Done < r.Counts.Total:
		return messages.PartiallyComplete(fmt.Errorf("%s: %d of %d items are not done (%s); see %s",
			command, r.Counts.Total-r.Counts.Done, r.Counts.Total, notDone(*r.Counts), displayPath(r.Record)))
	}
	return nil
}
