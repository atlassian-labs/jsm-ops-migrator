package credentials_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

func testingRealKeychain() bool { return os.Getenv("JSM_OPS_MIGRATOR_TEST_REAL_KEYCHAIN") == "1" }

// Canary values are built here, never stored under testdata, so the testdata
// scan stays clean.
var (
	phone       = "+90 555 " + "123 45 67"
	calendarURL = "webcal://acme.pagerduty.com/private/" + "abcdef123456/feed/P1"
)

func TestRedactText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Authorization: Bearer abcdefghijk", "Authorization: [redacted]"},
		{"proxy-authorization=Basic dXNlcjpwYXNz", "proxy-authorization=[redacted]"},
		{"sent with Bearer abcdefghijkl to the API", "sent with Bearer [redacted] to the API"},
		{"header Token token=y_Xz123", "header Token token=[redacted]"},
		{"feed " + calendarURL + " ok", "feed [redacted] ok"},
		{"call " + phone + " now", "call [redacted] now"},
		{`{"api_token": "abc", "name": "Ops"}`, `{"api_token": "[redacted]", "name": "Ops"}`},
		{"GET /v1/x?api_key=abc123&limit=100", "GET /v1/x?api_key=[redacted]&limit=100"},
		{"address=jane@example.com", "address=[redacted]"},
		{"Authorization: [redacted]", "Authorization: [redacted]"},
		// Plain values stay as they are.
		{"2026-10-27T10:00:00+03:00", "2026-10-27T10:00:00+03:00"},
		{"team 0d7f2c3e-1b2a-4c5d-9e8f-001122334455 ready", "team 0d7f2c3e-1b2a-4c5d-9e8f-001122334455 ready"},
		{"user PABC123 jane@example.com", "user PABC123 jane@example.com"},
		{"limit=100 offset=200", "limit=100 offset=200"},
	}
	for _, c := range cases {
		if got := credentials.RedactText(c.in); got != c.want {
			t.Errorf("RedactText(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestRedactorKnownSecrets(t *testing.T) {
	secret := credentials.NewSecret("s3cr3t-value-123")
	r := credentials.NewRedactor(secret, credentials.NewSecret("short"))
	got := r.String("echo s3cr3t-value-123 and short")
	if strings.Contains(got, "s3cr3t-value-123") {
		t.Errorf("known secret not removed: %q", got)
	}
	if !strings.Contains(got, "short") {
		t.Errorf("a value shorter than the minimum must not be searched for: %q", got)
	}
}

func TestRedactJSON(t *testing.T) {
	in := `{"user":{"name":"Jane","contact_methods":[{"type":"sms_contact_method","address":"5551234567"}]},` +
		`"headers":{"Authorization":"Basic abcdefgh"},"count":12345678901234567890,"note":"call ` + phone + `"}`
	got := credentials.RedactJSON([]byte(in))
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("output is not JSON: %s", got)
	}
	for _, leak := range []string{"5551234567", "abcdefgh", "555 123"} {
		if bytes.Contains(got, []byte(leak)) {
			t.Errorf("leaked %q: %s", leak, got)
		}
	}
	for _, keep := range []string{`"name":"Jane"`, `12345678901234567890`, `"type":"sms_contact_method"`} {
		if !bytes.Contains(got, []byte(keep)) {
			t.Errorf("lost %s: %s", keep, got)
		}
	}
	if got := string(credentials.RedactJSON([]byte("not json token=abc"))); got != "not json token=[redacted]" {
		t.Errorf("non-JSON fallback = %q", got)
	}
}

func TestDetectSensitive(t *testing.T) {
	got := credentials.DetectSensitive("Authorization: Bearer abcdefghijk " + phone + " " + calendarURL)
	for _, rule := range []string{"authorization", "credential_scheme", "phone_number", "calendar_url"} {
		if !slices.Contains(got, rule) {
			t.Errorf("rule %s not detected in %v", rule, got)
		}
	}
	if got := credentials.DetectSensitive("plan v3 created 2026-10-27T10:00:00+03:00 for team PABC123"); len(got) != 0 {
		t.Errorf("clean text flagged: %v", got)
	}
	if got := credentials.DetectSensitive(credentials.RedactText("token=abc " + phone)); len(got) != 0 {
		t.Errorf("redacted text still flagged: %v", got)
	}
}

func TestRedactAttr(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: credentials.RedactAttr}))
	log.Warn("calling "+phone,
		"token", "abc",
		slog.Group("request", "to", "90-5551234567", "path", "/v1/teams"),
		"err", errors.New("Authorization: Bearer abcdefghijk"),
		"secret", credentials.NewSecret(value))
	out := buf.String()
	for _, leak := range []string{"555 123", `"abc"`, "5551234567", "abcdefghijk", value} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "/v1/teams") {
		t.Errorf("redacted too much: %s", out)
	}
}

func FuzzRedactText(f *testing.F) {
	for _, s := range []string{phone, calendarURL, "Authorization: Basic abcdefgh", "token=abc", `{"to":"90-5551234567"}`, "plain"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := credentials.RedactText(s)
		for _, rule := range credentials.DetectSensitive(out) {
			if rule == "phone_number" || rule == "calendar_url" {
				t.Errorf("RedactText left a %s in %q -> %q", rule, s, out)
			}
		}
		_ = credentials.RedactJSON([]byte(s))
	})
}
