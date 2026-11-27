package run

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// approvedPlan keeps the sealed proposal intact. Apply and resume derive the
// same executable items from the approval, which is frozen once the run starts.
func (e *Engine) approvedPlan(ctx context.Context, v *PlanVersion, beforeRun bool) (review.Plan, error) {
	p := v.Plan
	if !slices.ContainsFunc(p.Items, func(it review.Item) bool { return it.ApprovalFor != "" }) {
		return p, nil
	}
	a, err := e.mappingApproval(ctx, v)
	if err != nil {
		return review.Plan{}, err
	}
	if a == nil || a.ApprovedBy == "" {
		return review.Plan{}, errors.New("the current plan requires named mapping approval")
	}
	decisions := make(map[string]review.MappingDecision, len(a.Decisions))
	for _, d := range a.Decisions {
		decisions[d.PagerDutyID] = d
	}
	p.Items = slices.Clone(p.Items)
	remaining := map[string][]review.Item{}
	for i := range p.Items {
		it := &p.Items[i]
		if it.ApprovalFor == "" {
			continue
		}
		d, ok := decisions[it.ApprovalFor]
		if !ok {
			return review.Plan{}, errors.New("missing decision for a planned user")
		}
		switch d.Decision {
		case "accept":
			if it.UserMapping != nil && it.UserMapping.AccountID != d.AccountID {
				return review.Plan{}, errors.New("approved account differs from the planned user")
			}
			if beforeRun {
				remaining[it.Step()] = append(remaining[it.Step()], *it)
			}
		case "defer", "skip":
			it.Action, it.JSM = review.Skip, nil
			it.Summary = fmt.Sprintf("The reviewer chose %s for this user.", d.Decision)
		default:
			return review.Plan{}, errors.New("invalid stored user decision")
		}
	}
	if !beforeRun {
		return p, nil
	}
	for _, s := range e.Steps {
		if preflight, ok := s.(interface {
			BeforeWrites(context.Context, []review.Item, string) error
		}); ok {
			if err := preflight.BeforeWrites(ctx, remaining[s.Name()], a.ApprovedBy); err != nil {
				return review.Plan{}, err
			}
		}
	}
	return p, nil
}
