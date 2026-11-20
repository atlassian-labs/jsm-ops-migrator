package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestPlanWritesAVersionOnlyWhenSomethingChanged(t *testing.T) {
	w := newWorkspace(t)
	s := newStep("teams", &remote{objects: map[string]int{}})
	s.plan = teamsPlan("T1", "T2")
	e := w.engine(s)

	first := w.plan(e)
	v := first.Version
	if first.Unchanged || v.Version != 1 || v.Calls != 2 || v.Hash == "" {
		t.Fatalf("first plan: %+v", first)
	}
	for _, f := range []string{review.PlanFile, review.PlanMarkdownFile, review.DiffFile, review.RiskQueueFile, review.AcceptanceTemplateFile} {
		if !exists(t, filepath.Join(w.ws.ArtifactsDir(), v.Dir, f)) {
			t.Errorf("v1 has no %s", f)
		}
	}
	data, err := os.ReadFile(filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.PlanFile))
	if err != nil || review.Hash(data) != v.Hash {
		t.Fatalf("plan.json does not have the recorded hash: %v", err)
	}

	// The same reads give the same plan: no new version.
	again := w.plan(e)
	if !again.Unchanged || again.Version.Version != 1 || exists(t, filepath.Join(w.ws.ArtifactsDir(), review.PlanDir(2))) {
		t.Fatalf("an unchanged plan made a new version: %+v", again)
	}

	// Another team is a change, with a diff against v1.
	s.plan = teamsPlan("T1", "T2", "T3")
	third := w.plan(e)
	if third.Unchanged || third.Version.Version != 2 {
		t.Fatalf("a changed plan: %+v", third)
	}
	diff, err := os.ReadFile(filepath.Join(w.ws.ArtifactsDir(), review.PlanDir(2), review.DiffFile))
	if err != nil || !bytes.Contains(diff, []byte("teams/T3")) {
		t.Errorf("diff.md does not show the new item: %v\n%s", err, diff)
	}
	latest, err := e.Latest(context.Background())
	if err != nil || latest.Version != 2 || len(latest.Plan.Items) != 3 {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
}

// A folder a crashed plan wrote but state.db never recorded keeps its
// number: the operator may have read it, so it is never reused.
func TestPlanNeverReusesAVersionNumber(t *testing.T) {
	w := newWorkspace(t)
	s := newStep("teams", &remote{objects: map[string]int{}})
	s.plan = teamsPlan("T1")
	if err := os.MkdirAll(filepath.Join(w.ws.ArtifactsDir(), "plans", "v3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if v := w.plan(w.engine(s)).Version; v.Version != 4 {
		t.Fatalf("version %d, want 4", v.Version)
	}
}

func TestPlanOrdersItemsAcrossSteps(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	users, teams := newStep("users", r), newStep("teams", r)
	users.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{Items: []review.Item{{Key: "users/U1", Action: review.Create, Summary: "Create user U1"}}}, nil
	}
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{
			Items: []review.Item{
				{Key: "teams/T1-ops", Action: review.Create, Summary: "Turn on Operations", DependsOn: []string{"teams/T1"}},
				{Key: "teams/T1", Action: review.Create, Summary: "Create team T1", DependsOn: []string{"users/U1"}},
			},
			Messages: []messages.Message{
				{Class: messages.Warning, Code: "member_dropped", Item: "teams/T1", Text: "Casey is not migrated."},
				{Class: messages.Information, Code: "teams_counted", Text: "Two teams were read."},
			},
		}, nil
	}
	v := w.plan(w.engine(teams, users)).Version
	if got := keysOf(v.Plan.Items); !slices.Equal(got, []string{"users/U1", "teams/T1", "teams/T1-ops"}) {
		t.Errorf("order %v", got)
	}
	if v.Warnings != 1 || v.Blockers != 0 {
		t.Errorf("counts %+v", v)
	}
}

func keysOf(items []review.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}

