package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var (
	// PagerDuty calendar feed URLs are bearer credentials (anyone holding one
	// can read the schedule), so the whole URL goes.
	calendarURLRe = regexp.MustCompile(`(?i)\b(?:webcal|https?)://[^\s"'<>]*pagerduty\.com/private/[^\s"'<>]*`)
	// An Authorization, Proxy-Authorization, Cookie or Set-Cookie header
	// written as text: the value runs to the end of the line, because a
	// cookie header holds several secrets separated by ';'.
	authHeaderRe = regexp.MustCompile(`(?i)\b(proxy-authorization|authorization|set-cookie|cookie)("?\s*[:=]\s*)([^\r\n]+)`)
	// Credentials that appear without their header name.
	schemeRe  = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)[A-Za-z0-9\-._~+/]{8,}=*`)
	pdTokenRe = regexp.MustCompile(`(?i)\b(token\s+token=)[^\s"',;]+`)
	// key: value, key=value and "key": "value" forms. The key decides whether
	// the value is redacted (see IsSensitiveKey). A value is a quoted string,
	// a one-level list or object, or unquoted text up to the end of the line or
	// the next '&', ',' or ';', so a passphrase with spaces goes whole.
	keyValueRe = regexp.MustCompile(`(?i)("?)([a-z][a-z0-9_\-]*)("?\s*[:=]\s*)` +
		`("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|\[[^\]\r\n]*\]|\{[^}\r\n]*\}|[^\s,;&"'\[{][^\r\n,;&]*)`)
	// A JSM contact number, written as {country_code}-{number}.
	jsmPhoneRe = regexp.MustCompile(`\b\d{1,4}-\d{6,14}\b`)
	// The user:password part of a URL.
	urlUserinfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/@\s:]+:[^/@\s]+@`)
	// A '+' followed by 8 to 15 digits, which may be separated by spaces,
	// dots, hyphens or parentheses.
	phoneRe = regexp.MustCompile(`\+\d(?:[ .\-()]{0,2}\d){7,14}`)
)

// Known secret values shorter than this are not searched for, so a short
// value cannot blank out unrelated text.
const minKnownSecretLen = 8

// Redactor removes credentials and personal contact data from text and JSON
// before they reach a log, an error message or a review file.
//
// The zero value and a nil *Redactor apply the pattern rules. NewRedactor adds
// known secret values, which are removed wherever they appear.
type Redactor struct{ known []string }

// NewRedactor returns a Redactor that also removes the given secret values.
func NewRedactor(known ...Secret) *Redactor {
	r := &Redactor{}
	for _, s := range known {
		if v := s.Reveal(); len(v) >= minKnownSecretLen {
			r.known = append(r.known, v)
		}
	}
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(r.known, func(i, j int) bool { return len(r.known[i]) > len(r.known[j]) })
	return r
}

// RedactText applies the pattern rules to a string.
func RedactText(s string) string { return (*Redactor)(nil).String(s) }

// HasCalendarURL reports whether s holds a PagerDuty calendar feed URL.
func HasCalendarURL(s string) bool { return calendarURLRe.MatchString(s) }

// RedactCalendarURLs replaces every PagerDuty calendar feed URL in s with
// Redacted and leaves the rest of s as it is.
func RedactCalendarURLs(s string) string { return calendarURLRe.ReplaceAllString(s, Redacted) }

// RedactJSON applies the pattern rules to a JSON document.
func RedactJSON(b []byte) []byte { return (*Redactor)(nil).JSON(b) }

// String redacts a string.
func (r *Redactor) String(s string) string {
	if r != nil {
		for _, k := range r.known {
			s = strings.ReplaceAll(s, k, Redacted)
		}
	}
	s = calendarURLRe.ReplaceAllString(s, Redacted)
	s = urlUserinfoRe.ReplaceAllString(s, "${1}"+Redacted+"@")
	s = authHeaderRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := authHeaderRe.FindStringSubmatch(m)
		if isRedacted(strings.TrimSpace(sub[3])) {
			return m
		}
		return sub[1] + sub[2] + Redacted
	})
	s = schemeRe.ReplaceAllString(s, "${1}${2}"+Redacted)
	s = pdTokenRe.ReplaceAllString(s, "${1}"+Redacted)
	s = r.pairs(s, false)
	s = jsmPhoneRe.ReplaceAllString(s, Redacted)
	return phoneRe.ReplaceAllString(s, Redacted)
}

// pairs redacts the value of every sensitive key-value pair in s. A match
// takes the whole value of its key, so the value of a key that is not
// sensitive is searched for pairs of its own, as in "config: token=…".
// Inside a value (nested), only the forms a credential takes count: key=value
// and quoted pairs. Prose such as "protected by a passphrase: set …" is not
// one. Each value is shorter than its match, so the search ends.
func (r *Redactor) pairs(s string, nested bool) string {
	return keyValueRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := keyValueRe.FindStringSubmatch(m)
		key, value := sub[2], sub[4]
		if isRedacted(value) {
			return m
		}
		if !IsSensitiveKey(key) || (nested && !strictPair(sub)) {
			return sub[1] + key + sub[3] + r.pairs(value, true)
		}
		if q := value[:1]; q == `"` || q == "'" {
			return sub[1] + key + sub[3] + q + Redacted + q
		}
		return sub[1] + key + sub[3] + Redacted
	})
}

