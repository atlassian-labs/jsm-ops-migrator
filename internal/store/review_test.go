package store_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

func TestSealUsesAFreshNonce(t *testing.T) {
	c := testkit.NewStore(t).Cipher()
	r := ref("t", "i", "c")
	a, _ := c.Seal(r, []byte("same"))
	b, _ := c.Seal(r, []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("sealing the same value twice must give different bytes")
	}
}

// renamedProvider wraps a working provider under another ID, so only the ID
// differs from the one that wrapped the key.
type renamedProvider struct {
	store.KeyProvider
	id string
}

func (p renamedProvider) ID() string { return p.id }

func TestProviderMismatchIsRefusedEvenIfTheKeyWouldOpen(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	kp := testkit.NewMemoryKeyProvider()
	st, err := store.Open(ctx, ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := store.Open(ctx, ws, renamedProvider{kp, "keychain"}, store.OpenExisting); !errors.Is(err, store.ErrKeyUnavailable) {
		t.Errorf("err = %v, want ErrKeyUnavailable", err)
	}
}

func TestPlaintextNeverReachesTheDisk(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	st, err := store.Open(ctx, ws, testkit.NewMemoryKeyProvider(), store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	canary := []byte("plaintext-canary-" + "5f2c9a")
	sealed, err := st.Cipher().Seal(ref("t", "1", "payload"), canary)
	if err != nil {
		t.Fatal(err)
	}
	err = st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE t(id TEXT, payload BLOB)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO t VALUES ('1', ?)`, sealed)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Read the files while the WAL still holds the write, then after close.
	check := func(when string) {
		for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
			data, err := os.ReadFile(filepath.Join(ws.Dir(), name))
			if err == nil && bytes.Contains(data, canary) {
				t.Errorf("%s: %s holds the plaintext", when, name)
			}
		}
	}
	check("open")
	if runtime.GOOS != "windows" {
		for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
			if info, err := os.Stat(filepath.Join(ws.Dir(), name)); err == nil && info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
			}
		}
	}
	_ = st.Close()
	check("closed")
}

// crashAfterWrap simulates a process that dies after the key provider has
// written its key but before data_keys is saved.
type crashAfterWrap struct{ store.KeyProvider }

func (p crashAfterWrap) WrapKey(ctx context.Context, id string, key []byte) ([]byte, error) {
	if _, err := p.KeyProvider.WrapKey(ctx, id, key); err != nil {
		return nil, err
	}
	return nil, errors.New("simulated crash")
}

func TestAKeychainKeyIsReusedAfterACrash(t *testing.T) {
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	kc := testkit.NewFakeKeychain()
	kp := credentials.NewKeychainKeyProvider(kc)
	if _, err := store.Open(ctx, ws, crashAfterWrap{kp}, store.OpenCreate); err == nil {
		t.Fatal("the simulated crash must fail the open")
	}
	if kc.Len() != 1 {
		t.Fatalf("keychain items after the crash = %d, want 1", kc.Len())
	}
	st, err := store.Open(ctx, ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if kc.Len() != 1 {
		t.Errorf("keychain items after reopening = %d: the first key was stranded", kc.Len())
	}
}

func TestOpenWorkspaceTightensStateFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows")
	}
	dir := t.TempDir()
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "config.yaml"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// Explicit, so the test does not depend on the umask.
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := store.OpenWorkspace(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Tightened()) != 4 {
		t.Errorf("tightened = %v", ws.Tightened())
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "config.yaml"} {
		if info, _ := os.Stat(filepath.Join(dir, name)); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", name, info.Mode().Perm())
		}
	}
}

// TestLockAcrossProcesses runs a second process that takes the lock, as a
// running apply would, then kills it: the OS must release the lock.
func TestLockAcrossProcesses(t *testing.T) {
	if dir := os.Getenv("JSM_OPS_MIGRATOR_LOCK_HELPER"); dir != "" {
		ws, err := store.OpenWorkspace(dir, false)
		if err != nil || ws.Lock() != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("locked\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	dir := t.TempDir()
	ws, _ := store.OpenWorkspace(dir, true)
	kp := testkit.NewMemoryKeyProvider()
	st, err := store.Open(ctx, ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "JSM_OPS_MIGRATOR_LOCK_HELPER="+dir)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper said %q", line)
	}

	other, _ := store.OpenWorkspace(dir, false)
	if err := other.Lock(); !errors.Is(err, store.ErrWorkspaceLocked) {
		t.Fatalf("lock held by another process: %v, want ErrWorkspaceLocked", err)
	}
	if _, err := store.Open(ctx, other, kp, store.OpenExisting); !errors.Is(err, store.ErrWorkspaceLocked) {
		t.Fatalf("OpenExisting beside a running process: %v", err)
	}
	shared, err := store.Open(ctx, other, kp, store.OpenShared)
	if err != nil {
		t.Fatalf("OpenShared beside a running process: %v", err)
	}
	_ = shared.Close()

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := other.Lock()
		if err == nil {
			_ = other.Unlock()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lock was not released when the process died: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
