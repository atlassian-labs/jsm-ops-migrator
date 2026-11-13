package httpx_test

import (
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
)

func TestIsStagingSite(t *testing.T) {
	for site, want := range map[string]bool{
		"acme.jira-dev.com":   true,
		"ACME.Jira-Dev.com":   true,
		"acme.atlassian.net":  false,
		".jira-dev.com":       false,
		"jira-dev.com":        false,
		"acmejira-dev.com":    false,
		"acme.jira-dev.com.x": false,
		"":                    false,
	} {
		if got := httpx.IsStagingSite(site); got != want {
			t.Errorf("IsStagingSite(%q) = %v, want %v", site, got, want)
		}
	}
}