func TestPlanRefusesAStepFault(t *testing.T) {
	cases := map[string]struct {
		planned run.Planned
		want    error
		text    string
	}{
		"another step's key": {run.Planned{Items: []review.Item{{Key: "users/U1", Action: review.Create}}},
			run.ErrStepFault, `teams planned item "users/U1"`},
		"a message about another item": {run.Planned{Messages: []messages.Message{
			{Class: messages.Warning, Code: "x", Item: "users/U1", Text: "?"}}}, run.ErrStepFault, "did not plan"},
		"a loop": {run.Planned{Items: []review.Item{
			{Key: "teams/a", Action: review.Create, DependsOn: []string{"teams/b"}},
			{Key: "teams/b", Action: review.Create, DependsOn: []string{"teams/a"}}}}, run.ErrOrder, "loop"},
		"what review refuses": {run.Planned{Items: []review.Item{{Key: "teams/T1", Action: "delete"}}},
			run.ErrStepFault, "unknown action"},
	}
	for name, c := range cases {
		w := newWorkspace(t)
		s := newStep("teams", &remote{objects: map[string]int{}})
		s.plan = func(run.PlanInput) (run.Planned, error) { return c.planned, nil }
		e := w.engine(s)
		ctx := context.Background()
		pd, _ := e.Read(ctx, review.PagerDuty)
		jsm, _ := e.Read(ctx, review.JSM)
		_, err := e.Plan(ctx, pd, jsm)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAFailedReadIsABlocker(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams, services := newStep("teams", r), newStep("services", r)
	teams.reads[review.JSM] = []object{{"team", "jsm-1", map[string]string{"name": "Platform"}}}
	teams.failOn[review.JSM] = fmt.Errorf("GET /v1/teams: 403: %w", httperr.ErrForbidden)
	// A feature the account's plan does not include is nothing to read.
	services.failOn[review.PagerDuty] = fmt.Errorf("GET /services: 402: %w", httperr.ErrNotInPlan)
	v := w.plan(w.engine(teams, services)).Version

	if v.Blockers != 1 {
		t.Fatalf("blockers %d, want 1: %+v", v.Blockers, v.Plan.Messages)
	}
	byID := map[string]messages.Message{}
	for _, m := range v.Plan.Messages {
		byID[m.ID()] = m
	}
	if m := byID["teams_jsm_read"]; m.Class != messages.Blocker || !strings.Contains(m.Text, "403") {
		t.Errorf("the failed read: %+v", m)
	}
	if m := byID["services_pagerduty_read"]; m.Class != messages.Information {
		t.Errorf("the read the plan does not include: %+v", m)
	}
	reads := map[review.Side]bool{}
	for _, rd := range v.Plan.Reads {
		reads[rd.Side] = rd.Complete
	}
	if reads[review.JSM] || !reads[review.PagerDuty] {
		t.Errorf("completeness %v, want JSM incomplete and PagerDuty complete", reads)
	}

	// What the step read before it failed is kept, so the plan can show it.
	jsm, err := discover.Open(context.Background(), w.st, v.JSMRead)
	if err != nil {
		t.Fatal(err)
	}
	var team map[string]string
	if ok, err := jsm.Get(context.Background(), "teams", "team", "jsm-1", &team); !ok || err != nil || team["name"] != "Platform" {
		t.Errorf("Get = %v, %v, %v", ok, err, team)
	}
	if _, err := w.engine(teams).Apply(context.Background(), v, confirmation(v)); messagesExit(err) != messages.ExitBlocked {
		t.Errorf("apply of a blocked plan: %v", err)
	}
}

func messagesExit(err error) int {
	code, _ := messages.ExitCodeOf(err)
	return code
}

// A snapshot never keeps a secret or contact data, sealed or not: the
// object is refused, the step's read is incomplete, and the value is
// nowhere in state.db.
func TestTheSnapshotRefusesSensitiveData(t *testing.T) {
	w := newWorkspace(t)
	users := newStep("users", &remote{objects: map[string]int{}})
	users.reads[review.PagerDuty] = []object{
		{"user", "PU1", map[string]any{"name": "Avery", "contact_methods": []any{map[string]string{"type": "sms", "address": "+14155550123"}}}},
		{"user", "PU2", map[string]any{"name": "Kai Codename"}},
	}
	// A step that goes on after a refusal still has an incomplete read.
	users.reads[review.JSM] = []object{{"contact", "+14155550199", map[string]string{"name": "x"}}}
	e := w.engine(users)
	ctx := context.Background()
	pd, err := e.Read(ctx, review.PagerDuty)
	if err != nil {
		t.Fatal(err)
	}
	if pd.Complete() || len(pd.Problems()) != 1 || !strings.Contains(pd.Problems()[0].Text, `"payload.contact_methods[0].address"`) ||
		strings.Contains(pd.Problems()[0].Text, "4155550123") {
		t.Fatalf("problems %+v", pd.Problems())
	}
	jsm, err := e.Read(ctx, review.JSM)
	if err != nil || jsm.Complete() || !strings.Contains(jsm.Problems()[0].Text, "its ID holds sensitive data") {
		t.Fatalf("a phone number as an ID: %v %+v", err, jsm.Problems())
	}
	if objs, err := pd.List(ctx, "users", "user"); err != nil || len(objs) != 0 {
		t.Errorf("objects after the refusal: %v, %v", objs, err)
	}
	if _, err := e.Plan(ctx, pd, jsm); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"4155550123", "4155550199", "Avery"} {
		for _, f := range []string{w.ws.StatePath(), w.ws.StatePath() + "-wal"} {
			data, _ := os.ReadFile(f)
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("%s holds %q", filepath.Base(f), s)
			}
		}
	}
}

// What a step writes is sealed: neither its names nor its payloads are in
// state.db in clear.
func TestSnapshotsAreSealed(t *testing.T) {
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.reads[review.PagerDuty] = []object{{"team", "PT1", map[string]string{"name": "Platform Wombats"}}}
	e := w.engine(teams)
	pd, err := e.Read(context.Background(), review.PagerDuty)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := pd.List(context.Background(), "teams", "team")
	if err != nil || len(objs) != 1 || objs[0].ID != "PT1" || !bytes.Contains(objs[0].Payload, []byte("Platform Wombats")) {
		t.Fatalf("List = %v, %v", objs, err)
	}
	latest, err := e.LatestRead(context.Background(), review.PagerDuty)
	if err != nil || latest.ID() != pd.ID() {
		t.Fatalf("LatestRead = %v, %v", latest, err)
	}
	for _, f := range []string{w.ws.StatePath(), w.ws.StatePath() + "-wal"} {
		if data, _ := os.ReadFile(f); bytes.Contains(data, []byte("Wombats")) {
			t.Errorf("%s holds the team's name in clear", filepath.Base(f))
		}
	}
}

