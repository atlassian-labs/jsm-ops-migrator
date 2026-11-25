package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
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

// The second Ctrl+C cancels the call in flight. Resume looks up the create
// until it can map the object, even when the lookup misses after a delay.
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
	r = s.run("resume", "--jsm-plan", "premium")
	if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "The create may have succeeded") {
		t.Fatalf("resume right after the call: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("calls %v: the create was sent again", step.calls)
	}
	s.wait(time.Hour)
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != messages.ExitPartial {
		t.Fatalf("resume after another lookup miss: exit %d, %s", r.code, r.stderr)
	}
	step.found["teams/PT1"] = engine.Lookup{Outcome: engine.LookupFound, JSM: &review.Ref{Type: "team", ID: "reconciled-team"}}
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK {
		t.Fatalf("resume after lookup found the object: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
	}
	if strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("calls %v: each create should be sent once", step.calls)
	}
}

func TestResumeWaitsForRequiredPreflight(t *testing.T) {
	s, step := paused(t)
	s.apis.AccessErr = fmt.Errorf("GET /v1/teams: %w", httperr.ErrUnavailable)
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitError ||
		!strings.Contains(r.stderr, "nothing was written") || !strings.Contains(r.stderr, "Atlassian email and API token") {
		t.Errorf("resume: exit %d, stderr %q", r.code, r.stderr)
	}
	if strings.Join(step.calls, " ") != "teams/PT1" {
		t.Errorf("calls after unavailable preflight: %v", step.calls)
	}
	s.apis.AccessErr = nil
	if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK || strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("resume after preflight recovered: exit %d, stderr %q, calls %v", r.code, r.stderr, step.calls)
	}
}

func TestResumeSkipCreateFlagLeavesTheWriteForANewPlan(t *testing.T) {
	s, step := planned(t)
	step.during = func(ctx context.Context, key string) error {
		if key == "teams/PT1" {
			s.env.RequestInterrupt()
			s.env.RequestInterrupt()
			return ctx.Err()
		}
		return nil
	}
	if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known")); r.code != messages.ExitPartial {
		t.Fatalf("apply: exit %d, stderr %s", r.code, r.stderr)
	}
	step.during = nil
	step.found["teams/PT1"] = engine.Lookup{Outcome: engine.LookupNotFound}
	s.wait(time.Hour)
	r := s.run("resume", "--jsm-plan", "premium", "--skip-create", "teams/PT1")
	if r.code != messages.ExitPartial || !strings.Contains(r.stdout, "create_skipped") || strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
		t.Errorf("resume: exit %d, calls %v, stderr %s\n%s", r.code, step.calls, r.stderr, r.stdout)
	}
	if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, "Plan v2: 1 call") {
		t.Errorf("replan: exit %d, stderr %s\n%s", r.code, r.stderr, r.stdout)
	}
}

