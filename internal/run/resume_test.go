package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func (w *workspace) activeRun() run.ActiveRun {
	w.t.Helper()
	data, err := w.ws.ReadFile(run.ActiveRunFile)
	if err != nil {
		w.t.Fatal(err)
	}
	var a run.ActiveRun
	if err := json.Unmarshal(data, &a); err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *workspace) resume(e *run.Engine) run.RunResult {
	w.t.Helper()
	res, err := e.Resume(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

// once fails unless every call the remote got was made exactly once.
func once(t *testing.T, r *remote, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if n := r.objects[k]; n != 1 {
			t.Errorf("%s was made %d times, want once", k, n)
		}
	}
	for k := range r.objects {
		if !slices.Contains(keys, k) {
			t.Errorf("%s was made, but is not a call of the plan", k)
		}
	}
}

func allDone(t *testing.T, res run.RunResult) {
	t.Helper()
	if res.Info.State != review.Completed || res.Info.Failed != 0 || res.Info.Written != res.Info.Total {
		t.Errorf("run %s, %d of %d written, %d failed; want completed and all done",
			res.Info.State, res.Info.Written, res.Info.Total, res.Info.Failed)
	}
	for key, s := range states(res.Record) {
		if s != review.Done {
			t.Errorf("%s is %s", key, s)
		}
	}
}

// A pause request stops the run before its next item: what was taken stays
// done, the rest waits, and resume takes the rest once.
func TestAPauseRequestStopsTheRunBeforeItsNextItem(t *testing.T) {
	keys := []string{"teams/T1", "teams/T2", "teams/T3"}
	for i, key := range keys[:2] {
		t.Run(key, func(t *testing.T) {
			w := newWorkspace(t)
			r := &remote{objects: map[string]int{}}
			teams := newStep("teams", r)
			teams.plan = teamsPlan("T1", "T2", "T3")
			teams.during = func(it review.Item) {
				if it.Key == key {
					if err := run.RequestPause(w.ws, w.activeRun().RunID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			e := w.engine(teams)
			res := w.apply(e, w.plan(e).Version)

			want := map[string]review.EntryState{}
			for j, k := range keys {
				want[k] = review.Pending
				if j <= i {
					want[k] = review.Done
				}
			}
			if got := states(res.Record); res.Info.State != review.Paused || !maps(got, want) {
				t.Fatalf("run %s, states %v; want paused and %v", res.Info.State, got, want)
			}
			if !res.Info.EndedAt.IsZero() {
				t.Errorf("a paused run ended at %v", res.Info.EndedAt)
			}
			for _, f := range []string{run.PauseRequestFile, run.ActiveRunFile} {
				if exists(t, filepath.Join(w.ws.Dir(), f)) {
					t.Errorf("%s is left after the pause", f)
				}
			}
			if !exists(t, filepath.Join(w.ws.ArtifactsDir(), review.RunDir(res.Info.ID), review.RunMarkdownFile)) {
				t.Error("no run.md after the pause")
			}
			if u, err := e.Unfinished(context.Background()); err != nil || u == nil || u.State != review.Paused {
				t.Errorf("Unfinished = %+v, %v", u, err)
			}

			teams.during = nil
			done := w.resume(e)
			allDone(t, done)
			if done.Info.ID != res.Info.ID {
				t.Errorf("resume made run %s, want %s continued", done.Info.ID, res.Info.ID)
			}
			if !slices.Equal(teams.applied, keys) {
				t.Errorf("calls %v, want each once in order", teams.applied)
			}
			once(t, r, keys...)
		})
	}
}

// The first Ctrl+C does what pause does.
func TestTheFirstCtrlCPausesTheRun(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1", "T2", "T3")
	interrupted := make(chan struct{})
	teams.during = func(it review.Item) {
		if it.Key == "teams/T2" {
			close(interrupted)
		}
	}
	e := w.engine(teams)
	e.Interrupted = interrupted
	res := w.apply(e, w.plan(e).Version)
	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Done, "teams/T3": review.Pending}
	if got := states(res.Record); res.Info.State != review.Paused || !maps(got, want) {
		t.Fatalf("run %s, states %v; want paused and %v", res.Info.State, got, want)
	}
	teams.during = nil
	e = w.engine(teams)
	allDone(t, w.resume(e))
	once(t, r, "teams/T1", "teams/T2", "teams/T3")
}

// A pause request for another run is left from one that stopped before it
// was written; one that cannot be read is taken as meant for this run.
func TestAPauseRequestForAnotherRunIsIgnored(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1", "T2")
	// Left before the run starts: the run removes it.
	if err := run.RequestPause(w.ws, "20261117-090000-aaaaaa", now); err != nil {
		t.Fatal(err)
	}
	teams.during = func(it review.Item) {
		if exists(t, filepath.Join(w.ws.Dir(), run.PauseRequestFile)) {
			t.Errorf("%s: the old pause request is still there", it.Key)
		}
		if it.Key == "teams/T1" {
			if err := run.RequestPause(w.ws, "20261117-090000-bbbbbb", now); err != nil {
				t.Fatal(err)
			}
			teams.during = nil
		}
	}
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)
	allDone(t, res)
	if exists(t, filepath.Join(w.ws.Dir(), run.PauseRequestFile)) {
		t.Error("the pause request is left after the run")
	}

	// T1 and T2 are mapped now, so T3 is the first call.
	teams.plan = teamsPlan("T1", "T2", "T3", "T4")
	teams.during = func(it review.Item) {
		if err := w.ws.WriteFileAtomic(run.PauseRequestFile, []byte("not json")); err != nil {
			t.Fatal(err)
		}
	}
	res = w.apply(e, w.plan(e).Version)
	if res.Info.State != review.Paused {
		t.Errorf("an unreadable pause request: run %s, want paused", res.Info.State)
	}
}

// Resume looks up uncertain updates before it decides whether to send them.
func TestResumeLooksUpBeforeItSendsAgain(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamUpdates("T1", "T2", "T3", "T4")
	unknown := fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable)
	for _, k := range []string{"teams/T1", "teams/T2", "teams/T3"} {
		teams.errs[k] = unknown
	}
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)
	if res.Info.State != review.Interrupted {
		t.Fatalf("run %s, want interrupted by the unknown outcomes", res.Info.State)
	}

	// The calls did not take effect, did, and cannot be told.
	clear(teams.errs)
	teams.found["teams/T1"] = run.Lookup{Outcome: run.LookupNotFound}
	teams.found["teams/T2"] = run.Lookup{Outcome: run.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-found"}}
	teams.applied, teams.looked = nil, nil
	w.wait(run.ResendAfter)
	res = w.resume(e)
	if !slices.Equal(teams.looked, []string{"teams/T1", "teams/T2", "teams/T3"}) {
		t.Errorf("lookups %v", teams.looked)
	}
	if !slices.Equal(teams.applied, []string{"teams/T1"}) {
		t.Errorf("calls %v: only the call proven not to have taken effect is sent again", teams.applied)
	}
	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Done, "teams/T3": review.Unknown, "teams/T4": review.Done}
	if got := states(res.Record); res.Info.State != review.Interrupted || !maps(got, want) {
		t.Errorf("run %s, states %v; want interrupted and %v", res.Info.State, got, want)
	}
	if res.Info.Written != 4 || res.Info.Failed != 1 {
		t.Errorf("counts %d written, %d failed; want 4 and 1", res.Info.Written, res.Info.Failed)
	}
	var written, failed int
	if err := w.st.DB().QueryRow(`SELECT written, failed FROM runs WHERE id = ?`, res.Info.ID).Scan(&written, &failed); err != nil ||
		written != 4 || failed != 1 {
		t.Errorf("state.db's counts %d and %d, %v", written, failed, err)
	}
	for _, en := range res.Record.Entries {
		codes := map[string]bool{}
		for _, m := range en.Messages {
			codes[m.Code] = true
		}
		switch en.Key {
		case "teams/T1":
			if en.Attempts != 2 || !codes["sent_again"] {
				t.Errorf("T1: %d attempts, messages %v", en.Attempts, en.Messages)
			}
		case "teams/T2":
			if en.Attempts != 1 || en.JSM == nil || en.JSM.ID != "jsm-found" || !codes["outcome_found"] {
				t.Errorf("T2: %+v", en)
			}
		case "teams/T3":
			if !codes["outcome_unknown"] || en.Messages[0].Class != messages.ManualTask {
				t.Errorf("T3's messages %v", en.Messages)
			}
		}
	}
	var states []string
	rows, err := w.st.DB().Query(`SELECT state FROM journal WHERE item_key = 'teams/T1' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	_ = rows.Close()
	if !slices.Equal(states, []string{"failed", "done"}) {
		t.Errorf("T1's journal rows %v: the attempt proven not to have taken effect is failed", states)
	}

	// The next resume looks T3 up again, and nothing else.
	teams.found["teams/T3"] = run.Lookup{Outcome: run.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-later"}}
	teams.applied, teams.looked = nil, nil
	allDone(t, w.resume(e))
	if len(teams.applied) != 0 || !slices.Equal(teams.looked, []string{"teams/T3"}) {
		t.Errorf("calls %v, lookups %v", teams.applied, teams.looked)
	}
	if _, err := e.Resume(context.Background()); !errors.Is(err, run.ErrNothingToResume) {
		t.Errorf("a resume after the run completed: %v", err)
	}
}

// A lookup soon after a call can miss what the call made, so resume believes
// not found only once the attempt is ResendAfter old. A resume sooner sends
// nothing, stops at the item, so the items that depend on it are taken by
// the resume that settles it instead of being declined, and says when to
// resume.
func TestResumeBelievesNotFoundOnlyOnceTheCallIsOldEnough(t *testing.T) {
	ctx := context.Background()
	w, e, r := crashSetup(t)
	// A crash after T1's sending row, before its call: a lookup finds nothing.
	e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[1] && k == "teams/T1" })
	if _, err := e.Apply(ctx, w.mustLatest(e), confirmation(w.mustLatest(e))); !errors.Is(err, run.ErrCrashed) {
		t.Fatalf("Apply = %v, want the crash", err)
	}
	w.reopen()
	teams := e.Steps[0].(*fakeStep)
	e = w.engine(teams)
	for _, after := range []time.Duration{0, time.Minute, run.ResendAfter - time.Second} {
		w.clock = now.Add(after)
		teams.applied, teams.looked = nil, nil
		res := w.resume(e)
		want := map[string]review.EntryState{"teams/T1": review.Unknown, "teams/T1-ops": review.Pending,
			"teams/T2": review.Pending, "teams/T3": review.Pending}
		if got := states(res.Record); res.Info.State != review.Interrupted || !maps(got, want) {
			t.Fatalf("%s after the call: run %s, states %v; want interrupted and %v", after, res.Info.State, got, want)
		}
		if len(teams.applied) != 0 || !slices.Equal(teams.looked, []string{"teams/T1"}) {
			t.Errorf("%s after the call: calls %v, lookups %v", after, teams.applied, teams.looked)
		}
		en := res.Record.Entries[slices.IndexFunc(res.Record.Entries, func(en review.Entry) bool { return en.Key == "teams/T1" })]
		if len(en.Messages) != 1 || en.Messages[0].Code != "lookup_too_soon" || en.Messages[0].Class != messages.ManualTask ||
			!strings.Contains(en.Messages[0].Text, "sent at 2026-11-17T09:00:00Z") ||
			!strings.Contains(en.Messages[0].Text, "Resume it after 2026-11-17T09:10:00Z") {
			t.Errorf("%s after the call: T1's messages %+v", after, en.Messages)
		}
	}
	if len(r.objects) != 0 {
		t.Fatalf("made %v before the lookup was believed", r.objects)
	}

	w.clock = now.Add(run.ResendAfter)
	res := w.resume(e)
	allDone(t, res)
	once(t, r, "teams/T1", "teams/T1-ops", "teams/T3")
	if en := res.Record.Entries[0]; en.Key != "teams/T1" || en.Attempts != 2 {
		t.Errorf("T1: %+v; want sent again as the second attempt", en)
	}
}

// The review's case: a call that never gets an answer, and a lookup that
// always finds nothing. However many resumes there are, the call is sent
// again at most once per ResendAfter and MaxAttempts times in all; then the
// item fails, for the operator to check, and the run can end.
func TestResumeSendsACallAtMostMaxAttemptsTimes(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamUpdates("T1")
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable)
	teams.found["teams/T1"] = run.Lookup{Outcome: run.LookupNotFound}
	e := w.engine(teams)
	if res := w.apply(e, w.plan(e).Version); res.Info.State != review.Interrupted || r.objects["teams/T1"] != 1 {
		t.Fatalf("apply: run %s, calls %v", res.Info.State, r.objects)
	}

	var res run.RunResult
	for i, step := range []struct {
		wait  time.Duration
		calls int
		state review.RunState
	}{
		{0, 1, review.Interrupted},                   // too soon after apply's call
		{run.ResendAfter, 2, review.Interrupted},     // sent again, and unknown again
		{0, 2, review.Interrupted},                   // too soon after that one
		{run.ResendAfter / 2, 2, review.Interrupted}, // still too soon
		{run.ResendAfter, 3, review.Interrupted},     // the last attempt
		{run.ResendAfter, 3, review.Completed},       // not sent again: failed
	} {
		w.wait(step.wait)
		res = w.resume(e)
		if got := r.objects["teams/T1"]; got != step.calls || res.Info.State != step.state {
			t.Errorf("resume %d: %d calls, run %s; want %d and %s", i+1, got, res.Info.State, step.calls, step.state)
		}
	}
	if r.objects["teams/T1"] != run.MaxAttempts {
		t.Errorf("%d calls, want %d", r.objects["teams/T1"], run.MaxAttempts)
	}
	en := res.Record.Entries[0]
	if en.State != review.Failed || en.Attempts != run.MaxAttempts || len(en.Messages) != 1 ||
		en.Messages[0].Code != "not_sent_again" || en.Messages[0].Class != messages.ManualTask {
		t.Errorf("T1: %+v", en)
	}
	if res.Info.Written != 1 || res.Info.Failed != 1 {
		t.Errorf("counts %d written, %d failed; want 1 and 1", res.Info.Written, res.Info.Failed)
	}
	var states []string
	rows, err := w.st.DB().Query(`SELECT state FROM journal WHERE item_key = 'teams/T1' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	_ = rows.Close()
	if !slices.Equal(states, []string{"failed", "failed", "failed"}) {
		t.Errorf("T1's journal rows %v", states)
	}
	w.wait(10 * run.ResendAfter)
	if _, err := e.Resume(context.Background()); !errors.Is(err, run.ErrNothingToResume) || r.objects["teams/T1"] != run.MaxAttempts {
		t.Errorf("a resume after the run ended: %v, %d calls", err, r.objects["teams/T1"])
	}
}

// pause asked while resume runs its checks stops the run before its first
// item, after a clean pause as after a crash: the resume command announces
// the run before its checks. A request left from before the command is not
// for it and is dropped: one a crash left unanswered, or one that came as
// the run stopped.
func TestAPauseDuringResumesChecksStopsItBeforeItsFirstItem(t *testing.T) {
	for _, stop := range []string{"paused", "crashed"} {
		t.Run(stop, func(t *testing.T) {
			ctx := context.Background()
			w, e, r := crashSetup(t)
			teams := e.Steps[0].(*fakeStep)
			v := w.mustLatest(e)
			// pause during T1's call: the run stops before T1-ops, or crashes
			// after T1 before it reads the request.
			teams.during = func(it review.Item) {
				if it.Key == "teams/T1" {
					if err := run.RequestPause(w.ws, w.activeRun().RunID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if stop == "crashed" {
				e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[3] && k == "teams/T1" })
			}
			res, err := e.Apply(ctx, v, confirmation(v))
			teams.during = nil
			switch {
			case stop == "crashed" && !errors.Is(err, run.ErrCrashed):
				t.Fatalf("Apply = %v, want the crash", err)
			case stop == "paused" && (err != nil || res.Info.State != review.Paused):
				t.Fatalf("Apply = %s, %v; want paused", res.Info.State, err)
			case stop == "paused":
				// A second pause that came as the run stopped.
				if err := run.RequestPause(w.ws, res.Info.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			if !exists(t, filepath.Join(w.ws.Dir(), run.PauseRequestFile)) {
				t.Fatal("no pause request left from before resume")
			}

			// The resume command holds the lock from here on, and announces
			// the run before its checks.
			w.reopen()
			resumed := w.engine(teams)
			u, err := resumed.Unfinished(ctx)
			if err != nil || u == nil {
				t.Fatalf("Unfinished = %+v, %v", u, err)
			}
			announce := func() {
				t.Helper()
				if err := run.Announce(w.ws, run.ActiveRun{RunID: u.ID, PlanVersion: u.PlanVersion, StartedAt: u.StartedAt}); err != nil {
					t.Fatal(err)
				}
			}
			announce()
			if exists(t, filepath.Join(w.ws.Dir(), run.PauseRequestFile)) {
				t.Error("the pause request from before resume is still there")
			}
			other, err := store.OpenWorkspace(w.ws.Dir(), false)
			if err != nil {
				t.Fatal(err)
			}
			active, err := run.FindActive(other)
			if err != nil || active == nil || active.RunID != u.ID {
				t.Fatalf("pause during resume's checks: FindActive = %+v, %v; want run %s", active, err, u.ID)
			}
			if err := run.RequestPause(other, active.RunID, now); err != nil {
				t.Fatal(err)
			}
			calls := len(teams.applied)
			res = w.resume(resumed)
			want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T1-ops": review.Pending,
				"teams/T2": review.Pending, "teams/T3": review.Pending}
			if got := states(res.Record); res.Info.State != review.Paused || !maps(got, want) || len(teams.applied) != calls {
				t.Fatalf("run %s, states %v, calls %v; want paused before T1-ops with no call", res.Info.State, got, teams.applied)
			}
			for _, f := range []string{run.PauseRequestFile, run.ActiveRunFile} {
				if exists(t, filepath.Join(w.ws.Dir(), f)) {
					t.Errorf("%s is left after the pause", f)
				}
			}

			announce()
			allDone(t, w.resume(resumed))
			once(t, r, "teams/T1", "teams/T1-ops", "teams/T3")
			if err := run.Withdraw(w.ws); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Withdraw removes what Announce wrote and a request asked since, so a
// resume that ends before its run starts leaves neither behind.
func TestWithdrawRemovesTheAnnouncement(t *testing.T) {
	w := newWorkspace(t)
	if err := run.Announce(w.ws, run.ActiveRun{RunID: "20261117-090000-aaaaaa", PlanVersion: 1, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if a := w.activeRun(); a.RunID != "20261117-090000-aaaaaa" || a.PlanVersion != 1 {
		t.Errorf("active-run.json %+v", a)
	}
	if err := run.RequestPause(w.ws, "20261117-090000-aaaaaa", now); err != nil {
		t.Fatal(err)
	}
	if err := run.Withdraw(w.ws); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{run.PauseRequestFile, run.ActiveRunFile} {
		if exists(t, filepath.Join(w.ws.Dir(), f)) {
			t.Errorf("%s is left after Withdraw", f)
		}
	}
}

// A crash at any point of an update, then resume, applies every change once:
// the row is committed as sending before the call, and resume looks a
// sending row up before it sends anything. The resume comes once a lookup
// that finds nothing is believed; one sooner sends nothing (see
// TestResumeBelievesNotFoundOnlyOnceTheCallIsOldEnough).
func TestACrashAtAnyPointThenResumeMakesEachObjectOnce(t *testing.T) {
	calls := []string{"teams/T1", "teams/T1-ops", "teams/T3"}
	for _, key := range calls {
		for _, p := range run.CrashPoints {
			t.Run(key+" "+p.String(), func(t *testing.T) {
				t.Parallel()
				w, e, r := crashSetup(t)
				e.CrashAt(func(at run.CrashPoint, k string) bool { return at == p && k == key })
				if _, err := e.Apply(context.Background(), w.mustLatest(e), confirmation(w.mustLatest(e))); !errors.Is(err, run.ErrCrashed) {
					t.Fatalf("Apply = %v, want the crash", err)
				}
				w.reopen()
				w.wait(run.ResendAfter)
				e = w.engine(e.Steps...)
				res := w.resume(e)
				allDone(t, res)
				once(t, r, calls...)
				if exists(t, filepath.Join(w.ws.Dir(), run.ActiveRunFile)) {
					t.Error("active-run.json is left after resume")
				}
				var mapped int
				if err := w.st.DB().QueryRow(`SELECT count(*) FROM mappings`).Scan(&mapped); err != nil || mapped != 3 {
					t.Errorf("%d mappings, %v; want T1, T2 and T3 once each", mapped, err)
				}
			})
		}
	}
}

// A second crash while resuming changes nothing: each resume starts from
// what was committed.
func TestACrashWhileResumingMakesEachObjectOnce(t *testing.T) {
	calls := []string{"teams/T1", "teams/T1-ops", "teams/T3"}
	for _, first := range calls {
		for _, p := range run.CrashPoints {
			for _, second := range calls {
				for _, q := range run.CrashPoints {
					t.Run(fmt.Sprintf("%s %s, then %s %s", first, p, second, q), func(t *testing.T) {
						t.Parallel()
						w, e, r := crashSetup(t)
						e.CrashAt(func(at run.CrashPoint, k string) bool { return at == p && k == first })
						if _, err := e.Apply(context.Background(), w.mustLatest(e), confirmation(w.mustLatest(e))); !errors.Is(err, run.ErrCrashed) {
							t.Fatalf("Apply = %v, want the crash", err)
						}
						w.reopen()
						w.wait(run.ResendAfter)
						e = w.engine(e.Steps...)
						e.CrashAt(func(at run.CrashPoint, k string) bool { return at == q && k == second })
						res, err := e.Resume(context.Background())
						if errors.Is(err, run.ErrCrashed) {
							w.reopen()
							w.wait(run.ResendAfter)
							e = w.engine(e.Steps...)
							res = w.resume(e)
						} else if err != nil {
							t.Fatal(err)
						}
						allDone(t, res)
						once(t, r, calls...)
					})
				}
			}
		}
	}
}

// crashSetup plans updates, one with a dependent call, and a map that
// makes no call, with a step whose lookups answer as JSM would.
func crashSetup(t *testing.T) (*workspace, *run.Engine, *remote) {
	t.Helper()
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.honest = true
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{Items: []review.Item{
			{Key: "teams/T1", Action: review.Update, Summary: "Update Platform", PagerDuty: pdRef("T1"), JSM: pdRef("jsm-platform")},
			{Key: "teams/T1-ops", Action: review.Update, Summary: "Turn on Operations", JSM: pdRef("jsm-platform"), DependsOn: []string{"teams/T1"}},
			{Key: "teams/T2", Action: review.Map, Summary: "Map Payments", PagerDuty: pdRef("T2"), JSM: &review.Ref{Type: "team", ID: "jsm-existing"}},
			{Key: "teams/T3", Action: review.Update, Summary: "Update Search", PagerDuty: pdRef("T3"), JSM: pdRef("jsm-search")},
		}}, nil
	}
	e := w.engine(teams)
	w.plan(e)
	return w, e, r
}

func teamUpdates(ids ...string) func(run.PlanInput) (run.Planned, error) {
	return func(run.PlanInput) (run.Planned, error) {
		var p run.Planned
		for _, id := range ids {
			p.Items = append(p.Items, review.Item{Key: "teams/" + id, Action: review.Update, Summary: "Update team " + id,
				PagerDuty: pdRef(id), JSM: pdRef("jsm-" + id)})
		}
		return p, nil
	}
}

func TestResumeReconcilesASuccessfulCreateWithoutResending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ref   *review.Ref
		crash bool
	}{
		{name: "missing ID"},
		{name: "unrecordable ID", ref: pdRef("two words")},
		{name: "crash before recording the response", ref: pdRef("jsm-team"), crash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := newWorkspace(t)
			r := &remote{objects: map[string]int{}}
			const key = "teams/T1"
			teams := &answerStep{fakeStep: newStep("teams", r), objects: map[string]*review.Ref{key: tc.ref}}
			teams.plan = teamsPlan("T1")
			teams.found[key] = run.Lookup{Outcome: run.LookupNotFound}
			e := w.engine(teams)
			v := w.plan(e).Version
			if tc.crash {
				e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[2] && k == key })
			}
			res, err := e.Apply(ctx, v, confirmation(v))
			if tc.crash {
				if !errors.Is(err, run.ErrCrashed) {
					t.Fatalf("apply after crash: %v", err)
				}
			} else if err != nil || res.Info.State != review.Interrupted {
				t.Fatalf("apply: run %s, error %v", res.Info.State, err)
			}
			once(t, r, key)
			for range run.MaxAttempts + 1 {
				w.reopen()
				w.wait(run.ResendAfter)
				e = w.engine(teams)
				res = w.resume(e)
				en := res.Record.Entries[0]
				if res.Info.State != review.Interrupted || en.State != review.Unknown || en.Attempts != 1 {
					t.Fatalf("lookup miss: run %s, entry %+v", res.Info.State, en)
				}
				once(t, r, key)
			}
			teams.found[key] = run.Lookup{Outcome: run.LookupFound, JSM: pdRef("jsm-team")}
			allDone(t, w.resume(e))
			once(t, r, key)
			var id string
			if err := w.st.DB().QueryRow(`SELECT jsm_id FROM mappings WHERE pagerduty_id = 'T1'`).Scan(&id); err != nil || id != "jsm-team" {
				t.Errorf("mapping after resume: %q, %v", id, err)
			}
		})
	}
}

func TestResumeUsesTheRunsSnapshotFormat(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1")
	e := w.engine(&versionedStep{fakeStep: teams, version: 1})
	stop := make(chan struct{})
	close(stop)
	e.Interrupted = stop
	if res := w.apply(e, w.plan(e).Version); res.Info.State != review.Paused {
		t.Fatalf("apply: %s", res.Info.State)
	}
	e.Interrupted = nil
	other := w.engine(&versionedStep{fakeStep: teams, version: 2})
	if _, err := other.Read(ctx, review.JSM); err != nil {
		t.Fatal(err)
	}
	if res, err := other.Resume(ctx); messagesExit(err) != messages.ExitValidationFailed || res.Info.State != review.Paused {
		t.Errorf("resume with newer format: run %s, error %v", res.Info.State, err)
	}
	if len(r.objects) != 0 || len(teams.looked) != 0 {
		t.Fatalf("writes %v, lookups %v on incompatible resume", r.objects, teams.looked)
	}
	allDone(t, w.resume(e))
	once(t, r, "teams/T1")
}

// resume writes again what a crash cut short: the run record, and the
// confirmation.md a crash right after the confirmation was recorded left
// unwritten.
func TestResumeRebuildsWhatACrashCutShort(t *testing.T) {
	w, e, r := crashSetup(t)
	v := w.mustLatest(e)
	e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[1] && k == "teams/T3" })
	if _, err := e.Apply(context.Background(), v, confirmation(v)); !errors.Is(err, run.ErrCrashed) {
		t.Fatalf("Apply = %v", err)
	}
	confirmed := filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.ConfirmationFile)
	if err := os.Remove(confirmed); err != nil {
		t.Fatal(err)
	}
	w.reopen()
	w.wait(run.ResendAfter)
	e = w.engine(e.Steps...)
	var seen bool
	e.Steps[0].(*fakeStep).during = func(review.Item) {
		u, err := e.Unfinished(context.Background())
		if err != nil || u == nil {
			t.Fatalf("Unfinished = %v, %v", u, err)
		}
		// Written before the first item resume takes.
		data, err := os.ReadFile(filepath.Join(w.ws.ArtifactsDir(), u.Record))
		seen = err == nil && len(data) > 0
	}
	res := w.resume(e)
	allDone(t, res)
	once(t, r, "teams/T1", "teams/T1-ops", "teams/T3")
	if !seen {
		t.Error("no run.md before resume took its first item")
	}
	if !exists(t, confirmed) {
		t.Error("resume did not write confirmation.md again")
	}
}

