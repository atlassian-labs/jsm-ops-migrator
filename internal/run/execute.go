package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// sending is the journal state of a call that was about to be made, or was
// made and has no answer recorded yet.
const sending review.EntryState = "sending"

// resendAfter gives writes time to appear before an update is retried or
// an operator can skip a create. A JSM write can take a moment to show, and the
// call can still be in flight well after its row was written: one attempt
// waits up to 30 seconds for its answer, and turning on Operations is polled
// for 2 minutes. A lookup sooner than this can miss what the call made.
const resendAfter = 10 * time.Minute

// maxAttempts caps update attempts even when lookups keep missing the change.
const maxAttempts = 3

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

// createAcknowledged reports evidence already in the journal that a create
// succeeded. The engine's receipt messages also cover steps without a status.
func (row journalRow) createAcknowledged() bool {
	return (row.status >= 200 && row.status < 300) || slices.ContainsFunc(row.msgs, func(m messages.Message) bool {
		return m.Code == "object_unidentified" || m.Code == "object_unrecorded" || m.Code == "create_acknowledged"
	})
}

// An older journal can have lost a receipt when a lookup replaced its
// messages. Only an in-flight row or an explicit record of the missing
// receipt can be used for an operator skip.
func (row journalRow) canSkipCreate() bool {
	return !row.createAcknowledged() && (row.state == sending || slices.ContainsFunc(row.msgs, func(m messages.Message) bool {
		return m.Code == "create_without_receipt"
	}))
}

// Preserve evidence about a create when validation or a lookup replaces its
// messages. Earlier acknowledged outcomes take precedence over lookup misses.
func (row *journalRow) preserveCreateEvidence(acknowledged, canSkip bool) {
	if row.state != review.Unknown || row.createAcknowledged() {
		return
	}
	if acknowledged {
		row.msgs = append(row.msgs, messages.Message{Class: messages.Information, Code: "create_acknowledged",
			Text: "JSM previously acknowledged this create. Resume can finish the item once its object is identified."})
	} else if canSkip && !row.canSkipCreate() {
		row.msgs = append(row.msgs, messages.Message{Class: messages.Information, Code: "create_without_receipt",
			Text: "The journal has no successful response for this create attempt."})
	}
}

// settled reports whether a row is the end of its item's turn: an item whose
// call is sending or unknown is looked up when the run is resumed.
func settled(s review.EntryState) bool {
	return s == review.Done || s == review.Failed || s == review.Declined
}

// counted is how an item whose latest row has state s counts in the run's
// written and failed: written once it has a final state, failed when that
// state is not done.
func counted(s review.EntryState) (written, failed int) {
	switch s {
	case "", sending:
		return 0, 0
	case review.Done:
		return 1, 0
	}
	return 1, 1
}

// Progress is one item's turn in a run, as Engine.Progress is told it.
type Progress struct {
	// Step is the item's step. Taken is how many of the step's items have a
	// final state, this one included, and Total how many the plan has.
	Step         string
	Taken, Total int
	// Entry is the item as the run record shows it, its messages cleaned.
	Entry review.Entry
}

// journal is what a run's journal holds when it is resumed: each item's
// latest row, and the highest sequence number.
type journal struct {
	latest   map[string]journalRow
	attempts map[string]int
	seq      int
}

type executor struct {
	e       *Engine
	info    *RunInfo
	plan    review.Plan
	jsm     *discover.Snapshot
	results Results
	seq     int
	// latest is each item's latest row from before this command, for a run
	// that is resumed.
	latest map[string]journalRow
	// skipCreates are operator-confirmed omissions for journaled items.
	skipCreates, skipVerifies map[string]bool
	// taken and total count each step's items: taken those with a final
	// state.
	taken, total map[string]int
}

func newExecutor(e *Engine, info *RunInfo, p review.Plan, jsm *discover.Snapshot, j journal) *executor {
	x := &executor{e: e, info: info, plan: p, jsm: jsm, results: Results{}, seq: j.seq, latest: j.latest,
		taken: map[string]int{}, total: map[string]int{}}
	for _, it := range p.Items {
		x.total[it.Step()]++
		if row, ok := j.latest[it.Key]; ok {
			if w, _ := counted(row.state); w > 0 {
				x.taken[it.Step()]++
			}
		}
	}
	return x
}

