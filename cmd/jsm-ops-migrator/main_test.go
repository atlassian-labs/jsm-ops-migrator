package main

import (
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// The scripts in testdata/script run the real binary: main is registered as
// the jsm-ops-migrator command, so each script starts it as a separate process.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"jsm-ops-migrator": main})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{Dir: "testdata/script"})
}
