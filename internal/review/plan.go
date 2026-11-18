package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// Action is what apply does with an item.
type Action string

const (
	// Create makes a call that creates the JSM object.
	Create Action = "create"
	// Update makes a call that changes the JSM object the item names.
	Update Action = "update"
	// Map links the PagerDuty object to the JSM object that already exists.
	// apply records the link and makes no call.
	Map Action = "map"
	// Skip leaves the object out; the item's messages say why.
	Skip Action = "skip"
	// Manual is done by the operator by hand; the item's messages say how.
	Manual Action = "manual"
)

// Actions are every action, in the order reports list them.
var Actions = []Action{Create, Update, Map, Skip, Manual}

// Known reports whether a is one of the actions.
func (a Action) Known() bool {
	for _, k := range Actions {
		if a == k {
			return true
		}
	}
	return false
}

// Calls reports whether apply makes a call for an item with this action.
func (a Action) Calls() bool { return a == Create || a == Update }

// Ref names an object in PagerDuty or JSM.
type Ref struct {
	// Type is the kind of object, such as "user" or "team".
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (r Ref) String() string { return r.Type + " " + r.ID }

// Item is one entry of the plan: one call apply makes, or one object that
// needs none.
type Item struct {
	// Key is "<step>/<id>", built from PagerDuty IDs, so it is the same in
	// every plan version and the diff can follow the item.
	Key     string `json:"key"`
	Action  Action `json:"action"`
	Summary string `json:"summary"`
	// DependsOn are the keys of the items apply must take first. Each comes
	// earlier in the plan.
	DependsOn []string `json:"depends_on,omitempty"`
	PagerDuty *Ref     `json:"pagerduty,omitempty"`
	// JSM is the object an update changes or a map links to.
	JSM *Ref `json:"jsm,omitempty"`
	// Payload is the step's own input for the call. It is never redacted,
	// because apply sends what it says, so a payload that holds a secret or
	// contact data is refused instead.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Step is the name of the step that planned the item.
func (it Item) Step() string {
	step, _, _ := strings.Cut(it.Key, "/")
	return step
}

// Side is one of the two systems a plan reads.
type Side string

const (
	PagerDuty Side = "pagerduty"
	JSM       Side = "jsm"
)

// Label is the side's name in reports.
func (s Side) Label() string {
	switch s {
	case PagerDuty:
		return "PagerDuty"
	case JSM:
		return "JSM"
	}
	return string(s)
}

// Read is one read of a side that the plan was built from.
type Read struct {
	Side     Side      `json:"side"`
	At       time.Time `json:"at"`
	Complete bool      `json:"complete"`
}

// Plan is one plan version: what apply does, in the order it does it, and
// every message the steps raised.
type Plan struct {
	Version   int                `json:"version"`
	CreatedAt time.Time          `json:"created_at"`
	Reads     []Read             `json:"reads"`
	Items     []Item             `json:"items"`
	Messages  []messages.Message `json:"messages"`
}

var (
	// keyRe keeps a key usable as a Markdown code span and a YAML scalar,
	// and safe to print in a terminal: no spaces, backticks, control
	// characters or invisible format characters, so two keys that look the
	// same are the same. Keys must also be valid UTF-8 (validKey).
	keyRe = regexp.MustCompile("^[a-z][a-z0-9_]*/[^\\s`\\p{Cc}\\p{Cf}\\x{FFFE}\\x{FFFF}]+$")
	refRe = regexp.MustCompile("^[^\\s`\\p{Cc}\\p{Cf}\\x{FFFE}\\x{FFFF}]+$")
)

// validKey reports whether k is a well-formed item key. The pattern alone
// would take invalid UTF-8, which JSON then changes on the way out.
func validKey(k string) bool { return keyRe.MatchString(k) && utf8.ValidString(k) }

// Check reports everything that makes p unfit to be applied or shown, all
// at once. An error never shows a value that holds sensitive data (quote).
func (p Plan) Check() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if p.Version < 1 {
		bad("plan version %d: versions start at 1", p.Version)
	}
	incomplete := false
	for _, r := range p.Reads {
		if r.Side != PagerDuty && r.Side != JSM {
			bad("read of unknown side %s", quote(string(r.Side)))
		}
		incomplete = incomplete || !r.Complete
	}
	keys := make(map[string]bool, len(p.Items))
	for i, it := range p.Items {
		errs = append(errs, it.check(i, keys)...)
		keys[it.Key] = true
	}
	ids := map[string]bool{}
	blockers := 0
	for _, m := range p.Messages {
		if err := m.Check(); err != nil {
			errs = append(errs, fmt.Errorf("message %s: %w", quote(m.ID()), err))
		}
		if m.Item != "" && !keys[m.Item] {
			bad("message %s: no such item in the plan", quote(m.ID()))
		}
		if ids[m.ID()] {
			bad("message %s is raised twice", quote(m.ID()))
		}
		ids[m.ID()] = true
		if m.Class == messages.Blocker {
			blockers++
		}
	}
	// apply refuses only a plan with a blocker, so a plan built from a list
	// that was not read in full must carry one.
	if incomplete && blockers == 0 {
		bad("a read is incomplete, but the plan has no blocker")
	}
	return errors.Join(errs...)
}

// check checks the item at position i against the keys of the items before
// it. The item is named by its key, or by its position when the key itself is
// what the check refuses.
func (it Item) check(i int, before map[string]bool) []error {
	var errs []error
	name := fmt.Sprintf("item %d", i+1)
	if validKey(it.Key) && len(screenID(it.Key)) == 0 {
		name = "item " + strconv.Quote(it.Key)
	}
	// The name is an argument, never part of the format: a key can hold %.
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %w", name, fmt.Errorf(format, args...)))
	}
	if !validKey(it.Key) {
		bad("key must be <step>/<id>, without spaces, backticks or control characters")
	}
	if before[it.Key] {
		bad("planned twice")
	}
	if !it.Action.Known() {
		bad("unknown action %s", quote(string(it.Action)))
	}
	switch {
	case (it.Action == Map || it.Action == Update) && it.JSM == nil:
		bad("%s needs the JSM object", it.Action)
	case it.Action == Create && it.JSM != nil:
		bad("create cannot name an existing JSM object")
	}
	if it.Action == Map && it.PagerDuty == nil {
		bad("map needs the PagerDuty object it links")
	}
	seen := map[string]bool{}
	for _, d := range it.DependsOn {
		switch {
		case seen[d]:
			bad("depends on %s twice", quote(d))
		case !before[d]:
			bad("depends on %s, which is not earlier in the plan", quote(d))
		}
		seen[d] = true
	}
	for _, r := range []struct {
		side string
		ref  *Ref
	}{{"PagerDuty", it.PagerDuty}, {"JSM", it.JSM}} {
		if r.ref != nil && (!plainRef(r.ref.Type) || !plainRef(r.ref.ID)) {
			bad("its %s reference is not a plain type and ID, without spaces, backticks or control characters", r.side)
		}
	}
	var rules []string
	for _, s := range append([]string{it.Key}, it.DependsOn...) {
		rules = append(rules, screenID(s)...)
	}
	for _, r := range []*Ref{it.PagerDuty, it.JSM} {
		if r != nil {
			rules = append(rules, screenID(r.Type)...)
			rules = append(rules, screenID(r.ID)...)
		}
	}
	if len(rules) > 0 {
		slices.Sort(rules)
		// The rules only, never the value, so the error cannot leak it.
		bad("its key, a dependency or a reference holds sensitive data (%s)", strings.Join(slices.Compact(rules), ", "))
	}
	if len(it.Payload) > 0 {
		// A second value after the first would be dropped from plan.json.
		if !json.Valid(it.Payload) {
			bad("%w", errNotOneJSONValue)
			return errs
		}
		found, err := payloadFindings(it.Payload)
		switch {
		case err != nil:
			bad("%w", err)
		case len(found) > 0:
			var all []string
			for _, f := range found {
				all = append(all, f.rules...)
			}
			slices.Sort(all)
			where := quote(found[0].path)
			if len(found) > 1 {
				where += fmt.Sprintf(" and %d more", len(found)-1)
			}
			bad("its payload holds sensitive data (%s) at %s", strings.Join(slices.Compact(all), ", "), where)
		}
	}
	return errs
}