func TestResumeRefusesWhatItCannotRun(t *testing.T) {
	ctx := context.Background()
	w, e, _ := crashSetup(t)
	if _, err := e.CheckResume(ctx); !errors.Is(err, run.ErrNothingToResume) {
		t.Errorf("no run: %v", err)
	}
	v := w.mustLatest(e)
	interrupted := make(chan struct{})
	close(interrupted)
	e.Interrupted = interrupted
	if res := w.apply(e, v); res.Info.State != review.Paused || res.Info.Written != 0 {
		t.Fatalf("run %s, %d written; want paused before its first item", res.Info.State, res.Info.Written)
	}
	e.Interrupted = nil

	path := filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.PlanFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx); messagesExit(err) != messages.ExitValidationFailed {
		t.Errorf("an edited plan.json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	other := w.engine(newStep("users", &remote{objects: map[string]int{}}))
	if _, err := other.Resume(ctx); !errors.Is(err, run.ErrUnknownStep) {
		t.Errorf("a plan whose step this build lacks: %v", err)
	}
	accounts := e.Accounts
	e.Accounts = map[review.Side]review.MigrationAccount{
		review.PagerDuty: accounts[review.PagerDuty],
		review.JSM:       {ID: "cloud-2", OrgID: accounts[review.JSM].OrgID},
	}
	if _, err := e.Resume(ctx); messagesExit(err) != messages.ExitValidationFailed {
		t.Errorf("another JSM site: %v", err)
	}
	e.Accounts = accounts
	r, err := e.CheckResume(ctx)
	if err != nil || r.Confirmation.AccountID != "account-1" || r.Version.Version != v.Version {
		t.Errorf("CheckResume = %+v, %v", r, err)
	}
}

// The status is read without the data key or the lock, so the menu and
// pause can show where a run stands while it is being made.
func TestTheStatusIsReadWithoutTheKey(t *testing.T) {
	ctx := context.Background()
	w, e, _ := crashSetup(t)
	other := func() *store.Workspace {
		ws, err := store.OpenWorkspace(w.ws.Dir(), false)
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	s, err := run.ReadStatus(ctx, other())
	if err != nil || s.Plan == nil || s.Plan.Version != 1 || s.Plan.Calls != 3 || s.Plan.Dir != review.PlanDir(1) || s.Run != nil {
		t.Fatalf("after plan: %+v, %v", s, err)
	}

	var during run.Status
	e.Steps[0].(*fakeStep).during = func(it review.Item) {
		if it.Key == "teams/T3" {
			during, err = run.ReadStatus(ctx, other())
		}
	}
	e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[2] && k == "teams/T3" })
	if _, err := e.Apply(ctx, w.mustLatest(e), confirmation(w.mustLatest(e))); !errors.Is(err, run.ErrCrashed) {
		t.Fatalf("Apply = %v", err)
	}
	if err != nil || during.Run == nil || during.Run.State != review.Running || during.Active == nil ||
		during.Active.RunID != during.Run.ID || during.Run.Written != 3 || during.Run.Total != 4 {
		t.Errorf("during the run: %+v %+v, %v", during.Run, during.Active, err)
	}

	// The crashed run is still marked running, and no command makes it.
	if err := w.st.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = run.ReadStatus(ctx, other())
	if err != nil || s.Run == nil || s.Run.State != review.Interrupted || s.Active != nil {
		t.Errorf("after the crash: %+v %+v, %v", s.Run, s.Active, err)
	}
	if a, err := run.FindActive(other()); a != nil || err != nil {
		t.Errorf("FindActive after the crash = %+v, %v", a, err)
	}
}

// Progress is told about each item once its result is committed, with the
// step's counts, which a resume carries on from.
func TestProgressIsToldEachItem(t *testing.T) {
	w, e, _ := crashSetup(t)
	var got []string
	e.Progress = func(p run.Progress) {
		got = append(got, fmt.Sprintf("%s %s %d/%d", p.Entry.Key, p.Entry.State, p.Taken, p.Total))
	}
	e.CrashAt(func(at run.CrashPoint, k string) bool { return at == run.CrashPoints[1] && k == "teams/T1-ops" })
	if _, err := e.Apply(context.Background(), w.mustLatest(e), confirmation(w.mustLatest(e))); !errors.Is(err, run.ErrCrashed) {
		t.Fatalf("Apply = %v", err)
	}
	w.reopen()
	w.wait(run.ResendAfter)
	resumed := w.engine(e.Steps...)
	resumed.Progress = e.Progress
	w.resume(resumed)
	want := []string{"teams/T1 done 1/4", "teams/T1-ops done 2/4", "teams/T2 done 3/4", "teams/T3 done 4/4"}
	if !slices.Equal(got, want) {
		t.Errorf("progress %q, want %q", got, want)
	}
}
