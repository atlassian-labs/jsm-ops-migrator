package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// OpenMode says how Open treats state.db and the workspace lock.
type OpenMode int

const (
	// OpenCreate creates state.db when it is missing, applies pending
	// migrations and takes the workspace lock.
	OpenCreate OpenMode = iota
	// OpenExisting requires state.db, applies pending migrations and takes
	// the workspace lock.
	OpenExisting
	// OpenShared requires state.db at the binary's schema version, applies
	// nothing and takes no lock, so a command such as pause never blocks a
	// running apply.
	OpenShared
)

var (
	// ErrStateNotFound means state.db is missing or was never initialized.
	ErrStateNotFound = errors.New("state.db not found")
	// ErrSchemaTooNew means state.db was written by a newer jsm-ops-migrator.
	ErrSchemaTooNew = errors.New("state.db was written by a newer jsm-ops-migrator; upgrade it first")
	// ErrSchemaMismatch means a shared open found a different schema version.
	ErrSchemaMismatch = errors.New("state.db schema version does not match this jsm-ops-migrator")
	// ErrKeyUnavailable means the data key could not be unwrapped, so
	// state.db cannot be read.
	ErrKeyUnavailable = errors.New("state.db's key is not available")
)

// Store is the workspace's SQLite database.
type Store struct {
	db            *sql.DB
	ws            *Workspace
	cipher        *fieldCipher
	dataKey       []byte
	workspaceID   string
	schemaVersion int
	tookLock      bool
	closeOnce     sync.Once
	closeErr      error
}

// Open opens state.db in the workspace.
func Open(ctx context.Context, ws *Workspace, kp KeyProvider, mode OpenMode) (*Store, error) {
	migrations, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, err
	}
	return open(ctx, ws, kp, mode, migrations)
}

func open(ctx context.Context, ws *Workspace, kp KeyProvider, mode OpenMode, migrationFS fs.FS) (_ *Store, err error) {
	migrations, err := loadMigrations(migrationFS)
	if err != nil {
		return nil, err
	}
	s := &Store{ws: ws}
	if mode != OpenShared {
		if err := ws.Lock(); err != nil {
			return nil, err
		}
		s.tookLock = true
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	if err := prepareFile(ws.StatePath(), mode == OpenCreate); err != nil {
		return nil, err
	}
	if s.db, err = sql.Open("sqlite", dsn(ws.StatePath())); err != nil {
		return nil, err
	}
	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return nil, fmt.Errorf("opening state.db: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		return nil, fmt.Errorf("state.db journal mode is %q, want wal", journal)
	}

	current, err := schemaVersion(ctx, s.db)
	if err != nil {
		return nil, err
	}
	latest := len(migrations)
	switch {
	case mode == OpenShared && current == 0:
		return nil, ErrStateNotFound
	case current > latest && mode == OpenShared:
		return nil, fmt.Errorf("%w: %w (schema %d, this binary knows %d)", ErrSchemaMismatch, ErrSchemaTooNew, current, latest)
	case current > latest:
		return nil, fmt.Errorf("%w (schema %d, this binary knows %d)", ErrSchemaTooNew, current, latest)
	case mode == OpenShared && current != latest:
		return nil, fmt.Errorf("%w (schema %d, this binary knows %d)", ErrSchemaMismatch, current, latest)
	case mode != OpenShared:
		if err := applyMigrations(ctx, s.db, migrations, current); err != nil {
			return nil, err
		}
	}
	s.schemaVersion = latest

	if err := s.initKeys(ctx, kp, mode); err != nil {
		return nil, err
	}
	return s, nil
}

// dsn uses a plain path, not a file: URI, so Windows paths need no escaping.
// The driver strips the query before opening the file. synchronous=FULL keeps
// a committed journal entry across a power loss, and immediate transactions
// take the write lock at BEGIN, so a second writer waits for busy_timeout
// instead of failing on a lock upgrade.
func dsn(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
}

// prepareFile creates an empty state.db with mode 0600 before SQLite opens it,
// so the database and its WAL files are never readable by others.
func prepareFile(path string, create bool) error {
	if strings.ContainsRune(path, '?') {
		return fmt.Errorf("the workspace path must not contain '?': %s", path)
	}
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	case !create:
		return ErrStateNotFound
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	return f.Close()
}

func (s *Store) initKeys(ctx context.Context, kp KeyProvider, mode OpenMode) error {
	if mode == OpenShared {
		return s.loadKeys(ctx, kp)
	}
	// The workspace ID is committed on its own, before the key provider runs.
	// The keychain provider stores its key under that ID, so if the process
	// dies after the keychain write but before data_keys is saved, the next
	// open finds the same ID and reuses that key instead of stranding it.
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO meta(key, value) VALUES ('workspace_id', ?), ('created_at', ?)`,
		newWorkspaceID(), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'workspace_id'`).Scan(&id); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM data_keys`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		dataKey := make([]byte, dataKeyLen)
		if _, err := rand.Read(dataKey); err != nil {
			return err
		}
		wrapped, err := kp.WrapKey(ctx, id, dataKey)
		clear(dataKey)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrKeyUnavailable, err)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO data_keys(id, provider, wrapped, created_at) VALUES (1, ?, ?, ?)`,
			kp.ID(), wrapped, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	if err != nil {
		return err
	}
	return s.loadKeys(ctx, kp)
}

