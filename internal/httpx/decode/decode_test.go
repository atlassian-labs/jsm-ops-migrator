package decode_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/decode"
)

// The shapes PagerDuty documents for escalation_rule_assignment_strategy.
func strategy(raw json.RawMessage) (int, *decode.Unknown) {
	var s string
	var o struct {
		Type string `json:"type"`
	}
	var a []json.RawMessage
	return decode.OneOf(raw,
		func(r json.RawMessage) error { return json.Unmarshal(r, &s) },
		func(r json.RawMessage) error {
			if err := json.Unmarshal(r, &o); err != nil || o.Type == "" {
				return errNoType
			}
			return nil
		},
		func(r json.RawMessage) error { return json.Unmarshal(r, &a) },
	)
}

var errNoType = errors.New("no type")

func TestOneOf(t *testing.T) {
	cases := map[string]int{
		`"round_robin"`:                 0,
		`{"type":"assign_to_everyone"}`: 1,
		`[{"type":"round_robin"}]`:      2,
	}
	for raw, want := range cases {
		if got, unknown := strategy(json.RawMessage(raw)); got != want || unknown != nil {
			t.Errorf("%s: got %d, %v; want %d", raw, got, unknown, want)
		}
	}
}

func TestOneOfKeepsAnUnknownValueRedacted(t *testing.T) {
	raw := json.RawMessage(`{"kind":"new","routing_key":"R0123456789abcdef","note":"call +44 20 7946 0958"}`)
	got, unknown := strategy(raw)
	if got != -1 || unknown == nil {
		t.Fatalf("got %d, %v; want -1 and the value", got, unknown)
	}
	s := string(unknown.JSON)
	if strings.Contains(s, "R0123456789abcdef") || strings.Contains(s, "7946") || !strings.Contains(s, `"kind":"new"`) {
		t.Errorf("the unknown value must be kept, with its secrets and phone numbers redacted: %s", s)
	}
}
