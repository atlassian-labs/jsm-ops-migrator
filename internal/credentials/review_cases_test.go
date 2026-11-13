package credentials_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// The inputs the security review showed leaking through the first redactor.
func TestRedactTextWholeValues(t *testing.T) {
	cases := []struct{ in, leak string }{
		{"password=correct horse battery staple", "horse battery staple"},
		{"address: 555 111 2233", "111 2233"},
		{"phone: (555) 111-2233", "111-2233"},
		{`{"to":["90-5551112233"]}`, "5551112233"},
		{`"to": {"value": "90-5551112233"}`, "5551112233"},
		{"Cookie: a=1; JSESSIONID=cookiesecret1", "cookiesecret1"},
		{`"Set-Cookie":["JSESSIONID=abcdef123456"]`, "abcdef123456"},
		{"token: 'abc def ghi'", "def ghi"},
		{"contact 90-5551112233 already exists", "5551112233"},
		{"proxy https://user:proxysecret@proxy.example.com:8080", "proxysecret"},
		// A pair inside the value of a key that is not sensitive.
		{"Webhook config: token=abcdef1234567890", "abcdef1234567890"},
		{"settings: api_key=k123456789 and more", "k123456789"},
		{"a: b: password=nested-secret-1", "nested-secret-1"},
	}
	for _, c := range cases {
		out := credentials.RedactText(c.in)
		if strings.Contains(out, c.leak) {
			t.Errorf("RedactText(%q) = %q, leaks %q", c.in, out, c.leak)
		}
		if got := credentials.DetectSensitive(out); len(got) != 0 {
			t.Errorf("RedactText(%q) = %q, still flagged %v", c.in, out, got)
		}
		if got := credentials.DetectSensitive(c.in); len(got) == 0 {
			t.Errorf("DetectSensitive(%q) found nothing", c.in)
		}
	}
	// Ordinary text with hyphens and colons stays, and so does a sentence
	// that names a credential inside another key's value.
	for _, keep := range []string{"plan v3 created on 2026-10-27", "PABC123 maps to team-42", "https://example.com:8443/path",
		"state.db's key is not available: this workspace is protected by a passphrase: set JSM_OPS_MIGRATOR_PASSPHRASE"} {
		if got := credentials.RedactText(keep); got != keep {
			t.Errorf("RedactText(%q) = %q, want unchanged", keep, got)
		}
	}
}

type contact struct {
	Type    string
	Address string
}

type feedHolder struct{ Feed *url.URL }

func TestRedactAttrValuesOfEveryKind(t *testing.T) {
	feed, _ := url.Parse("https://acme.pagerduty.com/private/feedsecret123456/feed")
	for _, newHandler := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler {
			return slog.NewTextHandler(b, &slog.HandlerOptions{ReplaceAttr: credentials.RedactAttr})
		},
		func(b *bytes.Buffer) slog.Handler {
			return slog.NewJSONHandler(b, &slog.HandlerOptions{ReplaceAttr: credentials.RedactAttr})
		},
	} {
		var buf bytes.Buffer
		log := slog.New(newHandler(&buf))
		log.Warn("x",
			"headers", map[string][]string{"Authorization": {"Token token=pdtokensecret1"}},
			"body", []byte(`{"token":"bodysecret99"}`),
			"feed", feed,
			"holder", feedHolder{Feed: feed},
			"contact", contact{Type: "sms_contact_method", Address: "5551112233"},
			slog.Group("phone", "number", "5551112233"),
		)
		log.WithGroup("device").Warn("y", "model", "pixel-secret-model")
		out := buf.String()
		for _, leak := range []string{"pdtokensecret1", "bodysecret99", "feedsecret123456", "5551112233", "pixel-secret-model"} {
			if strings.Contains(out, leak) {
				t.Errorf("leaked %q:\n%s", leak, out)
			}
		}
		if !strings.Contains(out, "sms_contact_method") {
			t.Errorf("redacted too much:\n%s", out)
		}
	}
}

