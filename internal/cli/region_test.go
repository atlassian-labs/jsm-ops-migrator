package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

// pdAccount is a PagerDuty account hosted in region that accepts the key
// setCredentialsInEnv sets.
func pdAccount(t *testing.T, region httpx.Region) *pdfake.Fake {
	f := pdfake.New(t, "pd-canary-0123456789", pdfake.InRegion(region))
	f.Answer("/abilities", map[string]any{"abilities": []string{"teams"}})
	return f
}

func hosts(f *pdfake.Fake) string {
	var out []string
	for _, r := range f.Requests() {
		out = append(out, r.Host)
	}
	return strings.Join(out, " ")
}

func TestLoginFindsAnEUAccount(t *testing.T) {
	s := newLoginSetup(t)
	s.pd = pdAccount(t, httpx.RegionEU)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", r.code, r.stderr, r.stdout)
	}
	// One rejected call on the US host, then the EU host; the pre-flight
	// check reuses the answer.
	if got := hosts(s.pd); got != pdfake.HostUS+" "+pdfake.HostEU {
		t.Errorf("hosts = %s", got)
	}
	if c := s.config(); c.PagerDutyRegion != "eu" {
		t.Errorf("saved region %q", c.PagerDutyRegion)
	}
	if !s.stored().Has(credentials.PagerDutyKey) {
		t.Error("the key was not stored")
	}
	if !strings.Contains(r.stdout, "PagerDuty region eu.") {
		t.Errorf("stdout does not name the region:\n%s", r.stdout)
	}
	noLeaks(t, s.dir, r)

	// plan reads from the saved region only.
	before := len(s.pd.Requests())
	s.run("plan", "--only", "pagerduty")
	for _, req := range s.pd.Requests()[before:] {
		if req.Host != pdfake.HostEU {
			t.Errorf("plan sent a request to %s", req.Host)
		}
	}
	if len(s.pd.Requests()) == before {
		t.Error("plan did not check the key")
	}
}

func TestLoginChecksAUSAccountOnce(t *testing.T) {
	s := newLoginSetup(t)
	s.pd = pdAccount(t, httpx.RegionUS)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	if r := s.run("login", "--site", "acme", "--org-id", "org-1"); r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := hosts(s.pd); got != pdfake.HostUS {
		t.Errorf("hosts = %s", got)
	}
	if c := s.config(); c.PagerDutyRegion != "us" {
		t.Errorf("saved region %q", c.PagerDutyRegion)
	}
}

func TestAGivenRegionIsNotSwitched(t *testing.T) {
	for name, given := range map[string]func(t *testing.T) []string{
		"flag": func(*testing.T) []string { return []string{"--pagerduty-region", "US"} },
		"environment": func(t *testing.T) []string {
			t.Setenv("JSM_OPS_MIGRATOR_PAGERDUTY_REGION", "us")
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newLoginSetup(t)
			s.pd = pdAccount(t, httpx.RegionEU)
			setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
			r := s.run(append([]string{"login", "--site", "acme", "--org-id", "org-1"}, given(t)...)...)
			if r.code != ExitError || !strings.Contains(r.stdout, "PagerDuty rejected the key") {
				t.Fatalf("exit %d: %s", r.code, r.stdout)
			}
			if got := hosts(s.pd); got != pdfake.HostUS {
				t.Errorf("hosts = %s", got)
			}
			if c := s.config(); c.PagerDutyRegion != "" {
				t.Errorf("saved region %q for a rejected key", c.PagerDutyRegion)
			}
		})
	}
}

func TestARejectedKeySavesNoRegion(t *testing.T) {
	s := newLoginSetup(t)
	s.pd = pdfake.New(t, "the-only-valid-key", pdfake.InRegion(httpx.RegionEU))
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitError || !strings.Contains(r.stdout, "PagerDuty rejected the key") {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
	if got := hosts(s.pd); got != pdfake.HostUS+" "+pdfake.HostEU {
		t.Errorf("hosts = %s", got)
	}
	if c := s.config(); c.PagerDutyRegion != "" {
		t.Errorf("saved region %q", c.PagerDutyRegion)
	}
}

func TestAnUnknownRegionIsRefused(t *testing.T) {
	s := newLoginSetup(t)
	s.pd = pdAccount(t, httpx.RegionUS)
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	for _, cmd := range [][]string{{"login", "--site", "acme"}, {"plan", "--jsm-plan", "premium"}} {
		r := s.run(append(cmd, "--pagerduty-region", "apac")...)
		if r.code != ExitError || !strings.Contains(r.stderr, `pagerduty_region "apac": want us or eu`) {
			t.Errorf("%s: exit %d: %s", cmd[0], r.code, r.stderr)
		}
	}
	if n := len(s.pd.Requests()); n != 0 {
		t.Errorf("sent %d requests", n)
	}
}

// Only two 401s reject the key: an outage in the other region says nothing
// about it, so the check is reported as not run, and nothing is stored or
// saved.
func TestAnOutageInTheOtherRegionIsNotARejection(t *testing.T) {
	for name, fault := range map[string]pdfake.Fault{
		"503": {Status: http.StatusServiceUnavailable},
		"429": {Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"0"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newLoginSetup(t)
			s.pd = pdAccount(t, httpx.RegionEU)
			// Every attempt the client makes fails.
			s.pd.Fail("/abilities", fault, fault, fault)
			setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
			r := s.run("login", "--site", "acme", "--org-id", "org-1")
			line := ""
			for l := range strings.SplitSeq(r.stdout, "\n") {
				if strings.Contains(l, "PagerDuty API key") && !strings.Contains(l, "Stored") && !strings.Contains(l, "stored") {
					line = l
					break
				}
			}
			if !strings.Contains(line, "not checked") || strings.Contains(r.stdout, "rejected the key") {
				t.Fatalf("exit %d, PagerDuty check %q:\n%s", r.code, line, r.stdout)
			}
			if got := hosts(s.pd); got != pdfake.HostUS+" "+strings.Repeat(pdfake.HostEU+" ", 2)+pdfake.HostEU {
				t.Errorf("hosts = %s", got)
			}
			if s.stored().Has(credentials.PagerDutyKey) {
				t.Error("a key whose check did not run was stored")
			}
			if c := s.config(); c.PagerDutyRegion != "" {
				t.Errorf("saved region %q", c.PagerDutyRegion)
			}
		})
	}
}

// A 403 from the other region also finds the account: the key is known
// there, but may not read it.
func TestAForbiddenKeyInTheOtherRegionIsReportedAsForbidden(t *testing.T) {
	s := newLoginSetup(t)
	s.pd = pdAccount(t, httpx.RegionEU)
	s.pd.Fail("/abilities", pdfake.Fault{Status: http.StatusForbidden})
	setCredentialsInEnv(t, credentials.Set{}, credentials.Kinds...)
	r := s.run("login", "--site", "acme", "--org-id", "org-1")
	if r.code != ExitError || !strings.Contains(r.stdout, "PagerDuty does not allow this key to read the account") {
		t.Fatalf("exit %d:\n%s", r.code, r.stdout)
	}
	if c := s.config(); c.PagerDutyRegion != "" {
		t.Errorf("saved region %q for a forbidden key", c.PagerDutyRegion)
	}
}
