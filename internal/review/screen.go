package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// The review files refuse sensitive data in what they cannot redact: keys,
// references and payloads, which apply acts on as they are. Texts are
// redacted instead (messages.Clean).
//
// The redactor's rules are built to catch too much rather than too little,
// which suits redaction but not a refusal, so a refusal uses the precise ones
// (credentials.DetectSecrets). An ID is screened as a whole and by its parts,
// so "contacts/1-4155550123" is a phone number.

// idPartRe splits an ID into the parts a step builds it from.
var idPartRe = regexp.MustCompile(`[/:?&#]`)

// screenID returns the names of the sensitive-data rules an ID matches.
func screenID(s string) []string {
	rules := credentials.DetectSecrets(s)
	for _, part := range idPartRe.Split(s, -1) {
		rules = append(rules, credentials.DetectSecrets(part)...)
	}
	slices.Sort(rules)
	return slices.Compact(rules)
}

// finding is sensitive data found in a payload: the rules, and the path of
// the value that holds it, such as "payload.contact.to".
type finding struct {
	path  string
	rules []string
}

// errNotOneJSONValue means a payload is not a single JSON value.
var errNotOneJSONValue = errors.New("payload is not one JSON value")

// payloadFindings reads a payload value by value. Every object key is
// checked by its name and every string, and every number, as the text it
// decodes to. Reading the raw text instead misses a key inside a nested
// object and a URL after an escaped line break. A key given twice is refused
// too: the decoder keeps the last value, so a screen of the decoded value
// would pass the first, which plan.json keeps.
func payloadFindings(payload []byte) ([]finding, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var out []finding
	if err := walkJSON(dec, "payload", &out); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errNotOneJSONValue
	}
	return out, nil
}

// walkJSON reads one JSON value from dec, at path, and adds what it finds.
func walkJSON(dec *json.Decoder, path string, out *[]finding) error {
	tok, err := dec.Token()
	if err != nil {
		return errNotOneJSONValue
	}
	add := func(p string, rules []string) {
		if len(rules) > 0 {
			*out = append(*out, finding{path: p, rules: rules})
		}
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return errNotOneJSONValue
				}
				key := kt.(string)
				p := path + "." + key
				if seen[key] {
					return fmt.Errorf("payload gives the key at %s twice", quote(p))
				}
				seen[key] = true
				add(p, screenID(key))
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return errNotOneJSONValue
				}
				if rule := keyRule(key, raw); rule != "" {
					add(p, []string{rule})
				}
				inner := json.NewDecoder(bytes.NewReader(raw))
				inner.UseNumber()
				if err := walkJSON(inner, p, out); err != nil {
					return err
				}
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := walkJSON(dec, fmt.Sprintf("%s[%d]", path, i), out); err != nil {
					return err
				}
			}
		}
		if _, err := dec.Token(); err != nil {
			return errNotOneJSONValue
		}
	case string:
		add(path, credentials.DetectSecrets(t))
	case json.Number:
		add(path, credentials.DetectSecrets(t.String()))
	}
	return nil
}

// contactKeys hold a contact's address only when their value looks like one:
// "to" also names the end of a time window, as in {"from":"09:00","to":"17:00"}.
var contactKeys = map[string]bool{"to": true, "address": true}

// contactRe is what a contact address looks like: an email address, or at
// least seven digits.
var contactRe = regexp.MustCompile(`@|(?:\d[^\d]{0,3}){7}`)

// keyRule returns "sensitive_key" when a key's name says its value is a
// credential or contact data and the value is not empty.
func keyRule(key string, raw json.RawMessage) string {
	if !credentials.IsSensitiveKey(key) {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil || empty(v) {
		return ""
	}
	if contactKeys[strings.ToLower(strings.TrimSpace(key))] && !contactRe.Match(raw) {
		return ""
	}
	return "sensitive_key"
}

func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// withheld stands in for a value an error must not show.
const withheld = "(withheld: it holds sensitive data)"

// quote is s as an error message shows it: quoted, so a control character
// cannot reach the terminal, and withheld when it holds sensitive data, so
// the error never shows what it refuses.
func quote(s string) string {
	// The one-line form too: a line break can hide a phone number.
	if len(screenID(s)) > 0 || len(screenID(messages.OneLine(s))) > 0 ||
		len(credentials.DetectSensitive(messages.OneLine(s))) > 0 {
		return withheld
	}
	return strconv.Quote(s)
}
