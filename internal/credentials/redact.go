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
	// can read the schedule), so the whole URL goes. The scheme alone starts
	// the match, with no word boundary before it: in quoted text a line break
	// is the two characters \n, and its n would hide the URL after it.
	calendarURLRe = regexp.MustCompile(`(?i)(?:webcal|https?)://[^\s"'<>]*pagerduty\.com/private/[^\s"'<>]*`)
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
	// A key-value pair inside another key's value, in the forms a credential
	// takes there: key=value, "key": value, or key: "value". An unquoted value
	// stops at a space or a quote, so this never takes the rest of the line,
	// and one pass over a value finds every pair in it.
	nestedPairRe = regexp.MustCompile(`(?i)` +
		`(")([a-z][a-z0-9_\-]*)("\s*[:=]\s*)(` + nestedValue + `)` +
		`|()([a-z][a-z0-9_\-]*)(\s*=\s*)(` + nestedValue + `)` +
		`|()([a-z][a-z0-9_\-]*)(\s*:\s*)(` + quotedValue + `)`)
	// A JSM contact number, written as {country_code}-{number}. What comes
	// before it is matched instead of a word boundary, so a number after the
	// two characters \n of a quoted line break is found too.
	jsmPhoneRe = regexp.MustCompile(`(^|[^0-9A-Za-z_]|\\[nrt])(\d{1,4}-\d{6,14})\b`)
	// A UUID. Its last two groups can be all digits, which the JSM contact
	// number rule would otherwise take for a number.
	uuidRe = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// The user:password part of a URL.
	urlUserinfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/@\s:]+:[^/@\s]+@`)
	// A '+' followed by 8 to 15 digits, which may be separated by spaces,
	// dots, hyphens or parentheses.
	phoneRe = regexp.MustCompile(`\+\d(?:[ .\-()]{0,2}\d){7,14}`)
)

const (
	quotedValue = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`
	nestedValue = quotedValue + `|\[[^\]\r\n]*\]|\{[^}\r\n]*\}|[^\s,;&"'\[{][^\s,;&"']*`
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
	s = pairs(s)
	s = redactJSMPhones(s)
	return phoneRe.ReplaceAllString(s, Redacted)
}

// pairs redacts the value of every sensitive key-value pair in s. A match
// takes the whole value of its key, which can hold pairs of its own, as in
// "config: token=…", so the value of a key that is not sensitive is searched
// once more, with nestedPairRe: only the forms a credential takes there
// count, and prose such as "protected by a passphrase: set …" is not one.
// Each pass is linear, and the second never goes deeper, so a long line
// costs no more than twice a short one per byte.
func pairs(s string) string {
	return keyValueRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := keyValueRe.FindStringSubmatch(m)
		key, value := sub[2], sub[4]
		if isRedacted(value) {
			return m
		}
		if !IsSensitiveKey(key) {
			return sub[1] + key + sub[3] + nestedPairs(value)
		}
		return sub[1] + key + sub[3] + redactValue(value)
	})
}

// maxNestedDepth bounds how far nestedPairs looks into an object, list or
// quoted value inside a value, so the cost stays linear in the text.
const maxNestedDepth = 4

// nestedPairs redacts the sensitive pairs nestedPairRe finds in a value.
func nestedPairs(v string) string { return nestedPairsAt(v, 1) }

func nestedPairsAt(v string, depth int) string {
	return nestedPairRe.ReplaceAllStringFunc(v, func(m string) string {
		sub := nestedPairRe.FindStringSubmatch(m)
		// One of the three alternatives matched: its four groups are the
		// opening quote, the key, the separator and the value.
		for i := 1; i < len(sub); i += 4 {
			key, value := sub[i+1], sub[i+3]
			if key == "" {
				continue
			}
			switch {
			case isRedacted(value):
				return m
			case IsSensitiveKey(key):
				return sub[i] + key + sub[i+2] + redactValue(value)
			case depth < maxNestedDepth && strings.ContainsAny(value[:1], `{["'`):
				// An object, list or quoted text can hold pairs of its own.
				return sub[i] + key + sub[i+2] + nestedPairsAt(value, depth+1)
			}
			return m
		}
		return m
	})
}