// strictPair reports whether a keyValueRe match is written as a credential
// is: with '=', or with its key or value quoted.
func strictPair(sub []string) bool {
	return strings.Contains(sub[3], "=") || sub[1] == `"` || strings.HasPrefix(sub[4], `"`) || strings.HasPrefix(sub[4], "'")
}

// isRedacted reports whether a value holds nothing but "[redacted]" and
// punctuation.
func isRedacted(value string) bool {
	rest := strings.ReplaceAll(value, Redacted, "")
	return rest != value && strings.Trim(rest, ` "'[]{},`) == ""
}

// JSON redacts a JSON document: the value of every sensitive key, at any
// depth, becomes "[redacted]", and every other string passes through String.
// Input that is not a single JSON value is redacted as text instead.
func (r *Redactor) JSON(b []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return []byte(r.String(string(b)))
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return []byte(r.String(string(b)))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r.walk(v)); err != nil {
		return []byte(r.String(string(b)))
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (r *Redactor) walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if IsSensitiveKey(k) && val != nil && val != "" {
				t[k] = Redacted
				continue
			}
			t[k] = r.walk(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = r.walk(t[i])
		}
		return t
	case string:
		return r.String(t)
	default:
		return v
	}
}

// DetectSensitive returns the names of the rules that match s, never the
// matched values, so callers can reject or report content without leaking it.
func DetectSensitive(s string) []string {
	var found []string
	add := func(rule string, ok bool) {
		if ok {
			found = append(found, rule)
		}
	}
	add("calendar_url", calendarURLRe.MatchString(s))
	add("authorization", hasUnredactedAuthHeader(s))
	add("credential_scheme", schemeRe.MatchString(stripRedacted(s)))
	add("pagerduty_token", pdTokenRe.MatchString(stripRedacted(s)))
	add("sensitive_key", hasSensitiveKeyValue(s))
	add("phone_number", phoneRe.MatchString(s) || jsmPhoneRe.MatchString(s))
	add("url_credentials", urlUserinfoRe.MatchString(strings.ReplaceAll(s, "://"+Redacted+"@", "://@")))
	return found
}

func stripRedacted(s string) string { return strings.ReplaceAll(s, Redacted, "") }

func hasUnredactedAuthHeader(s string) bool {
	for _, sub := range authHeaderRe.FindAllStringSubmatch(s, -1) {
		if !isRedacted(strings.TrimSpace(sub[3])) {
			return true
		}
	}
	return false
}

func hasSensitiveKeyValue(s string) bool {
	return hasSensitivePair(s, false)
}

// hasSensitivePair is hasSensitiveKeyValue with the rules of pairs.
func hasSensitivePair(s string, nested bool) bool {
	for _, sub := range keyValueRe.FindAllStringSubmatch(s, -1) {
		if isRedacted(sub[4]) {
			continue
		}
		if IsSensitiveKey(sub[2]) && (!nested || strictPair(sub)) {
			return true
		}
		if hasSensitivePair(sub[4], true) {
			return true
		}
	}
	return false
}

// RedactAttr is a slog ReplaceAttr function. It replaces the value of every
// sensitive key, including every value inside a group with a sensitive name,
// and redacts every other value: strings and the log message as text, and
// errors, byte slices, Stringers, maps and structs after turning them into
// text or JSON. slog would otherwise print those with %+v, unredacted.
func RedactAttr(groups []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) || slices.ContainsFunc(groups, IsSensitiveKey) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactText(a.Value.String()))
	case slog.KindAny:
		return slog.String(a.Key, redactAny(a.Value.Any()))
	}
	return a
}

func redactAny(v any) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case error:
		return RedactText(t.Error())
	case []byte:
		return string(RedactJSON(t))
	case fmt.Stringer:
		return RedactText(t.String())
	}
	text := fmt.Sprintf("%+v", v)
	// JSON keeps keys, so sensitive keys are found, but it splits values such
	// as a URL into fields. When the text form shows a URL credential,
	// calendar URL or phone number, the redacted text form is used instead.
	if slices.ContainsFunc(DetectSensitive(text), func(rule string) bool {
		return rule == "calendar_url" || rule == "url_credentials" || rule == "phone_number"
	}) {
		return RedactText(text)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(RedactJSON(b))
	}
	return RedactText(text)
}
