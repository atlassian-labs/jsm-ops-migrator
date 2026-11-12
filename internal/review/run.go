package review

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// Confirmation is the record of who confirmed a plan version before apply
// wrote anything.
type Confirmation struct {
	PlanVersion int    `json:"plan_version"`
	PlanHash    string `json:"plan_hash"`
	// AccountID is the Atlassian account whose API token runs apply. The
	// token is personal, so the account names the person who confirmed. No
	// API shows whether it is an org admin, so that is recorded, not checked.
	AccountID string    `json:"account_id"`
	At        time.Time `json:"confirmed_at"`
	// Via is how the warnings were accepted: ViaPrompt or ViaFile.
	Via      string     `json:"via"`
	Accepted []Accepted `json:"accepted_warnings"`
}

// How a confirmation was given.
const (
	ViaPrompt = "prompt"
	ViaFile   = "acceptance file"
)

// Check reports what makes c unfit to confirm p, whose plan.json has the
// given hash: another version or hash, an unknown way of giving it, no
// account, no time, or accepted warnings that are not exactly p's, each once
// and with a reason.
func (c Confirmation) Check(p Plan, hash string) error {
	var errs []error
	if c.Via != ViaPrompt && c.Via != ViaFile {
		errs = append(errs, fmt.Errorf("confirmation of plan v%d: unknown way of giving it %s", c.PlanVersion, quote(c.Via)))
	}
	if c.AccountID == "" || !plainRef(c.AccountID) || len(screen(c.AccountID)) > 0 {
		errs = append(errs, fmt.Errorf("confirmation of plan v%d: the account ID is not a plain ID", c.PlanVersion))
	}
	if c.At.IsZero() {
		errs = append(errs, fmt.Errorf("confirmation of plan v%d: no time", c.PlanVersion))
	}
	a := Acceptance{PlanVersion: c.PlanVersion, PlanHash: c.PlanHash, Warnings: c.Accepted}
	if _, err := a.Accept(p, hash); err != nil {
		errs = append(errs, fmt.Errorf("confirmation of plan v%d: %w", c.PlanVersion, err))
	}
	return errors.Join(errs...)
}

// RunState is where a run stands.
type RunState string

const (
	Running RunState = "running"
	// Paused means the run stopped between two calls, on a pause request or
	// the first Ctrl+C.
	Paused RunState = "paused"
	// Interrupted means the run stopped with a call that may have been in
	// flight: a second Ctrl+C or a crash. resume looks that call up first.
	Interrupted RunState = "interrupted"
	Completed   RunState = "completed"
)

// EntryState is where one item of a run stands.
type EntryState string

const (
	// Pending items have not been taken yet.
	Pending EntryState = "pending"
	// Done items made their call, or need none, and their result is known.
	Done EntryState = "done"
	// Failed items made their call and it failed.
	Failed EntryState = "failed"
	// Unknown items made a call whose outcome is not known yet.
	Unknown EntryState = "unknown"
	// Declined items made no call, because the step decided from the results
	// of the items it depends on.
	Declined EntryState = "declined"
)

// EntryStates are every entry state, in the order reports count them.
var EntryStates = []EntryState{Done, Failed, Unknown, Declined, Pending}

// Entry is one item of a run.
type Entry struct {
	Key    string     `json:"key"`
	Action Action     `json:"action"`
	State  EntryState `json:"state"`
	// Attempts is how many times the call was sent: more than one only when a
	// lookup proved an earlier attempt did not take effect.
	Attempts  int                `json:"attempts"`
	Status    int                `json:"status,omitempty"`
	RequestID string             `json:"request_id,omitempty"`
	PagerDuty *Ref               `json:"pagerduty,omitempty"`
	JSM       *Ref               `json:"jsm,omitempty"`
	At        time.Time          `json:"at,omitzero"`
	Messages  []messages.Message `json:"messages,omitempty"`
}

// Run is the record of one apply, however many times it paused and resumed.
type Run struct {
	ID          string    `json:"id"`
	PlanVersion int       `json:"plan_version"`
	PlanHash    string    `json:"plan_hash"`
	State       RunState  `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitzero"`
	Entries     []Entry   `json:"entries"`
}