func TestResumeBatchesSkipCreateWithOnePreflight(t *testing.T) {
	for _, firstMisses := range []bool{true, false} {
		t.Run(fmt.Sprintf("first lookup misses=%t", firstMisses), func(t *testing.T) {
			s, step := planned(t)
			const first, second = "teams/PT1", "teams/PT2"
			step.fail[first] = httperr.ErrUnknownOutcome
			step.fail[second] = httperr.ErrUnknownOutcome
			if r := s.run("apply", "--jsm-plan", "premium", "--accept-file", s.acceptanceFile("known")); r.code != messages.ExitPartial {
				t.Fatalf("apply: exit %d, stderr %s", r.code, r.stderr)
			}
			if firstMisses {
				step.found[first] = engine.Lookup{Outcome: engine.LookupNotFound}
			}
			step.found[second] = engine.Lookup{Outcome: engine.LookupNotFound}
			s.wait(time.Hour)
			if r := s.run("resume", "--jsm-plan", "premium", "--skip-create", first, "--skip-create", "teams/missing"); r.code != messages.ExitValidationFailed {
				t.Fatalf("resume with an unknown key: exit %d, stderr %s", r.code, r.stderr)
			}
			before := len(s.apis.Calls())
			// Journal order governs execution even when flags are supplied in reverse.
			r := s.run("resume", "--jsm-plan", "premium", "--skip-create", second, "--skip-create", first)
			wantSkips := 2
			if !firstMisses {
				wantSkips = 1
			}
			if r.code != messages.ExitPartial || strings.Count(r.stdout, "create_skipped") != wantSkips {
				t.Fatalf("resume: exit %d, stderr %s\n%s", r.code, r.stderr, r.stdout)
			}
			if checks := strings.Count(strings.Join(s.apis.Calls()[before:], "\n"), "ops teams "); checks != 1 {
				t.Errorf("%d JSM access checks, want one preflight for the batch", checks)
			}
			if !firstMisses {
				step.found[first] = engine.Lookup{Outcome: engine.LookupFound, JSM: &review.Ref{Type: "team", ID: "jsm-recovered"}}
				if r := s.run("resume", "--jsm-plan", "premium"); r.code != messages.ExitPartial || !strings.Contains(r.stdout, "create_skipped") {
					t.Fatalf("resume after the earlier create reconciles: exit %d, stderr %s\n%s", r.code, r.stderr, r.stdout)
				}
			}
			if strings.Join(step.calls, " ") != first+" "+second {
				t.Errorf("resume resent a create: %v", step.calls)
			}
			want := fmt.Sprintf("Plan v2: %d call", wantSkips)
			if r := s.run("plan", "--jsm-plan", "premium"); r.code != ExitOK || !strings.Contains(r.stdout, want) {
				t.Errorf("replan: exit %d, stderr %s\n%s", r.code, r.stderr, r.stdout)
			}
		})
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
	for _, f := range []string{engine.ActiveRunFile, engine.PauseRequestFile} {
		if _, err := os.Stat(filepath.Join(s.dir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s after a resume that stopped at its checks: %v", f, err)
		}
	}
}

// pause from another window while resume runs its checks is accepted, and
// the run stops before its first item, whether it was paused or crashed. A
// request left from before resume started, which a crash or a pause as the
// run stopped leaves, is not for it, and resume goes on.
func TestPauseDuringResumesChecks(t *testing.T) {
	for _, c := range []struct {
		name string
		// left is what is in the workspace when resume starts.
		left []string
		// pause is whether pause runs during the checks.
		pause bool
	}{
		{"after a pause", nil, true},
		{"after a crash", []string{engine.ActiveRunFile, engine.PauseRequestFile}, true},
		{"a request from before", []string{engine.PauseRequestFile}, false},
		{"a crash's request from before", []string{engine.ActiveRunFile, engine.PauseRequestFile}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, step := paused(t)
			ws, err := store.OpenWorkspace(s.dir, false)
			if err != nil {
				t.Fatal(err)
			}
			status, err := engine.ReadStatus(context.Background(), ws)
			if err != nil || status.Run == nil {
				t.Fatalf("ReadStatus = %+v, %v", status, err)
			}
			run := status.Run
			for _, f := range c.left {
				var data []byte
				if f == engine.ActiveRunFile {
					data, err = json.Marshal(engine.ActiveRun{RunID: run.ID, PlanVersion: run.PlanVersion, StartedAt: run.StartedAt})
				} else {
					data, err = json.Marshal(engine.PauseRequest{RunID: run.ID, RequestedAt: loginNow})
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := ws.WriteFileAtomic(f, data); err != nil {
					t.Fatal(err)
				}
			}
			if c.pause {
				s.onSteps = func() {
					r := s.run("pause")
					if r.code != ExitOK || !strings.Contains(r.stdout, "Asked run "+run.ID) {
						t.Errorf("pause during resume's checks: exit %d, stderr %q\n%s", r.code, r.stderr, r.stdout)
					}
				}
			}
			r := s.run("resume", "--jsm-plan", "premium")
			s.onSteps = nil
			for _, f := range []string{engine.ActiveRunFile, engine.PauseRequestFile} {
				if _, err := os.Stat(filepath.Join(s.dir, f)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s after resume: %v", f, err)
				}
			}
			if !c.pause {
				if r.code != ExitOK || strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
					t.Fatalf("resume: exit %d, calls %v, stderr %q", r.code, step.calls, r.stderr)
				}
				return
			}
			if r.code != messages.ExitPartial || !strings.Contains(r.stderr, "was paused after 1 of 2 items") ||
				strings.Join(step.calls, " ") != "teams/PT1" {
				t.Fatalf("resume with pause during its checks: exit %d, calls %v, stderr %q", r.code, step.calls, r.stderr)
			}
			if r := s.run("resume", "--jsm-plan", "premium"); r.code != ExitOK || strings.Join(step.calls, " ") != "teams/PT1 teams/PT2" {
				t.Errorf("the next resume: exit %d, calls %v, stderr %q", r.code, step.calls, r.stderr)
			}
		})
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