// plainRef reports whether s can be a reference's type or ID: valid UTF-8,
// without spaces, backticks or control characters.
func plainRef(s string) bool { return refRe.MatchString(s) && utf8.ValidString(s) }

// Encode returns plan.json, the plan as apply reads it. Summaries and message
// texts are redacted, and the same plan always gives the same bytes, so the
// hash of the file names exactly what was reviewed.
func Encode(p Plan) ([]byte, error) {
	if err := p.Check(); err != nil {
		return nil, err
	}
	return encodeJSON(normalize(p))
}

// ErrNotAsWritten means a plan.json is not byte for byte what Encode writes
// for the plan it holds: it was edited by hand or by another program.
var ErrNotAsWritten = errors.New("plan.json is not as jsm-ops-migrator wrote it")

// Decode reads plan.json strictly, because apply must run exactly the plan
// that was reviewed: an unknown field or a second value is an error, and so
// is a file laid out other than as Encode lays out the plan it holds. That
// last rule also refuses what the JSON decoder takes without a word, such as
// a key given twice, where the last one wins, or a key in other letter case.
//
// The texts are compared as they were read, not cleaned again: a later
// version of jsm-ops-migrator may clean them differently, and must still
// read the plan.json an earlier one wrote, for instance to resume a run. The
// hash recorded with the plan proves the file was not edited.
func Decode(data []byte) (Plan, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("reading plan.json: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Plan{}, errors.New("reading plan.json: more than one JSON value")
	}
	if err := p.Check(); err != nil {
		return Plan{}, fmt.Errorf("reading plan.json: %w", err)
	}
	p = canonical(p)
	again, err := encodeJSON(p)
	if err != nil {
		return Plan{}, fmt.Errorf("reading plan.json: %w", err)
	}
	if !bytes.Equal(again, data) {
		return Plan{}, ErrNotAsWritten
	}
	return p, nil
}