// runIDRe keeps a run ID usable as a folder name on every system.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Check reports what makes r unfit to be written, with the same checks a
// plan's items get, so the record holds nothing the plan could not. An error
// never shows a value that holds sensitive data (quote).
func (r Run) Check() error {
	var errs []error
	run := "run " + quote(r.ID)
	if !runIDRe.MatchString(r.ID) {
		errs = append(errs, fmt.Errorf("%s: the ID must be letters, digits, '-' and '_'", run))
	}
	switch r.State {
	case Running, Paused, Interrupted, Completed:
	default:
		errs = append(errs, fmt.Errorf("%s: unknown state %s", run, quote(string(r.State))))
	}
	for i, e := range r.Entries {
		name := fmt.Sprintf("%s, entry %d", run, i+1)
		if validKey(e.Key) && len(screen(e.Key)) == 0 {
			name = fmt.Sprintf("%s, item %s", run, strconv.Quote(e.Key))
		}
		bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(name+": "+format, args...)) }
		if !validKey(e.Key) {
			bad("key must be <step>/<id>, without spaces, backticks or control characters")
		}
		if !e.Action.Known() {
			bad("unknown action %s", quote(string(e.Action)))
		}
		if !slices.Contains(EntryStates, e.State) {
			bad("unknown state %s", quote(string(e.State)))
		}
		rules := screen(e.Key)
		for _, ref := range []struct {
			side string
			ref  *Ref
		}{{"PagerDuty", e.PagerDuty}, {"JSM", e.JSM}} {
			if ref.ref == nil {
				continue
			}
			if !plainRef(ref.ref.Type) || !plainRef(ref.ref.ID) {
				bad("its %s reference is not a plain type and ID, without spaces, backticks or control characters", ref.side)
			}
			rules = append(rules, screen(ref.ref.Type)...)
			rules = append(rules, screen(ref.ref.ID)...)
		}
		if len(rules) > 0 {
			slices.Sort(rules)
			bad("holds sensitive data (%s)", strings.Join(slices.Compact(rules), ", "))
		}
		for _, m := range e.Messages {
			if err := m.Check(); err != nil {
				bad("message %s: %w", quote(m.ID()), err)
			}
			if m.Item != "" && m.Item != e.Key {
				bad("message %s is about another item", quote(m.ID()))
			}
		}
	}
	return errors.Join(errs...)
}

// RunCounts is how many entries of a run are in each state.
type RunCounts struct {
	Total, Done, Failed, Unknown, Declined, Pending int
}

// Of returns the count of one state.
func (c RunCounts) Of(s EntryState) int {
	switch s {
	case Done:
		return c.Done
	case Failed:
		return c.Failed
	case Unknown:
		return c.Unknown
	case Declined:
		return c.Declined
	case Pending:
		return c.Pending
	}
	return 0
}

// Counts counts r's entries by state.
func (r Run) Counts() RunCounts {
	c := RunCounts{Total: len(r.Entries)}
	for _, e := range r.Entries {
		switch e.State {
		case Done:
			c.Done++
		case Failed:
			c.Failed++
		case Unknown:
			c.Unknown++
		case Declined:
			c.Declined++
		case Pending:
			c.Pending++
		}
	}
	return c
}

// EncodeRun returns run.json, with every message text and request ID
// cleaned (messages.Clean): a request ID comes from the API's answer.
func EncodeRun(r Run) ([]byte, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	r.StartedAt, r.EndedAt = r.StartedAt.UTC(), r.EndedAt.UTC()
	entries := make([]Entry, len(r.Entries))
	for i, e := range r.Entries {
		e.At = e.At.UTC()
		e.RequestID = messages.Clean(e.RequestID)
		msgs := make([]messages.Message, len(e.Messages))
		for j, m := range e.Messages {
			msgs[j] = m.Redacted()
		}
		if len(msgs) == 0 {
			msgs = nil
		}
		e.Messages = msgs
		entries[i] = e
	}
	r.Entries = entries
	return encodeJSON(r)
}

// MappingCSV returns mapping.csv: the JSM object each PagerDuty object maps
// to, for every entry that is done and names both.
func MappingCSV(r Run) ([]byte, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if err := w.Write([]string{"item", "pagerduty_type", "pagerduty_id", "jsm_type", "jsm_id"}); err != nil {
		return nil, err
	}
	for _, e := range r.Entries {
		if e.State != Done || e.PagerDuty == nil || e.JSM == nil {
			continue
		}
		row := []string{e.Key, e.PagerDuty.Type, e.PagerDuty.ID, e.JSM.Type, e.JSM.ID}
		for i := range row {
			row[i] = csvCell(row[i])
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

// csvCell keeps a spreadsheet from running a cell as a formula: one that
// starts with = + - or @ gets a leading ', as OWASP recommends. The IDs come
// from the APIs, so the record is written whatever they hold.
func csvCell(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@") {
		return "'" + s
	}
	return s
}
