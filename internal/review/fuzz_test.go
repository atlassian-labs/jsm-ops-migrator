package review_test

import (
	"bytes"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// plan.json can be edited by hand before apply reads it: Decode must reject a
// bad file and never panic on one. A file it accepts is exactly what Encode
// writes for the plan it gives back, so what apply runs is what was hashed.
func FuzzDecode(f *testing.F) {
	data, err := review.Encode(samplePlan())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	// A text whose redaction is not stable in one pass.
	p := samplePlan()
	p.Messages[4].Text = "On-call line +1 415 555 0123https://acme.pagerduty.com/private/feed123/x.ics"
	if data, err = review.Encode(p); err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add([]byte(`{"version":1,"created_at":"2026-11-12T10:00:00Z","reads":[],"items":[],"messages":[]}`))
	f.Add([]byte(`{"version":1,"items":[{"key":"a/b","action":"create","depends_on":["a/b"]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := review.Decode(data)
		if err != nil {
			return
		}
		again, err := review.Encode(p)
		if err != nil {
			t.Fatalf("a plan Decode accepted does not encode: %v", err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("Decode accepted a file Encode does not write:\n%s\n%s", data, again)
		}
		h1, _ := review.ContentHash(p)
		back, _ := review.Decode(again)
		if h2, _ := review.ContentHash(back); h1 != h2 {
			t.Fatalf("the content hash changed on a round trip")
		}
	})
}

// The acceptance template is filled in by hand, so whatever a warning's ID
// and text hold, the template must read back with every ID intact.
func FuzzAcceptanceTemplate(f *testing.F) {
	f.Add("PT1", "member_dropped", "Casey is not migrated.")
	f.Add(`P"1#: x`, "a", "line\nbreak # not a comment")
	f.Add("Pü1\\", "z9", "")
	// YAML refuses a control character even in a comment.
	f.Add("0", "a", "nul\x00 and escape\x1b[2J")
	f.Fuzz(func(t *testing.T, id, code, text string) {
		p := review.Plan{Version: 1, Items: []review.Item{{Key: "teams/" + id, Action: review.Skip}},
			Messages: []messages.Message{{Class: messages.Warning, Code: code, Item: "teams/" + id, Text: text}}}
		if p.Check() != nil {
			return
		}
		data, err := review.Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		hash := review.Hash(data)
		filled := bytes.ReplaceAll(review.AcceptanceTemplate(p, hash), []byte(`reason: ""`), []byte(`reason: "ok"`))
		a, err := review.ParseAcceptance(filled)
		if err != nil {
			t.Fatalf("the template does not parse: %v\n%s", err, filled)
		}
		got, err := a.Accept(p, hash)
		if err != nil || len(got) != 1 || got[0].ID != p.Messages[0].ID() {
			t.Fatalf("Accept = %+v, %v\n%s", got, err, filled)
		}
	})
}
