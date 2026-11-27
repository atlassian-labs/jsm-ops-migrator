package run_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
)

func mappingEngine(w *workspace) *run.Engine {
	step := newStep("users", &remote{})
	step.plan = func(run.PlanInput) (run.Planned, error) {
		item := review.Item{
			Key: "users/P1", Action: review.Manual,
			PagerDuty:   &review.Ref{Type: "user", ID: "P1"},
			UserMapping: &review.UserMapping{Decision: "review_ambiguous"},
		}
		return run.Planned{Items: []review.Item{item}}, nil
	}
	return w.engine(step)
}

func TestMappingApprovalHistoryAndReceiptRepair(t *testing.T) {
	w := newWorkspace(t)
	e := mappingEngine(w)
	v := w.plan(e).Version
	needApproval := func(want bool) {
		t.Helper()
		s, err := run.ReadStatus(t.Context(), w.ws)
		if err != nil || s.Plan == nil || s.Plan.NeedsMappingApproval != want {
			t.Fatalf("menu status %+v, %v", s, err)
		}
	}
	needApproval(true)
	if err := e.CheckPlanSteps(t.Context(), v); err == nil {
		t.Fatal("plan ready before approval")
	}
	save := func(who, decision string) *run.MappingApproval {
		t.Helper()
		a, err := e.SaveMappingApproval(t.Context(), v, who, []review.MappingDecision{{PagerDutyID: "P1", Decision: decision}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := save("admin-a", "defer")
	needApproval(false)
	if err := e.CheckPlanSteps(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	w.wait(time.Minute)
	second := save("admin-a", "skip")
	w.wait(time.Minute)
	last := save("admin-b", "skip")
	expected := []run.MappingRevision{
		{ApprovedBy: first.ApprovedBy, ApprovedAt: first.ApprovedAt, Decisions: first.Decisions},
		{ApprovedBy: second.ApprovedBy, ApprovedAt: second.ApprovedAt, Decisions: second.Decisions},
	}
	if !reflect.DeepEqual(second.History, expected[:1]) || !reflect.DeepEqual(last.History, expected) {
		t.Fatalf("history: %+v", last)
	}
	path := filepath.Join(w.ws.ArtifactsDir(), last.Artifact)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	w.reopen()
	e = mappingEngine(w)
	w.wait(time.Minute)
	repaired := save("admin-b", "skip")
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, back) || !reflect.DeepEqual(last, repaired) {
		t.Fatal("retry changed the approval or lost a revision")
	}
	var receipt run.MappingApproval
	if err := json.Unmarshal(back, &receipt); err != nil || len(receipt.History) != 2 {
		t.Fatalf("invalid receipt: %v", err)
	}
}

func TestLegacyMappingExportNeedsFreshPlan(t *testing.T) {
	w := newWorkspace(t)
	e := mappingEngine(w)
	v := w.plan(e).Version
	ctx := t.Context()
	// Existing rows have NULL in the column added by the review-format migration.
	if _, err := w.st.DB().ExecContext(ctx, "UPDATE plan_versions SET requires_mapping_review = NULL WHERE version = ?", v.Version); err != nil {
		t.Fatal(err)
	}
	old := w.mustLatest(e)
	status, err := run.ReadStatus(ctx, w.ws)
	if err != nil || !old.LegacyMappingExport || status.Plan == nil || !status.Plan.LegacyMappingExport {
		t.Fatalf("legacy plan status %+v, %v", status, err)
	}
	if err := e.CheckPlanSteps(ctx, old); err == nil || !strings.Contains(err.Error(), "run plan again") {
		t.Fatalf("legacy approval hint: %v", err)
	}
	// Simulate an already-approved plan from the earlier CLI.
	if _, err := e.SaveMappingApproval(ctx, old, "admin", []review.MappingDecision{{PagerDutyID: "P1", Decision: "defer"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckPlanSteps(ctx, old); err != nil {
		t.Fatal(err)
	}
	status, err = run.ReadStatus(ctx, w.ws)
	if err != nil || status.Plan == nil || status.Plan.LegacyMappingExport || status.Plan.NeedsMappingApproval {
		t.Fatalf("previously approved plan status %+v, %v", status, err)
	}
	next := w.plan(e)
	if next.Unchanged || next.Version.Version != old.Version+1 || next.Version.LegacyMappingExport {
		t.Fatal("plan did not create a fresh mapping export for identical source data")
	}
	if err := e.CheckPlanSteps(ctx, next.Version); err == nil || !strings.Contains(err.Error(), "approve-mappings") {
		t.Fatalf("new plan approval hint: %v", err)
	}
}
