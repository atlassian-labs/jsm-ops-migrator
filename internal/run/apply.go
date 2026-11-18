package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

var (
	// ErrApplied means the plan version already has a run. A version is run
	// once: running it again would make its calls a second time. A new plan
	// maps what the run made and plans only what is left.
	ErrApplied = errors.New("the plan version was already applied")
	// ErrUnknownStep means the plan has a call for a step this
	// jsm-ops-migrator does not have, so nothing could make it.
	ErrUnknownStep = errors.New("the plan has calls for a step this jsm-ops-migrator does not have")
)

// recordEvery is how many items a run takes between two rewrites of the run
// record. Rewriting it after each one would cost time that grows with the
// run, and a run can have tens of thousands of items; the journal has every
// result anyway, and the record is rebuilt from it.
const recordEvery = 100

// CheckApply reports why v cannot be applied, before anyone is asked to
// confirm it: a run that is unfinished, a run of v, a blocker, a version
// that is not the latest, a plan.json that is not the one plan wrote, or a
// call of a step this build does not have.
func (e *Engine) CheckApply(ctx context.Context, v *PlanVersion) error {
	if err := e.CheckPlan(ctx, v); err != nil {
		return err
	}
	return e.CheckPlanSteps(v)
}

// CheckPlan validates the workspace files and journal before prompting or building write clients.
func (e *Engine) CheckPlan(ctx context.Context, v *PlanVersion) error {
	if r, err := e.Unfinished(ctx); err != nil {
		return err
	} else if r != nil {
		return r.UnfinishedError()
	}
	if r, err := e.RunOf(ctx, v.Version); err != nil {
		return err
	} else if r != nil {
		return fmt.Errorf("%w: plan v%d was applied by run %s; run plan to plan what changed since", ErrApplied, v.Version, r.ID)
	}
	latest, err := e.Latest(ctx)
	if err != nil {
		return err
	}
	if latest == nil || latest.Version != v.Version {
		return messages.ValidationFailed(fmt.Errorf("plan v%d is not the latest plan version", v.Version))
	}
	// A plan that crashed after writing its folder left files the operator
	// may have reviewed instead of v's; the next plan gets past them.
	if onDisk, err := e.highestFolder(); err != nil {
		return err
	} else if onDisk > v.Version {
		return messages.ValidationFailed(fmt.Errorf("%s was written by a plan that did not finish, so plan v%d may not be what you reviewed; run plan again",
			filepath.Join(store.ArtifactsDir, review.PlanDir(onDisk)), v.Version))
	}
	if v.Blockers > 0 {
		return v.Blocked()
	}
	return e.checkPlanFile(v, "run plan again")
}

// checkPlanFile refuses a version whose plan.json is missing or is not the
// one plan wrote: the operator reviewed the file, and the run makes the calls
// of state.db's copy.
func (e *Engine) checkPlanFile(v *PlanVersion, then string) error {
	path := filepath.Join(v.Dir, review.PlanFile)
	data, err := os.ReadFile(filepath.Join(e.Workspace.ArtifactsDir(), path))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return messages.ValidationFailed(fmt.Errorf("%s is missing; %s", path, then))
	case err != nil:
		return err
	case review.Hash(data) != v.Hash:
		return messages.ValidationFailed(fmt.Errorf("%s was changed after plan wrote it; %s", path, then))
	}
	return nil
}

// CheckPlanSteps checks the build can execute every call in the plan.
func (e *Engine) CheckPlanSteps(v *PlanVersion) error {
	if err := e.checkSteps(); err != nil {
		return err
	}
	return e.checkKnownSteps(v, "run plan again")
}

func (e *Engine) checkKnownSteps(v *PlanVersion, then string) error {
	for _, it := range v.Plan.Items {
		if it.Action.Calls() && e.step(it.Step()) == nil {
			return fmt.Errorf("%w: %s; %s", ErrUnknownStep, review.Quote(it.Step()), then)
		}
	}
	return nil
}

// CheckAccounts compares the live identities with the reads in the signed-off plan.
func (e *Engine) CheckAccounts(v *PlanVersion) error {
	for _, side := range []review.Side{review.PagerDuty, review.JSM} {
		i := slices.IndexFunc(v.Plan.Reads, func(r review.Read) bool { return r.Side == side })
		if i < 0 || !v.Plan.Reads[i].Valid(side) {
			return messages.ValidationFailed(fmt.Errorf("plan v%d has no verified %s account; run plan again", v.Version, side.Label()))
		}
		if e.Accounts[side] != v.Plan.Reads[i].MigrationAccount {
			return messages.ValidationFailed(fmt.Errorf("the %s account or organization differs from plan v%d; use the original accounts or a separate --workspace", side.Label(), v.Version))
		}
	}
	return nil
}

// RunResult is what Apply or Resume did.
type RunResult struct {
	Info RunInfo
	// Record is the run record as it was last written.
	Record review.Run
}