func FuzzSecretNeverPrinted(f *testing.F) {
	for _, s := range []string{"tok-123456", "x", "[redacted]", "credentials.Secret"} {
		f.Add(s)
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p", "%10.3s"}
	render := func(s credentials.Secret) string {
		var b strings.Builder
		for _, v := range verbs {
			b.WriteString(fmt.Sprintf(v, s) + fmt.Sprintf(v, struct{ s credentials.Secret }{s}) + fmt.Sprintf(v, &s))
		}
		return b.String()
	}
	// Text every rendering contains whatever the value: the value cannot be
	// told apart from it, so those inputs prove nothing.
	// Pointer addresses vary between runs and print in hex and decimal, so
	// they are masked before looking for the value.
	addresses := regexp.MustCompile(`0[xX][0-9a-fA-F]+|[0-9a-fA-F]{6,}`)
	mask := func(s string) string { return addresses.ReplaceAllString(s, "#") }
	baseline := mask(render(credentials.NewSecret("")))
	f.Fuzz(func(t *testing.T, v string) {
		if len(v) < 3 || strings.Contains(v, "#") || addresses.MatchString(v) || strings.Contains(baseline, v) {
			t.Skip()
		}
		if out := mask(render(credentials.NewSecret(v))); strings.Contains(out, v) {
			t.Fatalf("value %q printed: %s", v, out)
		}
	})
}

// A line of pairs costs time in proportion to its length: the redactor once
// searched each value again, so a 32 KB line took seconds.
func TestRedactorTimeGrowsWithTheLength(t *testing.T) {
	for _, unit := range []string{"a=b ", "a:", "config: token=abcdef123 "} {
		took := func(n int) time.Duration {
			s := strings.Repeat(unit, n/len(unit))
			start := time.Now()
			credentials.RedactText(s)
			credentials.DetectSensitive(s)
			credentials.DetectSecrets(s)
			return time.Since(start)
		}
		small, large := took(64<<10), took(1<<20)
		// 16 times the text; quadratic growth would take about 256 times as long.
		if large > 80*small+50*time.Millisecond {
			t.Errorf("%q: 64 KB took %v, 1 MB took %v", unit, small, large)
		}
	}
}

func TestRedactTextAfterAnEscapedLineBreak(t *testing.T) {
	cases := []struct{ in, leak string }{
		{`Create schedule "Primary on-call\nhttps://acme.pagerduty.com/private/abc123feed/feed"`, "abc123feed"},
		{`The description "NOC contact:\n1-4155550123" is kept`, "4155550123"},
		{`tab\t90-5551112233`, "5551112233"},
		{`ishttps://acme.pagerduty.com/private/feed9/x.ics`, "feed9"},
	}
	for _, c := range cases {
		if out := credentials.RedactText(c.in); strings.Contains(out, c.leak) {
			t.Errorf("RedactText(%q) = %q, leaks %q", c.in, out, c.leak)
		}
		if len(credentials.DetectSensitive(c.in)) == 0 {
			t.Errorf("DetectSensitive(%q) found nothing", c.in)
		}
	}
}

// A UUID whose last two groups are all digits has the shape of a JSM contact
// number; about 1 random UUID in 2,000 was cut short.
func TestUUIDsAreNotPhoneNumbers(t *testing.T) {
	for _, s := range []string{
		"request 43a29c5c-3dbf-4fa4-8123-123456789012",
		"team 3f2a8c1e-7d44-4b5e-8001-000012345678 and account 712020:1b9d6bcd-bbfd-4b2d-9123-123456789012",
	} {
		if got := credentials.RedactText(s); got != s {
			t.Errorf("RedactText(%q) = %q", s, got)
		}
		if got := credentials.DetectSensitive(s); len(got) != 0 {
			t.Errorf("DetectSensitive(%q) = %v", s, got)
		}
	}
	// A number next to a UUID is still one.
	if got := credentials.RedactText("3f2a8c1e-7d44-4b5e-8001-000012345678 call 90-5551112233"); strings.Contains(got, "5551112233") {
		t.Errorf("the number next to the UUID leaked: %q", got)
	}
}

func TestCamelCaseKeysAreSensitive(t *testing.T) {
	for _, k := range []string{"accessToken", "clientSecret", "refreshToken", "routingKey", "integrationKey", "phoneNumber", "deviceId", "webCalUrl", "httpCalURL", "APIKey", "X-API-Key", "orgApiKey"} {
		if !credentials.IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false", k)
		}
	}
	for _, k := range []string{"toDate", "displayName", "adminAccountIds", "requestId", "emailAddress", "deviceType", "keyName"} {
		if credentials.IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true", k)
		}
	}
	out := string(credentials.RedactJSON([]byte(`{"accessToken":"tok123456789","phoneNumber":"4155550123","displayName":"Ops"}`)))
	if strings.Contains(out, "tok123456789") || strings.Contains(out, "4155550123") || !strings.Contains(out, "Ops") {
		t.Errorf("RedactJSON = %s", out)
	}
}

func TestNestedRedactionKeepsTheQuotes(t *testing.T) {
	in := `msg="token=abc123456" next=1`
	if got, want := credentials.RedactText(in), `msg="token=[redacted]" next=1`; got != want {
		t.Errorf("RedactText(%q) = %q, want %q", in, got, want)
	}
}

func TestDetectSecrets(t *testing.T) {
	secrets := map[string]string{
		"config: token=abcdef1234567890":                            "sensitive_key",
		`{"auth": {"password": "x"}}`:                               "sensitive_key",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abcdef":         "credential_scheme",
		"Basic QWxhZGRpbjpvcGVuIHNlc2FtZQ==":                        "credential_scheme",
		"call +1 415 555 0123":                                      "phone_number",
		"1-4155550123":                                              "phone_number",
		`calendar:\nhttps://acme.pagerduty.com/private/abc123/feed`: "calendar_url",
		"https://user:secretpw@example.com/x":                       "url_credentials",
		"Token token=abc123def456":                                  "pagerduty_token",
	}
	for in, rule := range secrets {
		if got := credentials.DetectSecrets(in); !slices.Contains(got, rule) {
			t.Errorf("DetectSecrets(%q) = %v, want %s", in, got, rule)
		}
	}
	for _, prose := range []string{
		"Escalate to: Tier 2 after 30 minutes", "Reports to: VP Engineering", "Address: Building 4, floor 2",
		"Authorization: handled by the SRE lead", "Handles 100-200000 requests a day", "Ticket 2024-123456 tracks this",
		"Basic Schedule", "Bearer responsibilities", "team 3f2a8c1e-7d44-4b5e-8001-000012345678",
		"this workspace is protected by a passphrase: set JSM_OPS_MIGRATOR_PASSPHRASE",
	} {
		if got := credentials.DetectSecrets(prose); len(got) != 0 {
			t.Errorf("DetectSecrets(%q) = %v, want nothing", prose, got)
		}
	}
}
