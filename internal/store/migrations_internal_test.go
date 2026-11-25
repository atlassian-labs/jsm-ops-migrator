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

func TestReadCoverageUpgradePreservesLegacyUnfinishedWrites(t *testing.T) {
	ctx := context.Background()
	current := embedded(t)
	legacy := fstest.MapFS{}
	for _, name := range []string{"0001_foundation.sql", "0002_run_engine.sql", "0003_migration_accounts.sql"} {
		data, err := fs.ReadFile(current, name)
		if err != nil {
			t.Fatal(err)
		}
		legacy[name] = &fstest.MapFile{Data: data}
	}
	for _, tc := range []struct {
		name, state string
		calls       int
		blocked     bool
	}{
		{"unfinished write run", "paused", 1, true},
		{"completed write run", "completed", 1, false},
		{"unfinished no-op run", "interrupted", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := OpenWorkspace(t.TempDir(), true)
			if err != nil {
				t.Fatal(err)
			}
			prior, err := open(ctx, ws, staticProvider{}, OpenCreate, legacy)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []struct {
				sql  string
				args []any
			}{
				{sql: `INSERT INTO snapshot_reads(id, side, started_at) VALUES (1, 'jsm', 'time')`},
				{`INSERT INTO plan_versions(version, hash, content_hash, created_at, calls, blockers, warnings, dir, plan)
					VALUES (1, 'hash', 'content', 'time', ?, 0, 0, 'v1', x'00')`, []any{tc.calls}},
				{sql: `INSERT INTO confirmations(plan_version, confirmed_at, account_id, via, record)
					VALUES (1, 'time', 'account', 'file', x'00')`},
				{`INSERT INTO runs(id, plan_version, state, started_at, total, record, jsm_read)
					VALUES ('legacy-run', 1, ?, 'time', ?, 'run.md', 1)`, []any{tc.state, tc.calls}},
			} {
				if _, err := prior.DB().ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
					_ = prior.Close()
					t.Fatal(err)
				}
			}
			if err := prior.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.blocked {
				if _, err := open(ctx, ws, staticProvider{}, OpenExisting, current); !errors.Is(err, ErrUnfinishedRunBeforeUpgrade) ||
					!strings.Contains(err.Error(), "legacy-run") {
					t.Fatalf("upgrading an unfinished run: %v", err)
				}
				prior, err := open(ctx, ws, staticProvider{}, OpenExisting, legacy)
				if err != nil {
					t.Fatalf("original build cannot reopen the workspace: %v", err)
				}
				_, done := prior.DB().ExecContext(ctx, `UPDATE runs SET state = 'completed' WHERE id = 'legacy-run'`)
				if err := errors.Join(done, prior.Close()); err != nil {
					t.Fatal(err)
				}
			}
			upgraded, err := open(ctx, ws, staticProvider{}, OpenExisting, current)
			if err != nil {
				t.Fatalf("upgrading a workspace without an unfinished write run: %v", err)
			}
			defer func() { _ = upgraded.Close() }()
			if upgraded.SchemaVersion() <= len(legacy) {
				t.Fatalf("schema version %d did not advance", upgraded.SchemaVersion())
			}
		})
	}
}
