package testkit_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

func rules(fs []testkit.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

// Inputs are built in code, never stored under testdata, so the scan of
// testdata/ stays clean.
func TestScanSensitive(t *testing.T) {
	plusPhone := "+90 555 " + "123 45 67"
	pdAddress := "555" + "1234567"
	jsmTo := "90-555" + "1234567"
	cases := []struct {
		name, path, data string
		want             []string // rules expected; nil means clean
	}{
		{"auth header in text", "x.txt", "Authorization: Bearer abcdefghijk", []string{"authorization-header", "credential-scheme"}},
		{"redacted auth header", "x.txt", "Authorization: [redacted]", nil},
		{"cassette header list", "c.yaml", "headers:\n  Authorization:\n    - Basic abcdefghijk\n", []string{"authorization-header", "credential-scheme"}},
		{"redacted cassette header", "c.yaml", "headers:\n  Authorization:\n    - '[redacted]'\n", nil},
		{"secret key in JSON", "x.json", `{"api_token": "abc"}`, []string{"secret-key"}},
		{"redacted secret key", "x.json", `{"api_token": "[redacted]", "token": ""}`, nil},
		{"calendar URL key", "s.json", `{"web_cal_url": "https://x"}`, []string{"secret-key"}},
		{"PagerDuty token", "x.txt", "Token token=y_Xz1234", []string{"pagerduty-token"}},
		{"calendar URL", "x.txt", "webcal://acme.pagerduty.com/private/abc/feed", []string{"calendar-url"}},
		{"PagerDuty SMS address", "u.json", `{"type": "sms_contact_method", "address": "` + pdAddress + `"}`, []string{"phone-number"}},
		{"masked PagerDuty address", "u.json", `{"type": "phone_contact_method", "address": "0000000033"}`, nil},
		{"PagerDuty email address", "u.json", `{"type": "email_contact_method", "address": "jane@example.com"}`, nil},
		{"JSM voice contact", "c.json", `{"method": "voice", "to": "` + jsmTo + `"}`, []string{"phone-number"}},
		{"masked JSM contact", "c.json", `{"method": "sms", "to": "90-0000000001"}`, nil},
		{"plus number in text", "x.md", "call " + plusPhone, []string{"phone-number"}},
		{"masked plus number", "x.md", "call +90 000 000 00 01", nil},
		{"timestamps and IDs", "x.json", `{"at": "2026-10-27T10:00:00+03:00", "id": "0d7f2c3e-1b2a-4c5d-9e8f-001122334455", "pd": "PABC123"}`, nil},
		{"cookie header", "x.txt", "Cookie: a=1; JSESSIONID=abc123", []string{"authorization-header"}},
		{"set-cookie in a cassette", "c.yaml", "headers:\n  Set-Cookie:\n    - JSESSIONID=abc123\n", []string{"authorization-header"}},
		{"routing key", "x.json", `{"routing_key": "R0ABC123"}`, []string{"secret-key"}},
		{"second YAML document", "c.yaml", "a: 1\n---\napi_key: abc\n", []string{"secret-key"}},
		{"phone inside a cassette body", "c.yaml",
			"response:\n  body: '{\"contact_methods\":[{\"type\":\"sms_contact_method\",\"address\":\"" + pdAddress + "\"}]}'\n",
			[]string{"phone-number"}},
		{"JSON golden file", "plan.golden", `{"contacts":[{"method":"sms","to":"` + jsmTo + `"}]}`, []string{"phone-number"}},
		{"Go timestamp", "x.txt", "2026-10-27 10:00:00.123456789 +0300 +03 m=+0.000123456", nil},
		{"plus-joined date", "x.txt", "window 2026-10-01+2026-10-27", nil},
	}
	for _, c := range cases {
		got := rules(testkit.ScanSensitive(c.path, []byte(c.data)))
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("%s: rule %s not found in %v", c.name, w, got)
			}
		}
		if c.want == nil && len(got) != 0 {
			t.Errorf("%s: want clean, got %v", c.name, got)
		}
	}
}

func TestScanReportsLines(t *testing.T) {
	got := testkit.ScanSensitive("c.yaml", []byte("a: 1\nheaders:\n  Authorization:\n    - Basic abcdefghijk\n"))
	if len(got) == 0 || got[0].Line != 4 || got[0].Path != "c.yaml" {
		t.Fatalf("findings = %+v, want line 4", got)
	}
}

func FuzzScanSensitive(f *testing.F) {
	f.Add("x.json", `{"type":"sms_contact_method","address":"1"}`)
	f.Add("c.yaml", "Authorization:\n  - x\n")
	f.Add("c.yaml", "body: '{\"to\":\"90-1\"}'\n---\na: b\n")
	f.Fuzz(func(t *testing.T, path, data string) {
		first := testkit.ScanSensitive(path, []byte(data))
		if second := testkit.ScanSensitive(path, []byte(data)); !slices.Equal(first, second) {
			t.Fatalf("not deterministic: %v vs %v", first, second)
		}
		lines := strings.Count(data, "\n") + 1
		for _, f := range first {
			if f.Path != path || f.Line < 1 || f.Line > lines || f.Rule == "" {
				t.Fatalf("bad finding %+v for %d lines", f, lines)
			}
		}
	})
}
