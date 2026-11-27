package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// PlanVersion is a plan version as state.db keeps it.
type PlanVersion struct {
	Version int
	// Hash is the hash of the version's plan.json, which apply checks the
	// file against before it runs it.
	Hash                string
	ContentHash         string
	LegacyMappingExport bool
	CreatedAt           time.Time
	// PagerDutyRead and JSMRead are the reads the plan was built from.
	PagerDutyRead, JSMRead    int64
	Calls, Blockers, Warnings int
	// Dir is the version's folder, relative to artifacts/.
	Dir string
	// Plan is the plan, read from state.db's sealed copy of plan.json.
	Plan review.Plan
}

// PlanResult is what Plan made.
type PlanResult struct {
	Version *PlanVersion
	// Unchanged means the plan does the same as the latest version, so no
	// new version was written and Version is the latest one.
	Unchanged bool
}

// ErrStepFault means a step planned something the engine cannot take: an
// item outside its own keys, a message about an item it did not plan, or a
// plan that fails review's checks. plan stops with it, because it is a fault
// in the step, not in the data.
var ErrStepFault = errors.New("a step planned something apply cannot run")

// Plan builds a plan from both reads with every step, orders it, and writes
// it as a new version with its review files, unless it does the same as the
// latest version. The reads' problems come first among its messages, so an
// incomplete read always carries its blocker.
func (e *Engine) Plan(ctx context.Context, pagerDuty, jsm *discover.Snapshot) (PlanResult, error) {
	if err := e.checkSteps(); err != nil {
		return PlanResult{}, err
	}
	if r, err := e.Unfinished(ctx); err != nil {
		return PlanResult{}, err
	} else if r != nil {
		return PlanResult{}, r.UnfinishedError()
	}
	mappings, err := e.mappings(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	in := PlanInput{PagerDuty: pagerDuty, JSM: jsm, Mappings: mappings}
	var items []review.Item
	msgs := append(pagerDuty.Problems(), jsm.Problems()...)
	for _, s := range e.Steps {
		planned, err := s.Plan(ctx, in)
		if err != nil {
			return PlanResult{}, fmt.Errorf("planning %s: %w", s.Name(), err)
		}
		if err := ownItems(s.Name(), planned); err != nil {
			return PlanResult{}, err
		}
		items = append(items, planned.Items...)
		msgs = append(msgs, planned.Messages...)
	}
	ordered, err := order(items)
	if err != nil {
		return PlanResult{}, err
	}

	latest, err := e.Latest(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	next, err := e.nextVersion(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	p := review.Plan{
		Version:   next,
		CreatedAt: e.now(),
		Reads:     []review.Read{pagerDuty.Read(), jsm.Read()},
		Items:     ordered,
		Messages:  msgs,
	}
	if err := p.Check(); err != nil {
		return PlanResult{}, fmt.Errorf("%w: %w", ErrStepFault, err)
	}
	content, err := review.ContentHash(p)
	if err != nil {
		return PlanResult{}, err
	}
	if same, err := e.unchanged(ctx, latest, content, next); err != nil {
		return PlanResult{}, err
	} else if same {
		return PlanResult{Version: latest, Unchanged: true}, nil
	}

	var prev *review.Plan
	if latest != nil {
		prev = &latest.Plan
	}
	hash, err := review.WritePlanVersion(e.Workspace, p, prev)
	if err != nil {
		return PlanResult{}, err
	}
	data, err := review.Encode(p)
	if err != nil {
		return PlanResult{}, err
	}
	if review.Hash(data) != hash {
		return PlanResult{}, fmt.Errorf("plan v%d: plan.json changed while it was written", p.Version)
	}
	sealed, err := e.Store.Cipher().Seal(planRef(p.Version), data)
	if err != nil {
		return PlanResult{}, err
	}
	sum := review.Summarize(p)
	v := &PlanVersion{
		Version: p.Version, Hash: hash, ContentHash: content, CreatedAt: p.CreatedAt,
		PagerDutyRead: pagerDuty.ID(), JSMRead: jsm.ID(),
		Calls: sum.Calls, Blockers: sum.Messages.Blockers, Warnings: sum.Messages.Warnings,
		Dir: review.PlanDir(p.Version), Plan: p,
	}
	_, err = e.Store.DB().ExecContext(ctx, `INSERT INTO plan_versions(version, hash, content_hash, created_at,
		pagerduty_read, jsm_read, calls, blockers, warnings, dir, plan, requires_mapping_review) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.Version, v.Hash, v.ContentHash, formatTime(v.CreatedAt), v.PagerDutyRead, v.JSMRead,
		v.Calls, v.Blockers, v.Warnings, filepath.ToSlash(v.Dir), sealed, p.HasUserMappings())
	if err != nil {
		return PlanResult{}, err
	}
	// Read back as apply will, so the version returned is the one kept.
	if v, err = e.Version(ctx, v.Version); err != nil {
		return PlanResult{}, err
	}
	return PlanResult{Version: v}, nil
}

// unchanged reports whether p can stay the latest version instead of
// becoming a new one: it does the same, and the latest version can still be
// applied, or needs no applying. A version that already has a run is never
// run again, so a plan that still makes calls after it, such as every call
// of a run that failed for a permission fixed since, is a new version. So is
// any plan while a folder newer than the latest version is on disk: apply
// refuses until a version is newer than it. So is one whose folder lacks a
// review file: the operator reviews them, apply points at the acceptance
// template, and a version's folder is never written into again.
func (e *Engine) unchanged(ctx context.Context, latest *PlanVersion, content string, next int) (bool, error) {
	if latest == nil || latest.LegacyMappingExport || latest.ContentHash != content || next != latest.Version+1 {
		return false, nil
	}
	dir := filepath.Join(e.Workspace.ArtifactsDir(), latest.Dir)
	data, err := os.ReadFile(filepath.Join(dir, review.PlanFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if review.Hash(data) != latest.Hash {
		return false, nil
	}
	for _, f := range review.PlanFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}
	if latest.Calls == 0 {
		return true, nil
	}
	// A fresh plan with identical items must replace an incompatible baseline.
	original, err := discover.Open(ctx, e.Store, latest.JSMRead)
	if err != nil {
		return false, err
	}
	if !original.MatchesReaders(e.readers()) {
		return false, nil
	}
	r, err := e.RunOf(ctx, latest.Version)
	return r == nil, err
}

// ownItems refuses items outside the step's own keys and messages about
// items the step did not plan: two steps planning the same key would make
// one item's results reach the other's dependents.
func ownItems(step string, planned Planned) error {
	own := make(map[string]bool, len(planned.Items))
	for _, it := range planned.Items {
		if !strings.HasPrefix(it.Key, step+"/") {
			return fmt.Errorf("%w: %s planned item %s, whose key is not %s/<id>", ErrStepFault, step, review.Quote(it.Key), step)
		}
		own[it.Key] = true
	}
	for _, m := range planned.Messages {
		if m.Item != "" && !own[m.Item] {
			return fmt.Errorf("%w: %s raised message %s about an item it did not plan", ErrStepFault, step, review.Quote(m.ID()))
		}
	}
	return nil
}

var versionDirRe = regexp.MustCompile(`^v(\d+)$`)

// nextVersion is one more than the highest version state.db or artifacts/
// knows. A folder state.db does not know is from a plan that crashed after
// writing it; its number is never used again, because the operator may have
// read its files.
func (e *Engine) nextVersion(ctx context.Context) (int, error) {
	var highest sql.NullInt64
	if err := e.Store.DB().QueryRowContext(ctx, `SELECT max(version) FROM plan_versions`).Scan(&highest); err != nil {
		return 0, err
	}
	onDisk, err := e.highestFolder()
	if err != nil {
		return 0, err
	}
	return max(int(highest.Int64), onDisk) + 1, nil
}

// highestFolder is the highest version number among the folders in
// artifacts/plans/, or 0.
func (e *Engine) highestFolder() (int, error) {
	entries, err := os.ReadDir(filepath.Join(e.Workspace.ArtifactsDir(), "plans"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	n := 0
	for _, d := range entries {
		if m := versionDirRe.FindStringSubmatch(d.Name()); m != nil && d.IsDir() {
			if v, err := strconv.Atoi(m[1]); err == nil {
				n = max(n, v)
			}
		}
	}
	return n, nil
}

func planRef(version int) store.FieldRef {
	return store.FieldRef{Table: "plan_versions", Item: "v" + strconv.Itoa(version), Column: "plan"}
}

// Latest returns the latest plan version, or nil when there is none.
func (e *Engine) Latest(ctx context.Context) (*PlanVersion, error) {
	var v sql.NullInt64
	if err := e.Store.DB().QueryRowContext(ctx, `SELECT max(version) FROM plan_versions`).Scan(&v); err != nil {
		return nil, err
	}
	if !v.Valid {
		return nil, nil
	}
	return e.Version(ctx, int(v.Int64))
}

// Version returns a plan version, with its plan read from state.db's sealed
// copy.
func (e *Engine) Version(ctx context.Context, version int) (*PlanVersion, error) {
	v := &PlanVersion{Version: version}
	var created, dir string
	var pd, jsm sql.NullInt64
	var sealed []byte
	err := e.Store.DB().QueryRowContext(ctx, `SELECT hash, content_hash, created_at, pagerduty_read, jsm_read,
		calls, blockers, warnings, dir, plan, requires_mapping_review IS NULL FROM plan_versions WHERE version = ?`, version).
		Scan(&v.Hash, &v.ContentHash, &created, &pd, &jsm, &v.Calls, &v.Blockers, &v.Warnings, &dir, &sealed, &v.LegacyMappingExport)
	if err != nil {
		return nil, fmt.Errorf("plan v%d: %w", version, err)
	}
	v.PagerDutyRead, v.JSMRead, v.Dir = pd.Int64, jsm.Int64, filepath.FromSlash(dir)
	if v.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	data, err := e.Store.Cipher().Open(planRef(version), sealed)
	if err != nil {
		return nil, err
	}
	if review.Hash(data) != v.Hash {
		return nil, fmt.Errorf("plan v%d: state.db's copy of plan.json does not match its hash", version)
	}
	if v.Plan, err = review.Decode(data); err != nil {
		return nil, fmt.Errorf("plan v%d: %w", version, err)
	}
	return v, nil
}

// Blocked is the error for a plan version with blockers: exit code 3.
func (v *PlanVersion) Blocked() error {
	return messages.Blocked(fmt.Errorf("plan v%d has %s to fix first; see %s",
		v.Version, messages.Quantity(v.Blockers, messages.Blocker), filepath.Join(store.ArtifactsDir, v.Dir, review.RiskQueueFile)))
}

func (e *Engine) mappings(ctx context.Context) (Mappings, error) {
	rows, err := e.Store.DB().QueryContext(ctx,
		`SELECT pagerduty_type, pagerduty_id, jsm_type, jsm_id, item_key FROM mappings`)
	if err != nil {
		return Mappings{}, err
	}
	defer func() { _ = rows.Close() }()
	m := Mappings{byPD: map[mappingKey]Mapping{}}
	for rows.Next() {
		var l Mapping
		if err := rows.Scan(&l.PagerDuty.Type, &l.PagerDuty.ID, &l.JSM.Type, &l.JSM.ID, &l.Item); err != nil {
			return Mappings{}, err
		}
		m.byPD[mappingKey{l.PagerDuty.Type, l.PagerDuty.ID, l.JSM.Type}] = l
	}
	return m, rows.Err()
}
