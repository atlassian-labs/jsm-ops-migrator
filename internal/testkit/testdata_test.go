package testkit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

// TestTestdataIsClean scans every file under every testdata folder in the
// repository, tools/pdseed included, for secrets and unmasked phone numbers.
// CI also runs it as its own check.
func TestTestdataIsClean(t *testing.T) {
	root, err := testkit.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !inTestdata(root, p) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, p)
		for _, f := range testkit.ScanSensitive(filepath.ToSlash(rel), data) {
			t.Errorf("%s:%d: %s", f.Path, f.Line, f.Rule)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no files found under any testdata folder; is the repository root right?")
	}
}

func inTestdata(root, p string) bool {
	for dir := filepath.Dir(p); dir != root && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "testdata" {
			return true
		}
	}
	return false
}