func (s *Store) loadKeys(ctx context.Context, kp KeyProvider) error {
	var provider string
	var wrapped []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'workspace_id'`).Scan(&s.workspaceID)
	if err == nil {
		err = s.db.QueryRowContext(ctx, `SELECT provider, wrapped FROM data_keys WHERE id = 1`).Scan(&provider, &wrapped)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStateNotFound
	}
	if err != nil {
		return err
	}
	if provider != kp.ID() {
		return fmt.Errorf("%w: state.db's key was protected with %q, but this command uses %q",
			ErrKeyUnavailable, provider, kp.ID())
	}
	dataKey, err := kp.UnwrapKey(ctx, s.workspaceID, wrapped)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrKeyUnavailable, err)
	}
	if s.cipher, err = newFieldCipher(dataKey); err != nil {
		clear(dataKey)
		return err
	}
	s.dataKey = dataKey
	return nil
}

func newWorkspaceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// DB returns the database handle.
func (s *Store) DB() *sql.DB { return s.db }

// Cipher returns the field cipher for this workspace's data key.
func (s *Store) Cipher() Cipher { return s.cipher }

// WorkspaceID is the random ID created with state.db. It binds keychain items
// and sealed values to this workspace.
func (s *Store) WorkspaceID() string { return s.workspaceID }

// SchemaVersion is the number of migrations applied.
func (s *Store) SchemaVersion() int { return s.schemaVersion }

// WithTx runs fn in a transaction. It commits when fn returns nil and rolls
// back when fn returns an error or panics; a panic is re-raised.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close closes the database, overwrites the in-memory data key and releases
// the workspace lock if Open took it. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.db != nil {
			s.closeErr = s.db.Close()
		}
		if s.cipher != nil {
			s.cipher.zero()
		}
		clear(s.dataKey)
		if s.tookLock {
			if err := s.ws.Unlock(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

type migration struct {
	version int
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// loadMigrations reads NNNN_name.sql files. Versions must run from 0001 with
// no gaps, and every file must start with a comment naming the task that owns
// it, so two parallel changes that pick the same number collide loudly.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s: name must be NNNN_snake_case.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		if version != len(out)+1 {
			return nil, fmt.Errorf("migration %s: expected version %04d", e.Name(), len(out)+1)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(string(body), "--") {
			return nil, fmt.Errorf("migration %s: the first line must be a comment naming the owning task", e.Name())
		}
		out = append(out, migration{version: version, name: path.Base(e.Name()), sql: string(body)})
	}
	return out, nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n)
	if err != nil || n == 0 {
		return 0, err
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func applyMigrations(ctx context.Context, db *sql.DB, migrations []migration, current int) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	for _, m := range migrations[current:] {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}
