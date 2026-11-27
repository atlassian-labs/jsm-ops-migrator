package users

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

func readSource(t *testing.T, f *pdfake.Fake) *discover.Snapshot {
	t.Helper()
	s, err := discover.Read(t.Context(), testkit.NewStore(t), review.PagerDuty, review.MigrationAccount{ID: "example.pagerduty.com"}, []discover.Reader{Step{PD: f.Client()}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCappedExportUsesTeamsAndNameSearch(t *testing.T) {
	f := pdfake.New(t, "test-key")
	people := make([]any, pd.Window+2)
	for i := range people {
		people[i] = user{ID: fmt.Sprintf("P%d", i)}
	}
	teammate := people[pd.Window].(user).ID
	teamless := people[pd.Window+1].(user).ID
	f.List("/teams", "teams", []any{map[string]string{"id": "T1"}})
	f.List("/users", "users", people, pdfake.Filter(func(q url.Values, row any) bool {
		id := row.(user).ID
		if q.Has("team_ids[]") {
			return id == "P0" || id == teammate
		}
		if q.Has("query") {
			return q.Get("query") == "a" && id == teamless
		}
		return true
	}))
	s := readSource(t, f)
	if !s.Complete() {
		t.Fatal(s.Problems())
	}
	for _, id := range []string{teammate, teamless} {
		var got user
		ok, err := s.Get(t.Context(), "users", "user", id, &got)
		if err != nil || !ok || got.ID != id {
			t.Fatalf("missing %s: %v", id, err)
		}
	}
}

func TestRepeatedUsersAndScreenedEmails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changed     bool
		total       int
		wantProblem string
	}{
		{"unchanged overlap", false, 2, ""},
		{"changed overlap", true, 2, "changed during export"},
		{"missing unique user", false, 3, "read 2 unique users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pdfake.New(t, "test-key")
			pending := false
			u := user{ID: "P1", Email: "oncall+15551234567@example.com", Role: "user", Pending: &pending}
			duplicate := u
			if tc.changed {
				duplicate.Email = "different@example.com"
			}
			f.Answer("/users", map[string]any{"users": []user{u, duplicate, {ID: "P2", Email: "second@example.com", Role: "user", Pending: &pending}}, "more": false, "total": tc.total})
			s := readSource(t, f)
			if tc.wantProblem != "" {
				if s.Complete() || !strings.Contains(fmt.Sprint(s.Problems()), tc.wantProblem) {
					t.Fatal(s.Problems())
				}
				return
			}
			if !s.Complete() {
				t.Fatal(s.Problems())
			}
			got, err := sourceUsers(t.Context(), s)
			if err != nil || len(got) != 2 || got[0].Email != "" || !got[0].EmailWithheld || got[1].Email != "second@example.com" {
				t.Fatalf("screened export or following user lost: %v", err)
			}
			if action, _ := got[0].exclusion(false); action != "review" {
				t.Fatalf("withheld login action = %s", action)
			}
		})
	}
}

func TestIneligibleSourceAndUnknownSiteAccess(t *testing.T) {
	pending := false
	for _, u := range []user{{Role: "user", Email: "invalid", Pending: &pending}, {Role: "unknown", Email: "user@example.com", Pending: &pending}, {Role: "user", Email: "user@example.com"}} {
		if action, _ := u.exclusion(false); action != "review" {
			t.Fatalf("action = %s", action)
		}
	}
	if action, _ := proposal(admin.UserMatch{AccountID: "a", AccountStatus: "active", MembershipStatus: "active"}); action != "review_access" {
		t.Fatal(action)
	}
	if action, _ := proposal(admin.UserMatch{Incomplete: true}); action != "review_incomplete" {
		t.Fatal(action)
	}
}
