package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

var ctx = context.Background()

func ref(table, item, column string) store.FieldRef {
	return store.FieldRef{Table: table, Item: item, Column: column}
}

func TestSealIsBoundToItsPlace(t *testing.T) {
	c := testkit.NewStore(t).Cipher()
	plain := []byte(`{"name":"Primary on-call"}`)
	sealed, err := c.Seal(ref("pd_objects", "1/user/PABC", "payload"), plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("sealed value contains the plaintext")
	}
	got, err := c.Open(ref("pd_objects", "1/user/PABC", "payload"), sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	for name, r := range map[string]store.FieldRef{
		"table":  ref("jsm_objects", "1/user/PABC", "payload"),
		"item":   ref("pd_objects", "1/user/PXYZ", "payload"),
		"column": ref("pd_objects", "1/user/PABC", "meta"),
	} {
		if _, err := c.Open(r, sealed); !errors.Is(err, store.ErrDecrypt) {
			t.Errorf("opening with another %s: err = %v, want ErrDecrypt", name, err)
		}
	}
	for i := range sealed {
		changed := bytes.Clone(sealed)
		changed[i] ^= 0x01
		if _, err := c.Open(ref("pd_objects", "1/user/PABC", "payload"), changed); !errors.Is(err, store.ErrDecrypt) {
			t.Fatalf("changing byte %d: err = %v, want ErrDecrypt", i, err)
		}
	}
}

func TestSealPartsCannotCollide(t *testing.T) {
	c := testkit.NewStore(t).Cipher()
	// Plain concatenation would give "abcx" for both.
	sealed, err := c.Seal(ref("ab", "c", "x"), []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(ref("a", "bc", "x"), sealed); !errors.Is(err, store.ErrDecrypt) {
		t.Errorf("(ab, c) opened as (a, bc): err = %v", err)
	}
}

func TestAnotherWorkspaceCannotOpen(t *testing.T) {
	a, b := testkit.NewStore(t), testkit.NewStore(t)
	sealed, err := a.Cipher().Seal(ref("t", "i", "c"), []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Cipher().Open(ref("t", "i", "c"), sealed); !errors.Is(err, store.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
	if a.Cipher().Fingerprint("d", []byte("x")) == b.Cipher().Fingerprint("d", []byte("x")) {
		t.Error("fingerprints must differ between workspaces")
	}
}

func TestFingerprint(t *testing.T) {
	c := testkit.NewStore(t).Cipher()
	first, second := c.Fingerprint("plan.payload", []byte("x")), c.Fingerprint("plan.payload", []byte("x"))
	if first != second {
		t.Error("the same input must give the same fingerprint")
	}
	if c.Fingerprint("plan.payload", []byte("x")) == c.Fingerprint("plan.lookup", []byte("x")) {
		t.Error("different domains must give different fingerprints")
	}
	if c.Fingerprint("ab", []byte("c")) == c.Fingerprint("a", []byte("bc")) {
		t.Error("the domain must be length-prefixed")
	}
}

func TestOpenCreateThenExisting(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	kp := testkit.NewMemoryKeyProvider()
	st, err := store.Open(ctx, ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := st.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode = %q, %v", journal, err)
	}
	if st.SchemaVersion() < 1 || len(st.WorkspaceID()) != 32 {
		t.Fatalf("schema %d, workspace id %q", st.SchemaVersion(), st.WorkspaceID())
	}
	sealed, err := st.Cipher().Seal(ref("t", "i", "c"), []byte("kept"))
	if err != nil {
		t.Fatal(err)
	}
	id := st.WorkspaceID()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("a second Close must be a no-op: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(ws.StatePath())
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("state.db mode = %v, %v", info.Mode().Perm(), err)
		}
	}

	again, err := store.Open(ctx, ws, kp, store.OpenExisting)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if again.WorkspaceID() != id {
		t.Errorf("workspace id changed: %s -> %s", id, again.WorkspaceID())
	}
	if got, err := again.Cipher().Open(ref("t", "i", "c"), sealed); err != nil || string(got) != "kept" {
		t.Errorf("value sealed before reopening: %q, %v", got, err)
	}
}

func TestOpenModes(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	kp := testkit.NewMemoryKeyProvider()
	if _, err := store.Open(ctx, ws, kp, store.OpenExisting); !errors.Is(err, store.ErrStateNotFound) {
		t.Fatalf("OpenExisting without state.db: %v", err)
	}
	if _, err := store.Open(ctx, ws, kp, store.OpenShared); !errors.Is(err, store.ErrStateNotFound) {
		t.Fatalf("OpenShared without state.db: %v", err)
	}
	st, err := store.Open(ctx, ws, kp, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// A second process would get its own Workspace value and file descriptor.
	other, err := store.OpenWorkspace(ws.Dir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, other, kp, store.OpenExisting); !errors.Is(err, store.ErrWorkspaceLocked) {
		t.Fatalf("OpenExisting while locked: %v, want ErrWorkspaceLocked", err)
	}
	shared, err := store.Open(ctx, other, kp, store.OpenShared)
	if err != nil {
		t.Fatalf("OpenShared must not need the lock: %v", err)
	}
	defer func() { _ = shared.Close() }()
	if shared.WorkspaceID() != st.WorkspaceID() {
		t.Error("shared open read another workspace")
	}
}

type failingProvider struct{ id string }

func (p failingProvider) ID() string { return p.id }
func (p failingProvider) WrapKey(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("no keychain")
}
func (p failingProvider) UnwrapKey(context.Context, string, []byte) ([]byte, error) {
	return nil, errors.New("keychain item missing")
}

func TestKeyProviderFailures(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, ws, failingProvider{id: "memory"}, store.OpenCreate); !errors.Is(err, store.ErrKeyUnavailable) {
		t.Fatalf("wrap failure: %v", err)
	}
	st, err := store.Open(ctx, ws, testkit.NewMemoryKeyProvider(), store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, ws, failingProvider{id: "keychain"}, store.OpenExisting); !errors.Is(err, store.ErrKeyUnavailable) {
		t.Errorf("provider mismatch: %v", err)
	}
	if _, err := store.Open(ctx, ws, failingProvider{id: "memory"}, store.OpenExisting); !errors.Is(err, store.ErrKeyUnavailable) {
		t.Errorf("unwrap failure: %v", err)
	}
	// A failed open must release the lock it took.
	if err := ws.Lock(); err != nil {
		t.Fatalf("lock still held after failed opens: %v", err)
	}
	_ = ws.Unlock()
}

func TestWithTx(t *testing.T) {
	st := testkit.NewStore(t)
	if _, err := st.DB().ExecContext(ctx, `CREATE TABLE t(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		_ = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n)
		return n
	}
	insert := func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `INSERT INTO t VALUES ('x')`); return err }

	if err := st.WithTx(ctx, insert); err != nil || count() != 1 {
		t.Fatalf("commit: err=%v count=%d", err, count())
	}
	boom := errors.New("boom")
	if err := st.WithTx(ctx, func(tx *sql.Tx) error { _ = insert(tx); return boom }); !errors.Is(err, boom) || count() != 1 {
		t.Fatalf("rollback on error: err=%v count=%d", err, count())
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must be re-raised")
			}
		}()
		_ = st.WithTx(ctx, func(tx *sql.Tx) error { _ = insert(tx); panic("boom") })
	}()
	if count() != 1 {
		t.Fatalf("rollback on panic: count=%d", count())
	}
}

func FuzzOpen(f *testing.F) {
	f.Add([]byte{0x01})
	f.Add(bytes.Repeat([]byte{0x01}, 40))
	st := testkit.NewStore(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		// Random bytes never open...
		if _, err := st.Cipher().Open(ref("t", "i", "c"), data); !errors.Is(err, store.ErrDecrypt) {
			t.Fatalf("random input opened: %v", err)
		}
		// ...and any plaintext survives a seal and an open unchanged.
		sealed, err := st.Cipher().Seal(ref("t", "i", "c"), data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Cipher().Open(ref("t", "i", "c"), sealed)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("round trip: %x, %v", got, err)
		}
	})
}
