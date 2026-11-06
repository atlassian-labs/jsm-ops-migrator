package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

func check(title string, s preflight.Status, detail string) preflight.Check {
	return preflight.Check{Title: title, Status: s, Detail: detail}
}

// A detail several checks share is printed once, under the table.
func TestWriteChecksSaysASharedReasonOnce(t *testing.T) {
	var b strings.Builder
	err := writeChecks(&b, "Login checks", []preflight.Check{
		check("PagerDuty API key", preflight.NotChecked, "the API clients are not built yet"),
		check("Site", preflight.OK, "acme.atlassian.net, cloudId cloud-1"),
		check("Org API key", preflight.NotChecked, "the API clients are not built yet"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Login checks\n" +
		"  PagerDuty API key  not checked\n" +
		"  Site               ok           acme.atlassian.net, cloudId cloud-1\n" +
		"  Org API key        not checked\n" +
		"  PagerDuty API key and Org API key: the API clients are not built yet.\n"
	if got := b.String(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestChecksSummary(t *testing.T) {
	cases := []struct {
		name   string
		checks []preflight.Check
		want   []string
	}{
		{"all passed", []preflight.Check{
			check("Site", preflight.OK, "acme"),
			check("Org admin", preflight.Warning, "make sure it is"),
		}, []string{"All checks passed", "Warning: Org admin (make sure it is)"}},
		{"problems first, a shared reason once", []preflight.Check{
			check("PagerDuty API key", preflight.Failed, "PagerDuty rejected the key"),
			check("Site", preflight.NotChecked, "the API clients are not built yet"),
			check("JSM plan", preflight.Missing, "not stated"),
			check("Org API key", preflight.NotChecked, "the API clients are not built yet"),
			check("Clock", preflight.Missing, "needs the site"),
		}, []string{
			"Failed: PagerDuty API key (PagerDuty rejected the key)",
			"Not checked: Site and Org API key (the API clients are not built yet)",
			"Missing: JSM plan and Clock",
		}},
	}
	for _, c := range cases {
		if got := checksSummary(c.checks); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestLoginSummary(t *testing.T) {
	checks := []preflight.Check{check("PagerDuty API key", preflight.OK, "works")}
	got := loginSummary(loginResult{Checks: checks, storedNow: []credentials.Kind{credentials.PagerDutyKey, credentials.OrgAPIKey}, savedSite: true})
	if want := []string{"All checks passed", "Stored the PagerDuty API key and org API key; saved the site"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := loginSummary(loginResult{Checks: checks}); got[len(got)-1] != "Nothing new was stored or saved" {
		t.Errorf("got %q", got)
	}
}

func TestJoinWords(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a,b": "a and b", "a,b,c": "a, b and c"} {
		var items []string
		if in != "" {
			items = strings.Split(in, ",")
		}
		if got := joinWords(items); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