func TestApplyTakesEveryItemInOrder(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{Items: []review.Item{
			{Key: "teams/T1", Action: review.Create, Summary: "Create Platform", PagerDuty: pdRef("T1")},
			{Key: "teams/T1-ops", Action: review.Create, Summary: "Turn on Operations", DependsOn: []string{"teams/T1"}},
			{Key: "teams/T2", Action: review.Map, Summary: "Map Payments", PagerDuty: pdRef("T2"), JSM: &review.Ref{Type: "team", ID: "jsm-existing"}},
			{Key: "teams/T3", Action: review.Skip, Summary: "Skip an empty team", PagerDuty: pdRef("T3")},
		}}, nil
	}
	e := w.engine(teams)
	v := w.plan(e).Version
	res := w.apply(e, v)

	if !slices.Equal(teams.applied, []string{"teams/T1", "teams/T1-ops"}) {
		t.Errorf("calls %v: only the items that make a call, in order", teams.applied)
	}
	if d := teams.deps["teams/T1-ops"]["teams/T1"]; d.State != review.Done || d.JSM == nil || d.JSM.ID != "jsm-teams/T1" {
		t.Errorf("the dependent saw %+v", d)
	}
	if res.Info.State != review.Completed || res.Info.Written != 4 || res.Info.Failed != 0 {
		t.Errorf("run %+v", res.Info)
	}
	for key, s := range states(res.Record) {
		if s != review.Done {
			t.Errorf("%s is %s", key, s)
		}
	}
	for _, f := range []string{review.RunFile, review.RunMarkdownFile, review.MappingFile} {
		if !exists(t, filepath.Join(w.ws.ArtifactsDir(), review.RunDir(res.Info.ID), f)) {
			t.Errorf("no %s", f)
		}
	}
	if !exists(t, filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.ConfirmationFile)) {
		t.Error("no confirmation.md")
	}
	if exists(t, filepath.Join(w.ws.Dir(), run.ActiveRunFile)) {
		t.Error("active-run.json is left after the run")
	}
	csv, _ := os.ReadFile(filepath.Join(w.ws.ArtifactsDir(), review.RunDir(res.Info.ID), review.MappingFile))
	for _, want := range []string{"teams/T1,team,T1,team,jsm-teams/T1", "teams/T2,team,T2,team,jsm-existing"} {
		if !bytes.Contains(csv, []byte(want)) {
			t.Errorf("mapping.csv misses %q:\n%s", want, csv)
		}
	}

	// The next plan sees what the run made, and maps it.
	teams.plan = teamsPlan("T1")
	next := w.plan(e).Version
	if it := next.Plan.Items[0]; it.Action != review.Map || it.JSM == nil || it.JSM.ID != "jsm-teams/T1" {
		t.Errorf("the next plan: %+v", it)
	}
}

// A failure never stops the run: it is recorded, and the items that depend
// on the failed one are still taken, each step deciding from the result.
func TestAFailedItemsDependentsAreStillTaken(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{Items: []review.Item{
			{Key: "teams/T1", Action: review.Create, Summary: "Create Platform", PagerDuty: pdRef("T1")},
			{Key: "teams/T1-ops", Action: review.Create, Summary: "Turn on Operations", DependsOn: []string{"teams/T1"}},
			{Key: "teams/T2", Action: review.Create, Summary: "Create Payments", PagerDuty: pdRef("T2")},
			{Key: "teams/T3", Action: review.Create, Summary: "Create Search", PagerDuty: pdRef("T3")},
		}}, nil
	}
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: 409: %w", httperr.ErrConflict)
	teams.errs["teams/T3"] = fmt.Errorf("POST /v1/teams: 403: %w", httperr.ErrForbidden)
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)

	if !slices.Equal(teams.applied, []string{"teams/T1", "teams/T1-ops", "teams/T2", "teams/T3"}) {
		t.Errorf("calls %v: every item is taken", teams.applied)
	}
	want := map[string]review.EntryState{"teams/T1": review.Failed, "teams/T1-ops": review.Declined,
		"teams/T2": review.Done, "teams/T3": review.Failed}
	if got := states(res.Record); !maps(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	if res.Info.State != review.Completed || res.Info.Written != 4 || res.Info.Failed != 3 {
		t.Errorf("run %+v", res.Info)
	}
	classes := map[string]messages.Class{}
	for _, e := range res.Record.Entries {
		wantAttempts := 1
		if e.State == review.Declined {
			wantAttempts = 0
		}
		if e.Attempts != wantAttempts {
			t.Errorf("%s: %d attempts, want %d", e.Key, e.Attempts, wantAttempts)
		}
		for _, m := range e.Messages {
			classes[m.ID()] = m.Class
		}
	}
	// A conflict fails one item; a refused credential no later call can
	// get past either, so it is a blocker for its item.
	if classes["teams/T1/call_failed"] != messages.Warning || classes["teams/T3/call_failed"] != messages.Blocker ||
		classes["teams/T1-ops/needs_dependency"] != messages.Warning {
		t.Errorf("messages %v", classes)
	}
	if _, err := e.Apply(context.Background(), w.mustLatest(e), review.Confirmation{}); !errors.Is(err, run.ErrApplied) {
		t.Errorf("a second apply of the same version: %v", err)
	}
}

func maps(a, b map[string]review.EntryState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (w *workspace) mustLatest(e *run.Engine) *run.PlanVersion {
	w.t.Helper()
	v, err := e.Latest(context.Background())
	if err != nil || v == nil {
		w.t.Fatalf("Latest = %v, %v", v, err)
	}
	return v
}

// An unknown outcome is looked up once, at once. Found is done; anything
// else stays unknown and is never sent again within the run.
func TestAnUnknownOutcomeIsLookedUpOnce(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1", "T2", "T3")
	unknown := fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable)
	for _, id := range []string{"teams/T1", "teams/T2", "teams/T3"} {
		teams.errs[id] = unknown
	}
	teams.found["teams/T1"] = run.Lookup{Outcome: run.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-found"}}
	teams.found["teams/T2"] = run.Lookup{Outcome: run.LookupNotFound}
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)

	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Unknown, "teams/T3": review.Unknown}
	if got := states(res.Record); !maps(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	for key, n := range r.objects {
		if n != 1 {
			t.Errorf("%s was sent %d times", key, n)
		}
	}
	if !slices.Equal(teams.looked, []string{"teams/T1", "teams/T2", "teams/T3"}) {
		t.Errorf("lookups %v", teams.looked)
	}
	for _, e := range res.Record.Entries {
		if e.Key == "teams/T1" && (e.JSM == nil || e.JSM.ID != "jsm-found") {
			t.Errorf("T1's JSM object %+v", e.JSM)
		}
		if e.Key == "teams/T2" && (len(e.Messages) != 1 || e.Messages[0].Class != messages.ManualTask) {
			t.Errorf("T2's messages %+v", e.Messages)
		}
	}

	// Second review: the run used to end completed, so the next plan
	// planned T2 again and the next apply sent it a second time. A call
	// that may have made its object leaves the run unfinished until resume
	// looks it up.
	ctx := context.Background()
	if res.Info.State != review.Interrupted || !res.Info.EndedAt.IsZero() {
		t.Errorf("run %s, ended %v; want interrupted, not ended", res.Info.State, res.Info.EndedAt)
	}
	if u, err := e.Unfinished(ctx); err != nil || u == nil || u.ID != res.Info.ID {
		t.Errorf("Unfinished = %v, %v", u, err)
	}
	pd, _ := e.LatestRead(ctx, review.PagerDuty)
	jsm, _ := e.LatestRead(ctx, review.JSM)
	if _, err := e.Plan(ctx, pd, jsm); !errors.Is(err, run.ErrUnfinishedRun) {
		t.Errorf("plan after an unknown outcome: %v", err)
	}
}

