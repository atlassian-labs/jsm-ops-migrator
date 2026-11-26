// Package users plans identity mappings for review before provisioning.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
)

type Step struct {
	PD             *pd.Client
	Admin          *admin.Client
	OrgID, CloudID string
}

func (Step) Name() string { return "users" }

type user struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Role    string `json:"role"`
	Pending *bool  `json:"invitation_sent"`
}

func (u user) exclusion() (string, string) {
	switch {
	case u.Pending == nil:
		return "review", "PagerDuty did not report the invitation state."
	case *u.Pending:
		return "exclude_pending", "PagerDuty invitation is pending."
	case u.Role == "read_only_user" || u.Role == "read_only_limited_user":
		return "exclude_stakeholder", "Stakeholders are excluded by default."
	}
	switch u.Role {
	case "admin", "limited_user", "observer", "owner", "restricted_access", "user":
	default:
		return "review", "Unrecognized PagerDuty role."
	}
	address, err := mail.ParseAddress(u.Email)
	if err != nil || address.Address != u.Email {
		return "review", "A valid PagerDuty login email is required."
	}
	return "", ""
}

func (s Step) readUsers(ctx context.Context, w *discover.Writer) error {
	req := pd.ListRequest{Path: "/users", Key: "users"}
	users, coverage, err := pd.List[user](ctx, s.PD, req)
	seen := make(map[string]user)
	save := func(users []user, overlap bool) error {
		for _, u := range users {
			if previous, ok := seen[u.ID]; ok && overlap {
				if previous.Email != u.Email || previous.Role != u.Role || !samePending(previous.Pending, u.Pending) {
					return errors.New("PagerDuty user changed during export; repeat the read")
				}
				continue
			}
			if err := w.Put("user", u.ID, u); err != nil {
				return err
			}
			seen[u.ID] = u
		}
		return nil
	}
	if saveErr := save(users, false); saveErr != nil {
		return saveErr
	}
	if err != nil {
		// Users are required even on plans that omit other PagerDuty domains.
		return fmt.Errorf("reading PagerDuty users: %v", err)
	}
	if coverage.Complete() {
		return nil
	}
	if coverage.Capped {
		teams, tc, err := pd.List[struct{ ID string }](ctx, s.PD, pd.ListRequest{Path: "/teams", Key: "teams"})
		if err != nil {
			return fmt.Errorf("splitting capped user export by team: %v", err)
		}
		if !tc.Complete() {
			return errors.New("team listing incomplete while splitting the capped user export")
		}
		for _, team := range teams {
			if team.ID == "" {
				return errors.New("missing PagerDuty team ID")
			}
			req.Query = url.Values{"team_ids[]": {team.ID}}
			part, _, err := pd.List[user](ctx, s.PD, req)
			if err != nil {
				return fmt.Errorf("reading team users: %v", err)
			}
			if err := save(part, true); err != nil {
				return err
			}
		}
		if coverage.Total >= 0 && len(seen) == coverage.Total {
			return nil
		}
	}
	return fmt.Errorf("incomplete PagerDuty user export: read %d unique users, reported total %d", len(seen), coverage.Total)
}

func samePending(a, b *bool) bool { return a == b || (a != nil && b != nil && *a == *b) }

type lookup struct {
	Email string          `json:"email"`
	Match admin.UserMatch `json:"match"`
}

func sourceUsers(ctx context.Context, source *discover.Snapshot) ([]user, error) {
	rows, err := source.List(ctx, "users", "user")
	if err != nil {
		return nil, err
	}
	users := make([]user, len(rows))
	for i, row := range rows {
		if err := json.Unmarshal(row.Payload, &users[i]); err != nil {
			return nil, err
		}
	}
	return users, nil
}

func (s Step) Read(ctx context.Context, side review.Side, w *discover.Writer) error {
	if side == review.PagerDuty {
		return s.readUsers(ctx, w)
	}
	if w.PagerDuty == nil {
		return errors.New("read PagerDuty users before searching the directory")
	}
	users, err := sourceUsers(ctx, w.PagerDuty)
	if err != nil {
		return err
	}
	var eligible []user
	var emails []credentials.Sensitive
	for _, u := range users {
		if action, _ := u.exclusion(); action != "" {
			continue
		}
		eligible = append(eligible, u)
		emails = append(emails, credentials.NewSensitive(u.Email))
	}
	matches, err := s.Admin.FindSiteUsers(ctx, s.OrgID, s.CloudID, emails)
	if err != nil {
		return fmt.Errorf("directory lookup incomplete: %v", err)
	}
	for i, u := range eligible {
		if err := w.Put("lookup", u.ID, lookup{u.Email, matches[i]}); err != nil {
			return err
		}
	}
	return nil
}

func proposal(m admin.UserMatch) (action, reason string) {
	switch {
	case m.Ambiguous:
		return "review_ambiguous", "Conflicting directory records for this exact email."
	case m.AccountID == "":
		return "review_directory_miss", "No exact match in visible directories; an account may exist outside the organization."
	case !m.Active():
		return "review_inactive", "The directory did not confirm an active account."
	case m.SiteAccess == "yes":
		return "map_existing", "Active exact match with JSM access on the target site."
	case m.SiteAccess == "no":
		return "propose_access", "Active exact match needs JSM access on the target site."
	default:
		return "review_access", "Active exact match; target-site JSM access is unknown."
	}
}

func (s Step) Plan(ctx context.Context, in run.PlanInput) (run.Planned, error) {
	users, err := sourceUsers(ctx, in.PagerDuty)
	if err != nil {
		return run.Planned{}, err
	}
	var plan run.Planned
	for _, u := range users {
		m := &review.UserMapping{Email: u.Email, Role: u.Role}
		it := review.Item{Key: "users/" + u.ID, Action: review.Manual, PagerDuty: &review.Ref{Type: "user", ID: u.ID}, UserMapping: m}
		m.Decision, it.Summary = u.exclusion()
		if strings.HasPrefix(m.Decision, "exclude_") {
			it.Action = review.Skip
		}
		if m.Decision == "" {
			var cached lookup
			ok, err := in.JSM.Get(ctx, "users", "lookup", u.ID, &cached)
			if err != nil {
				return run.Planned{}, err
			}
			if !ok || !strings.EqualFold(cached.Email, u.Email) {
				m.Decision, it.Summary = "refresh_lookup", "Refresh the JSM read for this PagerDuty login email."
				plan.Messages = append(plan.Messages, messages.Message{Item: it.Key, Code: "lookup_required", Class: messages.Blocker, Text: it.Summary})
			} else {
				m.Decision, it.Summary = proposal(cached.Match)
				if cached.Match.Active() {
					m.AccountID = cached.Match.AccountID
					m.SiteAccess = cached.Match.SiteAccess
				}
			}
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// These proposals never plan a remote write or record an approved mapping.
func (Step) Apply(context.Context, review.Item, run.Results) (run.Outcome, error) {
	return run.Outcome{}, errors.New("identity proposals are review-only")
}
func (Step) Lookup(context.Context, review.Item, run.Results, *discover.Snapshot) (run.Lookup, error) {
	return run.Lookup{}, errors.New("identity proposals are review-only")
}
