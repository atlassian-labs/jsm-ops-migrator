package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// runResume continues the unfinished run on its own plan version, from the
// first item without a result. It looks up unresolved creates and can retry
// unresolved updates after a lookup miss. Pre-flight checks run before any
// writes. The run keeps its confirmation and needs a token for the account
// that confirmed it.
//
// With no unfinished run, resume says so and exits 0. A run that stops again
// or does not end with every item done exits with ExitPartial.
//
// The run is announced to pause before anything else, so pause asked during
// the checks is accepted and stops the run before its first item, whether
// the run was paused or crashed.
func runResume(ctx context.Context, env *Env, f ResumeFlags) (err error) {
	// The wrapper holds the workspace lock, so the status cannot change
	// between this keyless check and opening the encrypted store.
	status, err := engine.ReadStatus(ctx, env.Workspace)
	if err != nil {
		return err
	}
	if status.Run == nil || status.Run.State == review.Completed {
		if f.SkipCreate != "" {
			return messages.ValidationFailed(engine.ErrNothingToResume)
		}
		env.summarize([]string{"Nothing to resume: no run is unfinished"})
		if env.Output == OutputJSON {
			return writeJSON(env.Stdout, struct {
				Run *runOutput `json:"run"`
			}{})
		}
		_, err := io.WriteString(env.Stdout, "Nothing to resume: no run is unfinished.\n")
		return err
	}
	u := status.Run
	if err := engine.Announce(env.Workspace, engine.ActiveRun{RunID: u.ID, PlanVersion: u.PlanVersion, StartedAt: u.StartedAt}); err != nil {
		return err
	}
	defer func() {
		if werr := engine.Withdraw(env.Workspace); werr != nil {
			err = errors.Join(err, werr)
		}
	}()
	st, err := env.OpenStore(ctx, store.OpenExisting)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	e := env.newEngine(st)
	creds, err := env.credentials(st)
	if err != nil {
		return err
	}
	r, err := e.CheckResume(ctx)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
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
	if err := writePreflightError("resume", report); err != nil {
		env.summarize(checksSummary(report.Checks))
		return out.emit(env, err)
	}
	// The confirmation names the account whose API token makes the run's
	// calls; another account's token would make it untrue.
	switch {
	case report.Operator == nil || report.Operator.AccountID == "":
		return out.emit(env, errors.New("resume: the org admin check did not find your account, so it cannot be matched with the confirmation"))
	case report.Operator.AccountID != r.Confirmation.AccountID:
		return out.emit(env, messages.ValidationFailed(fmt.Errorf(
			"resume: run %s was confirmed for the API token of account %s, and this token is account %s's; "+
				"resume it with the token of the account that confirmed it", r.Run.ID, r.Confirmation.AccountID, report.Operator.AccountID)))
	}
	e.Accounts = migrationAccounts(report, env.Config.OrgID)
	if err := e.CheckAccounts(r.Version); err != nil {
		return out.emit(env, fmt.Errorf("resume: %w", err))
	}
	env.Config.CloudID = report.CloudID
	if e.Steps, err = env.Steps(creds.set); err != nil {
		return out.emit(env, err)
	}
	if err := e.CheckResumeSteps(r); err != nil {
		return out.emit(env, fmt.Errorf("resume: %w", err))
	}
	if env.Output == OutputText {
		if _, err := fmt.Fprintf(env.Stdout, "\nResuming run %s of plan v%d: %d of %d items have a result.\n",
			r.Run.ID, r.Run.PlanVersion, r.Run.Written, r.Run.Total); err != nil {
			return err
		}
	}
	if interrupted(env) {
		return out.emit(env, fmt.Errorf("resume: stopped at Ctrl+C before run %s went on", r.Run.ID))
	}

	view := startRunView(env, e)
	res, err := e.Resume(ctx, f.SkipCreate)
	view.close()
	return finishRun(env, "resume", out, res, err)
}

// runPause asks the apply or resume running in another window to stop
// before its next item. It needs neither the data key nor the lock, so it
// never waits for the run, and with no run going it says so and exits 0.
func runPause(ctx context.Context, env *Env, _ PauseFlags) error {
	active, err := engine.FindActive(env.Workspace)
	if err != nil {
		return err
	}
	if active != nil {
		if err := engine.RequestPause(env.Workspace, active.RunID, env.Now()); err != nil {
			return err
		}
	}
	status, err := engine.ReadStatus(ctx, env.Workspace)
	if err != nil {
		return err
	}
	out := pauseOutput{Running: active != nil, Requested: active != nil}
	if r := status.Run; r != nil {
		out.Run = &pausedRun{ID: r.ID, PlanVersion: r.PlanVersion, State: r.State, Written: r.Written, Total: r.Total, Failed: r.Failed}
	}
	lines := pauseLines(out)
	env.summarize(lines)
	if env.Output == OutputJSON {
		return writeJSON(env.Stdout, out)
	}
	_, err = io.WriteString(env.Stdout, strings.Join(lines, "\n")+"\n")
	return err
}

// pauseOutput is what pause reports. Run is the latest run, as state.db
// keeps it in clear.
type pauseOutput struct {
	Running   bool       `json:"running"`
	Requested bool       `json:"requested"`
	Run       *pausedRun `json:"run,omitempty"`
}

type pausedRun struct {
	ID          string          `json:"id"`
	PlanVersion int             `json:"plan_version"`
	State       review.RunState `json:"state"`
	Written     int             `json:"written"`
	Total       int             `json:"total"`
	Failed      int             `json:"failed"`
}

func pauseLines(o pauseOutput) []string {
	r := o.Run
	switch {
	case o.Requested && r != nil:
		return []string{
			fmt.Sprintf("Asked run %s of plan v%d to pause after its current call; %d of %d items have a result so far.",
				r.ID, r.PlanVersion, r.Written, r.Total),
			"A call that waits on JSM, such as turning on Operations, can take a few minutes. Then run 'jsm-ops-migrator resume' to continue.",
		}
	case o.Requested:
		return []string{"Asked the running apply to pause after its current call."}
	case r != nil && r.State != review.Completed:
		return []string{
			"No apply is running, so there is nothing to pause.",
			fmt.Sprintf("Run %s of plan v%d is %s after %d of %d items; run 'jsm-ops-migrator resume' to continue it.",
				r.ID, r.PlanVersion, r.State, r.Written, r.Total),
		}
	}
	return []string{"No apply is running, so there is nothing to pause."}
}