// run takes every item in order that has no final state yet, then marks the
// run and writes its record. An item whose call is sending or unknown, which
// only a resumed run has, is looked up first. Before each item it checks
// whether the operator asked the run to stop, and if so stops there, paused.
// An unresolved result defers its dependents. Other items can proceed in
// plan order. Cancellation and a too-recent update lookup miss stop the pass.
//
// The bookkeeping after a call uses a context that ctx ending does not
// cancel: a call that was answered is recorded whatever happens to the
// command.
func (x *executor) run(ctx context.Context) (review.Run, error) {
	db := context.WithoutCancel(ctx)
	state := x.info.State
	sinceRecord := 0
	for _, it := range x.plan.Items {
		row, seen := x.latest[it.Key]
		waiting := x.waitingForDependency(it)
		if seen && settled(row.state) && !waiting {
			x.results[it.Key] = Result{State: row.state, JSM: doneObject(row)}
			continue
		}
		if ctx.Err() != nil {
			state = review.Interrupted
			break
		}
		if x.stopRequested() {
			state = review.Paused
			break
		}
		if waiting {
			// Older journals may already have a result for this item.
			// Keep that row and carry the unresolved dependency forward.
			x.results[it.Key] = Result{State: review.Pending}
			continue
		}
		var res Result
		var stop bool
		var err error
		if it.Action == review.Verify {
			res, err = x.verify(ctx, db, it, row, seen)
		} else if seen {
			res, stop, err = x.settle(ctx, db, it, row)
		} else {
			res, stop, err = x.take(ctx, db, it, nil)
		}
		if err != nil {
			return review.Run{}, err
		}
		x.results[it.Key] = res
		if stop {
			state = review.Interrupted
			break
		}
		if sinceRecord++; sinceRecord == recordEvery {
			sinceRecord = 0
			if _, err := x.writeRecord(db, *x.info); err != nil {
				return review.Run{}, err
			}
		}
	}
	final := *x.info
	switch {
	case state != review.Running:
		final.State = state
	case x.hasUnsettled():
		// A call whose outcome is unknown may have made its object. Until
		// resume looks it up, the run is unfinished, so no new plan or run
		// plans the object again and sends a second create.
		final.State = review.Interrupted
	default:
		final.State = review.Completed
		final.EndedAt = x.e.now()
	}
	// A pause request is answered once the run stops, however it stops: left
	// in place, it would stop the run again the moment it is resumed.
	if err := x.e.Workspace.RemoveFile(PauseRequestFile); err != nil {
		return review.Run{}, err
	}
	// The run stays running in state.db until its final record is on disk, so
	// a record that cannot be written leaves it unfinished, for resume.
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

// Verification can be repeated after interruption. Its journal row holds no
// usable mapping until the account is ready, so its dependents stay pending.
func (x *executor) verify(ctx, db context.Context, it review.Item, row journalRow, seen bool) (Result, error) {
	before := row.state
	if !seen {
		x.seq++
		row = journalRow{seq: x.seq, key: it.Key, started: x.e.now()}
	}
	var out Outcome
	var err error
	if x.skipVerifies[it.Key] {
		out.Declined = true
		out.Messages = []messages.Message{{Class: messages.ManualTask, Code: "verification_skipped", Text: "The operator stopped waiting for this user. Review any granted site access before planning again."}}
	} else {
		out, err = x.e.step(it.Step()).Apply(ctx, it, x.deps(it))
	}
	row.finished, row.jsm, row.msgs = x.e.now(), nil, out.Messages
	switch {
	case errors.Is(err, httperr.ErrForbidden), errors.Is(err, httperr.ErrUnauthorized), errors.Is(err, httperr.ErrBadResponse), errors.Is(err, httperr.ErrInvalid):
		row.state = review.Failed
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "verification_failed", Text: fmt.Sprintf("Readiness check failed: %v. Correct the issue and review a fresh plan.", err)}}
	case err != nil:
		row.state = sending
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "verification_error", Text: fmt.Sprintf("Readiness check failed: %v. Resume to check again.", err)}}
	case out.Pending:
		row.state = sending
	case out.Declined:
		row.state = review.Declined
	case out.JSM != nil:
		row.state, row.jsm = review.Done, out.JSM
	default:
		return Result{}, errors.New("verification returned no object or pending status")
	}
	x.checkAnswer(it, &row)
	if err := x.finish(db, row, it, !seen, before); err != nil {
		return Result{}, err
	}
	return Result{State: entryState(it.Action, row.state), JSM: doneObject(row)}, nil
}