func redactValue(value string) string {
	if q := value[:1]; q == `"` || q == "'" {
		return q + Redacted + q
	}
	return Redacted
}

// redactJSMPhones redacts every JSM contact number that is not part of a
// UUID.
func redactJSMPhones(s string) string {
	matches := jsmPhones(s)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	done := 0
	for _, m := range matches {
		b.WriteString(s[done:m[0]])
		b.WriteString(Redacted)
		done = m[1]
	}
	b.WriteString(s[done:])
	return b.String()
}

// jsmPhones returns the start and end of each JSM contact number in s that is
// not part of a UUID.
func jsmPhones(s string) [][2]int {
	uuids := uuidRe.FindAllStringIndex(s, -1)
	var out [][2]int
	for _, m := range jsmPhoneRe.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[4], m[5]
		inUUID := slices.ContainsFunc(uuids, func(u []int) bool { return start >= u[0] && end <= u[1] })
		if !inUUID {
			out = append(out, [2]int{start, end})
		}
	}
	return out
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
	add("phone_number", phoneRe.MatchString(s) || len(jsmPhones(s)) > 0)
	add("url_credentials", urlUserinfoRe.MatchString(strings.ReplaceAll(s, "://"+Redacted+"@", "://@")))
	return found
}

var (
	// strictSchemeRe is a Bearer or Basic credential: a token with a digit
	// or one of + / = . _ -, or a long one. "Basic Schedule" is not one.
	strictSchemeRe = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+(?:[A-Za-z0-9\-._~+/]*[0-9+/=._\-][A-Za-z0-9\-._~+/]*=*|[A-Za-z]{24,})`)
	// wholeJSMPhoneRe is a text that is nothing but a JSM contact number.
	wholeJSMPhoneRe = regexp.MustCompile(`^\s*\d{1,4}-\d{6,14}\s*$`)
)

// DetectSecrets returns the names of the rules that match s, like
// DetectSensitive, but only the rules precise enough to refuse data on.
// DetectSensitive is built for redaction, where catching too much is safe;
// refusing a plan for ordinary prose is not. So here:
//   - a key-value pair counts only in the forms a credential takes inside
//     text (key=value or quoted), and "Escalate to: Tier 2" is not one;
//   - a Bearer or Basic scheme counts only with a credential-shaped token;
//   - a JSM contact number counts when it is the whole text, or has at
//     least seven digits after its '-', because "Handles 100-200000
//     requests" and "Ticket 2024-123456" have its shape;
//   - an Authorization header written as prose does not count; a real one
//     holds a scheme, which does.
func DetectSecrets(s string) []string {
	var found []string
	add := func(rule string, ok bool) {
		if ok {
			found = append(found, rule)
		}
	}
	plain := stripRedacted(s)
	add("calendar_url", calendarURLRe.MatchString(s))
	add("credential_scheme", strictSchemeRe.MatchString(plain))
	add("pagerduty_token", pdTokenRe.MatchString(plain))
	add("sensitive_key", nestedPairs(s) != s)
	add("phone_number", phoneRe.MatchString(s) || wholeJSMPhoneRe.MatchString(s) || hasLongJSMPhone(s))
	add("url_credentials", urlUserinfoRe.MatchString(strings.ReplaceAll(s, "://"+Redacted+"@", "://@")))
	return found
}

// hasLongJSMPhone reports whether s holds a JSM contact number with at least
// seven digits after its '-'.
func hasLongJSMPhone(s string) bool {
	for _, m := range jsmPhones(s) {
		number := s[m[0]:m[1]]
		if _, digits, _ := strings.Cut(number, "-"); len(digits) >= 7 {
			return true
		}
	}
	return false
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

// hasSensitiveKeyValue reports whether s holds a pair pairs would redact, so
// detection and redaction can never disagree.
func hasSensitiveKeyValue(s string) bool { return pairs(s) != s }

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
