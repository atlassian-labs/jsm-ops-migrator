package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

// keychainEnv names the file that stands in for the OS keychain, so the
// scripts never touch the real one. A script that empties it runs as on a
// machine without a keychain.
const keychainEnv = "JSM_OPS_MIGRATOR_TEST_KEYCHAIN"

// fakeAPIsEnv, set to "healthy", makes the binary call fake APIs where
// everything works instead of the placeholders, so a script can store
// credentials.
const fakeAPIsEnv = "JSM_OPS_MIGRATOR_TEST_APIS"

// The scripts in testdata/script run the real binary: run is registered as
// the jsm-ops-migrator command, so each script starts it as a separate process.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"jsm-ops-migrator": func() {
		var kc credentials.Keychain
		if path := os.Getenv(keychainEnv); path != "" {
			kc = testkit.NewFileKeychain(path)
		}
		newClients := placeholders
		if os.Getenv(fakeAPIsEnv) == "healthy" {
			newClients = testkit.HealthyAPIs(time.Now()).ClientsFor
		}
		os.Exit(run(kc, newClients))
	}})
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