// The second Ctrl+C cancels the call in flight. Its row stays sending: the
// call may have reached JSM, so the run is interrupted, and plan and apply
// refuse until resume settles it.
func TestACanceledCallLeavesTheRunInterrupted(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1", "T2", "T3")
	ctx, cancel := context.WithCancel(context.Background())
	teams.during = func(it review.Item) {
		// The row is committed before the call is made, so a crash during
		// the call leaves it sending for resume to look up.
		var state string
		if err := w.st.DB().QueryRow(`SELECT state FROM journal WHERE item_key = ?`, it.Key).Scan(&state); err != nil || state != "sending" {
			t.Errorf("%s during its call: %q, %v", it.Key, state, err)
		}
		if it.Key == "teams/T2" {
			cancel()
			teams.errs[it.Key] = fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, context.Canceled)
		}
	}
	e := w.engine(teams)
	v := w.plan(e).Version
	res, err := e.Apply(ctx, v, confirmation(v))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Unknown, "teams/T3": review.Pending}
	if got := states(res.Record); res.Info.State != review.Interrupted || !maps(got, want) {
		t.Fatalf("run %s, states %v, want interrupted and %v", res.Info.State, got, want)
	}
	if len(teams.looked) != 0 {
		t.Errorf("a canceled call was looked up: %v", teams.looked)
	}
	var state string
	if err := w.st.DB().QueryRow(`SELECT state FROM journal WHERE item_key = 'teams/T2'`).Scan(&state); err != nil || state != "sending" {
		t.Errorf("T2's journal row: %q, %v", state, err)
	}
	if !exists(t, filepath.Join(w.ws.ArtifactsDir(), review.RunDir(res.Info.ID), review.RunFile)) {
		t.Error("no run record after the interrupt")
	}
	bg := context.Background()
	pd, _ := e.LatestRead(bg, review.PagerDuty)
	jsm, _ := e.LatestRead(bg, review.JSM)
	if _, err := e.Plan(bg, pd, jsm); !errors.Is(err, run.ErrUnfinishedRun) {
		t.Errorf("plan during an unfinished run: %v", err)
	}
	if err := e.CheckApply(bg, v); !errors.Is(err, run.ErrUnfinishedRun) {
		t.Errorf("apply during an unfinished run: %v", err)
	}
}

func TestApplyRefusesWhatItCannotRun(t *testing.T) {
	setup := func(t *testing.T) (*workspace, *run.Engine, *run.PlanVersion) {
		w := newWorkspace(t)
		teams := newStep("teams", &remote{objects: map[string]int{}})
		teams.plan = func(run.PlanInput) (run.Planned, error) {
			return run.Planned{
				Items: []review.Item{{Key: "teams/T1", Action: review.Create, Summary: "Create Platform"}},
				Messages: []messages.Message{
					{Class: messages.Warning, Code: "member_dropped", Item: "teams/T1", Text: "Casey is not migrated."}},
			}, nil
		}
		e := w.engine(teams)
		return w, e, w.plan(e).Version
	}
	ctx := context.Background()

	t.Run("an edited plan.json", func(t *testing.T) {
		w, e, v := setup(t)
		path := filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.PlanFile)
		data, _ := os.ReadFile(path)
		if err := os.WriteFile(path, bytes.Replace(data, []byte("Create Platform"), []byte("Create Platfrom"), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.CheckApply(ctx, v); messagesExit(err) != messages.ExitValidationFailed || !strings.Contains(err.Error(), "changed") {
			t.Errorf("%v", err)
		}
	})
	t.Run("a confirmation that misses a warning", func(t *testing.T) {
		w, e, v := setup(t)
		c := confirmation(v)
		c.Accepted = nil
		if _, err := e.Apply(ctx, v, c); messagesExit(err) != messages.ExitValidationFailed {
			t.Errorf("%v", err)
		}
		if exists(t, filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.ConfirmationFile)) {
			t.Error("a refused confirmation was written")
		}
	})
	t.Run("a version that is not the latest", func(t *testing.T) {
		w, e, v := setup(t)
		e.Steps[0].(*fakeStep).plan = teamsPlan("T9")
		w.plan(e)
		if err := e.CheckApply(ctx, v); messagesExit(err) != messages.ExitValidationFailed {
			t.Errorf("%v", err)
		}
	})
	// Second review: a plan that crashed after writing its folder left v2
	// on disk, and apply ran v1 while the operator had reviewed v2.
	t.Run("a folder newer than state.db knows", func(t *testing.T) {
		w, e, v := setup(t)
		if err := os.MkdirAll(filepath.Join(w.ws.ArtifactsDir(), review.PlanDir(2)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := e.CheckApply(ctx, v); messagesExit(err) != messages.ExitValidationFailed || !strings.Contains(err.Error(), "did not finish") {
			t.Errorf("%v", err)
		}
		// The next plan gets past it, even when it plans the same.
		next := w.plan(e)
		if next.Unchanged || next.Version.Version != 3 {
			t.Fatalf("the next plan: %+v", next)
		}
		if err := e.CheckApply(ctx, next.Version); err != nil {
			t.Errorf("the next version: %v", err)
		}
	})
	t.Run("a step this build does not have", func(t *testing.T) {
		_, e, v := setup(t)
		e.Steps = []run.Step{newStep("users", &remote{objects: map[string]int{}})}
		if err := e.CheckApply(ctx, v); !errors.Is(err, run.ErrUnknownStep) {
			t.Errorf("%v", err)
		}
	})
}

// The run record is rewritten every 100 items, so a long run that crashes
// has a recent record before resume rebuilds it.
func TestTheRecordIsWrittenDuringALongRun(t *testing.T) {
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	var ids []string
	for i := range 150 {
		ids = append(ids, fmt.Sprintf("T%03d", i))
	}
	teams.plan = teamsPlan(ids...)
	var during bool
	teams.during = func(it review.Item) {
		if it.Key == "teams/T120" {
			entries, _ := os.ReadDir(filepath.Join(w.ws.ArtifactsDir(), "runs"))
			during = len(entries) == 1
		}
	}
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)
	if !during {
		t.Error("no run record while the run was going")
	}
	if c := res.Record.Counts(); c.Done != 150 {
		t.Errorf("counts %+v", c)
	}
}

// The messages a run records are sealed in state.db, like everything a step
// or an API wrote.
func TestTheJournalSealsMessages(t *testing.T) {
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.plan = teamsPlan("T1")
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: 400: name Wombats taken: %w", httperr.ErrInvalid)
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)
	if res.Record.Entries[0].State != review.Failed || !strings.Contains(res.Record.Entries[0].Messages[0].Text, "Wombats") {
		t.Fatalf("entry %+v", res.Record.Entries[0])
	}
	for _, f := range []string{w.ws.StatePath(), w.ws.StatePath() + "-wal"} {
		if data, _ := os.ReadFile(f); bytes.Contains(data, []byte("Wombats")) {
			t.Errorf("%s holds a message in clear", filepath.Base(f))
		}
	}
}

