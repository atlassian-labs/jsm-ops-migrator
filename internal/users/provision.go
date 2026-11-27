package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/jira"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
)

// An absent account ID means the administrator reviewed an invitation to this
// exact login, including the possibility it belongs to an outside-org account.
type grant struct {
	Email     string `json:"email"`
	AccountID string `json:"account_id,omitempty"`
}

func (s Step) planAccess(p *run.Planned, it *review.Item) error {
	m := it.UserMapping
	switch m.Decision {
	case "map_existing":
	case "review_directory_miss", "propose_access":
		if s.Provisioning != "non-scim" {
			m.Decision = "identity_provider"
			it.Summary = "Have the identity provider provision JSM access, then plan again."
			return nil
		}
		if m.AccountID == "" {
			m.Decision = "propose_invite"
		}
		body, err := json.Marshal(grant{m.Email, m.AccountID})
		if err != nil {
			return err
		}
		key := "users/access/" + it.PagerDuty.ID
		p.Items = append(p.Items, review.Item{
			Key: key, Action: review.Create, ApprovalFor: it.PagerDuty.ID, Payload: body,
			Summary: "If approved, invite this login by email to grant JSM access on the target site (one agent seat).",
		})
		it.DependsOn = []string{key}
		it.Summary = "If approved, invite this login by email for JSM access, then wait for its Operations account."
	default:
		return nil
	}
	it.Action = review.Verify
	it.ApprovalFor = it.PagerDuty.ID
	return nil
}

func capacityMessage(items []review.Item, seats jira.Seats) messages.Message {
	n := 0
	for _, it := range items {
		if it.Action == review.Create {
			n++
		}
	}
	text := fmt.Sprintf("Up to %d JSM agent seats are needed if every proposed invitation is approved. The site has unlimited seats.", n)
	if !seats.Unlimited {
		text = fmt.Sprintf("Up to %d JSM agent seats are needed if every proposed invitation is approved. %d seats remain. Apply checks approved invitations again before writing.", n, seats.Remaining)
	}
	return messages.Message{Class: messages.Information, Code: "user_seats", Text: text}
}

func grantFrom(it review.Item) (grant, error) {
	var g grant
	if err := json.Unmarshal(it.Payload, &g); err != nil || g.Email == "" {
		return grant{}, errors.New("invalid user access request in plan")
	}
	return g, nil
}

// needsGrant revalidates the identity before an invitation addressed by email.
// A newly appearing directory entry requires a new review, even for the same email.
func (g grant) needsGrant(m admin.UserMatch) (bool, error) {
	if g.AccountID == "" && m == (admin.UserMatch{}) {
		return true, nil
	}
	if !m.Active() || m.AccountID != g.AccountID || (m.SiteAccess != "yes" && m.SiteAccess != "no") {
		return false, errors.New("directory identity or access differs from the reviewed proposal; review a fresh plan")
	}
	return m.SiteAccess == "no", nil
}

// BeforeWrites counts only outstanding, approved access calls on this pass.
// Resuming a previously acknowledged invitation only reads its account.
func (s Step) BeforeWrites(ctx context.Context, items []review.Item) error {
	var grants []grant
	var emails []credentials.Sensitive
	seen := map[string]bool{}
	for _, it := range items {
		if it.Action != review.Create {
			continue
		}
		if s.Provisioning != "non-scim" {
			return errors.New("approved invitations require --user-provisioning non-scim on this workspace")
		}
		g, err := grantFrom(it)
		if err != nil {
			return err
		}
		key := strings.ToLower(g.Email)
		if seen[key] {
			return errors.New("multiple approved invitations for the same login; review the mappings again")
		}
		seen[key] = true
		grants = append(grants, g)
		emails = append(emails, credentials.NewSensitive(g.Email))
	}
	if len(grants) == 0 {
		return nil
	}
	if err := s.Admin.CheckJSMWorkspace(ctx, s.OrgID, s.CloudID); err != nil {
		return err
	}
	matches, err := s.Admin.FindSiteUsers(ctx, s.OrgID, s.CloudID, emails)
	if err != nil {
		return err
	}
	required := 0
	for i, g := range grants {
		need, err := g.needsGrant(matches[i])
		if err != nil {
			return err
		}
		if need {
			required++
		}
	}
	if required == 0 {
		return nil
	}
	seats, err := s.Jira.Seats(ctx)
	if err != nil {
		return err
	}
	if !seats.Unlimited && required > seats.Remaining {
		return fmt.Errorf("approved invitations need %d JSM agent seats, but only %d remain", required, seats.Remaining)
	}
	return nil
}

