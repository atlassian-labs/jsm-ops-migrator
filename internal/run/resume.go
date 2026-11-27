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

// Resume continues the unfinished run on its own plan version. It looks up
// unresolved creates in plan order and defers their dependents until the
// outcome is known. An operator who confirmed the absence of several
// creates can supply their keys in skipCreates to leave them for a new plan
// after fresh lookup misses. An update can be sent again after a lookup miss
// once the attempt is old enough, up to maxAttempts in all.
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
func (e *Engine) Resume(ctx context.Context, skipCreates ...string) (RunResult, error) {
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
	p, err := e.approvedPlan(ctx, r.Version, j)
	if err != nil {
		return RunResult{}, err
	}
	skips, err := checkSkipCreates(p, j, skipCreates, e.now())
	if err != nil {
		return RunResult{}, err
	}
	if err := review.WriteConfirmation(e.Workspace, r.Confirmation, r.Version.Plan); err != nil &&
		!errors.Is(err, review.ErrAlreadyConfirmed) {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	if _, err := e.Store.DB().ExecContext(ctx, `UPDATE runs SET state = ? WHERE id = ?`, string(review.Running), info.ID); err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	info.State = review.Running
	x := newExecutor(e, &info, p, jsm, j)
	x.skipCreates = skips
	if _, err := x.writeRecord(ctx, info); err != nil {
		return e.stoppedRun(ctx, info, r.Version.Plan, err)
	}
	return e.execute(ctx, x)
}

// Validate every requested key before any item proceeds, so a typo halfway
// through a batch cannot leave a partly processed request.
func checkSkipCreates(p review.Plan, j journal, keys []string, now time.Time) (map[string]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	requested := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key == "" || requested[key] {
			return nil, messages.ValidationFailed(errors.New("each --skip-create argument must be a distinct, nonempty item key"))
		}
		requested[key] = true
	}
	invalid := errors.New("each --skip-create key must name a journaled, unresolved create in this run")
	checked := 0
	for _, it := range p.Items {
		if !requested[it.Key] {
			continue
		}
		row, seen := j.latest[it.Key]
		if it.Action != review.Create || !seen || settled(row.state) {
			return nil, messages.ValidationFailed(invalid)
		}
		if row.createAcknowledged() {
			return nil, messages.ValidationFailed(fmt.Errorf("JSM acknowledged create %s. Omit this key from --skip-create to look up its object", review.Quote(it.Key)))
		}
		if !row.canSkipCreate() {
			return nil, messages.ValidationFailed(fmt.Errorf("the journal lacks the receipt history needed to skip create %s. Omit this key from --skip-create to look up its object", review.Quote(it.Key)))
		}
		if after := row.started.Add(resendAfter); now.Before(after) {
			return nil, messages.ValidationFailed(fmt.Errorf("wait until %s before skipping create %s, so an in-flight create has time to appear", messageTime(after), review.Quote(it.Key)))
		}
		checked++
	}
	if checked != len(keys) {
		return nil, messages.ValidationFailed(invalid)
	}
	return requested, nil
}