// An answer the run record cannot hold never stops the run. Its messages are
// replaced by one that says so; a JSM object it cannot hold leaves the call
// unknown, so resume looks it up instead of sending it again.
func TestAnAnswerTheRecordCannotHold(t *testing.T) {
	w := newWorkspace(t)
	teams := &oddStep{fakeStep: newStep("teams", &remote{objects: map[string]int{}})}
	teams.plan = teamsPlan("T1", "T2", "T3")
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)
	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Unknown, "teams/T3": review.Done}
	if got := states(res.Record); res.Info.State != review.Interrupted || !maps(got, want) {
		t.Fatalf("run %s, states %v, want %v", res.Info.State, got, want)
	}
	codes := map[string]string{}
	for _, e := range res.Record.Entries {
		for _, m := range e.Messages {
			codes[e.Key] += m.Code + " "
		}
	}
	if codes["teams/T1"] != "answer_unrecorded " || codes["teams/T2"] != "object_unrecorded " {
		t.Errorf("messages %v", codes)
	}
}

// oddStep answers T1 with a message code that is not snake_case and T2 with
// a JSM ID that holds a space.
type oddStep struct{ *fakeStep }

func (s *oddStep) Apply(ctx context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	out, err := s.fakeStep.Apply(ctx, it, deps)
	switch it.Key {
	case "teams/T1":
		out.Messages = []messages.Message{{Class: messages.Information, Code: "Not-Snake", Text: "x"}}
	case "teams/T2":
		out.JSM = &review.Ref{Type: "team", ID: "two words"}
	}
	return out, err
}

// Second review: when every call of a run failed, for a permission fixed
// since, the next plan was the same, so it stayed the applied version and
// apply refused it for good. A plan that still makes calls after a run is a
// new version.
func TestAFailedRunCanBeAppliedAgain(t *testing.T) {
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := newStep("teams", r)
	teams.plan = teamsPlan("T1", "T2")
	denied := fmt.Errorf("POST /v1/teams: 403: %w", httperr.ErrForbidden)
	teams.errs["teams/T1"], teams.errs["teams/T2"] = denied, denied
	e := w.engine(teams)
	if res := w.apply(e, w.plan(e).Version); res.Info.State != review.Completed || res.Info.Failed != 2 {
		t.Fatalf("first run %+v", res.Info)
	}
	teams.errs = map[string]error{}
	again := w.plan(e)
	if again.Unchanged || again.Version.Version != 2 || again.Version.Calls != 2 {
		t.Fatalf("the plan after the failed run: unchanged %v, %+v", again.Unchanged, again.Version)
	}
	res := w.apply(e, again.Version)
	if c := res.Record.Counts(); c.Done != 2 {
		t.Errorf("second run %+v", c)
	}
	// Once applied, the same reads give a plan that maps what was made.
	third := w.plan(e)
	if third.Version.Calls != 0 || third.Version.Version != 3 {
		t.Errorf("the plan after the second run: %+v", third.Version)
	}
	if fourth := w.plan(e); !fourth.Unchanged || fourth.Version.Version != 3 {
		t.Errorf("a plan with nothing to call is unchanged: %+v", fourth)
	}
}

