// Package users plans identity mappings for review before provisioning.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/jira"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/ops"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
)

type Step struct {
	PD                  *pd.Client
	Admin               *admin.Client
	OrgID, CloudID      string
	Jira                *jira.Client
	Ops                 *ops.Client
	Provisioning        string
	IncludeStakeholders map[string]bool
}

func (Step) Name() string { return "users" }

// Fresh directory reads must verify organization membership.
func (Step) ReadVersion() int { return 1 }

type user struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	Pending       *bool  `json:"invitation_sent"`
	EmailWithheld bool   `json:"email_withheld,omitempty"`
}

func (u user) stakeholder() bool {
	return u.Role == "read_only_user" || u.Role == "read_only_limited_user"
}

func (u user) exclusion(includeStakeholder bool) (string, string) {
	switch {
	case u.Pending == nil:
		return "review", "PagerDuty did not report the invitation state."
	case *u.Pending:
		return "exclude_pending", "PagerDuty invitation is pending."
	case u.stakeholder() && !includeStakeholder:
		return "exclude_stakeholder", "Stakeholders are excluded by default."
	}
	switch u.Role {
	case "admin", "limited_user", "observer", "owner", "restricted_access", "user", "read_only_user", "read_only_limited_user":
	default:
		return "review", "Unrecognized PagerDuty role."
	}
	if u.EmailWithheld {
		return "review", "Login email withheld because it contains sensitive data."
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
	save := func(users []user) error {
		for _, u := range users {
			if previous, ok := seen[u.ID]; ok {
				if previous.Email != u.Email || previous.Role != u.Role || !samePending(previous.Pending, u.Pending) {
					return errors.New("PagerDuty user changed during export; repeat the read")
				}
				continue
			}
			stored := u
			if len(credentials.DetectSecrets(u.Email)) != 0 {
				stored.Email = ""
				stored.EmailWithheld = true
			}
			if err := w.Put("user", u.ID, stored); err != nil {
				return err
			}
			seen[u.ID] = u
		}
		return nil
	}
	if saveErr := save(users); saveErr != nil {
		return saveErr
	}
	if err != nil {
		// Users are required even on plans that omit other PagerDuty domains.
		return fmt.Errorf("reading PagerDuty users: %v", err)
	}
	if coverage.Complete() && ((coverage.Total < 0 && len(seen) == coverage.Read) || len(seen) == coverage.Total) {
		return nil
	}
	if coverage.Capped {
		teams, _, err := pd.List[struct{ ID string }](ctx, s.PD, pd.ListRequest{Path: "/teams", Key: "teams"})
		if err != nil && !errors.Is(err, httperr.ErrNotInPlan) {
			return fmt.Errorf("splitting capped user export by team: %v", err)
		}
		// The filters can overlap and can themselves hit the offset window.
		// Only a count of distinct IDs matching the unfiltered total proves coverage.
		readPartition := func(filter url.Values) error {
			req.Query = filter
			part, _, err := pd.List[user](ctx, s.PD, req)
			if err != nil {
				return fmt.Errorf("reading a partition of PagerDuty users: %v", err)
			}
			return save(part)
		}
		for _, team := range teams {
			if team.ID == "" {
				return errors.New("missing PagerDuty team ID")
			}
			if err := readPartition(url.Values{"team_ids[]": {team.ID}}); err != nil {
				return err
			}
		}
		if coverage.Total >= 0 {
			// Name searches can reach team-less users beyond the unfiltered window.
			// These searches are best-effort: names need not contain ASCII.
			for _, ch := range "abcdefghijklmnopqrstuvwxyz0123456789" {
				if len(seen) >= coverage.Total {
					break
				}
				if err := readPartition(url.Values{"query": {string(ch)}}); err != nil {
					return err
				}
			}
			if len(seen) == coverage.Total {
				return nil
			}
		}
		return fmt.Errorf("PagerDuty pagination window reached: verified %d unique users, reported total %d; team and name searches could not establish complete coverage", len(seen), coverage.Total)
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
		if action, _ := u.exclusion(s.IncludeStakeholders[u.ID]); action != "" {
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
	if s.Provisioning == "non-scim" {
		seats, err := s.Jira.Seats(ctx)
		if err != nil {
			return w.Put("capacity_error", "jsm", messages.Clean(err.Error()))
		}
		return w.Put("capacity", "jsm", seats)
	}
	return nil
}

func proposal(m admin.UserMatch) (action, reason string) {
	switch {
	case m.Incomplete:
		return "review_incomplete", "The directory returned an incomplete record for this login."
	case m.Ambiguous:
		return "review_ambiguous", "Conflicting directory records for this exact email."
	case m.AccountID == "":
		return "review_directory_miss", "No user in your Atlassian organization’s directory matched this PagerDuty email. Choose defer while you confirm the person’s Atlassian login email."
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
	directoryFailed := slices.ContainsFunc(in.JSM.Problems(), func(m messages.Message) bool { return m.Code == "users_jsm_read" && m.Class == messages.Blocker })
	var plan run.Planned
	selected := make(map[string]bool)
	for _, u := range users {
		m := &review.UserMapping{Email: u.Email, Role: u.Role}
		it := review.Item{Key: "users/" + u.ID, Action: review.Manual, PagerDuty: &review.Ref{Type: "user", ID: u.ID}, UserMapping: m}
		m.Decision, it.Summary = u.exclusion(s.IncludeStakeholders[u.ID])
		if strings.HasPrefix(m.Decision, "exclude_") {
			it.Action = review.Skip
		}
		if m.Decision == "" && directoryFailed {
			m.Decision, it.Summary = "refresh_lookup", "The directory read is incomplete. Refresh it before reviewing this match."
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
		if s.Provisioning != "" {
			if err := s.planAccess(&plan, &it); err != nil {
				return run.Planned{}, err
			}
		}
		if u.stakeholder() && s.IncludeStakeholders[u.ID] {
			selected[u.ID] = true
			it.Summary = "Stakeholder selected for standard user access. " + it.Summary
		}
		plan.Items = append(plan.Items, it)
	}
	var unmatched []string
	for id := range s.IncludeStakeholders {
		if !selected[id] {
			unmatched = append(unmatched, review.Quote(id))
		}
	}
	if len(unmatched) > 0 {
		slices.Sort(unmatched)
		plan.Messages = append(plan.Messages, messages.Message{Class: messages.Warning, Code: "stakeholder_not_found", Text: "These selected IDs did not match a PagerDuty stakeholder: " + strings.Join(unmatched, ", ")})
	}
	if s.Provisioning == "non-scim" && !directoryFailed {
		var seats jira.Seats
		ok, err := in.JSM.Get(ctx, "users", "capacity", "jsm", &seats)
		if err != nil {
			return run.Planned{}, err
		}
		if !ok {
			var why string
			if _, err := in.JSM.Get(ctx, "users", "capacity_error", "jsm", &why); err != nil {
				return run.Planned{}, err
			}
			text := "Refresh the JSM read for non-SCIM provisioning."
			if why != "" {
				text = "Reading JSM agent seats failed. Check the Jira administrator permission and token scope. " + why
			}
			plan.Messages = append(plan.Messages, messages.Message{Class: messages.Blocker, Code: "seat_read_required", Text: text})
		} else {
			plan.Messages = append(plan.Messages, capacityMessage(plan.Items, seats))
		}
	}
	return plan, nil
}
