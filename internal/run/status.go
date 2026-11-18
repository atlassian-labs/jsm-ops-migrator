package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// Status is where a workspace stands, as state.db keeps it in clear.
type Status struct {
	// Plan is the latest plan version, or nil when there is none.
	Plan *PlanSummary
	// Run is the latest run, or nil when there is none.
	Run *RunInfo
	// Active is the run an apply or resume is making now, or nil.
	Active *ActiveRun
}

// PlanSummary is a plan version without its plan, which is sealed.
type PlanSummary struct {
	Version, Calls, Blockers, Warnings int
	// Dir is the version's folder, relative to artifacts/.
	Dir string
}

// ReadStatus reads the latest plan version and run without the data key or
// the lock, so the menu and pause can show them while a run is being made,
// in a workspace whose key is behind a passphrase or the keychain. A run
// marked running that no command is making ended without marking itself,
// as in a crash, and is reported interrupted, as resume takes it.
func ReadStatus(ctx context.Context, ws *store.Workspace) (Status, error) {
	db, err := store.OpenReadOnly(ws)
	if errors.Is(err, store.ErrStateNotFound) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = db.Close() }()
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'
		AND name IN ('plan_versions', 'runs')`).Scan(&tables); err != nil {
		return Status{}, fmt.Errorf("reading state.db: %w", err)
	}
	if tables < 2 {
		// A workspace no plan has used yet.
		return Status{}, nil
	}
	var s Status
	var p PlanSummary
	var dir string
	err = db.QueryRowContext(ctx, `SELECT version, calls, blockers, warnings, dir FROM plan_versions
		ORDER BY version DESC LIMIT 1`).Scan(&p.Version, &p.Calls, &p.Blockers, &p.Warnings, &dir)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Status{}, fmt.Errorf("reading state.db: %w", err)
	default:
		p.Dir = filepath.FromSlash(dir)
		s.Plan = &p
	}
	// Runs apply increasing plan versions, even if the system clock moves back.
	if s.Run, err = queryRun(ctx, db, `1 = 1 ORDER BY plan_version DESC LIMIT 1`); err != nil {
		return Status{}, fmt.Errorf("reading state.db: %w", err)
	}
	if s.Run == nil || s.Run.State != review.Running {
		return s, nil
	}
	if s.Active, err = FindActive(ws); err != nil {
		return Status{}, err
	}
	if s.Active == nil || s.Active.RunID != s.Run.ID {
		s.Run.State = review.Interrupted
	}
	return s, nil
}
