package credentials_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"

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
	// Ordinary text with hyphens and colons stays.
	for _, keep := range []string{"plan v3 created on 2026-10-27", "PABC123 maps to team-42", "https://example.com:8443/path"} {
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
