package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

// paused plans two teams and applies them, with pause run from another
// window during the first call, as an operator would.
func paused(t *testing.T) (*loginSetup, *teamStep) {
	t.Helper()
	s, step := planned(t)
	step.during = func(_ context.Context, key string) error {
		if key != "teams/PT1" {
			return nil
		}
		r := s.run("pause")
		if r.code != ExitOK || !strings.Contains(r.stdout, "Asked run") || !strings.Contains(r.stdout, "0 of 2 items have a result") {
			t.Errorf("pause during the apply: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
		}
		return nil
	}
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known"))
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "was paused after 1 of 2 items") ||
		!strings.Contains(r.stderr, "jsm-ops-migrator resume") {
		t.Fatalf("apply: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	step.during = nil
	return s, step
}

func TestPauseWithNoRunGoing(t *testing.T) {
	s, _ := planned(t)
	if r := s.run("pause"); r.code != ExitOK || strings.TrimSpace(r.stdout) != "No apply is running, so there is nothing to pause." {
		t.Errorf("exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to resume") {
		t.Errorf("resume: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
}

func TestResumeWithNothingToDoNeedsNoWorkspaceKey(t *testing.T) {
	s, _ := planned(t)
	keychain := s.kc
	check := func() {
		t.Helper()
		s.kc = testkit.NewFakeKeychain()
		defer func() { s.kc = keychain }()
		r := s.run("resume", "--output", "json")
		var out map[string]json.RawMessage
		if err := decodeOne(r.stdout, &out); err != nil || r.code != ExitOK || string(out["run"]) != "null" {
			t.Fatalf("resume: exit %d, stderr %q, stdout %q, decode error %v", r.code, r.stderr, r.stdout, err)
		}
	}
	check()
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known")); r.code != ExitOK {
		t.Fatal(r.stderr)
	}
	check()
}

// pause stops an apply running in another window before its next call;
// resume takes only what is left.
func TestPauseThenResume(t *testing.T) {
	s, step := paused(t)
	if strings.Join(step.calls, " ") != "teams/PT1" {
		t.Errorf("calls %v", step.calls)
	}
	r := s.run("pause")
	if r.code != ExitOK || !strings.Contains(r.stdout, "No apply is running") || !strings.Contains(r.stdout, "of plan v1 is paused after 1 of 2 items") {
		t.Errorf("pause after the pause: exit %d\n%s", r.code, r.stdout)
	}
	if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitError || !strings.Contains(r.stderr, "resume") {
		t.Errorf("plan during a paused run: exit %d, stderr %q", r.code, r.stderr)
	}

	r = s.run("resume", "--jsm-plan", "premium")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Resuming run") || !strings.Contains(r.stdout, "1 of 2 items have a result") ||
		!strings.Contains(r.stdout, "of plan v1: 2 of 2 items done") {
		t.Fatalf("resume: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("calls %v: each once", step.calls)
	}
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to resume") {
		t.Errorf("a second resume: exit %d\n%s", r.code, r.stdout)
	}
}

// The first Ctrl+C stops apply after the call it is making, with a notice
// that says how long that can take.
func TestTheFirstCtrlCPausesApply(t *testing.T) {
	s, step := planned(t)
	step.during = func(_ context.Context, key string) error {
		if key == "teams/PT1" {
			s.env.RequestInterrupt()
		}
		return nil
	}
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known"))
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, stopNotice) || !strings.Contains(r.stderr, "was paused after 1 of 2 items") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if strings.Join(step.calls, " ") != "teams/PT1" {
		t.Errorf("calls %v", step.calls)
	}
}

// A Ctrl+C before the run starts, at the prompt or during the checks, stops
// apply before the run exists, so nothing waits for resume.
func TestACtrlCBeforeTheRunStopsApply(t *testing.T) {
	s, step := planned(t)
	s.terminal = true
	s.answers = [][]string{{"Casey left the company"}}
	s.choices = []string{"yes"}
	s.onChoose = func() { s.env.RequestInterrupt() }
	r := s.run("apply", "--jsm-plan", "premium")
	if r.code != ExitError || !strings.Contains(r.stderr, "stopped at Ctrl+C: plan v1 was not applied") || len(step.calls) != 0 {
		t.Fatalf("exit %d, stderr %q, calls %v", r.code, r.stderr, step.calls)
	}
	s.onChoose = nil

	// During the checks: nobody is asked to confirm after asking to stop.
	s.asked = nil
	s.onSteps = func() { s.env.RequestInterrupt() }
	r = s.run("apply", "--jsm-plan", "premium")
	if r.code != ExitError || !strings.Contains(r.stderr, "stopped at Ctrl+C") || len(s.asked) != 0 {
		t.Errorf("a Ctrl+C during the checks: exit %d, stderr %q, asked %v", r.code, r.stderr, s.asked)
	}
	s.onSteps, s.terminal = nil, false
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to resume") {
		t.Errorf("resume: exit %d\n%s", r.code, r.stdout)
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known")); r.code != ExitOK {
		t.Errorf("apply after: exit %d, stderr %q", r.code, r.stderr)
	}
}

// The second Ctrl+C cancels the call in flight, which resume looks up
// before it sends anything: unknown waits, and not found is sent again.
func TestTheSecondCtrlCLeavesTheCallForResume(t *testing.T) {
	s, step := planned(t)
	step.during = func(ctx context.Context, key string) error {
		if key == "teams/PT1" {
			s.env.RequestInterrupt()
			s.env.RequestInterrupt()
			return ctx.Err()
		}
		return nil
	}
	r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known"))
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "was interrupted after 1 of 2 items") {
		t.Fatalf("apply: exit %d, stderr %q", r.code, r.stderr)
	}
	step.during = nil
	r = s.run("resume", "--jsm-plan", "premium")
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "1 call has an unknown outcome") {
		t.Fatalf("resume with a lookup that cannot tell: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("calls %v: the unknown call is never sent again", step.calls)
	}
	step.found["teams/PT1"] = engine.Lookup{Outcome: engine.LookupNotFound}
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK {
		t.Fatalf("resume after not found: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2 teams/PT1" {
		t.Errorf("calls %v: sent again once proven not to have taken effect", step.calls)
	}
}

// The confirmation names the account whose API token makes the run's calls,
// so resume needs that account's token.
func TestResumeNeedsTheAccountThatConfirmed(t *testing.T) {
	s, step := paused(t)
	s.apis.AccountID = "someone-else"
	r := s.run("resume", "--jsm-plan", "premium")
	if r.code != messages.ExitValidationFailed || !strings.Contains(r.stderr, "confirmed for the API token of account account-of-org-1") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if strings.Join(step.calls, " ") != "teams/PT1" {
		t.Errorf("calls %v", step.calls)
	}
}

func TestResumeAndPausePrintOneJSONDocument(t *testing.T) {
	s, _ := paused(t)
	r := s.run("pause", "--output", "json")
	var p pauseOutput
	if err := decodeOne(r.stdout, &p); err != nil || r.code != ExitOK || p.Running || p.Run == nil || p.Run.State != review.Paused || p.Run.Written != 1 {
		t.Errorf("pause: exit %d, %v, %+v\n%s", r.code, err, p.Run, r.stdout)
	}
	r = s.run("resume", "--jsm-plan", "premium", "--output", "json")
	var out applyOutput
	if err := decodeOne(r.stdout, &out); err != nil || r.code != ExitOK || out.Run == nil || out.Run.State != review.Completed ||
		out.Run.Counts.Done != 2 {
		t.Errorf("resume: exit %d, %v, %+v\n%s", r.code, err, out.Run, r.stdout)
	}
	r = s.run("resume", "--jsm-plan", "premium", "--output", "json")
	var none map[string]json.RawMessage
	if err := decodeOne(r.stdout, &none); err != nil || r.code != ExitOK || string(none["run"]) != "null" {
		t.Errorf("nothing to resume: exit %d, %v\n%s", r.code, err, r.stdout)
	}
}

// The menu reads where the run stands without the data key, so it shows it
// even when the keychain that holds the key is not there.
func TestTheMenuShowsTheRunWithoutTheKey(t *testing.T) {
	// The credentials are in the environment, so only the key is missing.
	s, _ := paused(t)
	m := &scriptedMenu{}
	runMenuSession(t, defaultBodies(), Options{Keychain: testkit.NewFakeKeychain()}, m)
	shown := strings.Join(m.shown[0], "\n")
	if !strings.Contains(shown, "Apply of plan v1 was paused after 1 of 2 writes") || m.selected[0] != "resume" {
		t.Errorf("shown %q, selected %v", shown, m.selected)
	}

	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK {
		t.Fatalf("resume: exit %d, stderr %q", r.code, r.stderr)
	}
	m = &scriptedMenu{}
	runMenuSession(t, defaultBodies(), Options{Keychain: testkit.NewFakeKeychain()}, m)
	if shown := strings.Join(m.shown[0], "\n"); !strings.Contains(shown, "Plan v1 applied: 2 written, 0 failed") {
		t.Errorf("after resume: %q", shown)
	}
}
