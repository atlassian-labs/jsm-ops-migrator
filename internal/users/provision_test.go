package users

import (
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

func TestRecheckGrantAgainstReviewedIdentity(t *testing.T) {
	match := admin.UserMatch{AccountID: "target", AccountStatus: "active", MembershipStatus: "active", SiteAccess: "no"}
	for _, tc := range []struct {
		name, approved string
		live           admin.UserMatch
		need, valid    bool
	}{
		{"approved miss", "", admin.UserMatch{}, true, true},
		{"new match after review", "", match, false, false},
		{"approved grant", "target", match, true, true},
		{"different account", "other", match, false, false},
		{"ambiguous miss", "", admin.UserMatch{Ambiguous: true}, false, false},
		{"incomplete miss", "", admin.UserMatch{Incomplete: true}, false, false},
		{"lost account", "target", admin.UserMatch{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, err := (grant{AccountID: tc.approved}).needsGrant(tc.live)
			if need != tc.need || (err == nil) != tc.valid {
				t.Fatalf("need=%t, err=%v", need, err)
			}
		})
	}
	match.SiteAccess = "yes"
	if need, err := (grant{AccountID: "target"}).needsGrant(match); need || err != nil {
		t.Fatal("repeated an existing access grant")
	}
}

func TestReviewedGrantReconcilesOnlyTheApprovedAccount(t *testing.T) {
	f := jsmfake.New(t, jsmfake.Account{Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1", OrgKey: "org-test-value-1234"})
	step := Step{Admin: admin.New(f.Client()), OrgID: "org-1", CloudID: "cloud-1", Provisioning: "non-scim"}
	item := review.Item{Key: "users/access/P7", Action: review.Create, ApprovalFor: "P7", Payload: []byte(`{"email":"user@example.com","account_id":"approved"}`)}
	search := f.AdminPath("/v2/orgs/org-1/directories/-/users/search")
	for _, id := range []string{"replacement", "approved"} {
		f.Answer("POST", search, 200, map[string]any{"data": []any{map[string]any{
			"accountId": id, "email": "user@example.com", "accountStatus": "active", "membershipStatus": "active",
			"productAccess": []any{map[string]string{"id": "ari:cloud:jira-servicedesk::site/cloud-1"}},
		}}})
		out, err := step.Apply(t.Context(), item, nil)
		lookup, lookupErr := step.Lookup(t.Context(), item, nil, nil)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if id == "replacement" {
			if err == nil || out.JSM != nil || lookup.Outcome != run.LookupUnknown {
				t.Fatalf("accepted a replacement: %+v, %v, %+v", out, err, lookup)
			}
		} else if err != nil || out.JSM == nil || out.JSM.ID != id || lookup.Outcome != run.LookupFound || lookup.JSM == nil || lookup.JSM.ID != id {
			t.Fatalf("did not reuse the approved grant: %+v, %v, %+v", out, err, lookup)
		}
	}
	for _, req := range f.Requests() {
		if req.Path != search {
			t.Fatalf("unexpected %s %s", req.Method, req.Path)
		}
	}
}