// Apply confirms v with c and runs it: every item in the plan's order, each
// through the journal. The confirmation is recorded before the first call,
// in state.db and as the version's confirmation.md.
//
// A failed call never stops the run: it is recorded, and the items after it
// are taken as planned, each step deciding from the results of the items it
// depends on. So a run whose items did not all end done is a result, not an
// error. Apply returns an error only when the run cannot go on: state.db or
// the record cannot be written.
//
// The run stops before its next item when Interrupted is closed or pause
// asks it to, and is marked paused. When ctx ends, it stops after the call
// in flight and is marked interrupted.
func (e *Engine) Apply(ctx context.Context, v *PlanVersion, c review.Confirmation) (RunResult, error) {
	if err := e.CheckApply(ctx, v); err != nil {
		return RunResult{}, err
	}
	if err := e.CheckAccounts(v); err != nil {
		return RunResult{}, err
	}
	if err := c.Check(v.Plan, v.Hash); err != nil {
		return RunResult{}, messages.ValidationFailed(err)
	}
	jsm, err := e.lookupRead(ctx, v)
	if err != nil {
		return RunResult{}, err
	}
	now := e.now()
	info := RunInfo{
		ID: newRunID(now), PlanVersion: v.Version, State: review.Running, StartedAt: now,
		Total: len(v.Plan.Items), JSMRead: jsm.ID(),
	}
	info.Record = filepath.Join(review.RunDir(info.ID), review.RunMarkdownFile)
	if err := e.confirm(ctx, info, c); err != nil {
		return RunResult{}, err
	}
	// From here on the run exists: a failure leaves it unfinished, for
	// resume, which rebuilds what is missing from state.db.
	if err := review.WriteConfirmation(e.Workspace, c, v.Plan); err != nil {
		return RunResult{Info: info}, err
	}
	// A fresh run has a new ID and cannot have a request until it is
	// advertised. Resume keeps the same ID, so it must honor a request
	// accepted during recovery or left unacknowledged by a crash.
	if err := e.Workspace.RemoveFile(PauseRequestFile); err != nil {
		return RunResult{Info: info}, err
	}
	return e.execute(ctx, newExecutor(e, &info, v.Plan, jsm, journal{}))
}

// execute runs x with the root files a running apply keeps: active-run.json,
// which tells pause a run is going, written first, and removed once the run
// stops. A crash leaves it, and the next apply or resume writes it again.
func (e *Engine) execute(ctx context.Context, x *executor) (RunResult, error) {
	info := x.info
	active, err := json.Marshal(ActiveRun{RunID: info.ID, PlanVersion: info.PlanVersion, StartedAt: info.StartedAt})
	if err != nil {
		return RunResult{Info: *info}, err
	}
	if err := e.Workspace.WriteFileAtomic(ActiveRunFile, active); err != nil {
		return RunResult{Info: *info}, err
	}
	record, err := x.run(ctx)
	if err != nil {
		return RunResult{Info: *info}, err
	}
	if err := e.Workspace.RemoveFile(ActiveRunFile); err != nil {
		return RunResult{Info: *info, Record: record}, err
	}
	return RunResult{Info: *info, Record: record}, nil
}

// lookupRead is the JSM read a run's lookups compare with: the latest
// complete one, which is the plan's own or one taken since by a plan that
// gave the same plan. Lookup counts an object that matches an item but is
// not in this read as made by the run, so the newest read before the run
// leaves the fewest objects the customer made to be taken for ours.
func (e *Engine) lookupRead(ctx context.Context, v *PlanVersion) (*discover.Snapshot, error) {
	latest, err := discover.LatestComplete(ctx, e.Store, review.JSM)
	if err != nil {
		return nil, err
	}
	if latest != nil && latest.ID() >= v.JSMRead {
		return latest, nil
	}
	return discover.Open(ctx, e.Store, v.JSMRead)
}

// confirm records the confirmation and the run it starts in one
// transaction, so a run never exists without the confirmation it ran under.
func (e *Engine) confirm(ctx context.Context, info RunInfo, c review.Confirmation) error {
	// What the operator typed is kept as confirmation.md shows it, so a
	// phone number in a reason is never stored, sealed or not.
	c.Accepted = slices.Clone(c.Accepted)
	for i := range c.Accepted {
		c.Accepted[i].Reason = messages.Clean(c.Accepted[i].Reason)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := e.Store.Cipher().Seal(confirmationRef(c.PlanVersion), data)
	if err != nil {
		return err
	}
	return e.Store.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO confirmations(plan_version, confirmed_at, account_id, via, record)
			VALUES (?, ?, ?, ?, ?)`, c.PlanVersion, formatTime(c.At), c.AccountID, c.Via, sealed); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO runs(id, plan_version, state, started_at, total, record, jsm_read)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, info.ID, info.PlanVersion, string(info.State), formatTime(info.StartedAt),
			info.Total, filepath.ToSlash(info.Record), info.JSMRead)
		return err
	})
}

// confirmation reads the confirmation of a version from state.db.
func (e *Engine) confirmation(ctx context.Context, version int) (review.Confirmation, error) {
	var sealed []byte
	if err := e.Store.DB().QueryRowContext(ctx, `SELECT record FROM confirmations WHERE plan_version = ?`,
		version).Scan(&sealed); err != nil {
		return review.Confirmation{}, fmt.Errorf("the confirmation of plan v%d: %w", version, err)
	}
	data, err := e.Store.Cipher().Open(confirmationRef(version), sealed)
	if err != nil {
		return review.Confirmation{}, err
	}
	var c review.Confirmation
	if err := json.Unmarshal(data, &c); err != nil {
		return review.Confirmation{}, fmt.Errorf("the confirmation of plan v%d: %w", version, err)
	}
	return c, nil
}

func confirmationRef(version int) store.FieldRef {
	return store.FieldRef{Table: "confirmations", Item: "v" + strconv.Itoa(version), Column: "record"}
}
