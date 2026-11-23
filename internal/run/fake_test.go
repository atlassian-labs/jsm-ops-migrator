package run_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

var now = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// remote stands in for JSM: what the calls created, by item key.
type remote struct{ objects map[string]int }

// object is what a step put in the snapshot.
type object struct {
	kind, id string
	v        any
}

// fakeStep is a step scripted by the test: what it reads from each side,
// what it plans, and how each call goes.
type fakeStep struct {
	name   string
	reads  map[review.Side][]object
	failOn map[review.Side]error
	plan   func(in run.PlanInput) (run.Planned, error)
	// errs makes the call of an item fail with the error; found says what
	// its lookup finds.
	errs  map[string]error
	found map[string]run.Lookup
	// honest makes each lookup say what the remote holds, as a lookup in
	// JSM would, instead of what found says.
	honest bool
	// during runs inside Apply, before the call is answered.
	during func(it review.Item)
	remote *remote

	applied []string
	deps    map[string]run.Results
	looked  []string
	// lookedIn holds the JSM read each lookup was given.
	lookedIn []int64
}

func newStep(name string, r *remote) *fakeStep {
	return &fakeStep{name: name, reads: map[review.Side][]object{}, failOn: map[review.Side]error{},
		errs: map[string]error{}, found: map[string]run.Lookup{}, remote: r, deps: map[string]run.Results{}}
}

func (s *fakeStep) Name() string { return s.name }

func (s *fakeStep) Read(_ context.Context, side review.Side, w *discover.Writer) error {
	for _, o := range s.reads[side] {
		if err := w.Put(o.kind, o.id, o.v); err != nil {
			return err
		}
	}
	return s.failOn[side]
}

func (s *fakeStep) Plan(_ context.Context, in run.PlanInput) (run.Planned, error) {
	if s.plan == nil {
		return run.Planned{}, nil
	}
	return s.plan(in)
}

// Apply declines when an item it depends on is not done, as a real step
// does: it cannot make a team's members call without the team.
func (s *fakeStep) Apply(_ context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	s.applied = append(s.applied, it.Key)
	s.deps[it.Key] = deps
	for key, d := range deps {
		if d.State != review.Done {
			return run.Outcome{Declined: true, Messages: []messages.Message{{Class: messages.Warning, Code: "needs_dependency",
				Text: fmt.Sprintf("Not sent, because %s is %s.", key, d.State)}}}, nil
		}
	}
	if s.during != nil {
		s.during(it)
	}
	s.remote.objects[it.Key]++
	if err := s.errs[it.Key]; err != nil {
		return run.Outcome{Status: 500, RequestID: "req-" + it.Key}, err
	}
	return run.Outcome{JSM: &review.Ref{Type: "team", ID: "jsm-" + it.Key}, Status: 201, RequestID: "req-" + it.Key}, nil
}

func (s *fakeStep) Lookup(_ context.Context, it review.Item, _ run.Results, jsm *discover.Snapshot) (run.Lookup, error) {
	s.looked = append(s.looked, it.Key)
	if jsm == nil {
		return run.Lookup{}, fmt.Errorf("no JSM snapshot")
	}
	s.lookedIn = append(s.lookedIn, jsm.ID())
	if s.honest {
		if s.remote.objects[it.Key] > 0 {
			return run.Lookup{Outcome: run.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-" + it.Key}}, nil
		}
		return run.Lookup{Outcome: run.LookupNotFound}, nil
	}
	return s.found[it.Key], nil
}

// workspace is one test's workspace with its store open, as a command has
// it.
type workspace struct {
	t  *testing.T
	ws *store.Workspace
	st *store.Store
	kp store.KeyProvider
	// clock is what the engines' Now returns; wait moves it on.
	clock time.Time
}

// wait moves the workspace's clock on by d, as the time between two
// commands does.
func (w *workspace) wait(d time.Duration) { w.clock = w.clock.Add(d) }

func newWorkspace(t *testing.T) *workspace {
	t.Helper()
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	kp := testkit.NewMemoryKeyProvider()
	st, err := store.Open(context.Background(), ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &workspace{t: t, ws: ws, st: st, kp: kp, clock: now}
}

// reopen closes the store and opens it again, as the next command after a
// crash does: only what was committed is there.
func (w *workspace) reopen() {
	w.t.Helper()
	if err := w.st.Close(); err != nil {
		w.t.Fatal(err)
	}
	ws, err := store.OpenWorkspace(w.ws.Dir(), false)
	if err != nil {
		w.t.Fatal(err)
	}
	st, err := store.Open(context.Background(), ws, w.kp, store.OpenExisting)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = st.Close() })
	w.ws, w.st = ws, st
}

func (w *workspace) engine(steps ...run.Step) *run.Engine {
	return &run.Engine{
		Store: w.st, Workspace: w.ws, Steps: steps, Now: func() time.Time { return w.clock },
		Accounts: map[review.Side]review.MigrationAccount{
			review.PagerDuty: {ID: "acme.pagerduty.com"},
			review.JSM:       {ID: "cloud-1", OrgID: "org-1"},
		},
	}
}

// plan reads both sides and plans.
func (w *workspace) plan(e *run.Engine) run.PlanResult {
	w.t.Helper()
	ctx := context.Background()
	pd, err := e.Read(ctx, review.PagerDuty)
	if err != nil {
		w.t.Fatal(err)
	}
	jsm, err := e.Read(ctx, review.JSM)
	if err != nil {
		w.t.Fatal(err)
	}
	res, err := e.Plan(ctx, pd, jsm)
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

// confirmation accepts every warning of v.
func confirmation(v *run.PlanVersion) review.Confirmation {
	c := review.Confirmation{PlanVersion: v.Version, PlanHash: v.Hash, AccountID: "account-1", At: now, Via: review.ViaFile}
	for _, m := range v.Plan.Messages {
		if m.Class == messages.Warning {
			c.Accepted = append(c.Accepted, review.Accepted{ID: m.ID(), Reason: "known and accepted"})
		}
	}
	return c
}

func (w *workspace) apply(e *run.Engine, v *run.PlanVersion) run.RunResult {
	w.t.Helper()
	res, err := e.Apply(context.Background(), v, confirmation(v))
	if err != nil {
		w.t.Fatal(err)
	}
	return res
}

func states(r review.Run) map[string]review.EntryState {
	out := map[string]review.EntryState{}
	for _, e := range r.Entries {
		out[e.Key] = e.State
	}
	return out
}

func pdRef(id string) *review.Ref { return &review.Ref{Type: "team", ID: id} }

// teamsPlan plans a team per ID, each depending on its users/<ID> item
// when users is true.
func teamsPlan(ids ...string) func(run.PlanInput) (run.Planned, error) {
	return func(in run.PlanInput) (run.Planned, error) {
		var p run.Planned
		for _, id := range ids {
			it := review.Item{Key: "teams/" + id, Action: review.Create, Summary: "Create team " + id, PagerDuty: pdRef(id)}
			if jsm, ok := in.Mappings.Find(*pdRef(id), "team"); ok {
				it.Action, it.JSM, it.Summary = review.Map, &jsm, "Map team "+id
			}
			p.Items = append(p.Items, it)
		}
		return p, nil
	}
}
