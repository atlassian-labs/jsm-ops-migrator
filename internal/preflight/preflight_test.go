package preflight_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

var now = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

func healthyInput() preflight.Input {
	return preflight.Input{
		Credentials:   testkit.AllCredentials(),
		SecretStorage: "the OS keychain",
		Site:          "acme.atlassian.net",
		CloudID:       "cloud-1",
		OrgID:         "org-1",
		JSMPlan:       "premium",
		Now:           func() time.Time { return now },
	}
}

func runChecks(t *testing.T, f *testkit.FakeAPIs, in preflight.Input) preflight.Report {
	t.Helper()
	r, err := preflight.Run(context.Background(), f.ClientsFor(in.Credentials), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func status(t *testing.T, r preflight.Report, id preflight.ID) (preflight.Status, string) {
	t.Helper()
	c, ok := r.Find(id)
	if !ok {
		t.Fatalf("no %s check in %+v", id, r.Checks)
	}
	return c.Status, c.Detail
}

func TestEveryCheckPassesWhenAllIsWell(t *testing.T) {
	r := runChecks(t, testkit.HealthyAPIs(now), healthyInput())
	var ids []preflight.ID
	for _, c := range r.Checks {
		ids = append(ids, c.ID)
		want := preflight.OK
		if c.ID == preflight.OrgAdmin {
			want = preflight.Warning // no API returns the org admin role
		}
		if c.Status != want || c.Title == "" || c.Detail == "" {
			t.Errorf("%s: %s %q, want %s", c.ID, c.Status, c.Detail, want)
		}
	}
	if !slices.Equal(ids, preflight.All) {
		t.Errorf("checks %v, want %v in that order", ids, preflight.All)
	}
	if r.CloudID != "cloud-1" || r.Operator == nil || r.Operator.AccountID != "account-of-org-1" {
		t.Errorf("cloudId %q, operator %+v", r.CloudID, r.Operator)
	}
}

// Each check, one failure at a time; every other check must still run.
func TestEachCheckResult(t *testing.T) {
	unavailable := fmt.Errorf("dial tcp: %w", httperr.ErrUnavailable)
	cases := []struct {
		name   string
		change func(*testkit.FakeAPIs, *preflight.Input)
		id     preflight.ID
		want   preflight.Status
		detail string
	}{
		{"secret storage unknown", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.SecretStorage = "" },
			preflight.SecretStorage, preflight.NotChecked, "unknown"},

		{"pagerduty key missing", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.PagerDutyKey, "")
		}, preflight.PagerDutyKey, preflight.Missing, "JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY"},
		{"pagerduty key rejected", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.PagerDutyKey = "another" },
			preflight.PagerDutyKey, preflight.Failed, "rejected"},
		{"pagerduty key forbidden", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AbilitiesErr = httperr.ErrForbidden },
			preflight.PagerDutyKey, preflight.Failed, "does not allow"},
		{"pagerduty unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AbilitiesErr = unavailable },
			preflight.PagerDutyKey, preflight.NotChecked, "dial tcp"},
		{"pagerduty key from the environment", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Sources = map[credentials.Kind]string{credentials.PagerDutyKey: "from JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY"}
		}, preflight.PagerDutyKey, preflight.OK, "works (from JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY)"},

		{"no site", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.Site = "" },
			preflight.Site, preflight.Missing, "--site"},
		{"site not found", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.Site = "gone.atlassian.net" },
			preflight.Site, preflight.Failed, "no Atlassian site at gone.atlassian.net"},
		{"site has a new cloudId", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.CloudID = "cloud-old" },
			preflight.Site, preflight.Failed, "now has cloudId cloud-1, but cloud-old is saved"},
		{"no cloudId saved yet", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.CloudID = "" },
			preflight.Site, preflight.OK, "cloudId cloud-1"},
		{"site unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.TenantErr = unavailable },
			preflight.Site, preflight.NotChecked, "dial tcp"},
		{"site gives no cloudId", func(f *testkit.FakeAPIs, _ *preflight.Input) {
			f.Sites["acme.atlassian.net"] = preflight.Tenant{ServerTime: now}
		}, preflight.Site, preflight.Failed, "acme.atlassian.net did not give a cloudId"},

		{"atlassian token missing", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.AtlassianToken, "")
		}, preflight.AtlassianToken, preflight.Missing, "JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN"},
		{"atlassian email missing", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.AtlassianEmail, "")
		}, preflight.AtlassianToken, preflight.Missing, "JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL"},
		{"atlassian token rejected", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AtlassianToken = "another" },
			preflight.AtlassianToken, preflight.Failed, "rejected the email and API token"},
		{"no JSM access", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AccessErr = httperr.ErrForbidden },
			preflight.AtlassianToken, preflight.Failed, "may not use JSM Operations"},
		{"JSM unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AccessErr = unavailable },
			preflight.AtlassianToken, preflight.NotChecked, "dial tcp"},
		{"token check without a cloudId", func(f *testkit.FakeAPIs, in *preflight.Input) { f.TenantErr = unavailable; in.CloudID = "" },
			preflight.AtlassianToken, preflight.NotChecked, "needs the site's cloudId"},
		{"token check falls back to the saved cloudId", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.TenantErr = unavailable },
			preflight.AtlassianToken, preflight.OK, "works"},

		{"org key missing", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.OrgAPIKey, "")
		}, preflight.OrgKey, preflight.Missing, "JSM_OPS_MIGRATOR_ORG_API_KEY"},
		{"org key rejected", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.OrgAPIKey = "another" },
			preflight.OrgKey, preflight.Failed, "rejected"},
		{"no organization set", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.OrgID = "" },
			preflight.OrgKey, preflight.Missing, "no organization is set"},
		{"org key forbidden", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.OrgsErr = httperr.ErrForbidden },
			preflight.OrgKey, preflight.Failed, "does not allow this key"},
		{"organization the key cannot manage", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.OrgID = "org-2" },
			preflight.OrgKey, preflight.Failed, "cannot manage organization org-2"},

		{"operator not in the organization", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.AtlassianEmail, "someone@example.com")
		}, preflight.OrgAdmin, preflight.Failed, "not in organization org-1"},
		{"user search forbidden", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.FindErr = httperr.ErrForbidden },
			preflight.OrgAdmin, preflight.Failed, "may not search"},
		{"user search unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.FindErr = unavailable },
			preflight.OrgAdmin, preflight.NotChecked, "dial tcp"},
		{"admin check without an organization", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.OrgID = "" },
			preflight.OrgAdmin, preflight.NotChecked, "needs the organization"},
		{"admin check after the org key was rejected", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.OrgAPIKey = "another" },
			preflight.OrgAdmin, preflight.NotChecked, "needs a working org API key"},
		{"admin check without the org key", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.OrgAPIKey, "")
		}, preflight.OrgAdmin, preflight.NotChecked, "needs the org API key"},

		{"JSM plan not stated", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.JSMPlan = "" },
			preflight.JSMPlan, preflight.Missing, "--jsm-plan"},
		{"unknown JSM plan", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.JSMPlan = "gold" },
			preflight.JSMPlan, preflight.Failed, `unknown plan "gold"`},

		{"clock a little ahead", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Now = func() time.Time { return now.Add(2 * time.Minute) }
		}, preflight.Clock, preflight.Warning, "2m0s ahead of the site's clock"},
		{"clock far behind", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Now = func() time.Time { return now.Add(-10 * time.Minute) }
		}, preflight.Clock, preflight.Failed, "10m0s behind the site's clock"},
		{"clock within a minute", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Now = func() time.Time { return now.Add(-50 * time.Second) }
		}, preflight.Clock, preflight.OK, "matches"},
		{"clock without a site", func(_ *testkit.FakeAPIs, in *preflight.Input) { in.Site = "" },
			preflight.Clock, preflight.NotChecked, "needs the site"},
		{"clock with the site unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.TenantErr = unavailable },
			preflight.Clock, preflight.NotChecked, "dial tcp"},
		{"site gives no time", func(f *testkit.FakeAPIs, _ *preflight.Input) {
			f.Sites["acme.atlassian.net"] = preflight.Tenant{CloudID: "cloud-1"}
		}, preflight.Clock, preflight.NotChecked, "did not give its time"},

		{"services API missing", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.APIs[preflight.ServicesAPI] = false },
			preflight.Services, preflight.Warning, "not available on this site yet"},
		{"API check unreachable", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.APIErr = unavailable },
			preflight.GlobalEscalations, preflight.NotChecked, "dial tcp"},
		{"API check without a cloudId", func(f *testkit.FakeAPIs, in *preflight.Input) { f.TenantErr = unavailable; in.CloudID = "" },
			preflight.Services, preflight.NotChecked, "needs the site's cloudId"},
		{"API check after the token was rejected", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AtlassianToken = "another" },
			preflight.Services, preflight.NotChecked, "needs a working Atlassian email and API token"},
		{"API check when the token could not be checked", func(f *testkit.FakeAPIs, _ *preflight.Input) { f.AccessErr = unavailable },
			preflight.GlobalEscalations, preflight.NotChecked, "needs a working Atlassian email and API token"},
		{"API check without Atlassian credentials", func(_ *testkit.FakeAPIs, in *preflight.Input) {
			in.Credentials = in.Credentials.With(credentials.AtlassianToken, "")
		}, preflight.UserNotifications, preflight.NotChecked, "needs the Atlassian email and API token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, in := testkit.HealthyAPIs(now), healthyInput()
			tc.change(f, &in)
			r := runChecks(t, f, in)
			if len(r.Checks) != len(preflight.All) {
				t.Errorf("ran %d checks, want all %d", len(r.Checks), len(preflight.All))
			}
			got, detail := status(t, r, tc.id)
			if got != tc.want || !strings.Contains(detail, tc.detail) {
				t.Errorf("%s: %s %q, want %s containing %q", tc.id, got, detail, tc.want, tc.detail)
			}
		})
	}
}