func (s Step) find(ctx context.Context, email string) (admin.UserMatch, error) {
	m, err := s.Admin.FindSiteUsers(ctx, s.OrgID, s.CloudID, []credentials.Sensitive{credentials.NewSensitive(email)})
	if err != nil {
		return admin.UserMatch{}, err
	}
	return m[0], nil
}

func userRef(id string) *review.Ref { return &review.Ref{Type: "user", ID: id} }

func (s Step) Apply(ctx context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	if it.Action == review.Verify {
		return s.verify(ctx, it, deps)
	}
	if it.Action != review.Create || it.ApprovalFor == "" || s.Provisioning != "non-scim" {
		return run.Outcome{}, errors.New("user access was not planned for non-SCIM provisioning")
	}
	g, err := grantFrom(it)
	if err != nil {
		return run.Outcome{}, err
	}
	m, err := s.find(ctx, g.Email)
	if err != nil {
		return run.Outcome{}, err
	}
	need, err := g.needsGrant(m)
	if err != nil {
		return run.Outcome{}, err
	}
	if !need {
		return run.Outcome{JSM: userRef(m.AccountID)}, nil
	}
	id, receipt, err := s.Admin.Invite(ctx, s.OrgID, s.CloudID, credentials.NewSensitive(g.Email))
	out := run.Outcome{Status: receipt.Status, RequestID: receipt.RequestID}
	if err != nil {
		return out, err
	}
	if g.AccountID != "" && g.AccountID != id {
		return out, fmt.Errorf("invitation returned a different account: %w", httperr.ErrUnknownOutcome)
	}
	out.JSM = userRef(id)
	return out, nil
}

func (s Step) Lookup(ctx context.Context, it review.Item, _ run.Results, _ *discover.Snapshot) (run.Lookup, error) {
	g, err := grantFrom(it)
	if err != nil {
		return run.Lookup{}, err
	}
	m, err := s.find(ctx, g.Email)
	if err != nil {
		return run.Lookup{}, err
	}
	if m == (admin.UserMatch{}) || (g.AccountID != "" && m.Active() && m.AccountID == g.AccountID && m.SiteAccess == "no") {
		return run.Lookup{Outcome: run.LookupNotFound}, nil
	}
	if m.Active() && m.SiteAccess == "yes" && (g.AccountID == "" || g.AccountID == m.AccountID) {
		return run.Lookup{Outcome: run.LookupFound, JSM: userRef(m.AccountID)}, nil
	}
	// Finding the original account alone does not establish a successful grant.
	return run.Lookup{}, nil
}

func (s Step) verify(ctx context.Context, it review.Item, deps run.Results) (run.Outcome, error) {
	m := it.UserMapping
	if m == nil {
		return run.Outcome{}, errors.New("user readiness check is missing its reviewed identity")
	}
	id := m.AccountID
	if len(it.DependsOn) > 0 {
		result := deps[it.DependsOn[0]]
		if result.State != review.Done || result.JSM == nil {
			return run.Outcome{Declined: true, Messages: []messages.Message{{Class: messages.ManualTask, Code: "access_unavailable", Text: "The invitation did not establish an account to verify. See its journal entry."}}}, nil
		}
		id = result.JSM.ID
	} else {
		live, err := s.find(ctx, m.Email)
		if err != nil {
			return run.Outcome{}, err
		}
		if !live.Active() || live.AccountID != id || live.SiteAccess != "yes" {
			return run.Outcome{Declined: true, Messages: []messages.Message{{Class: messages.ManualTask, Code: "identity_changed", Text: "The reviewed login or target access changed. Review a fresh plan."}}}, nil
		}
	}
	access, err := s.Jira.HasAccess(ctx, id)
	if err != nil {
		return run.Outcome{}, err
	}
	if !access {
		return waiting("invitation_pending", "Jira access is pending. Ask the user to accept their invitation, then resume."), nil
	}
	ready, err := s.Ops.WaitForUser(ctx, id)
	if err != nil {
		return run.Outcome{}, err
	}
	if !ready {
		return waiting("ops_user_pending", "JSM access is active and may consume a seat. Resume to check Operations again. If provisioning is abandoned, review whether to remove access."), nil
	}
	return run.Outcome{JSM: userRef(id)}, nil
}

func waiting(code, text string) run.Outcome {
	return run.Outcome{Pending: true, Messages: []messages.Message{{Class: messages.ManualTask, Code: code, Text: text}}}
}
