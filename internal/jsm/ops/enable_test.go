package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

// enableSite is a fake site whose enable-ops answers with request r1, and
// whose request status is successful unless a test says otherwise.
func enableSite(t *testing.T, cloudID string) (*jsmfake.Fake, *Client) {
	t.Helper()
	f := jsmfake.New(t, jsmfake.Account{
		Site: "acme.atlassian.net", CloudID: cloudID, OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-token-value-1234",
	})
	f.Answer(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), http.StatusOK, map[string]any{"requestId": "r1"})
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams/team-1/requests/r1"), http.StatusOK, map[string]any{"status": "successful"})
	c := New(f.Client(), cloudID)
	c.timing = timing{attempts: 4, teamAttempts: 3, minWait: time.Millisecond, maxWait: 2 * time.Millisecond,
		pollInterval: time.Millisecond, pollTimeout: time.Second, sleep: sleep}
	return f, c
}

func refusal(status int, message string) jsmfake.Fault {
	return jsmfake.Fault{Status: status, Body: map[string]any{"message": message}}
}

// The messages JSM answers enable-ops with: the team-not-found one at runtime
// and in the documentation.
const (
	lockMessage        = "Another request is already being processed now. Please try again."
	notFoundMessage    = "Team not found with identifier: team-1"
	notFoundDocMessage = "The team is not found."
)

func countPosts(f *jsmfake.Fake) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == http.MethodPost {
			n++
		}
	}
	return n
}

