package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Workspace file names, all at the workspace root.
const (
	ConfigFile    = "config.yaml"
	StateFile     = "state.db"
	LockFile      = "lock"
	ArtifactsDir  = "artifacts"
	dirMode       = 0o700
	fileMode      = 0o600
	looseModeBits = 0o077
)

// rootFiles are the only root names WriteFileAtomic writes. state.db and its
// WAL files belong to SQLite and the lock file to flock, so none of them is
// here.
var rootFiles = map[string]bool{
	ConfigFile:           true,
	"credentials.enc":    true,
	"active-run.json":    true,
	"pause-request.json": true,
	"decisions.json":     true,
}

var (
	// ErrWorkspaceNotFound means the directory does not exist or holds no
	// workspace.
	ErrWorkspaceNotFound = errors.New("workspace not found")
	// ErrWorkspaceLocked means another jsm-ops-migrator command holds the lock.
	ErrWorkspaceLocked = errors.New("another jsm-ops-migrator command is using this workspace")
)

// Workspace is the directory that holds config.yaml, state.db, artifacts/ and
// the lock file. It never leaves the operator's machine.
type Workspace struct {
	dir       string
	tightened []string

	mu        sync.Mutex
	lock      *flock.Flock
	lockCount int
}

// OpenWorkspace opens the workspace in dir. With create it makes the directory
// and artifacts/ when they are missing; without it, a missing directory or one
// that holds no workspace returns ErrWorkspaceNotFound and nothing is created.
func OpenWorkspace(dir string, create bool) (*Workspace, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: no directory given", ErrWorkspaceNotFound)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return nil, fmt.Errorf("%w: %s", ErrWorkspaceNotFound, abs)
		}
		if err := os.MkdirAll(abs, dirMode); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%w: %s is not a directory", ErrWorkspaceNotFound, abs)
	}
	w := &Workspace{dir: abs, lock: flock.New(filepath.Join(abs, LockFile), flock.SetPermissions(fileMode))}
	if !create && !w.exists() {
		return nil, fmt.Errorf("%w: %s holds no workspace", ErrWorkspaceNotFound, abs)
	}
	if create {
		// The mode of an existing directory is left alone: it may be the
		// operator's current directory.
		if err := os.Mkdir(w.ArtifactsDir(), dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	if err := w.tighten(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Workspace) exists() bool {
	for _, name := range []string{StateFile, ConfigFile, ArtifactsDir} {
		if _, err := os.Stat(filepath.Join(w.dir, name)); err == nil {
			return true
		}
	}
	return false
}

// tighten makes the workspace's own files readable only by the operator on
// macOS and Linux. Windows has no mode bits to tighten.
func (w *Workspace) tighten() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	targets := map[string]fs.FileMode{ArtifactsDir: dirMode}
	for _, name := range []string{StateFile, StateFile + "-wal", StateFile + "-shm", ConfigFile, "credentials.enc"} {
		targets[name] = fileMode
	}
	for name, mode := range targets {
		path := filepath.Join(w.dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Mode()&fs.ModeSymlink != 0) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().Perm()&looseModeBits != 0 {
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			w.tightened = append(w.tightened, path)
		}
	}
	return nil
}

// Dir is the workspace's absolute path.
func (w *Workspace) Dir() string { return w.dir }

// ConfigPath is the path of config.yaml.
func (w *Workspace) ConfigPath() string { return filepath.Join(w.dir, ConfigFile) }

// StatePath is the path of state.db.
func (w *Workspace) StatePath() string { return filepath.Join(w.dir, StateFile) }

// ArtifactsDir is the path of artifacts/.
func (w *Workspace) ArtifactsDir() string { return filepath.Join(w.dir, ArtifactsDir) }

// Tightened lists the paths whose permissions OpenWorkspace tightened, so the
// command can warn about them once its logger exists.
func (w *Workspace) Tightened() []string { return w.tightened }

// Lock takes the workspace lock without waiting. It is reference counted per
// Workspace value: a second flock on a new file descriptor in the same process
// would fail, so the command wrapper and the store share one Workspace.
func (w *Workspace) Lock() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lockCount == 0 {
		ok, err := w.lock.TryLock()
		if err != nil {
			return fmt.Errorf("taking the workspace lock: %w", err)
		}
		if !ok {
			return ErrWorkspaceLocked
		}
	}
	w.lockCount++
	return nil
}

// Unlock releases one Lock. The lock file is never deleted: deleting it would
// let two processes lock different files.
func (w *Workspace) Unlock() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lockCount == 0 {
		return errors.New("workspace is not locked")
	}
	w.lockCount--
	if w.lockCount == 0 {
		return w.lock.Unlock()
	}
	return nil
}