// Second review: an unchanged plan kept the JSM read of the plan that first
// gave it, so a lookup compared with a JSM older than the run and could take
// a team the customer made since for one the run made. The run's lookups use
// the latest complete JSM read, and the run records which.
func TestLookupsUseTheLatestJSMRead(t *testing.T) {
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.plan = teamsPlan("T1")
	e := w.engine(teams)
	first := w.plan(e).Version
	teams.reads[review.JSM] = []object{{"team", "made-by-the-customer", map[string]string{"name": "T1"}}}
	second := w.plan(e)
	if !second.Unchanged {
		t.Fatalf("the plan changed: %+v", second)
	}
	latest, err := discover.LatestComplete(context.Background(), w.st, review.JSM, []discover.Reader{teams})
	if err != nil || latest.ID() == first.JSMRead {
		t.Fatalf("LatestComplete = %v, %v", latest, err)
	}
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable)
	res := w.apply(e, second.Version)
	if res.Info.JSMRead != latest.ID() || !slices.Equal(teams.lookedIn, []int64{latest.ID()}) {
		t.Errorf("the run compares with read %d and looked up in %v, want %d", res.Info.JSMRead, teams.lookedIn, latest.ID())
	}
}

// Second review: what an API and a step answered was stored as given. A
// request ID is cleaned before it is stored in clear, and message texts and
// the operator's reasons before they are sealed, as the files show them.
func TestAnswersAreCleanedBeforeTheyAreStored(t *testing.T) {
	w := newWorkspace(t)
	teams := &requestIDStep{fakeStep: newStep("teams", &remote{objects: map[string]int{}})}
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{
			Items:    []review.Item{{Key: "teams/T1", Action: review.Create, Summary: "Create Platform"}},
			Messages: []messages.Message{{Class: messages.Warning, Code: "member_dropped", Item: "teams/T1", Text: "Casey is not migrated."}},
		}, nil
	}
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: 400: call +14155550124: %w", httperr.ErrInvalid)
	e := w.engine(teams)
	v := w.plan(e).Version
	c := confirmation(v)
	c.Accepted[0].Reason = "Casey moved; reach her on +14155550125"
	res, err := e.Apply(context.Background(), v, c)
	if err != nil {
		t.Fatal(err)
	}
	entry := res.Record.Entries[0]
	if strings.Contains(entry.RequestID, "4155550123") || strings.Contains(entry.RequestID, "SECRETSECRET") ||
		strings.Contains(entry.Messages[0].Text, "4155550124") {
		t.Errorf("the journal kept what it was given: %+v", entry)
	}
	for _, f := range []string{w.ws.StatePath(), w.ws.StatePath() + "-wal"} {
		if data, _ := os.ReadFile(f); bytes.Contains(data, []byte("4155550123")) || bytes.Contains(data, []byte("SECRETSECRET")) {
			t.Errorf("%s holds the request ID as the API gave it", filepath.Base(f))
		}
	}
	var sealed []byte
	if err := w.st.DB().QueryRow(`SELECT record FROM confirmations WHERE plan_version = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	record, err := w.st.Cipher().Open(store.FieldRef{Table: "confirmations", Item: "v1", Column: "record"}, sealed)
	if err != nil || bytes.Contains(record, []byte("4155550125")) {
		t.Errorf("the sealed confirmation keeps the reason as typed: %v\n%s", err, record)
	}
}

// requestIDStep answers with a request ID that holds a phone number and a
// token.
type requestIDStep struct{ *fakeStep }

func (s *requestIDStep) Apply(ctx context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	out, err := s.fakeStep.Apply(ctx, it, deps)
	out.RequestID = "call +14155550123 token=SECRETSECRET"
	return out, err
}

func TestFinalReportFailureLeavesJournalRecoverable(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)
	s := newStep("teams", &remote{objects: map[string]int{}})
	s.plan = teamsPlan("T1")
	e := w.engine(s)
	v := w.plan(e).Version
	if err := os.WriteFile(filepath.Join(w.ws.ArtifactsDir(), "runs"), []byte("unwritable report directory"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := e.Apply(ctx, v, confirmation(v))
	if err == nil {
		t.Fatal("expected report write to fail")
	}
	open, err := e.Unfinished(ctx)
	if err != nil || open == nil || open.ID != res.Info.ID || open.State != review.Running || open.Written != 1 || res.Info.State != open.State {
		t.Fatalf("run %+v, unfinished %+v, err %v", res.Info, open, err)
	}
	if _, err := e.Apply(ctx, v, confirmation(v)); !errors.Is(err, run.ErrUnfinishedRun) || len(s.applied) != 1 {
		t.Fatalf("second apply: %v, calls %v", err, s.applied)
	}
}

func TestUnboundReadsAndMappingsFromOlderWorkspaces(t *testing.T) {
	ctx := t.Context()
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%t", mapped), func(t *testing.T) {
			w := newWorkspace(t)
			s := newStep("teams", &remote{objects: map[string]int{}})
			s.plan = teamsPlan("T1")
			e := w.engine(s)
			v := w.plan(e).Version
			if mapped {
				w.apply(e, v)
			}
			// The migration leaves the new account column NULL for every old read.
			if _, err := w.st.DB().ExecContext(ctx, `UPDATE snapshot_reads SET account=NULL`); err != nil {
				t.Fatal(err)
			}
			read, err := e.LatestRead(ctx, review.JSM)
			if err != nil || read != nil {
				t.Fatalf("reused a legacy read: %+v, %v", read, err)
			}
			_, err = e.Read(ctx, review.JSM)
			if mapped {
				if err == nil || !strings.Contains(err.Error(), "existing mappings predate account binding") {
					t.Fatalf("legacy mappings: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// answerStep answers the calls named in objects with the JSM object given
// there instead: nil for none.
type answerStep struct {
	*fakeStep
	objects map[string]*review.Ref
}

func (s *answerStep) Apply(ctx context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	out, err := s.fakeStep.Apply(ctx, it, deps)
	if ref, ok := s.objects[it.Key]; ok {
		out.JSM = ref
	}
	return out, err
}

// Third review: a create that answered without naming the JSM object it made
// was recorded done with no mapping, so the next plan planned it again and
// the next apply created a second one. Such a create is looked up, as an
// unknown outcome is, and stays unknown, keeping the run unfinished, until
// something names what it made. An object the record cannot hold is the
// same: nothing is mapped, and the run is unfinished.
func TestACreateThatNamesNoObjectIsLookedUp(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)
	r := &remote{objects: map[string]int{}}
	teams := &answerStep{fakeStep: newStep("teams", r), objects: map[string]*review.Ref{
		"teams/T1": nil, "teams/T2": nil, "teams/T3": {Type: "team", ID: "two words"}, "teams/T4-ops": nil,
	}}
	teams.plan = func(in run.PlanInput) (run.Planned, error) {
		p, err := teamsPlan("T1", "T2", "T3")(in)
		// A call that links no PagerDuty object needs no JSM object to map.
		p.Items = append(p.Items, review.Item{Key: "teams/T4-ops", Action: review.Create, Summary: "Turn on Operations"})
		return p, err
	}
	teams.found["teams/T1"] = run.Lookup{Outcome: run.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-found"}}
	e := w.engine(teams)
	res := w.apply(e, w.plan(e).Version)

	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Unknown,
		"teams/T3": review.Unknown, "teams/T4-ops": review.Done}
	if got := states(res.Record); !maps(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	if !slices.Equal(teams.looked, []string{"teams/T1", "teams/T2"}) {
		t.Errorf("lookups %v, want the two creates that named no object", teams.looked)
	}
	codes := map[string]string{}
	for _, e := range res.Record.Entries {
		for _, m := range e.Messages {
			codes[e.Key] += m.Code + " "
		}
		if e.Key == "teams/T2" && (len(e.Messages) != 1 || e.Messages[0].Class != messages.ManualTask) {
			t.Errorf("T2's messages %+v", e.Messages)
		}
	}
	if codes["teams/T1"] != "object_found " || codes["teams/T2"] != "object_unidentified " ||
		codes["teams/T3"] != "object_unrecorded " || codes["teams/T4-ops"] != "" {
		t.Errorf("messages %v", codes)
	}
	if res.Info.State != review.Interrupted || !res.Info.EndedAt.IsZero() {
		t.Errorf("run %s, ended %v; want interrupted, not ended", res.Info.State, res.Info.EndedAt)
	}
	rows, err := w.st.DB().QueryContext(ctx, `SELECT pagerduty_id, jsm_id FROM mappings ORDER BY pagerduty_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var mapped []string
	for rows.Next() {
		var pd, jsm string
		if err := rows.Scan(&pd, &jsm); err != nil {
			t.Fatal(err)
		}
		mapped = append(mapped, pd+"="+jsm)
	}
	if !slices.Equal(mapped, []string{"T1=jsm-found"}) {
		t.Errorf("mappings %v, want only the one the lookup found", mapped)
	}
	pd, _ := e.LatestRead(ctx, review.PagerDuty)
	jsm, _ := e.LatestRead(ctx, review.JSM)
	if _, err := e.Plan(ctx, pd, jsm); !errors.Is(err, run.ErrUnfinishedRun) {
		t.Errorf("plan after a create that named no object: %v", err)
	}
	for key, n := range r.objects {
		if n != 1 {
			t.Errorf("%s was sent %d times", key, n)
		}
	}
}

