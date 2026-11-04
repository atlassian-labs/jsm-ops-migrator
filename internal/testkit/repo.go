package testkit

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Module is the CLI's module path.
const Module = "github.com/atlassian-labs/jsm-ops-migrator"

// RepoRoot walks up from the working directory to the directory whose go.mod
// declares Module.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if moduleOf(filepath.Join(dir, "go.mod")) == Module {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root not found: no go.mod for " + Module)
		}
		dir = parent
	}
}

func moduleOf(goMod string) string {
	f, err := os.Open(goMod)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