// Hash names the bytes of a review file: "sha256:<hex>".
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ContentHash names what a plan would do: its items and messages, without
// its version, its time or when the sides were read. Two plans with the same
// content hash are the same plan, so plan makes no new version for it.
func ContentHash(p Plan) (string, error) {
	if err := p.Check(); err != nil {
		return "", err
	}
	n := normalize(p)
	data, err := encodeJSON(struct {
		Items    []Item             `json:"items"`
		Messages []messages.Message `json:"messages"`
	}{n.Items, n.Messages})
	if err != nil {
		return "", err
	}
	return Hash(data), nil
}

// normalize returns a copy of p as plan.json holds it: canonical, with its
// texts cleaned (messages.Clean). It changes nothing in a plan it already
// returned, which keeps the hashes of a plan and of its plan.json read back
// the same.
func normalize(p Plan) Plan {
	p = canonical(p)
	for i := range p.Items {
		p.Items[i].Summary = messages.Clean(p.Items[i].Summary)
	}
	for i := range p.Messages {
		p.Messages[i] = p.Messages[i].Redacted()
	}
	return p
}

// canonical returns a copy of p laid out as plan.json lays it out, with its
// texts as they are: times in UTC, payloads compact, and empty lists as []
// rather than null. A payload that is not JSON stays as it is, never
// dropped; Check refuses it before anything is written.
func canonical(p Plan) Plan {
	p.CreatedAt = p.CreatedAt.UTC()
	reads := make([]Read, len(p.Reads))
	for i, r := range p.Reads {
		r.At = r.At.UTC()
		reads[i] = r
	}
	items := make([]Item, len(p.Items))
	for i, it := range p.Items {
		if compact, ok := compactJSON(it.Payload); ok {
			it.Payload = compact
		}
		items[i] = it
	}
	p.Reads, p.Items, p.Messages = reads, items, slices.Clone(p.Messages)
	if p.Messages == nil {
		p.Messages = []messages.Message{}
	}
	return p
}

func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Summary is a plan's counts, for the terminal, the JSON output and the menu.
type Summary struct {
	Version int          `json:"version"`
	Steps   []StepCounts `json:"steps"`
	// Calls is how many items apply makes a call for.
	Calls    int             `json:"calls"`
	Messages messages.Counts `json:"messages"`
}

// StepCounts is how many items one step planned for each action.
type StepCounts struct {
	Step   string `json:"step"`
	Create int    `json:"create"`
	Update int    `json:"update"`
	Map    int    `json:"map"`
	Skip   int    `json:"skip"`
	Manual int    `json:"manual"`
}

// Of returns the count of one action.
func (s StepCounts) Of(a Action) int {
	switch a {
	case Create:
		return s.Create
	case Update:
		return s.Update
	case Map:
		return s.Map
	case Skip:
		return s.Skip
	case Manual:
		return s.Manual
	}
	return 0
}

// Summarize counts p's items per step, in the order the steps first appear,
// and its messages per class.
func Summarize(p Plan) Summary {
	s := Summary{Version: p.Version, Steps: []StepCounts{}, Messages: messages.Count(p.Messages)}
	index := map[string]int{}
	for _, it := range p.Items {
		i, ok := index[it.Step()]
		if !ok {
			i = len(s.Steps)
			index[it.Step()] = i
			s.Steps = append(s.Steps, StepCounts{Step: it.Step()})
		}
		c := &s.Steps[i]
		switch it.Action {
		case Create:
			c.Create++
		case Update:
			c.Update++
		case Map:
			c.Map++
		case Skip:
			c.Skip++
		case Manual:
			c.Manual++
		}
		if it.Action.Calls() {
			s.Calls++
		}
	}
	return s
}
