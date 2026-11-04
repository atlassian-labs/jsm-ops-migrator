package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

type staticProvider struct{}

func (staticProvider) ID() string { return "static" }
func (staticProvider) WrapKey(_ context.Context, _ string, k []byte) ([]byte, error) {
	return append([]byte(nil), k...), nil
}
func (staticProvider) UnwrapKey(_ context.Context, _ string, w []byte) ([]byte, error) {
	return append([]byte(nil), w...), nil
}

func embedded(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestEmbeddedMigrationsLoad(t *testing.T) {
	migs, err := loadMigrations(embedded(t))
	if err != nil || len(migs) == 0 {
		t.Fatalf("loadMigrations = %d, %v", len(migs), err)
	}
}

func TestMigrationRules(t *testing.T) {
	ok := "-- Owner: test.\nCREATE TABLE x(a TEXT);"
	for name, files := range map[string]fstest.MapFS{
		"bad name":      {"0001_Foundation.sql": {Data: []byte(ok)}},
		"gap":           {"0001_a.sql": {Data: []byte(ok)}, "0003_c.sql": {Data: []byte(ok)}},
		"no owner line": {"0001_a.sql": {Data: []byte("CREATE TABLE x(a TEXT);")}},
	} {
		if _, err := loadMigrations(files); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSharedOpenErrors(t *testing.T) {
	ctx := context.Background()
	ws, err := OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	// An empty state.db, as a crash during the first open can leave.
	if err := prepareFile(ws.StatePath(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := open(ctx, ws, staticProvider{}, OpenShared, embedded(t)); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("uninitialized: %v, want ErrStateNotFound", err)
	}
	st, err := open(ctx, ws, staticProvider{}, OpenCreate, embedded(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	newer := fstest.MapFS{}
	entries, _ := fs.ReadDir(embedded(t), ".")
	for _, e := range entries {
		data, _ := fs.ReadFile(embedded(t), e.Name())
		newer[e.Name()] = &fstest.MapFile{Data: data}
	}
	newer[fmt.Sprintf("%04d_later.sql", len(entries)+1)] = &fstest.MapFile{Data: []byte("-- Owner: test.\nCREATE TABLE later(a TEXT);")}
	if _, err := open(ctx, ws, staticProvider{}, OpenShared, newer); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("binary newer than state.db: %v, want ErrSchemaMismatch", err)
	}
}

func TestSchemaTooNewAndFailingMigration(t *testing.T) {
	ctx := context.Background()
	ws, err := OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	newer := fstest.MapFS{}
	base := embedded(t)
	entries, _ := fs.ReadDir(base, ".")
	for _, e := range entries {
		data, _ := fs.ReadFile(base, e.Name())
		newer[e.Name()] = &fstest.MapFile{Data: data}
	}
	next := len(entries) + 1
	nextName := fmt.Sprintf("%04d_later.sql", next)
	newer[nextName] = &fstest.MapFile{Data: []byte("-- Owner: a later task.\nCREATE TABLE later(a TEXT);")}

	st, err := open(ctx, ws, staticProvider{}, OpenCreate, newer)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := open(ctx, ws, staticProvider{}, OpenExisting, base); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("older binary: %v, want ErrSchemaTooNew", err)
	}
	if _, err := open(ctx, ws, staticProvider{}, OpenShared, base); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("older binary, shared: %v, want ErrSchemaTooNew", err)
	}

	broken := fstest.MapFS{}
	for k, v := range newer {
		broken[k] = v
	}
	failName := fmt.Sprintf("%04d_broken.sql", next+1)
	broken[failName] = &fstest.MapFile{Data: []byte("-- Owner: test.\nCREATE TABLE ok(a TEXT); NOT SQL;")}
	_, err = open(ctx, ws, staticProvider{}, OpenExisting, broken)
	if err == nil || !strings.Contains(err.Error(), failName) {
		t.Fatalf("failing migration: %v, want an error naming %s", err, failName)
	}
	st, err = open(ctx, ws, staticProvider{}, OpenExisting, newer)
	if err != nil {
		t.Fatalf("after a failed migration the database must be unchanged: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int
	_ = st.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'ok'`).Scan(&n)
	if n != 0 {
		t.Error("the failing migration was partly applied")
	}
}
