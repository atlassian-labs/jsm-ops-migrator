package users

import (
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
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
