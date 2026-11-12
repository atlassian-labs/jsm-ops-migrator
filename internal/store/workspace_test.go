package store_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func TestOpenWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	if _, err := store.OpenWorkspace(dir, false); !errors.Is(err, store.ErrWorkspaceNotFound) {
		t.Fatalf("missing dir without create: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("OpenWorkspace without create must create nothing")
	}
	empty := t.TempDir()
	if _, err := store.OpenWorkspace(empty, false); !errors.Is(err, store.ErrWorkspaceNotFound) {
		t.Fatalf("directory that is not a workspace: %v", err)
	}
	ws, err := store.OpenWorkspace(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(ws.ArtifactsDir())
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("artifacts mode = %v, %v", info.Mode().Perm(), err)
		}
	}
	if _, err := store.OpenWorkspace(dir, false); err != nil {
		t.Fatalf("existing workspace: %v", err)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenWorkspace(file, true); !errors.Is(err, store.ErrWorkspaceNotFound) {
		t.Fatalf("a file is not a workspace: %v", err)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	a, _ := store.OpenWorkspace(dir, true)
	b, _ := store.OpenWorkspace(dir, true)
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatalf("the lock is reference counted within one Workspace: %v", err)
	}
	if err := b.Lock(); !errors.Is(err, store.ErrWorkspaceLocked) {
		t.Fatalf("second holder: %v, want ErrWorkspaceLocked", err)
	}
	_ = a.Unlock()
	if err := b.Lock(); !errors.Is(err, store.ErrWorkspaceLocked) {
		t.Fatal("one Unlock must not release a lock taken twice")
	}
	_ = a.Unlock()
	if err := b.Lock(); err != nil {
		t.Fatalf("after both unlocks: %v", err)
	}
	_ = b.Unlock()
	if err := b.Unlock(); err == nil {
		t.Error("Unlock without Lock must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, store.LockFile)); err != nil {
		t.Error("the lock file must be kept")
	}
}

func TestWriteArtifact(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	if err := ws.WriteArtifact(filepath.Join("plan-v1", "plan.md"), []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteArtifact(filepath.Join("plan-v1", "plan.md"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws.ArtifactsDir(), "plan-v1", "plan.md")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "v2" {
		t.Fatalf("content = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		dirInfo, _ := os.Stat(filepath.Dir(path))
		if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("modes: file %v, dir %v", info.Mode().Perm(), dirInfo.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
	for _, bad := range []string{"../escape.md", filepath.Join(ws.Dir(), "abs.md"), ""} {
		if err := ws.WriteArtifact(bad, []byte("x")); err == nil {
			t.Errorf("WriteArtifact(%q) must fail", bad)
		}
	}
}

func TestWriteArtifactRefusesSymlinkEscape(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws.ArtifactsDir(), "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symbolic link needs Developer Mode or admin rights here: %v", err)
		}
		t.Fatal(err)
	}
	if err := ws.WriteArtifact(filepath.Join("link", "x.md"), []byte("x")); err == nil {
		t.Fatal("a write through a symlink out of artifacts/ must fail")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside artifacts/: %v", entries)
	}
}

func TestRemoveArtifactDir(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	if err := ws.WriteArtifact(filepath.Join("plans", ".v1.tmp", "plan.md"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteArtifact(filepath.Join("plans", "v0", "plan.md"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ws.RemoveArtifactDir(filepath.Join("plans", ".v1.tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), "plans", ".v1.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the folder is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), "plans", "v0", "plan.md")); err != nil {
		t.Errorf("a sibling folder was removed: %v", err)
	}
	if err := ws.RemoveArtifactDir(filepath.Join("plans", "missing")); err != nil {
		t.Errorf("removing a missing folder: %v", err)
	}
	for _, bad := range []string{"", ".", "..", filepath.Join("..", "x"), ws.Dir()} {
		if err := ws.RemoveArtifactDir(bad); err == nil {
			t.Errorf("RemoveArtifactDir(%q) must fail", bad)
		}
	}
	if _, err := os.Stat(ws.ArtifactsDir()); err != nil {
		t.Errorf("artifacts/ itself is gone: %v", err)
	}
}

func TestRemoveArtifactDirStaysInArtifacts(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	// The kept file is in the folder a removal through the link would reach,
	// so a removal that followed the link deletes it.
	outside := t.TempDir()
	keep := filepath.Join(outside, "sub", "keep")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws.ArtifactsDir(), "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symbolic link needs Developer Mode or admin rights here: %v", err)
		}
		t.Fatal(err)
	}
	_ = ws.RemoveArtifactDir(filepath.Join("link", "sub"))
	_ = ws.RemoveArtifactDir("link")
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a removal followed the link out of artifacts/: %v", err)
	}
}

func TestCreateAndCommitArtifacts(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	w, err := ws.CreateArtifact(filepath.Join("logs", "a.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("line\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.CreateArtifact(filepath.Join("logs", "a.log")); err == nil {
		t.Error("CreateArtifact must not overwrite")
	}

	if err := ws.WriteArtifact(filepath.Join(".tmp-plan-v1", "plan.md"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ws.CommitArtifactDir(".tmp-plan-v1", "plan-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.ArtifactsDir(), "plan-v1", "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteArtifact(filepath.Join(".tmp-plan-v1b", "plan.md"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ws.CommitArtifactDir(".tmp-plan-v1b", "plan-v1"); err == nil {
		t.Error("committing over an existing folder must fail")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	if err := ws.WriteFileAtomic(store.ConfigFile, []byte("site: a\n")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{store.StateFile, store.LockFile, "other.yaml", filepath.Join("artifacts", "x")} {
		if err := ws.WriteFileAtomic(bad, []byte("x")); err == nil {
			t.Errorf("WriteFileAtomic(%q) must fail", bad)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(ws.ConfigPath())
		if info.Mode().Perm() != 0o600 {
			t.Errorf("config.yaml mode = %v", info.Mode().Perm())
		}
	}
}

func TestOpenWorkspaceTightensLooseModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.ConfigFile), []byte("site: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, store.ArtifactsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// Explicit, so the test does not depend on the umask.
	_ = os.Chmod(filepath.Join(dir, store.ConfigFile), 0o644)
	_ = os.Chmod(filepath.Join(dir, store.ArtifactsDir), 0o755)
	ws, err := store.OpenWorkspace(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Tightened()) != 2 {
		t.Fatalf("tightened = %v", ws.Tightened())
	}
	info, _ := os.Stat(ws.ConfigPath())
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.yaml mode = %v", info.Mode().Perm())
	}
}