func TestEnableOpsSendsTheAdminsAndWaitsForTheRequest(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	status := f.OpsPath("/v1/teams/team-1/requests/r1")
	// The status comes as the documented object first, then as the plain
	// string the API sends.
	f.Fail(http.MethodGet, status,
		jsmfake.Fault{Status: http.StatusOK, Body: map[string]any{"status": map[string]any{"value": "IN_PROGRESS"}}},
		jsmfake.Fault{Status: http.StatusOK, Body: map[string]any{"status": "in-progress"}})

	res, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1", "acc-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (EnableResult{RequestID: "r1"}) {
		t.Fatalf("result %+v", res)
	}
	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want the POST and three status reads", len(reqs))
	}
	var body struct {
		AdminAccountIDs []string `json:"adminAccountIds"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil || len(body.AdminAccountIDs) != 2 || body.AdminAccountIDs[1] != "acc-2" {
		t.Fatalf("body %s", reqs[0].Body)
	}
}

func TestEnableOpsRetriesTheLockAndAnUnknownTeam(t *testing.T) {
	for name, fault := range map[string]jsmfake.Fault{
		"lock":                   refusal(http.StatusUnprocessableEntity, lockMessage),
		"team not found 422":     refusal(http.StatusUnprocessableEntity, notFoundMessage),
		"team not found doc 422": refusal(http.StatusUnprocessableEntity, notFoundDocMessage),
		"team not found doc 404": refusal(http.StatusNotFound, notFoundDocMessage),
	} {
		t.Run(name, func(t *testing.T) {
			f, c := enableSite(t, "cloud-retry")
			f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), fault, fault)
			res, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
			if err != nil || res.RequestID != "r1" {
				t.Fatalf("got %+v, %v", res, err)
			}
			if n := countPosts(f); n != 3 {
				t.Fatalf("%d POSTs, want 3", n)
			}
		})
	}
}

func TestEnableOpsStopsRetryingAfterItsAttempts(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	lock := refusal(http.StatusUnprocessableEntity, lockMessage)
	f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), lock, lock, lock, lock, lock)
	_, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
	if !errors.Is(err, httperr.ErrInvalid) {
		t.Fatalf("got %v, want the lock's ErrInvalid", err)
	}
	if n := countPosts(f); n != c.timing.attempts {
		t.Fatalf("%d POSTs, want %d", n, c.timing.attempts)
	}
}

func TestEnableOpsGivesUpOnAnUnknownTeamSooner(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	missing := refusal(http.StatusUnprocessableEntity, notFoundMessage)
	f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), missing, missing, missing, missing, missing)
	_, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
	if !errors.Is(err, httperr.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if n := countPosts(f); n != c.timing.teamAttempts {
		t.Fatalf("%d POSTs, want %d, fewer than the %d a lock gets", n, c.timing.teamAttempts, c.timing.attempts)
	}
}

func TestEnableOpsFailsAtOnceOnANotFoundAboutSomethingElse(t *testing.T) {
	for _, tc := range []struct {
		fault jsmfake.Fault
		want  error
	}{
		{refusal(http.StatusUnprocessableEntity, "User not found with accountId: acc-9"), httperr.ErrInvalid},
		{refusal(http.StatusNotFound, "Not Found"), httperr.ErrNotFound},
		{refusal(http.StatusBadRequest, "Team not found with identifier: team-1"), httperr.ErrInvalid},
	} {
		f, c := enableSite(t, "cloud-1")
		f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), tc.fault, tc.fault)
		_, err := c.EnableOps(context.Background(), "team-1", []string{"acc-9"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%d %v: got %v, want %v", tc.fault.Status, tc.fault.Body, err, tc.want)
		}
		if n := countPosts(f); n != 1 {
			t.Errorf("%d %v: %d POSTs, want 1", tc.fault.Status, tc.fault.Body, n)
		}
	}
}

func TestEnableOpsTakesAlreadyExistsAsDone(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity} {
		f, c := enableSite(t, "cloud-1")
		f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), refusal(status, "Team already exists!"))
		res, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
		if err != nil || !res.AlreadyEnabled || res.RequestID != "" {
			t.Fatalf("%d: got %+v, %v", status, res, err)
		}
		if n := len(f.Requests()); n != 1 {
			t.Fatalf("%d: %d requests, want only the POST", status, n)
		}
	}
}

func TestEnableOpsFailsAtOnceOnOtherRefusals(t *testing.T) {
	for _, tc := range []struct {
		fault jsmfake.Fault
		want  error
	}{
		{refusal(http.StatusUnprocessableEntity, "adminAccountIds must not be empty"), httperr.ErrInvalid},
		{refusal(http.StatusForbidden, "not allowed"), httperr.ErrForbidden},
		// A 5xx is never read as a lock or "already exists": the write may
		// have taken effect.
		{refusal(http.StatusInternalServerError, lockMessage+" Team already exists!"), httperr.ErrUnknownOutcome},
	} {
		f, c := enableSite(t, "cloud-1")
		f.Fail(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), tc.fault)
		_, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%d: got %v, want %v", tc.fault.Status, err, tc.want)
		}
		if n := countPosts(f); n != 1 {
			t.Errorf("%d: %d POSTs, want 1", tc.fault.Status, n)
		}
	}
}

func TestEnableOpsRefusesWithoutAdmins(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	for _, tc := range []struct {
		team   string
		admins []string
	}{{"team-1", nil}, {"", []string{"acc-1"}}, {"team/1", []string{"acc-1"}}} {
		if _, err := c.EnableOps(context.Background(), tc.team, tc.admins); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestEnableOpsReportsAFailedRequest(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams/team-1/requests/r1"), http.StatusOK, map[string]any{"status": "FAILED"})
	res, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
	if !errors.Is(err, ErrEnableFailed) || errors.Is(err, httperr.ErrUnknownOutcome) || res.RequestID != "r1" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestEnableOpsOutcomeIsUnknownWhenTheStatusIsNot(t *testing.T) {
	for name, set := range map[string]func(f *jsmfake.Fake, status string){
		"poll timeout": func(f *jsmfake.Fake, status string) {
			f.Answer(http.MethodGet, status, http.StatusOK, map[string]any{"status": "in-progress"})
		},
		"status unreadable": func(f *jsmfake.Fake, status string) {
			f.Answer(http.MethodGet, status, http.StatusServiceUnavailable, map[string]any{"message": "down"})
		},
		"unknown status": func(f *jsmfake.Fake, status string) {
			f.Answer(http.MethodGet, status, http.StatusOK, map[string]any{"status": "queued-for-later"})
		},
		"status not decodable": func(f *jsmfake.Fake, status string) {
			f.Answer(http.MethodGet, status, http.StatusOK, map[string]any{"status": 7})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := enableSite(t, "cloud-1")
			c.timing.pollTimeout = 50 * time.Millisecond
			set(f, f.OpsPath("/v1/teams/team-1/requests/r1"))
			res, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
			if !errors.Is(err, httperr.ErrUnknownOutcome) || res.RequestID != "r1" {
				t.Fatalf("got %+v, %v", res, err)
			}
			if n := countPosts(f); n != 1 {
				t.Fatalf("%d POSTs, want 1", n)
			}
		})
	}
}

func TestEnableOpsKeepsWhatStoppedThePoll(t *testing.T) {
	t.Run("the caller's context", func(t *testing.T) {
		f, c := enableSite(t, "cloud-1")
		f.Answer(http.MethodGet, f.OpsPath("/v1/teams/team-1/requests/r1"), http.StatusOK, map[string]any{"status": "in-progress"})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := c.EnableOps(ctx, "team-1", []string{"acc-1"})
		if !errors.Is(err, httperr.ErrUnknownOutcome) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want ErrUnknownOutcome and the context's error", err)
		}
	})
	t.Run("a 403 on the status read", func(t *testing.T) {
		f, c := enableSite(t, "cloud-1")
		f.Answer(http.MethodGet, f.OpsPath("/v1/teams/team-1/requests/r1"), http.StatusForbidden, map[string]any{"message": "not allowed"})
		_, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"})
		if !errors.Is(err, httperr.ErrUnknownOutcome) || !errors.Is(err, httperr.ErrForbidden) {
			t.Fatalf("got %v, want ErrUnknownOutcome and ErrForbidden", err)
		}
	})
}

func TestEnableOpsWithoutARequestIDIsAnUnknownOutcome(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	f.Answer(http.MethodPost, f.OpsPath("/v1/teams/team-1/enable-ops"), http.StatusOK, map[string]any{})
	if _, err := c.EnableOps(context.Background(), "team-1", []string{"acc-1"}); !errors.Is(err, httperr.ErrUnknownOutcome) {
		t.Fatalf("got %v", err)
	}
}

func TestEnableOpsRunsOneCallPerSiteAtATime(t *testing.T) {
	f, _ := enableSite(t, "cloud-serial")
	for _, team := range []string{"team-1", "team-2"} {
		post := f.OpsPath("/v1/teams/" + team + "/enable-ops")
		f.Answer(http.MethodPost, post, http.StatusOK, map[string]any{"requestId": "r-" + team})
		f.Fail(http.MethodPost, post, jsmfake.Fault{Status: http.StatusOK, Body: map[string]any{"requestId": "r-" + team}, Delay: 30 * time.Millisecond})
		f.Answer(http.MethodGet, f.OpsPath("/v1/teams/"+team+"/requests/r-"+team), http.StatusOK, map[string]any{"status": "successful"})
	}
	// Two clients for the same site, so the slot cannot be the client's own.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, team := range []string{"team-1", "team-2"} {
		c := New(f.Client(), "cloud-serial")
		c.timing.pollInterval = time.Millisecond
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.EnableOps(context.Background(), team, []string{"acc-1"})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := f.Peak(); p != 1 {
		t.Fatalf("peak %d requests at once, want 1", p)
	}
}

func TestEnableOpsSlotsArePerSite(t *testing.T) {
	if siteSlot("cloud-a") == siteSlot("cloud-b") {
		t.Fatal("two sites share a slot")
	}
	first, again := siteSlot("cloud-a"), siteSlot("cloud-a")
	if first != again {
		t.Fatal("one site has two slots")
	}
}

func TestEnableOpsStopsWaitingForTheSlotWithItsContext(t *testing.T) {
	_, c := enableSite(t, "cloud-busy")
	slot := siteSlot("cloud-busy")
	slot <- struct{}{}
	defer func() { <-slot }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.EnableOps(ctx, "team-1", []string{"acc-1"})
	if !errors.Is(err, httperr.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultTimingGivesAnUnknownTeamAboutHalfAMinute(t *testing.T) {
	tm := defaultTiming()
	var waited time.Duration
	for n := 1; n < tm.teamAttempts; n++ {
		waited += tm.wait(n)
	}
	if tm.teamAttempts >= tm.attempts || waited != 30*time.Second {
		t.Fatalf("%d team attempts of %d, waiting %s in all", tm.teamAttempts, tm.attempts, waited)
	}
}

func TestWaitDoublesUpToItsCap(t *testing.T) {
	tm := timing{minWait: 2 * time.Second, maxWait: 30 * time.Second}
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := tm.wait(i + 1); got != w*time.Second {
			t.Errorf("wait(%d) = %s, want %ss", i+1, got, w)
		}
	}
}