// Third review: a credential pasted into a reason has no label for a pattern
// to find, so confirmation.md kept it as typed. The reasons are cleaned once,
// with the credentials of the invocation, and the same cleaned confirmation
// is sealed in state.db and written as confirmation.md.
func TestAKnownCredentialInAReasonIsNeverKept(t *testing.T) {
	const token = "Zq7vR2mK9pL4xW8n" // a bare value no pattern finds
	if credentials.RedactText(token) != token {
		t.Fatal("the patterns find the token; the test needs one they do not")
	}
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{
			Items:    []review.Item{{Key: "teams/T1", Action: review.Create, Summary: "Create Platform"}},
			Messages: []messages.Message{{Class: messages.Warning, Code: "member_dropped", Item: "teams/T1", Text: "Casey is not migrated."}},
		}, nil
	}
	e := w.engine(teams)
	e.Redactor = credentials.NewRedactor(credentials.NewSecret(token))
	v := w.plan(e).Version
	c := confirmation(v)
	c.Accepted[0].Reason = "approved with " + token
	if _, err := e.Apply(context.Background(), v, c); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(filepath.Join(w.ws.ArtifactsDir(), v.Dir, review.ConfirmationFile))
	// Markdown shows the brackets of [redacted] escaped.
	if err != nil || bytes.Contains(conf, []byte(token)) || !bytes.Contains(conf, []byte(`approved with \[redacted\]`)) {
		t.Errorf("confirmation.md: %v\n%s", err, conf)
	}
	var sealed []byte
	if err := w.st.DB().QueryRow(`SELECT record FROM confirmations WHERE plan_version = ?`, v.Version).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	record, err := w.st.Cipher().Open(store.FieldRef{Table: "confirmations", Item: "v1", Column: "record"}, sealed)
	if err != nil || bytes.Contains(record, []byte(token)) || !bytes.Contains(record, []byte("approved with "+credentials.Redacted)) {
		t.Errorf("the sealed confirmation: %v\n%s", err, record)
	}
}

// versionedStep is a step whose part of a read is in the given format.
type versionedStep struct {
	*fakeStep
	version int
}

func (s *versionedStep) ReadVersion() int { return s.version }