// A missing PagerDuty key must not stop the JSM side from being checked, so
// the other side can still be read.
func TestAMissingCredentialStillChecksTheOtherSide(t *testing.T) {
	f, in := testkit.HealthyAPIs(now), healthyInput()
	in.Credentials = in.Credentials.With(credentials.PagerDutyKey, "")
	r := runChecks(t, f, in)
	if got, _ := status(t, r, preflight.PagerDutyKey); got != preflight.Missing {
		t.Errorf("PagerDuty key: %s", got)
	}
	for _, id := range []preflight.ID{preflight.Site, preflight.AtlassianToken, preflight.OrgKey} {
		if got, _ := status(t, r, id); got != preflight.OK {
			t.Errorf("%s: %s, want ok", id, got)
		}
	}
	if slices.Contains(f.Calls(), "pagerduty abilities") {
		t.Error("called PagerDuty without a key")
	}
}

// Every call with a credential the API already rejected counts as another
// failed login, so a rejected token or org key is sent once, by its own check.
func TestARejectedCredentialIsSentOnce(t *testing.T) {
	cases := []struct {
		name   string
		reject func(*testkit.FakeAPIs)
		check  string // the call of the credential's own check
		never  string // the calls of the checks that depend on it
	}{
		{"atlassian token", func(f *testkit.FakeAPIs) { f.AtlassianToken = "another" }, "ops teams ", "api "},
		{"org API key", func(f *testkit.FakeAPIs) { f.OrgAPIKey = "another" }, "admin orgs", "admin find user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testkit.HealthyAPIs(now)
			tc.reject(f)
			runChecks(t, f, healthyInput())
			n := 0
			for _, call := range f.Calls() {
				switch {
				case strings.HasPrefix(call, tc.check):
					n++
				case strings.HasPrefix(call, tc.never):
					t.Errorf("sent the rejected credential again: %s", call)
				}
			}
			if n != 1 {
				t.Errorf("%d calls starting %q, want 1: %v", n, tc.check, f.Calls())
			}
		})
	}
}

