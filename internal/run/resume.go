package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// ErrNothingToResume means no run is unfinished.
var ErrNothingToResume = errors.New("no run is unfinished, so there is nothing to resume")

// Resumable is the unfinished run, as resume continues it.
type Resumable struct {
	Run     RunInfo
	Version *PlanVersion
	// Confirmation is the one the run started under. It names the account
	// whose API token makes the run's calls.
	Confirmation review.Confirmation
}

// CheckResume finds the unfinished run and reports why it cannot be resumed,
// before the pre-flight checks run: its plan.json is not the one plan wrote.
// It returns ErrNothingToResume when no run is unfinished. The steps are
// checked by CheckResumeSteps once they are built, after the pre-flight
// checks, as for apply.
func (e *Engine) CheckResume(ctx context.Context) (*Resumable, error) {
	r, err := e.Unfinished(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNothingToResume
	}
	v, err := e.Version(ctx, r.PlanVersion)
	if err != nil {
		return nil, err
	}
	// The run makes the calls of state.db's copy; a plan.json changed since
	// is no longer what the operator confirmed.
	then := fmt.Sprintf("put back the file plan wrote to resume run %s", r.ID)
	if err := e.checkPlanFile(v, then); err != nil {
		return nil, err
	}
	c, err := e.confirmation(ctx, v.Version)
	if err != nil {
		return nil, err
	}
	return &Resumable{Run: *r, Version: v, Confirmation: c}, nil
}

// CheckResumeSteps checks the build can make every call of the run's plan.
func (e *Engine) CheckResumeSteps(r *Resumable) error {
	if err := e.checkSteps(); err != nil {
		return err
	}
	return e.checkKnownSteps(r.Version, "resume it with the jsm-ops-migrator that started it")
}

// Resume continues the unfinished run on its own plan version, from the
// first item that has no final state. Before an item whose call is sending
// or unknown is taken further, its step looks it up. A create needs a
// recordable match to complete. An operator who confirmed the absence of a
// create can supply its key in skipCreate to leave it for a new plan after
// another lookup miss. An update can be sent again after a lookup miss once
// the attempt is old enough, up to maxAttempts in all.
//
// A run marked running is one whose command ended without marking it, such
// as in a crash: the caller holds the workspace lock, so no other command is
// making it. The run record and the version's confirmation.md are written
// again first, so both are right even if a crash cut them short.
//
// A pause request for the run stops it before its first item without a
// final state. The caller announces the run with Announce before its own
// checks, which removes a request left from before, so the one Resume finds
// was asked during them, after a clean pause and after a crash alike.
func (e *Engine) Resume(ctx context.Context, skipCreate string) (RunResult, error) {
	r, err := e.CheckResume(ctx)
	if err != nil {
		return RunResult{}, err
	}
	if err := e.CheckResumeSteps(r); err != nil {
		return RunResult{}, err
	}
	// The run's calls go to the accounts its plan read, whoever resumes it.
	if err := e.CheckAccounts(r.Version); err != nil {
		return RunResult{}, err
	}
	info := r.Run
	jsm, err := discover.Open(ctx, e.Store, info.JSMRead)
	if err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	if r.Version.Calls != 0 && !jsm.MatchesReaders(e.readers()) {
		err := messages.ValidationFailed(fmt.Errorf("run %s uses a JSM snapshot from different steps or read formats; resume with the build that started it", info.ID))
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	j, err := e.readJournal(ctx, info.ID)
	if err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	if skipCreate != "" {
		if err := checkSkipCreate(r.Version.Plan, j, skipCreate, e.now()); err != nil {
			return RunResult{}, err
		}
	}
	if err := review.WriteConfirmation(e.Workspace, r.Confirmation, r.Version.Plan); err != nil &&
		!errors.Is(err, review.ErrAlreadyConfirmed) {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	if _, err := e.Store.DB().ExecContext(ctx, `UPDATE runs SET state = ? WHERE id = ?`, string(review.Running), info.ID); err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	info.State = review.Running
	x := newExecutor(e, &info, r.Version.Plan, jsm, j)
	x.skipCreate = skipCreate
	if _, err := x.writeRecord(ctx, info); err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	return e.execute(ctx, x)
}

// checkSkipCreate restricts the flag to the first outstanding item so a
// mistaken key fails before resume can make any other write.
func checkSkipCreate(p review.Plan, j journal, key string, now time.Time) error {
	for _, it := range p.Items {
		row, seen := j.latest[it.Key]
		if seen && settled(row.state) {
			continue
		}
		if it.Key != key || it.Action != review.Create || !seen {
			break
		}
		if row.createAcknowledged() {
			return messages.ValidationFailed(fmt.Errorf("JSM acknowledged create %s. Resume without --skip-create to look up its object", review.Quote(it.Key)))
		}
		if !row.canSkipCreate() {
			return messages.ValidationFailed(fmt.Errorf("the journal lacks the receipt history needed to skip create %s. Resume without --skip-create to look up its object", review.Quote(it.Key)))
		}
		if after := row.started.Add(resendAfter); now.Before(after) {
			return messages.ValidationFailed(fmt.Errorf("wait until %s before skipping create %s, so an in-flight create has time to appear", messageTime(after), review.Quote(it.Key)))
		}
		return nil
	}
	return messages.ValidationFailed(errors.New("--skip-create must name the first unfinished item, which must be a journaled create"))
}
