package credentials_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

func TestKindsHaveLabelsAndEnvironmentVariables(t *testing.T) {
	want := []string{"JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY", "JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL", "JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN", "JSM_OPS_MIGRATOR_ORG_API_KEY"}
	for i, k := range credentials.Kinds {
		if k.EnvVar() != want[i] || k.Label() == "" {
			t.Errorf("%s: env %s, label %q", k, k.EnvVar(), k.Label())
		}
		// The redactor must treat every one but the email as a secret name.
		if k != credentials.AtlassianEmail && !credentials.IsSecretKey(k.EnvVar()) {
			t.Errorf("%s is not redacted as a secret", k.EnvVar())
		}
	}
}

func TestSetWithMergeAndOnly(t *testing.T) {
	s := credentials.Set{}.With(credentials.PagerDutyKey, "  pd-key-1\n")
	if !s.Has(credentials.PagerDutyKey) || s.PagerDutyKey.Reveal() != "pd-key-1" {
		t.Fatalf("With must trim what a paste brings along: %q", s.PagerDutyKey.Reveal())
	}
	if got := s.Missing(); !slices.Equal(got, []credentials.Kind{credentials.AtlassianEmail, credentials.AtlassianToken, credentials.OrgAPIKey}) {
		t.Errorf("Missing = %v", got)
	}
	other := credentials.Set{}.With(credentials.PagerDutyKey, "pd-key-2").With(credentials.OrgAPIKey, "org-key")
	merged := s.Merge(other).Merge(credentials.Set{})
	if merged.PagerDutyKey.Reveal() != "pd-key-2" || merged.OrgAPIKey.Reveal() != "org-key" {
		t.Errorf("Merge must take every credential the other set holds")
	}
	only := merged.Only(credentials.OrgAPIKey)
	if only.Has(credentials.PagerDutyKey) || !only.Has(credentials.OrgAPIKey) {
		t.Errorf("Only kept the wrong credentials")
	}
	if !(credentials.Set{}).IsZero() || merged.IsZero() {
		t.Error("IsZero")
	}
	if strings.Contains(strings.Join([]string{merged.PagerDutyKey.String()}, ""), "pd-key") {
		t.Error("a credential printed its value")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY": "pd-key", "JSM_OPS_MIGRATOR_ORG_API_KEY": "  ", "OTHER": "x"}
	s, found := credentials.FromEnv(func(k string) string { return env[k] })
	if !slices.Equal(found, []credentials.Kind{credentials.PagerDutyKey}) || s.PagerDutyKey.Reveal() != "pd-key" || s.Has(credentials.OrgAPIKey) {
		t.Errorf("found %v", found)
	}
}

func TestParseInput(t *testing.T) {
	s, err := credentials.ParseInput(strings.NewReader(`
# a comment
JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY=pd=key=with=equals
export JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL = operator@example.com
JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN="quoted token"
JSM_OPS_MIGRATOR_ORG_API_KEY='single quoted'
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.PagerDutyKey.Reveal() != "pd=key=with=equals" || s.AtlassianEmail.Value() != "operator@example.com" ||
		s.AtlassianToken.Reveal() != "quoted token" || s.OrgAPIKey.Reveal() != "single quoted" {
		t.Errorf("parsed %q %q %q %q", s.PagerDutyKey.Reveal(), s.AtlassianEmail.Value(), s.AtlassianToken.Reveal(), s.OrgAPIKey.Reveal())
	}
}

// An error names the line and the name, never the value: a pasted token on a
// line of its own must not end up in the terminal or a log.
func TestParseInputErrorsNeverShowTheValue(t *testing.T) {
	for input, want := range map[string]string{
		"canary-token-value\n": "line 1: want NAME=value",
		"JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY=a\nJSM_OPS_MIGRATOR_PAGERDUTY_API_KEY=canary-token-value\n": "line 2: JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY is given twice",
		"JSM_OPS_MIGRATOR_TOKEN=canary-token-value\n":                                                   `line 1: unknown name "JSM_OPS_MIGRATOR_TOKEN"`,
	} {
		_, err := credentials.ParseInput(strings.NewReader(input))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "canary") {
			t.Errorf("%q: err = %v, want %q without the value", input, err, want)
		}
	}
}

func TestSecretEqual(t *testing.T) {
	a, b := credentials.NewSecret("same"), credentials.NewSecret("same")
	if !a.Equal(b) || a.Equal(credentials.NewSecret("other")) {
		t.Error("Equal")
	}
}

func TestCheckNewPassphrase(t *testing.T) {
	if err := credentials.CheckNewPassphrase(credentials.NewSecret("short")); err == nil {
		t.Error("a short passphrase was accepted")
	}
	if err := credentials.CheckNewPassphrase(credentials.NewSecret("correct horse battery")); err != nil {
		t.Error(err)
	}
}
