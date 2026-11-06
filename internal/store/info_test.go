package store_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

func TestReadInfo(t *testing.T) {
	ctx := context.Background()
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.ReadInfo(ctx, ws)
	if err != nil || info != (store.Info{}) {
		t.Fatalf("no state.db: %+v, %v", info, err)
	}
	if _, err := os.Stat(ws.StatePath()); !errors.Is(err, fs.ErrNotExist) {
		t.Error("ReadInfo created state.db")
	}

	st, err := store.Open(ctx, ws, testkit.NewMemoryKeyProvider(), store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if st.KeyProviderID() != "memory" {
		t.Errorf("KeyProviderID = %q", st.KeyProviderID())
	}
	// It reads while another command holds the lock and the database is open.
	info, err = store.ReadInfo(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if info.WorkspaceID != st.WorkspaceID() || info.KeyProvider != "memory" {
		t.Errorf("info %+v, want workspace %s and provider memory", info, st.WorkspaceID())
	}
}

func TestReadInfoOnAnEmptyStateFile(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.StatePath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := store.ReadInfo(context.Background(), ws); err != nil || info != (store.Info{}) {
		t.Errorf("empty state.db: %+v, %v", info, err)
	}
}

func TestRootFileReadAndRemove(t *testing.T) {
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadFile("credentials.enc"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if err := ws.WriteFileAtomic("credentials.enc", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if b, err := ws.ReadFile("credentials.enc"); err != nil || string(b) != "sealed" {
		t.Errorf("read %q, %v", b, err)
	}
	if err := ws.RemoveFile("credentials.enc"); err != nil {
		t.Fatal(err)
	}
	if err := ws.RemoveFile("credentials.enc"); err != nil {
		t.Errorf("removing a missing file must succeed: %v", err)
	}
	for _, name := range []string{"state.db", "lock", filepath.Join("artifacts", "x")} {
		if _, err := ws.ReadFile(name); err == nil {
			t.Errorf("ReadFile(%q) is not a root file and must fail", name)
		}
		if err := ws.RemoveFile(name); err == nil {
			t.Errorf("RemoveFile(%q) is not a root file and must fail", name)
		}
	}
}
