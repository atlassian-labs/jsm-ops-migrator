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
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// The root files an apply keeps while it runs.
const (
	// ActiveRunFile names the run an apply is making, so pause can find it
	// without the data key.
	ActiveRunFile = "active-run.json"
	// PauseRequestFile asks the apply named in it to stop after its current
	// call.
	PauseRequestFile = "pause-request.json"
)

// ActiveRun is what active-run.json holds.
type ActiveRun struct {
	RunID       string    `json:"run_id"`
	PlanVersion int       `json:"plan_version"`
	StartedAt   time.Time `json:"started_at"`
}

var (
	// ErrApplied means the plan version already has a run. A version is run
	// once: running it again would make its calls a second time. A new plan
	// maps what the run made and plans only what is left.
	ErrApplied = errors.New("the plan version was already applied")
	// ErrUnknownStep means the plan has a call for a step this
	// jsm-ops-migrator does not have, so nothing could make it.
	ErrUnknownStep = errors.New("the plan has calls for a step this jsm-ops-migrator does not have")
)

// recordEvery is how many items apply takes between two rewrites of the run
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
	path := filepath.Join(v.Dir, review.PlanFile)
	data, err := os.ReadFile(filepath.Join(e.Workspace.ArtifactsDir(), path))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return messages.ValidationFailed(fmt.Errorf("%s is missing; run plan again", path))
	case err != nil:
		return err
	case review.Hash(data) != v.Hash:
		return messages.ValidationFailed(fmt.Errorf("%s was changed after plan wrote it; run plan again", path))
	}
	return nil
}

