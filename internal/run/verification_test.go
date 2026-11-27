package run_test

import (
	"errors"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
)

func TestReadinessDefersOnlyItsDependentsAcrossResume(t *testing.T) {
	w := newWorkspace(t)
	s := newStep("users", &remote{objects: map[string]int{}})
	s.plan = func(run.PlanInput) (run.Planned, error) {
		return run.Planned{Items: []review.Item{
			{Key: "users/check", Action: review.Verify, PagerDuty: pdRef("P1")},
			{Key: "users/dependent", Action: review.Create, DependsOn: []string{"users/check"}},
			{Key: "users/independent", Action: review.Create},
		}}, nil
	}
	s.errs["users/check"] = errors.New("temporary lookup failure")
	e := w.engine(s)
	r := w.apply(e, w.plan(e).Version)
	got := states(r.Record)
	if r.Info.State != review.Interrupted || r.Info.Written != 1 || got["users/check"] != review.Pending || got["users/dependent"] != review.Pending || got["users/independent"] != review.Done {
		t.Fatalf("first pass: %+v, %v", r.Info, got)
	}
	w.reopen()
	delete(s.errs, "users/check")
	r = w.resume(w.engine(s))
	if r.Info.State != review.Completed || r.Info.Written != 3 || s.remote.objects["users/independent"] != 1 || s.deps["users/dependent"]["users/check"].JSM == nil {
		t.Fatalf("resumed: %+v", r.Info)
	}
}