// Third review: a read was reused by side alone, so once a build gained a
// step, plan --only took a JSM read made without it as proof that JSM has
// none of that step's objects. A read records the steps that made it and
// the format of each, and only the same steps reuse it.
func TestAReadIsReusedOnlyByTheStepsThatMadeIt(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)
	before := w.engine(run.Placeholders()...)
	first := w.plan(before).Version

	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.reads[review.JSM] = []object{{"team", "jsm-1", map[string]string{"name": "Platform"}}}
	after := w.engine(append(run.Placeholders(), teams)...)
	for _, side := range []review.Side{review.PagerDuty, review.JSM} {
		if s, err := after.LatestRead(ctx, side); err != nil || s != nil {
			t.Errorf("%s: a read made without the teams step was reused: %v, %v", side, s, err)
		}
	}
	fresh, err := after.Read(ctx, review.JSM)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := after.LatestRead(ctx, review.JSM); err != nil || s == nil || s.ID() != fresh.ID() {
		t.Errorf("the read of the same steps: %v, %v; want read %d", s, err, fresh.ID())
	}
	// The steps that made the first read still reuse it.
	if s, err := before.LatestRead(ctx, review.JSM); err != nil || s == nil || s.ID() != first.JSMRead {
		t.Errorf("the placeholder's own read: %v, %v; want read %d", s, err, first.JSMRead)
	}
	// A step that changes the format of what it reads starts afresh too.
	newer := w.engine(append(run.Placeholders(), &versionedStep{fakeStep: teams, version: 2})...)
	if s, err := newer.LatestRead(ctx, review.JSM); err != nil || s != nil {
		t.Errorf("a read in the older format was reused: %v, %v", s, err)
	}
}

// The same holds for the read apply's lookups compare with: a newer complete
// JSM read made by other steps says nothing about what this plan's steps
// would find, so the lookups use the plan's own.
func TestLookupsUseOnlyAReadOfTheSameSteps(t *testing.T) {
	ctx := context.Background()
	w := newWorkspace(t)
	teams := newStep("teams", &remote{objects: map[string]int{}})
	teams.plan = teamsPlan("T1")
	e := w.engine(teams)
	v := w.plan(e).Version
	other, err := w.engine(teams, newStep("users", &remote{objects: map[string]int{}})).Read(ctx, review.JSM)
	if err != nil || other.ID() <= v.JSMRead || !other.Complete() {
		t.Fatalf("the other steps' read: %v, %v", other, err)
	}
	teams.errs["teams/T1"] = fmt.Errorf("POST /v1/teams: %w: %w", httperr.ErrUnknownOutcome, httperr.ErrUnavailable)
	res := w.apply(e, v)
	if res.Info.JSMRead != v.JSMRead || !slices.Equal(teams.lookedIn, []int64{v.JSMRead}) {
		t.Errorf("the run compares with read %d and looked up in %v, want %d", res.Info.JSMRead, teams.lookedIn, v.JSMRead)
	}
}

// Third review: when the run record could not be written, apply returned an
// empty record, so the command reported every call it had made as not taken
// yet. The record is rebuilt from the journal, which has every result, and
// says that its files were not written.
func TestARecordThatCannotBeWrittenStillSaysWhatTheRunDid(t *testing.T) {
	w := newWorkspace(t)
	s := newStep("teams", &remote{objects: map[string]int{}})
	s.plan = teamsPlan("T1", "T2")
	e := w.engine(s)
	v := w.plan(e).Version
	if err := os.WriteFile(filepath.Join(w.ws.ArtifactsDir(), "runs"), []byte("unwritable report directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.Apply(context.Background(), v, confirmation(v))
	if err == nil {
		t.Fatal("expected the record write to fail")
	}
	want := map[string]review.EntryState{"teams/T1": review.Done, "teams/T2": review.Done}
	if got := states(res.Record); res.Record.ID != res.Info.ID || !maps(got, want) || res.RecordWritten {
		t.Errorf("record %s, states %v, written %v; want %s, %v, not written", res.Record.ID, got, res.RecordWritten, res.Info.ID, want)
	}

	// A run that cannot start its calls has made none.
	w2 := newWorkspace(t)
	e2 := w2.engine(newStep("teams", &remote{objects: map[string]int{}}))
	e2.Steps[0].(*fakeStep).plan = teamsPlan("T1")
	v2 := w2.plan(e2).Version
	if err := os.Mkdir(filepath.Join(w2.ws.Dir(), run.ActiveRunFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w2.ws.Dir(), run.ActiveRunFile, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = e2.Apply(context.Background(), v2, confirmation(v2))
	if err == nil {
		t.Fatal("expected active-run.json to fail")
	}
	if got := states(res.Record); res.Record.ID != res.Info.ID || !maps(got, map[string]review.EntryState{"teams/T1": review.Pending}) {
		t.Errorf("record %s, states %v; want every item pending", res.Record.ID, got)
	}
}

// Third review: plan reused the latest version when its plan.json was there
// and unchanged, whatever else of its folder was missing, so a lost
// acceptance-template.yaml stayed lost and apply kept pointing at it. A
// version missing any review file is written anew, as one missing plan.json
// is.
func TestAVersionMissingAReviewFileIsWrittenAnew(t *testing.T) {
	for _, f := range review.PlanFiles {
		t.Run(f, func(t *testing.T) {
			w := newWorkspace(t)
			s := newStep("teams", &remote{objects: map[string]int{}})
			s.plan = teamsPlan("T1")
			e := w.engine(s)
			v := w.plan(e).Version
			if err := os.Remove(filepath.Join(w.ws.ArtifactsDir(), v.Dir, f)); err != nil {
				t.Fatal(err)
			}
			next := w.plan(e)
			if next.Unchanged || next.Version.Version != 2 {
				t.Fatalf("the plan after %s went missing: %+v", f, next)
			}
			for _, g := range review.PlanFiles {
				if !exists(t, filepath.Join(w.ws.ArtifactsDir(), next.Version.Dir, g)) {
					t.Errorf("v2 has no %s", g)
				}
			}
			if again := w.plan(e); !again.Unchanged || again.Version.Version != 2 {
				t.Errorf("a complete version was not kept: %+v", again)
			}
		})
	}
}
