package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// MappingApproval describes local review decisions. The execution journal
// records completed migration work separately.
type MappingApproval struct {
	PlanVersion int                      `json:"plan_version"`
	PlanHash    string                   `json:"plan_hash"`
	ApprovedBy  string                   `json:"approved_by"`
	ApprovedAt  time.Time                `json:"approved_at"`
	Decisions   []review.MappingDecision `json:"decisions"`
	History     []MappingRevision        `json:"previous_approvals,omitempty"`
	Artifact    string                   `json:"artifact"`
}

// MappingRevision preserves who made each superseded decision on the same plan.
type MappingRevision struct {
	ApprovedBy string                   `json:"approved_by"`
	ApprovedAt time.Time                `json:"approved_at"`
	Decisions  []review.MappingDecision `json:"decisions"`
}

func approvalRef(v int) store.FieldRef {
	return store.FieldRef{Table: "mapping_approvals", Item: strconv.Itoa(v), Column: "record"}
}

func (e *Engine) mappingApproval(ctx context.Context, v *PlanVersion) (*MappingApproval, error) {
	var sealed []byte
	err := e.Store.DB().QueryRowContext(ctx, "SELECT record FROM mapping_approvals WHERE plan_version = ?", v.Version).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := e.Store.Cipher().Open(approvalRef(v.Version), sealed)
	if err != nil {
		return nil, err
	}
	var a MappingApproval
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	if a.PlanHash != v.Hash || a.PlanVersion != v.Version {
		return nil, errors.New("stored mapping approval does not match the plan")
	}
	return &a, nil
}

// SaveMappingApproval runs under the workspace lock. Seal first, then write a
// readable receipt from the committed record. Repeating an import repairs a
// missing receipt and preserves its original approval time.
func (e *Engine) SaveMappingApproval(ctx context.Context, v *PlanVersion, accountID string, decisions []review.MappingDecision) (*MappingApproval, error) {
	old, err := e.mappingApproval(ctx, v)
	if err != nil {
		return nil, err
	}
	a := MappingApproval{PlanVersion: v.Version, PlanHash: v.Hash, ApprovedBy: accountID, ApprovedAt: e.now(), Decisions: decisions, Artifact: filepath.ToSlash(filepath.Join(v.Dir, "mapping-approval.json"))}
	same := old != nil && old.ApprovedBy == accountID && slices.Equal(old.Decisions, decisions)
	if old != nil {
		a.History = old.History
		if same {
			a.ApprovedAt = old.ApprovedAt
		} else {
			a.History = append(a.History, MappingRevision{old.ApprovedBy, old.ApprovedAt, old.Decisions})
		}
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	if !same {
		sealed, err := e.Store.Cipher().Seal(approvalRef(v.Version), data)
		if err != nil {
			return nil, err
		}
		_, err = e.Store.DB().ExecContext(ctx, `INSERT INTO mapping_approvals(plan_version,record) VALUES (?,?)
    ON CONFLICT(plan_version) DO UPDATE SET record=excluded.record`, v.Version, sealed)
		if err != nil {
			return nil, err
		}
	}
	if err := e.Workspace.WriteArtifact(a.Artifact, append(data, '\n')); err != nil {
		return nil, err
	}
	return &a, nil
}

func (e *Engine) checkMappingApproval(ctx context.Context, v *PlanVersion) error {
	if !v.Plan.HasUserMappings() {
		return nil
	}
	a, err := e.mappingApproval(ctx, v)
	if err != nil {
		return err
	}
	if a == nil {
		if v.LegacyMappingExport {
			return messages.AwaitingInput(errors.New("run plan again to export a CSV with a review_decision field for each user"))
		}
		return messages.AwaitingInput(fmt.Errorf("plan v%d needs reviewed user mappings: run plan approve-mappings --version %d --csv <reviewed.csv>", v.Version, v.Version))
	}
	return nil
}