func doneObject(row journalRow) *review.Ref {
	if row.state == review.Done {
		return row.jsm
	}
	return nil
}

// stopRequested reports whether the operator asked the run to stop: the
// first Ctrl+C, or a pause request for this run. A request for another run
// is left from one that stopped before pause wrote it, and is not for this
// one. A request that cannot be read is taken as one for this run, because
// stopping only waits for resume.
func (x *executor) stopRequested() bool {
	select {
	case <-x.e.Interrupted:
		return true
	default:
	}
	req, err := readPauseRequest(x.e.Workspace)
	switch {
	case err != nil:
		return true
	case req == nil:
		return false
	}
	return req.RunID == x.info.ID
}

func (x *executor) hasUnsettled() bool {
	for _, r := range x.results {
		if !settled(r.State) {
			return true
		}
	}
	return false
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

func (x *executor) deps(it review.Item) Results {
	deps := make(Results, len(it.DependsOn))
	for _, d := range it.DependsOn {
		deps[d] = x.results[d]
	}
	return deps
}

// A step decides how to handle a dependency that failed or was declined.
// An unresolved dependency may still succeed, so leave the dependent pending.
func (x *executor) waitingForDependency(it review.Item) bool {
	for _, key := range it.DependsOn {
		if !settled(x.results[key].State) {
			return true
		}
	}
	return false
}

// crashPoint is a point in an item's turn where a crash leaves a different
// state behind: before the sending row commits, after it but before the
// call, after the call but before its answer commits, and after that.
type crashPoint int

const (
	beforeSending crashPoint = iota
	afterSending
	afterCall
	afterDone
)

// errCrashed is what a run returns when a test stops it as a crash would.
var errCrashed = errors.New("the run crashed")

func (x *executor) crashed(p crashPoint, key string) error {
	if x.e.crash != nil && x.e.crash(p, key) {
		return errCrashed
	}
	return nil
}

// take takes one item. An item that makes no call is recorded as done at
// once: a map links its PagerDuty object to the JSM object it names. An item
// that makes a call is recorded as sending before the call, and with its
// answer after. inFlight reports that ctx ended while the call was being
// made, so its row stays sending: the call may have reached JSM, and resume
// looks it up before anything else.
//
// prev is the item's latest row when a lookup on resume found that its call
// did not take effect: the call is sent again as the next attempt.
func (x *executor) take(ctx, db context.Context, it review.Item, prev *journalRow) (res Result, inFlight bool, err error) {
	deps := x.deps(it)
	x.seq++
	row := journalRow{seq: x.seq, key: it.Key, state: review.Done, started: x.e.now(), jsm: it.JSM}
	if !it.Action.Calls() {
		if it.Action == review.Skip && it.ApprovalFor != "" {
			row.msgs = []messages.Message{{Class: messages.Information, Code: "user_excluded", Text: it.Summary}}
		} else if it.UserMapping != nil && (it.Action == review.Skip || it.Action == review.Manual) {
			row.msgs = []messages.Message{{Class: messages.Information, Code: "user_review", Text: it.Summary}}
		}
		row.finished = row.started
		if err := x.finish(db, row, it, true, ""); err != nil {
			return Result{}, false, err
		}
		return Result{State: review.Done, JSM: it.JSM}, false, nil
	}

	row.attempt, row.state = 1, sending
	if prev != nil {
		row.attempt = prev.attempt + 1
	}
	if err := x.crashed(beforeSending, it.Key); err != nil {
		return Result{}, false, err
	}
	if err := x.send(db, row, it, prev); err != nil {
		return Result{}, false, err
	}
	if err := x.crashed(afterSending, it.Key); err != nil {
		return Result{}, false, err
	}
	step := x.e.step(it.Step())
	out, callErr := step.Apply(ctx, it, deps)
	if err := x.crashed(afterCall, it.Key); err != nil {
		return Result{}, false, err
	}
	row.finished, row.status, row.requestID, row.msgs = x.e.now(), out.Status, out.RequestID, out.Messages
	if it.Action == review.Create {
		// A step can fail to decode the response with the 2xx receipt
		// still available on the error, even when it returns no Outcome.
		var response *httperr.Error
		if errors.As(callErr, &response) && response.Status >= 200 && response.Status < 300 {
			row.status = response.Status
			if row.requestID == "" {
				row.requestID = response.RequestID
			}
		}
	}
	if callErr != nil && ctx.Err() != nil && (it.Action != review.Create || !row.createAcknowledged()) {
		return Result{State: review.Unknown}, true, nil
	}
	acknowledged := false
	switch {
	case callErr == nil && out.Declined:
		row.state, row.jsm, row.attempt = review.Declined, nil, 0
		if len(row.msgs) == 0 {
			row.msgs = []messages.Message{{Class: messages.Warning, Code: "declined",
				Text: "The step made no call, because of the result of an item it depends on."}}
		}
	case callErr == nil:
		acknowledged = it.Action == review.Create
		row.state = review.Done
		switch {
		case out.JSM != nil:
			row.jsm = out.JSM
		case it.Action == review.Create && it.PagerDuty != nil:
			x.unnamed(ctx, step, it, deps, &row)
		}
	default:
		acknowledged = x.failed(ctx, step, it, deps, callErr, &row) && it.Action == review.Create
	}
	if prev != nil {
		row.msgs = append([]messages.Message{sentAgain(it)}, row.msgs...)
	}
	x.checkAnswer(it, &row)
	row.preserveCreateEvidence(acknowledged, it.Action == review.Create)
	if err := x.finish(db, row, it, false, sending); err != nil {
		return Result{}, false, err
	}
	if err := x.crashed(afterDone, it.Key); err != nil {
		return Result{}, false, err
	}
	res = Result{State: row.state}
	if row.state == review.Done {
		res.JSM = row.jsm
	}
	return res, false, nil
}

func sentAgain(it review.Item) messages.Message {
	return messages.Message{Class: messages.Information, Code: "sent_again", Item: it.Key,
		Text: fmt.Sprintf("A lookup found that an earlier %s call did not take effect, so it was sent again.", it.Action)}
}

// send commits the sending row of a call before the call is made. When the
// call is sent again, the earlier attempt is marked failed in the same
// transaction, so the item never has two rows that may still take effect.
// The item then counts as its new row does, which is not yet written.
func (x *executor) send(ctx context.Context, row journalRow, it review.Item, prev *journalRow) error {
	var sealed []byte
	if prev != nil {
		data, err := json.Marshal([]messages.Message{sentAgain(it)})
		if err != nil {
			return err
		}
		if sealed, err = x.e.Store.Cipher().Seal(journalRef(x.info.ID, prev.seq), data); err != nil {
			return err
		}
	}
	var written, failed int
	err := x.e.Store.WithTx(ctx, func(tx *sql.Tx) error {
		if prev != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE journal SET state = ?, finished_at = ?, messages = ? WHERE run_id = ? AND seq = ?`,
				string(review.Failed), formatTime(row.started), sealed, x.info.ID, prev.seq); err != nil {
				return err
			}
			written, failed = x.delta(prev.state, sending)
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET written = written + ?, failed = failed + ? WHERE id = ?`,
				written, failed, x.info.ID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO journal(run_id, seq, item_key, attempt, state, started_at)
			VALUES (?, ?, ?, ?, ?, ?)`, x.info.ID, row.seq, row.key, row.attempt, string(row.state), formatTime(row.started))
		return err
	})
	if err != nil {
		return err
	}
	x.addCounts(it, written, failed)
	return nil
}

// settle looks up a call whose result has not been settled. A create may
// have succeeded even after repeated lookup misses, so it requires a match
// or an explicit operator skip. Only updates are eligible for a resend.
//
// The age is measured from the attempt's started_at, which is written before
// the call and never changed, and the count is the journal's attempt number,
// which rises with each attempt in the transaction that writes it, so no
// number of resumes gets an update sent sooner or more often. A lookup
// miss before an update is old enough stops the pass.
func (x *executor) settle(ctx, db context.Context, it review.Item, row journalRow) (Result, bool, error) {
	deps := x.deps(it)
	found, err := x.e.step(it.Step()).Lookup(ctx, it, deps, x.jsm)
	if err != nil && ctx.Err() != nil {
		return Result{State: review.Unknown}, true, nil
	}
	now := x.e.now()
	notFound := err == nil && found.Outcome == LookupNotFound
	foundObject := err == nil && found.Outcome == LookupFound
	acknowledged := it.Action == review.Create && (foundObject || row.createAcknowledged())
	canSkip := it.Action == review.Create && row.canSkipCreate()
	skipRequested := canSkip && x.skipCreates[it.Key]
	believed := row.started.Add(resendAfter)
	if it.Action == review.Update && notFound && !now.Before(believed) && row.attempt < maxAttempts {
		return x.take(ctx, db, it, &row)
	}
	before := row.state
	row.finished = now
	stop := false
	switch {
	case foundObject && found.JSM != nil:
		row.state, row.jsm = review.Done, found.JSM
		row.msgs = []messages.Message{{Class: messages.Information, Code: "outcome_found",
			Text: fmt.Sprintf("A lookup when the run was resumed identified the JSM object for the %s call.", it.Action)}}
	case notFound && skipRequested:
		row.state, row.jsm = review.Failed, nil
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "create_skipped",
			Text: "The operator verified no object was made in JSM and skipped this create. A new plan can account for the skipped item."}}
	case notFound && it.Action == review.Create:
		row.state, row.jsm = review.Unknown, nil
		text := "The lookup did not find the JSM object. The create may have succeeded, so resume will only look it up. Check JSM using the recorded request ID if available, then resume again."
		if canSkip {
			text += fmt.Sprintf(" If you verify that no object was made, pass item key %s to 'jsm-ops-migrator resume --skip-create' after %s.",
				review.Quote(it.Key), messageTime(believed))
		}
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "create_unresolved", Text: text}}
	case notFound && now.Before(believed):
		row.state, row.jsm, stop = review.Unknown, nil, true
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "lookup_too_soon",
			Text: fmt.Sprintf("A lookup when the run was resumed found nothing, but the %s call was sent at %s, and a lookup this soon "+
				"after a call can miss what it made, so the run stopped here. Resume it after %s: the call is looked up again then, "+
				"and sent again only if it still did not take effect.", it.Action, messageTime(row.started), messageTime(believed))}}
	case notFound:
		row.state, row.jsm = review.Failed, nil
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "not_sent_again",
			Text: fmt.Sprintf("A lookup when the run was resumed found that the %s call did not take effect, but it was tried %d times "+
				"already, so it is not sent again: a lookup that keeps finding nothing can be wrong. Check in JSM whether it took "+
				"effect before you plan again.", it.Action, row.attempt)}}
	default:
		row.state, row.jsm = review.Unknown, nil
		text := fmt.Sprintf("It is still not known whether the %s call took effect: the lookup when the run was resumed "+
			"could not tell. Check it in JSM; the next resume looks it up again.", it.Action)
		if err != nil {
			text += fmt.Sprintf(" The lookup failed: %v.", err)
		}
		if skipRequested {
			text += " Skipping requires a lookup that finds no matching object."
		}
		row.msgs = []messages.Message{{Class: messages.ManualTask, Code: "outcome_unknown", Text: text}}
	}
	x.checkAnswer(it, &row)
	row.preserveCreateEvidence(acknowledged, canSkip)
	if err := x.finish(db, row, it, false, before); err != nil {
		return Result{}, false, err
	}
	res := Result{State: row.state}
	if row.state == review.Done {
		res.JSM = row.jsm
	}
	return res, stop, nil
}

// messageTime is t as a message names it: in UTC, to the second.
func messageTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// unnamed reconciles a successful create whose response had no object ID.
// Completing it without a mapping would let the next plan create it again.
func (x *executor) unnamed(ctx context.Context, step Step, it review.Item, deps Results, row *journalRow) {
	found, err := step.Lookup(ctx, it, deps, x.jsm)
	if err == nil && found.Outcome == LookupFound && found.JSM != nil {
		row.jsm = found.JSM
		row.msgs = append(row.msgs, messages.Message{Class: messages.Information, Code: "object_found",
			Text: "The create call gave no JSM object, but a lookup found the one it made."})
		return
	}
	row.state, row.jsm = review.Unknown, nil
	text := "The create succeeded without a JSM object ID. Check JSM and resume to look up the object."
	if err != nil {
		text += fmt.Sprintf(" The lookup failed: %v.", err)
	}
	row.msgs = append(row.msgs, messages.Message{Class: messages.ManualTask, Code: "object_unidentified", Text: text})
}

// failed records a call that returned an error. An unknown outcome gets one
// immediate lookup, since a write can take time to show. If no object is
// identified the item stays unknown for resume. The return value reports a
// positive lookup, even when it did not return a recordable object.
func (x *executor) failed(ctx context.Context, step Step, it review.Item, deps Results, callErr error, row *journalRow) bool {
	class := messages.Classify(callErr, messages.Write)
	if class != messages.ManualTask && (it.Action != review.Create || !row.createAcknowledged()) {
		row.state, row.jsm = review.Failed, nil
		row.msgs = append(row.msgs, messages.Message{Class: class, Code: "call_failed",
			Text: fmt.Sprintf("The %s call failed: %v", it.Action, callErr)})
		return false
	}
	found, lookErr := step.Lookup(ctx, it, deps, x.jsm)
	acknowledged := lookErr == nil && found.Outcome == LookupFound
	if acknowledged && found.JSM != nil {
		row.state, row.jsm = review.Done, found.JSM
		row.msgs = append(row.msgs, messages.Message{Class: messages.Information, Code: "outcome_found",
			Text: fmt.Sprintf("JSM did not answer the %s call (%v), but a lookup found that it took effect.", it.Action, callErr)})
		return true
	}
	row.state, row.jsm = review.Unknown, nil
	text := fmt.Sprintf("It is not known whether the %s call took effect: %v. Check it in JSM. Resume looks it up again.", it.Action, callErr)
	if it.Action == review.Update {
		text += fmt.Sprintf(" Resume may retry the update if a lookup at least %d minutes later finds it did not take effect.",
			int(resendAfter/time.Minute))
	}
	if lookErr != nil {
		text += fmt.Sprintf(" The lookup failed too: %v.", lookErr)
	}
	row.msgs = append(row.msgs, messages.Message{Class: messages.ManualTask, Code: "outcome_unknown", Text: text})
	return acknowledged
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
	entry := review.Entry{Key: it.Key, Action: it.Action, State: entryState(it.Action, row.state), PagerDuty: it.PagerDuty, Messages: row.msgs}
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
// counts the menu reads always match it. before is the state the item's
// latest row had, so an item counts once however many rows it has.
func (x *executor) finish(ctx context.Context, row journalRow, it review.Item, insert bool, before review.EntryState) error {
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
	written, failed := x.delta(before, row.state)
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
		_, err = tx.ExecContext(ctx, `UPDATE runs SET written = written + ?, failed = failed + ? WHERE id = ?`,
			written, failed, x.info.ID)
		return err
	})
	if err != nil {
		return err
	}
	x.addCounts(it, written, failed)
	x.report(it, row)
	return nil
}

// delta is how the run's counts change when an item's latest row goes from
// state before to state after.
func (x *executor) delta(before, after review.EntryState) (written, failed int) {
	w0, f0 := counted(before)
	w1, f1 := counted(after)
	return w1 - w0, f1 - f0
}

// addCounts applies a committed change of the counts to the run in memory.
func (x *executor) addCounts(it review.Item, written, failed int) {
	x.info.Written += written
	x.info.Failed += failed
	x.taken[it.Step()] += written
}

func (x *executor) report(it review.Item, row journalRow) {
	if x.e.Progress == nil {
		return
	}
	step := it.Step()
	x.e.Progress(Progress{Step: step, Taken: x.taken[step], Total: x.total[step], Entry: review.Entry{
		Key: it.Key, Action: it.Action, State: entryState(it.Action, row.state), Attempts: row.attempt, Status: row.status,
		RequestID: row.requestID, PagerDuty: it.PagerDuty, JSM: row.jsm, At: row.finished, Messages: row.msgs,
	}})
}

func journalRef(run string, seq int) store.FieldRef {
	return store.FieldRef{Table: "journal", Item: run + "/" + strconv.Itoa(seq), Column: "messages"}
}
