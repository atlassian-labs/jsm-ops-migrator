package review

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// The review files refuse sensitive data in what they cannot redact: keys,
// references and payloads, which apply acts on as they are. Texts are
// redacted instead (messages.Clean).
//
// The redactor's rules are built to catch too much rather than too little,
// which suits redaction but not a refusal, so screen leaves out the matches
// that ordinary data makes:
//   - A UUID whose last group is all digits matches the JSM phone number
//     pattern, and JSM IDs are UUIDs, so UUIDs are taken out first.
//   - "Basic Schedule" matches the credential scheme rule. A credential in a
//     payload sits under a key such as token or authorization, which the key
//     rule catches, so the scheme rule is left out.

var uuidRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// screen returns the names of the sensitive-data rules s matches.
func screen(s string) []string {
	rules := credentials.DetectSensitive(uuidRe.ReplaceAllString(s, "uuid"))
	return slices.DeleteFunc(rules, func(r string) bool { return r == "credential_scheme" })
}

// payloadRules returns the rules a JSON payload matches, read value by value:
// every object key, at any depth, is checked by name, and every string is
// screened as the text it decodes to. Reading the raw text instead misses a
// key inside a nested object, which the key rule takes as part of its
// parent's value, and a URL after a line break, which raw JSON writes as the
// two characters \n.
func payloadRules(payload []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	found := map[string]bool{}
	add := func(rules []string) {
		for _, r := range rules {
			found[r] = true
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if credentials.IsSensitiveKey(k) && !empty(val) {
					found["sensitive_key"] = true
				}
				add(screen(k))
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case string:
			add(screen(t))
		}
	}
	walk(v)
	rules := make([]string, 0, len(found))
	for r := range found {
		rules = append(rules, r)
	}
	slices.Sort(rules)
	return rules, nil
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
	if len(screen(s)) > 0 || len(screen(messages.OneLine(s))) > 0 {
		return withheld
	}
	return strconv.Quote(s)
}
