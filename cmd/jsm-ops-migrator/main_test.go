package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/cli"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

// keychainEnv names the file that stands in for the OS keychain, so the
// scripts never touch the real one. A script that empties it runs as on a
// machine without a keychain.
const keychainEnv = "JSM_OPS_MIGRATOR_TEST_KEYCHAIN"

// fakeAPIsEnv, set to "healthy", makes the binary call fake APIs where
// everything works instead of the placeholders, so a script can store
// credentials. The binary's real clients make the calls: PagerDuty is read
// from a fake account hosted in the region pagerDutyRegionEnv names (us when
// empty), and the Atlassian calls reach a fake site and organization.
const (
	fakeAPIsEnv        = "JSM_OPS_MIGRATOR_TEST_APIS"
	pagerDutyRegionEnv = "JSM_OPS_MIGRATOR_TEST_PAGERDUTY_REGION"
)

// The scripts in testdata/script run the real binary: run is registered as
// the jsm-ops-migrator command, so each script starts it as a separate process.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"jsm-ops-migrator": func() {
		var kc credentials.Keychain
		if path := os.Getenv(keychainEnv); path != "" {
			kc = testkit.NewFileKeychain(path)
		}
		newClients := func(*cli.Env, credentials.Set) (preflight.Clients, error) { return preflight.Placeholders(), nil }
		if os.Getenv(fakeAPIsEnv) == "healthy" {
			newClients = healthyAPIs()
		}
		os.Exit(run(kc, newClients))
	}})
}

// healthyAPIs is the clients factory for fakeAPIsEnv=healthy: the binary's
// real clients, reaching a fake PagerDuty account and a fake Atlassian site
// that accept the credentials the scripts use. The site acme.atlassian.net
// has cloudId cloud-1, and the org API key manages org-1, which has
// operator@example.com.
func healthyAPIs() clientsFactory {
	account := pdfake.New(stderrT{}, "pd-canary-0123456789", pdfake.InRegion(httpx.Region(os.Getenv(pagerDutyRegionEnv))))
	account.Answer("/abilities", map[string]any{"abilities": []string{"teams"}})

	site := jsmfake.New(stderrT{}, jsmfake.Account{
		Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-canary-0123456789", OrgKey: "org-canary-0123456789",
	})
	site.Answer(http.MethodGet, site.OpsPath("/v1/teams"), http.StatusOK, []any{})
	site.List(http.MethodGet, site.AdminPath("/v1/orgs"), "data",
		[]any{map[string]any{"id": "org-1", "type": "orgs", "attributes": map[string]any{"name": "Acme"}}})
	site.Answer(http.MethodPost, site.AdminPath("/v2/orgs/org-1/directories/-/users/search"), http.StatusOK,
		map[string]any{"data": []any{map[string]any{"accountId": "account-of-org-1", "email": "operator@example.com"}}})

	base := account.Config("")
	base.Transport = byHost{pagerDuty: account.Transport(), atlassian: site.Transport()}
	return apiClients(base)
}

// byHost sends each request to the fake that serves its host, because one
// HTTP layer client carries every API's calls.
type byHost struct{ pagerDuty, atlassian http.RoundTripper }

func (b byHost) RoundTrip(r *http.Request) (*http.Response, error) {
	if h := r.URL.Hostname(); h == "pagerduty.com" || strings.HasSuffix(h, ".pagerduty.com") {
		return b.pagerDuty.RoundTrip(r)
	}
	return b.atlassian.RoundTrip(r)
}

// A staging site's JSM Operations and Admin calls go to the staging API
// host, and a production site's to the production one. Each fake serves only
// its own environment's host, and answers the other one 401.
func TestTheSiteChoosesTheAtlassianAPIHost(t *testing.T) {
	for _, tc := range []struct {
		site    string
		staging bool
		host    string
	}{
		{"acme.jira-dev.com", true, jsmfake.APIHostStaging},
		{"acme.atlassian.net", false, jsmfake.APIHost},
	} {
		site := jsmfake.New(t, jsmfake.Account{
			Site: tc.site, CloudID: "cloud-1", OrgID: "org-1",
			Email: "operator@example.com", Token: "atl-token-value-1234", OrgKey: "org-key-value-1234",
			Staging: tc.staging,
		})
		site.Answer(http.MethodGet, site.OpsPath("/v1/teams"), http.StatusOK, []any{})
		site.List(http.MethodGet, site.AdminPath("/v1/orgs"), "data", []any{})
		base := site.Config()
		// The base says the opposite, so only the site can choose the host.
		base.Staging = !tc.staging
		clients, err := apiClients(base)(&cli.Env{Config: cli.Config{Site: tc.site}}, base.Credentials)
		if err != nil {
			t.Fatal(err)
		}
		if err := clients.Atlassian.CheckAccess(context.Background(), "cloud-1"); err != nil {
			t.Errorf("%s: CheckAccess: %v", tc.site, err)
		}
		if _, err := clients.Admin.Orgs(context.Background()); err != nil {
			t.Errorf("%s: Orgs: %v", tc.site, err)
		}
		for _, r := range site.Requests() {
			if r.Host != tc.host {
				t.Errorf("%s: %s %s went to %s, want %s", tc.site, r.Method, r.Path, r.Host, tc.host)
			}
		}
	}
}

// stderrT reports what the fake account rejects on stderr, which every
// script checks.
type stderrT struct{}

func (stderrT) Helper() {}
func (stderrT) Errorf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Setup: func(e *testscript.Env) error {
			e.Setenv(keychainEnv, filepath.Join(e.WorkDir, "keychain.json"))
			return nil
		},
	})
}
