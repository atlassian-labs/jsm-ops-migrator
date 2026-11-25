package run

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// Engine runs plan and apply in one workspace. The caller holds the
// workspace lock for as long as it uses the engine, as the commands do: the
// engine assumes nothing else writes state.db or artifacts/ meanwhile.
type Engine struct {
	Store     *store.Store
	Workspace *store.Workspace
	// Steps are every step, in the order their items are planned: a tie in
	// the call order goes to the step that comes first.
	Steps []Step
	// Accounts are the identities verified by preflight for this invocation.
	Accounts map[review.Side]review.MigrationAccount
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Redactor knows the credentials of the invocation, so a reason that
	// holds one is cleaned of it; nil applies the pattern rules only.
	Redactor *credentials.Redactor
	// Interrupted is closed when the operator asks a run to stop after the
	// call it is making, at the first Ctrl+C. pause asks the same of the run
	// through pause-request.json.
	Interrupted <-chan struct{}
	// Progress, when set, is told about each item a run takes, once its
	// result is committed.
	Progress func(Progress)

	// crash stops a run at one point of an item's turn as a crash would:
	// nothing more is written. Only tests set it.
	crash func(point crashPoint, key string) bool
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// checkSteps refuses steps the engine cannot keep apart: two with the same
// name would plan the same keys.
func (e *Engine) checkSteps() error {
	seen := map[string]bool{}
	for _, s := range e.Steps {
		switch {
		case !discover.ValidName(s.Name()):
			return fmt.Errorf("step name %s must be snake_case", review.Quote(s.Name()))
		case seen[s.Name()]:
			return fmt.Errorf("two steps are named %s", review.Quote(s.Name()))
		}
		seen[s.Name()] = true
	}
	return nil
}

func (e *Engine) step(name string) Step {
	for _, s := range e.Steps {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// readers are the steps as the reads see them.
func (e *Engine) readers() []discover.Reader {
	readers := make([]discover.Reader, len(e.Steps))
	for i, s := range e.Steps {
		readers[i] = s
	}
	return readers
}

// Read reads one side with every step; see discover.Read.
func (e *Engine) Read(ctx context.Context, side review.Side) (*discover.Snapshot, error) {
	if err := e.checkSteps(); err != nil {
		return nil, err
	}
	return discover.Read(ctx, e.Store, side, e.Accounts[side], e.readers(), e.now)
}

// LatestRead returns the latest finished read of side that these steps made,
// or nil when there is none, for plan --only, which reads one side again and
// reuses the other.
func (e *Engine) LatestRead(ctx context.Context, side review.Side) (*discover.Snapshot, error) {
	return discover.Latest(ctx, e.Store, side, e.readers())
}

// ErrUnfinishedRun means a run has not ended: plan and apply refuse until it
// is resumed to its end, because a new plan or run would be built on objects
// the unfinished one may still create.
var ErrUnfinishedRun = errors.New("a run is unfinished")

// RunInfo is a run as state.db keeps it in clear.
type RunInfo struct {
	ID          string
	PlanVersion int
	State       review.RunState
	StartedAt   time.Time
	EndedAt     time.Time
	// Total is how many items the plan has; Written how many have a final
	// state, and Failed how many of those are not done.
	Total, Written, Failed int
	// Record is the run's run.md, relative to artifacts/.
	Record string
	// JSMRead is the JSM read the run's lookups compare with.
	JSMRead int64
}

// Unfinished returns the run that has not ended, or nil when there is none.
func (e *Engine) Unfinished(ctx context.Context) (*RunInfo, error) {
	return e.runWhere(ctx, `state != 'completed' ORDER BY started_at DESC, id DESC LIMIT 1`)
}

// RunOf returns the run of a plan version, or nil when it has none.
func (e *Engine) RunOf(ctx context.Context, version int) (*RunInfo, error) {
	return e.runWhere(ctx, `plan_version = ? ORDER BY started_at DESC, id DESC LIMIT 1`, version)
}

func (e *Engine) runWhere(ctx context.Context, where string, args ...any) (*RunInfo, error) {
	return queryRun(ctx, e.Store.DB(), where, args...)
}

// queryRun reads one run from what the runs table keeps in clear, so the
// status read can use it without the data key.
func queryRun(ctx context.Context, db *sql.DB, where string, args ...any) (*RunInfo, error) {
	var r RunInfo
	var state, started string
	var ended sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT id, plan_version, state, started_at, ended_at, total, written, failed, record, jsm_read FROM runs WHERE `+where, args...).
		Scan(&r.ID, &r.PlanVersion, &state, &started, &ended, &r.Total, &r.Written, &r.Failed, &r.Record, &r.JSMRead)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.State, r.Record = review.RunState(state), filepath.FromSlash(r.Record)
	if r.StartedAt, err = parseTime(started); err != nil {
		return nil, err
	}
	if ended.Valid {
		if r.EndedAt, err = parseTime(ended.String); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// UnfinishedError is the refusal plan and apply give while r is unfinished.
func (r *RunInfo) UnfinishedError() error {
	return fmt.Errorf("%w: run %s of plan v%d is %s; run 'jsm-ops-migrator resume' to finish it first",
		ErrUnfinishedRun, r.ID, r.PlanVersion, r.State)
}

// newRunID names a run by when it started, so the run folders sort in the
// order they were made, plus a random part, so two runs never share one.
func newRunID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