// WriteFileAtomic replaces one of the known root files (config.yaml,
// credentials.enc, active-run.json, pause-request.json, decisions.json) with
// data, mode 0600, so a crash never leaves it half-written.
func (w *Workspace) WriteFileAtomic(name string, data []byte) error {
	if !rootFiles[name] {
		return fmt.Errorf("WriteFileAtomic: %q is not a workspace root file", name)
	}
	return writeAtomic(w.dir, name, data)
}

// ReadFile reads one of the known root files. A missing file returns an error
// that wraps fs.ErrNotExist.
func (w *Workspace) ReadFile(name string) ([]byte, error) {
	if !rootFiles[name] {
		return nil, fmt.Errorf("ReadFile: %q is not a workspace root file", name)
	}
	return os.ReadFile(filepath.Join(w.dir, name))
}

// RemoveFile removes one of the known root files. Removing a missing file
// succeeds.
func (w *Workspace) RemoveFile(name string) error {
	if !rootFiles[name] {
		return fmt.Errorf("RemoveFile: %q is not a workspace root file", name)
	}
	err := retryRename(func() error { return os.Remove(filepath.Join(w.dir, name)) })
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := writeAndSync(tmp, data); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, fileMode); err != nil {
		return err
	}
	if err := retryRename(func() error { return os.Rename(tmpName, filepath.Join(dir, name)) }); err != nil {
		return fmt.Errorf("replacing %s (is it open in another program?): %w", name, err)
	}
	return nil
}

// retryRename retries a rename on Windows, where replacing a file fails for a
// moment while another program, such as an antivirus scanner or an editor,
// has it open. Other systems replace open files, so they try once.
func retryRename(rename func() error) error {
	err := rename()
	if runtime.GOOS != "windows" {
		return err
	}
	for wait := 20 * time.Millisecond; err != nil && wait <= 640*time.Millisecond; wait *= 2 {
		time.Sleep(wait)
		err = rename()
	}
	return err
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// WriteArtifact writes data to rel under artifacts/, atomically and with mode
// 0600. rel must be a local path. The write goes through an os.Root opened on
// artifacts/, so a symbolic link cannot redirect it outside the folder.
func (w *Workspace) WriteArtifact(rel string, data []byte) error {
	root, err := w.artifactsRoot(rel)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".tmp-"+randomSuffix())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	if err := writeAndSync(f, data); err != nil {
		return err
	}
	if err := retryRename(func() error { return root.Rename(tmp, rel) }); err != nil {
		return fmt.Errorf("replacing artifact %s (is it open in another program?): %w", rel, err)
	}
	return nil
}

// CreateArtifact creates a new file at rel under artifacts/, mode 0600, for
// output written as it happens, such as a log file. It fails if the file
// exists.
func (w *Workspace) CreateArtifact(rel string) (io.WriteCloser, error) {
	root, err := w.artifactsRoot(rel)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &rootFile{File: f, root: root}, nil
}

// CommitArtifactDir renames a fully written folder under artifacts/ to its
// final name, so a version folder is either complete or absent. It fails if
// the final folder exists.
func (w *Workspace) CommitArtifactDir(tmpRel, finalRel string) error {
	if !filepath.IsLocal(tmpRel) || !filepath.IsLocal(finalRel) {
		return fmt.Errorf("artifact paths must be local: %q, %q", tmpRel, finalRel)
	}
	root, err := os.OpenRoot(w.ArtifactsDir())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(finalRel); err == nil {
		return fmt.Errorf("artifact folder %s: %w", finalRel, fs.ErrExist)
	}
	if dir := filepath.Dir(finalRel); dir != "." {
		if err := root.MkdirAll(dir, dirMode); err != nil {
			return err
		}
	}
	return retryRename(func() error { return root.Rename(tmpRel, finalRel) })
}

// RemoveArtifactDir removes the folder rel under artifacts/ with everything in
// it, such as a version folder whose writing failed before it was committed.
// Removing a missing folder succeeds. The removal goes through an os.Root, so
// a symbolic link cannot take it outside artifacts/.
func (w *Workspace) RemoveArtifactDir(rel string) error {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) == "." {
		return fmt.Errorf("artifact folder must be a local path under artifacts: %q", rel)
	}
	root, err := os.OpenRoot(w.ArtifactsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(rel)
}

func (w *Workspace) artifactsRoot(rel string) (*os.Root, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("artifact path must be local: %q", rel)
	}
	if err := os.Mkdir(w.ArtifactsDir(), dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	root, err := os.OpenRoot(w.ArtifactsDir())
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, dirMode); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return root, nil
}

type rootFile struct {
	*os.File
	root *os.Root
}

func (f *rootFile) Close() error {
	err := f.File.Close()
	if rerr := f.root.Close(); err == nil {
		err = rerr
	}
	return err
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
