package testkit

import (
	"bufio"
	"bytes"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// Finding is one rule that matched in a scanned file. It never holds the
// matched value, so CI logs cannot leak it.
type Finding struct {
	Path string
	Line int
	Rule string
}

var (
	scanAuthHeaderRe = regexp.MustCompile(`(?i)\b(proxy-authorization|authorization|set-cookie|cookie)"?\s*[:=]\s*(.*)$`)
	scanSchemeRe     = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9\-._~+/]{8,}=*)`)
	scanPDTokenRe    = regexp.MustCompile(`(?i)\btoken\s+token=([^\s"',;\]]+)`)
	scanCalendarRe   = regexp.MustCompile(`(?i)\b(?:webcal|https?)://[^\s"'<>]*pagerduty\.com/private/[^\s"'<>]*`)
	scanPhoneRe      = regexp.MustCompile(`\+\d(?:[ .\-()]{0,2}\d){7,14}`)
	// A '+' number counts as masked when its digits end in at least six zeros
	// followed by at most two other digits, because the length of its country
	// code is not known.
	scanMaskedPlusRe = regexp.MustCompile(`0{6,}\d{0,2}$`)
	nonDigitRe       = regexp.MustCompile(`\D`)
)

var (
	authHeaderNames = map[string]bool{"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true}
	// PagerDuty contact method types whose address is a phone number.
	pdPhoneTypes = map[string]bool{"phone_contact_method": true, "sms_contact_method": true, "whatsapp_contact_method": true}
	// JSM contact and notification-rule step methods whose to is a phone number.
	jsmPhoneMethods = map[string]bool{"sms": true, "voice": true}
)

// ScanSensitive checks one file for credentials and unmasked phone numbers.
// Every file is scanned as text. JSON and YAML are also parsed (every
// document of a multi-document file, and any file that starts with '{' or
// '[' whatever its extension), and so is JSON held in a string, such as a
// go-vcr cassette's response body, so values split over several lines or
// fields are checked too. Dates, timestamps, time zone offsets, IDs and email
// addresses are not flagged.
func ScanSensitive(path string, data []byte) []Finding {
	var out []Finding
	add := func(line int, rule string) { out = append(out, Finding{Path: path, Line: line, Rule: rule}) }

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		scanLine(sc.Text(), func(rule string) { add(n, rule) })
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".yaml", ".yml":
		walkDocuments(data, 0, add)
	default:
		if looksLikeJSON(string(data)) {
			walkDocuments(data, 0, add)
		}
	}
	return dedupe(out)
}

// walkDocuments parses data as YAML, a superset of JSON whose parser keeps
// line numbers, document by document. lineOffset places a document found
// inside a string at that string's line.
func walkDocuments(data []byte, lineOffset int, add func(int, string)) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			return
		}
		walkNode(&doc, lineOffset, add)
	}
}

func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

func scanLine(line string, add func(rule string)) {
	if m := scanAuthHeaderRe.FindStringSubmatch(line); m != nil && hasValue(m[2]) {
		add("authorization-header")
	}
	for _, m := range scanSchemeRe.FindAllStringSubmatch(line, -1) {
		if !strings.HasPrefix(m[1], credentials.Redacted) {
			add("credential-scheme")
		}
	}
	for _, m := range scanPDTokenRe.FindAllStringSubmatch(line, -1) {
		if m[1] != credentials.Redacted {
			add("pagerduty-token")
		}
	}
	if scanCalendarRe.MatchString(line) {
		add("calendar-url")
	}
	for _, loc := range scanPhoneRe.FindAllStringIndex(line, -1) {
		m := line[loc[0]:loc[1]]
		if !scanMaskedPlusRe.MatchString(digitsOf(m)) && !notAPhone(line, loc[0], loc[1]) {
			add("phone-number")
		}
	}
}

var (
	plusDateRe     = regexp.MustCompile(`^\+\d{4}-\d{2}-\d{2}`)
	fractionSecsRe = regexp.MustCompile(`\.\d{6,}`)
)

// notAPhone recognizes '+' numbers that are timestamps: Go's monotonic clock
// reading (m=+0.000123456), a fraction of a second, or a '+'-joined date.
func notAPhone(line string, start, end int) bool {
	m := line[start:end]
	return strings.HasSuffix(line[:start], "m=") || plusDateRe.MatchString(m) || fractionSecsRe.MatchString(m)
}

// hasValue reports whether a header value written as text holds anything but
// "[redacted]", quotes and list punctuation. A YAML header whose values sit on
// the next lines has none here; the YAML walk checks it.
func hasValue(s string) bool {
	return strings.ContainsFunc(strings.ReplaceAll(s, credentials.Redacted, ""), func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

func walkNode(n *yaml.Node, off int, add func(line int, rule string)) {
	at := func(line int, rule string) { add(line+off, rule) }
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walkNode(c, off, add)
		}
	case yaml.MappingNode:
		fields := map[string]*yaml.Node{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			name := strings.ToLower(key.Value)
			fields[name] = val
			switch {
			case authHeaderNames[name]:
				checkScalars(val, "authorization-header", at)
			case credentials.IsSecretKey(name):
				checkScalars(val, "secret-key", at)
			}
			walkNode(val, off, add)
		}
		checkContact(fields, at)
	case yaml.ScalarNode:
		if looksLikeJSON(n.Value) {
			walkDocuments([]byte(n.Value), n.Line-1+off, add)
		}
	}
}

// checkScalars flags a scalar, or the scalars of a list, that are neither
// empty nor redacted.
func checkScalars(n *yaml.Node, rule string, add func(int, string)) {
	switch n.Kind {
	case yaml.ScalarNode:
		if v := strings.TrimSpace(n.Value); v != "" && v != credentials.Redacted && n.Tag != "!!null" {
			add(n.Line, rule)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			checkScalars(c, rule, add)
		}
	}
}

// checkContact flags a PagerDuty phone, SMS or WhatsApp contact method whose
// address is not masked, and a JSM sms or voice contact or step whose to is
// not masked. JSM writes numbers as {country_code}-{number}, so only the
// digits after the first hyphen must be masked there.
func checkContact(fields map[string]*yaml.Node, add func(int, string)) {
	if t, a := fields["type"], fields["address"]; t != nil && a != nil && a.Kind == yaml.ScalarNode &&
		pdPhoneTypes[strings.ToLower(t.Value)] && !maskedDigits(digitsOf(a.Value)) {
		add(a.Line, "phone-number")
	}
	if m, to := fields["method"], fields["to"]; m != nil && to != nil && to.Kind == yaml.ScalarNode &&
		jsmPhoneMethods[strings.ToLower(m.Value)] {
		number := to.Value
		if i := strings.IndexByte(number, '-'); i >= 0 {
			number = number[i+1:]
		}
		if !maskedDigits(digitsOf(number)) {
			add(to.Line, "phone-number")
		}
	}
}

// maskedDigits reports whether every digit is 0 except at most the last two.
func maskedDigits(d string) bool {
	if len(d) <= 2 {
		return true
	}
	return strings.Trim(d[:len(d)-2], "0") == ""
}

func digitsOf(s string) string { return nonDigitRe.ReplaceAllString(s, "") }

func dedupe(in []Finding) []Finding {
	seen := map[Finding]bool{}
	var out []Finding
	for _, f := range in {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}
