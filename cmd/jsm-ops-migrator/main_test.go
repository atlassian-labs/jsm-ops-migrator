package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/cli"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

// keychainEnv names the file that stands in for the OS keychain, so the
// scripts never touch the real one. A script that empties it runs as on a
// machine without a keychain.
const keychainEnv = "JSM_OPS_MIGRATOR_TEST_KEYCHAIN"

// fakeAPIsEnv, set to "healthy", makes the binary call fake APIs where
// everything works instead of the placeholders, so a script can store
// credentials. PagerDuty is read with the real client from a fake account,
// hosted in the region pagerDutyRegionEnv names (us when empty).
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

// healthyAPIs is the clients factory for fakeAPIsEnv=healthy: testkit's fake
// Atlassian APIs, and the real PagerDuty client reading a fake account that
// accepts the key the scripts use.
func healthyAPIs() clientsFactory {
	account := pdfake.New(stderrT{}, "pd-canary-0123456789", pdfake.InRegion(httpx.Region(os.Getenv(pagerDutyRegionEnv))))
	account.Answer("/abilities", map[string]any{"abilities": []string{"teams"}})
	pagerDuty := apiClients(account.Config(""))
	fakes := testkit.HealthyAPIs(time.Now())
	return func(env *cli.Env, creds credentials.Set) (preflight.Clients, error) {
		real, err := pagerDuty(env, creds)
		if err != nil {
			return preflight.Clients{}, err
		}
		clients := fakes.ClientsFor(creds)
		clients.PagerDuty = real.PagerDuty
		return clients, nil
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