// CheckPlanSteps checks the build can execute every call in the plan.
func (e *Engine) CheckPlanSteps(v *PlanVersion) error {
	if err := e.checkSteps(); err != nil {
		return err
	}
	for _, it := range v.Plan.Items {
		if it.Action.Calls() && e.step(it.Step()) == nil {
			return fmt.Errorf("%w: %s; run plan again", ErrUnknownStep, review.Quote(it.Step()))
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

// RunResult is what Apply did.
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
// the record cannot be written. When ctx ends, the run stops after the call
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
	// A pause request left by an apply that has ended is not for this one.
	if err := e.Workspace.RemoveFile(PauseRequestFile); err != nil {
		return RunResult{Info: info}, err
	}
	active, err := json.Marshal(ActiveRun{RunID: info.ID, PlanVersion: info.PlanVersion, StartedAt: info.StartedAt})
	if err != nil {
		return RunResult{Info: info}, err
	}
	if err := e.Workspace.WriteFileAtomic(ActiveRunFile, active); err != nil {
		return RunResult{Info: info}, err
	}
	x := &executor{e: e, info: &info, plan: v.Plan, jsm: jsm, results: Results{}}
	record, err := x.run(ctx)
	if err != nil {
		return RunResult{Info: info}, err
	}
	if err := e.Workspace.RemoveFile(ActiveRunFile); err != nil {
		return RunResult{Info: info, Record: record}, err
	}
	return RunResult{Info: info, Record: record}, nil
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

func confirmationRef(version int) store.FieldRef {
	return store.FieldRef{Table: "confirmations", Item: "v" + strconv.Itoa(version), Column: "record"}
}

// sending is the journal state of a call that was about to be made, or was
// made and has no answer recorded yet.
const sending review.EntryState = "sending"

// journalRow is one row of the journal.
type journalRow struct {
	seq       int
	key       string
	attempt   int
	state     review.EntryState
	started   time.Time
	finished  time.Time
	status    int
	requestID string
	jsm       *review.Ref
	msgs      []messages.Message
}

type executor struct {
	e       *Engine
	info    *RunInfo
	plan    review.Plan
	jsm     *discover.Snapshot
	results Results
	seq     int
}

// run takes every item in order, then marks the run completed and writes
// its record. The bookkeeping after a call uses a context that ctx ending
// does not cancel: a call that was answered is recorded whatever happens to
// the command.
func (x *executor) run(ctx context.Context) (review.Run, error) {
	db := context.WithoutCancel(ctx)
	for i, it := range x.plan.Items {
		if ctx.Err() != nil {
			x.info.State = review.Interrupted
			break
		}
		res, inFlight, err := x.take(ctx, db, it)
		if err != nil {
			return review.Run{}, err
		}
		x.results[it.Key] = res
		if inFlight {
			x.info.State = review.Interrupted
			break
		}
		if (i+1)%recordEvery == 0 {
			if _, err := x.writeRecord(db, *x.info); err != nil {
				return review.Run{}, err
			}
		}
	}
	final := *x.info
	switch {
	case final.State != review.Running:
	case x.unsettled() > 0:
		// A call whose outcome is unknown may have made its object. Until
		// resume looks it up, the run is unfinished, so no new plan or run
		// plans the object again and sends a second create.
		final.State = review.Interrupted
	default:
		final.State = review.Completed
		final.EndedAt = x.e.now()
	}
	// Keep the journal recoverable until every final artifact is on disk.
	record, err := x.writeRecord(db, final)
	if err != nil {
		return review.Run{}, err
	}
	var ended any
	if !final.EndedAt.IsZero() {
		ended = formatTime(final.EndedAt)
	}
	if _, err := x.e.Store.DB().ExecContext(db, `UPDATE runs SET state = ?, ended_at = ? WHERE id = ?`,
		string(final.State), ended, final.ID); err != nil {
		return review.Run{}, err
	}
	*x.info = final
	return record, nil
}

// unsettled counts the items whose call may have taken effect without the
// run knowing what it made.
func (x *executor) unsettled() int {
	n := 0
	for _, r := range x.results {
		if r.State == review.Unknown {
			n++
		}
	}
	return n
}

func (x *executor) writeRecord(ctx context.Context, info RunInfo) (review.Run, error) {
	r, err := x.e.record(ctx, info, x.plan)
	if err != nil {
		return review.Run{}, err
	}
	if _, err := review.WriteRun(x.e.Workspace, r); err != nil {
		return review.Run{}, err
	}
	return r, nil
}

// take takes one item. An item that makes no call is recorded as done at
// once: a map links its PagerDuty object to the JSM object it names. An item
// that makes a call is recorded as sending before the call, and with its
// answer after. inFlight reports that ctx ended while the call was being
// made, so its row stays sending: the call may have reached JSM, and resume
// looks it up before anything else.
func (x *executor) take(ctx, db context.Context, it review.Item) (res Result, inFlight bool, err error) {
	deps := make(Results, len(it.DependsOn))
	for _, d := range it.DependsOn {
		deps[d] = x.results[d]
	}
	x.seq++
	row := journalRow{seq: x.seq, key: it.Key, state: review.Done, started: x.e.now(), jsm: it.JSM}
	if !it.Action.Calls() {
		row.finished = row.started
		if err := x.finish(db, row, it, true); err != nil {
			return Result{}, false, err
		}
		return Result{State: review.Done, JSM: it.JSM}, false, nil
	}

	row.attempt, row.state = 1, sending
	if _, err := x.e.Store.DB().ExecContext(db, `INSERT INTO journal(run_id, seq, item_key, attempt, state, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`, x.info.ID, row.seq, row.key, row.attempt, string(row.state), formatTime(row.started)); err != nil {
		return Result{}, false, err
	}
	step := x.e.step(it.Step())
	out, callErr := step.Apply(ctx, it, deps)
	if callErr != nil && ctx.Err() != nil {
		return Result{State: review.Unknown}, true, nil
	}
	row.finished, row.status, row.requestID, row.msgs = x.e.now(), out.Status, out.RequestID, out.Messages
	switch {
	case callErr == nil && out.Declined:
		row.state, row.jsm, row.attempt = review.Declined, nil, 0
		if len(row.msgs) == 0 {
			row.msgs = []messages.Message{{Class: messages.Warning, Code: "declined",
				Text: "The step made no call, because of the result of an item it depends on."}}
		}
	case callErr == nil:
		row.state = review.Done
		if out.JSM != nil {
			row.jsm = out.JSM
		}
	default:
		x.failed(ctx, step, it, deps, callErr, &row)
	}
	x.checkAnswer(it, &row)
	if err := x.finish(db, row, it, false); err != nil {
		return Result{}, false, err
	}
	res = Result{State: row.state}
	if row.state == review.Done {
		res.JSM = row.jsm
	}
	return res, false, nil
}

// failed records a call that returned an error. An unknown outcome is looked
// up once, at once: found means done. Anything else leaves it unknown, and
// it is never sent again within the run, because a JSM write can take a
// moment to show, so a lookup right after the call can miss what it made.
func (x *executor) failed(ctx context.Context, step Step, it review.Item, deps Results, callErr error, row *journalRow) {
	class := messages.Classify(callErr, messages.Write)
	if class != messages.ManualTask {
		row.state, row.jsm = review.Failed, nil
		row.msgs = append(row.msgs, messages.Message{Class: class, Code: "call_failed",
			Text: fmt.Sprintf("The %s call failed: %v", it.Action, callErr)})
		return
	}
	found, lookErr := step.Lookup(ctx, it, deps, x.jsm)
	if lookErr == nil && found.Outcome == LookupFound && found.JSM != nil {
		row.state, row.jsm = review.Done, found.JSM
		row.msgs = append(row.msgs, messages.Message{Class: messages.Information, Code: "outcome_found",
			Text: fmt.Sprintf("JSM did not answer the %s call (%v), but a lookup found that it took effect.", it.Action, callErr)})
		return
	}
	row.state, row.jsm = review.Unknown, nil
	text := fmt.Sprintf("It is not known whether the %s call took effect: %v. resume looks it up again and sends it "+
		"only if it did not; until then, check it in JSM.", it.Action, callErr)
	if lookErr != nil {
		text += fmt.Sprintf(" The lookup failed too: %v.", lookErr)
	}
	row.msgs = append(row.msgs, messages.Message{Class: messages.ManualTask, Code: "outcome_unknown", Text: text})
}

// checkAnswer checks what the step answered as the run record will check
// it, because an answer the record cannot hold would make every later
// rewrite of the record fail. Messages it cannot hold are replaced by one
// that says so. A JSM object it cannot hold is not recorded, so the call is
// unknown: it was made, but what it made is not known, and resume looks it
// up instead of sending it again.
func (x *executor) checkAnswer(it review.Item, row *journalRow) {
	// A request ID comes from the API's answer, so it is kept as the run
	// record shows it.
	row.requestID = messages.Clean(row.requestID)
	for i := range row.msgs {
		row.msgs[i].Item = it.Key
	}
	entry := review.Entry{Key: it.Key, Action: it.Action, State: row.state, PagerDuty: it.PagerDuty, Messages: row.msgs}
	if err := (review.Run{ID: x.info.ID, State: review.Running, Entries: []review.Entry{entry}}).Check(); err != nil {
		row.msgs = []messages.Message{{Class: messages.Warning, Code: "answer_unrecorded", Item: it.Key,
			Text: fmt.Sprintf("The step's messages about this call cannot be recorded: %v", err)}}
	}
	if row.jsm == nil {
		return
	}
	entry.JSM, entry.Messages = row.jsm, nil
	if err := (review.Run{ID: x.info.ID, State: review.Running, Entries: []review.Entry{entry}}).Check(); err != nil {
		row.state, row.jsm = review.Unknown, nil
		row.msgs = append(row.msgs, messages.Message{Class: messages.ManualTask, Code: "object_unrecorded", Item: it.Key,
			Text: fmt.Sprintf("The %s call was made, but the JSM object the step gave cannot be recorded: %v. resume looks it up; "+
				"until then, check it in JSM.", it.Action, err)})
	}
}

// finish writes the item's final row, its mapping and the run's counts in
// one transaction, so the mapping never runs ahead of the journal and the
// counts the menu reads always match it.
func (x *executor) finish(ctx context.Context, row journalRow, it review.Item, insert bool) error {
	var sealed []byte
	if len(row.msgs) > 0 {
		// Texts are kept as the run record shows them: a call's error can
		// quote what the API was sent, and a phone number is never stored,
		// sealed or not.
		for i := range row.msgs {
			row.msgs[i].Item = it.Key
			row.msgs[i] = row.msgs[i].Redacted()
		}
		data, err := json.Marshal(row.msgs)
		if err != nil {
			return err
		}
		if sealed, err = x.e.Store.Cipher().Seal(journalRef(x.info.ID, row.seq), data); err != nil {
			return err
		}
	}
	var jsmType, jsmID any
	if row.jsm != nil {
		jsmType, jsmID = row.jsm.Type, row.jsm.ID
	}
	var status any
	if row.status != 0 {
		status = row.status
	}
	var requestID any
	if row.requestID != "" {
		requestID = row.requestID
	}
	failed := 0
	if row.state != review.Done {
		failed = 1
	}
	err := x.e.Store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		if insert {
			_, err = tx.ExecContext(ctx, `INSERT INTO journal(run_id, seq, item_key, attempt, state, started_at, finished_at,
				status, request_id, jsm_type, jsm_id, messages) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				x.info.ID, row.seq, row.key, row.attempt, string(row.state), formatTime(row.started), formatTime(row.finished),
				status, requestID, jsmType, jsmID, sealed)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE journal SET attempt = ?, state = ?, finished_at = ?, status = ?, request_id = ?,
				jsm_type = ?, jsm_id = ?, messages = ? WHERE run_id = ? AND seq = ?`,
				row.attempt, string(row.state), formatTime(row.finished), status, requestID, jsmType, jsmID, sealed, x.info.ID, row.seq)
		}
		if err != nil {
			return err
		}
		if row.state == review.Done && it.PagerDuty != nil && row.jsm != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO mappings(pagerduty_type, pagerduty_id, jsm_type, jsm_id, run_id,
				item_key, mapped_at) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(pagerduty_type, pagerduty_id, jsm_type) DO UPDATE SET jsm_id = excluded.jsm_id,
				run_id = excluded.run_id, item_key = excluded.item_key, mapped_at = excluded.mapped_at`,
				it.PagerDuty.Type, it.PagerDuty.ID, row.jsm.Type, row.jsm.ID, x.info.ID, it.Key, formatTime(row.finished)); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE runs SET written = written + 1, failed = failed + ? WHERE id = ?`, failed, x.info.ID)
		return err
	})
	if err != nil {
		return err
	}
	x.info.Written++
	x.info.Failed += failed
	return nil
}

func journalRef(run string, seq int) store.FieldRef {
	return store.FieldRef{Table: "journal", Item: run + "/" + strconv.Itoa(seq), Column: "messages"}
}
