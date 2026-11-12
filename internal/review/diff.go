package review

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// Diff is what changed from one plan version to the next. Items are matched
// by key and messages by ID, which steps keep stable across versions.
type Diff struct {
	// From is the earlier version, or 0 when there is none.
	From, To int
	Added    []Item
	Removed  []Item
	Changed  []Change
	// NewMessages are raised by To and not by From; GoneMessages the other
	// way round. A message raised by both whose class or text differs is in
	// ChangedMessages: a warning someone accepted before may now say more.
	NewMessages     []messages.Message
	GoneMessages    []messages.Message
	ChangedMessages []MessageChange
	// ItemsReordered and MessagesReordered report that the items or the
	// messages both versions hold are in another order. The order of the
	// items is the order apply takes them in, so it is a change to review.
	ItemsReordered, MessagesReordered bool
}

// Change is an item in both versions that differs.
type Change struct {
	Before, After Item
	// Fields names what differs: action, summary, depends_on, pagerduty, jsm
	// or payload.
	Fields []string
}

// MessageChange is a message both versions raise, with another class or text.
type MessageChange struct {
	Before, After messages.Message
}

// Empty reports whether nothing changed. It agrees with ContentHash: two
// plans with the same content hash have an empty diff, and two without do
// not.
func (d Diff) Empty() bool {
	return len(d.Added)+len(d.Removed)+len(d.Changed)+len(d.NewMessages)+len(d.GoneMessages)+len(d.ChangedMessages) == 0 &&
		!d.ItemsReordered && !d.MessagesReordered
}

// Compare returns the diff from prev to next; prev is nil for the first
// version. Both are compared as plan.json holds them, so a version read back
// from its file compares equal to the plan it was written from.
func Compare(prev *Plan, next Plan) Diff {
	d := Diff{To: next.Version}
	if prev == nil {
		return d
	}
	p := normalize(*prev)
	prev, next = &p, normalize(next)
	d.From = prev.Version
	before := map[string]Item{}
	for _, it := range prev.Items {
		before[it.Key] = it
	}
	after := map[string]bool{}
	for _, it := range next.Items {
		after[it.Key] = true
		old, ok := before[it.Key]
		if !ok {
			d.Added = append(d.Added, it)
			continue
		}
		if fields := changedFields(old, it); len(fields) > 0 {
			d.Changed = append(d.Changed, Change{Before: old, After: it, Fields: fields})
		}
	}
	for _, it := range prev.Items {
		if !after[it.Key] {
			d.Removed = append(d.Removed, it)
		}
	}
	d.NewMessages = missingFrom(next.Messages, prev.Messages)
	d.GoneMessages = missingFrom(prev.Messages, next.Messages)
	was := map[string]messages.Message{}
	for _, m := range prev.Messages {
		was[m.ID()] = m
	}
	for _, m := range next.Messages {
		if old, ok := was[m.ID()]; ok && (old.Class != m.Class || old.Text != m.Text) {
			d.ChangedMessages = append(d.ChangedMessages, MessageChange{Before: old, After: m})
		}
	}
	itemKey := func(it Item) string { return it.Key }
	d.ItemsReordered = reordered(prev.Items, next.Items, itemKey)
	d.MessagesReordered = reordered(prev.Messages, next.Messages, messages.Message.ID)
	return d
}

// reordered reports whether the entries both a and b hold are in another
// order in b.
func reordered[T any](a, b []T, id func(T) string) bool {
	inB := map[string]bool{}
	for _, e := range b {
		inB[id(e)] = true
	}
	inA := map[string]bool{}
	var commonA []string
	for _, e := range a {
		if inB[id(e)] {
			commonA = append(commonA, id(e))
		}
		inA[id(e)] = true
	}
	var commonB []string
	for _, e := range b {
		if inA[id(e)] {
			commonB = append(commonB, id(e))
		}
	}
	return !slices.Equal(commonA, commonB)
}

func changedFields(a, b Item) []string {
	var out []string
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("action", a.Action != b.Action)
	add("summary", a.Summary != b.Summary)
	add("depends_on", !slices.Equal(a.DependsOn, b.DependsOn))
	add("pagerduty", !sameRef(a.PagerDuty, b.PagerDuty))
	add("jsm", !sameRef(a.JSM, b.JSM))
	add("payload", !samePayload(a.Payload, b.Payload))
	return out
}

func sameRef(a, b *Ref) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// samePayload compares payloads as JSON, so a payload read back from the
// indented plan.json equals the one the step built.
func samePayload(a, b []byte) bool {
	ca, aok := compactJSON(a)
	cb, bok := compactJSON(b)
	if !aok || !bok {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca, cb)
}

// compactJSON returns b without insignificant spaces, and false when b is
// not JSON. An empty payload stays empty.
func compactJSON(b []byte) ([]byte, bool) {
	if len(b) == 0 {
		return nil, true
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// missingFrom returns the messages of a whose ID is not in b.
func missingFrom(a, b []messages.Message) []messages.Message {
	ids := map[string]bool{}
	for _, m := range b {
		ids[m.ID()] = true
	}
	var out []messages.Message
	for _, m := range a {
		if !ids[m.ID()] {
			out = append(out, m)
		}
	}
	return out
}