func TestOnlyTheChosenChecksRun(t *testing.T) {
	f, in := testkit.HealthyAPIs(now), healthyInput()
	in.Checks = preflight.PagerDutySide
	r := runChecks(t, f, in)
	if len(r.Checks) != len(preflight.PagerDutySide) {
		t.Errorf("checks %+v", r.Checks)
	}
	if calls := f.Calls(); !slices.Equal(calls, []string{"pagerduty abilities"}) {
		t.Errorf("calls %v", calls)
	}
}

func TestTheSiteIsLookedUpOnce(t *testing.T) {
	f := testkit.HealthyAPIs(now)
	runChecks(t, f, healthyInput())
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "tenant_info") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("tenant_info called %d times: %v", n, f.Calls())
	}
}

func TestACanceledRunReturnsTheError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := healthyInput()
	if _, err := preflight.Run(ctx, testkit.HealthyAPIs(now).ClientsFor(in.Credentials), in); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// An error that carries a credential, as a careless client might return, is
// redacted before it reaches the report.
func TestDetailsAreRedacted(t *testing.T) {
	f, in := testkit.HealthyAPIs(now), healthyInput()
	f.AbilitiesErr = errors.New("request failed: Authorization: Token token=pd-canary-0123456789")
	r := runChecks(t, f, in)
	for _, c := range r.Checks {
		if strings.Contains(c.Detail, "canary") {
			t.Errorf("%s leaks a credential: %q", c.ID, c.Detail)
		}
	}
}

func TestPlaceholdersReportNotChecked(t *testing.T) {
	in := healthyInput()
	r, err := preflight.Run(context.Background(), preflight.Placeholders(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checks {
		switch c.ID {
		case preflight.SecretStorage, preflight.JSMPlan:
			if c.Status != preflight.OK {
				t.Errorf("%s: %s", c.ID, c.Status)
			}
		default:
			if c.Status != preflight.NotChecked {
				t.Errorf("%s: %s %q, want not checked", c.ID, c.Status, c.Detail)
			}
		}
	}
	if _, detail := status(t, r, preflight.PagerDutyKey); detail != "the API clients are not built yet" {
		t.Errorf("detail %q", detail)
	}
}

func TestReportFindAndWith(t *testing.T) {
	in := healthyInput()
	in.JSMPlan = ""
	r := runChecks(t, testkit.HealthyAPIs(now), in)
	if got := r.With(preflight.Missing); len(got) != 1 || got[0].ID != preflight.JSMPlan {
		t.Errorf("missing checks %+v, want only the JSM plan", got)
	}
	in.Checks = []preflight.ID{preflight.JSMPlan}
	if _, ok := runChecks(t, testkit.HealthyAPIs(now), in).Find(preflight.Clock); ok {
		t.Error("found a check that did not run")
	}
}
