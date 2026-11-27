package run

import (
	"context"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// A Step is one entity's part of the migration, such as users or teams.
//
// An item is exactly one call: a team that needs a create, Operations
// enabled and three batches of members is five items, each depending on the
// one it needs. That gives every write its own journal entry, so a run that
// stops knows exactly which calls were made. An item that needs no call (a
// map to an object that already exists, a skip or a manual task) is an item
// too, so the plan shows it and the items that depend on it get its JSM
// object.
type Step interface {
	// Name is stable across versions: it starts every item key and so every
	// message ID, which acceptance files name. It is snake_case.
	Name() string

	// Read reads the step's part of one side into the snapshot; see
	// discover.Reader. A step that changes what it writes there implements
	// discover.Versioned, so no read in the older format is reused.
	Read(ctx context.Context, side review.Side, w *discover.Writer) error

	// Plan turns both snapshots and the mappings of earlier runs into items.
	// It returns an error only when it cannot plan at all; a problem with
	// one object is a message on its item.
	Plan(ctx context.Context, in PlanInput) (Planned, error)

	// Apply makes the item's one call. deps holds the result of every item
	// it depends on, failed ones included, and the step decides from them
	// whether to make the call: the engine never skips a call. An error is
	// for the call Apply made; messages.Classify gives its class, and
	// httperr.ErrUnknownOutcome means the call may have taken effect.
	Apply(ctx context.Context, it review.Item, deps Results) (Outcome, error)

	// Lookup says whether the call of an item whose outcome is unknown took
	// effect, and what it made: a call JSM did not answer, or a create that
	// answered without naming its object. jsmAtPlan is the JSM snapshot the
	// plan was built from: an
	// object that matches the item but is not in it was made by this run,
	// which matters where JSM allows two objects with the same name.
	Lookup(ctx context.Context, it review.Item, deps Results, jsmAtPlan *discover.Snapshot) (Lookup, error)
}

// PlanInput is what a step plans from.
type PlanInput struct {
	PagerDuty, JSM *discover.Snapshot
	// Mappings are the JSM objects earlier runs migrated PagerDuty objects
	// to, or linked them with. A step plans a map item for an object found
	// here instead of creating it again.
	Mappings Mappings
}

// Planned is what a step planned.
type Planned struct {
	// Items start with the step's name: "<step>/<id>", built from PagerDuty
	// IDs, so the same object has the same key in every plan version. An
	// item may depend on items of any step.
	Items []review.Item
	// Messages are about one of the step's items, named by Item, or about
	// the step as a whole, with no Item.
	Messages []messages.Message
}

// Outcome is what Apply did.
type Outcome struct {
	// Pending leaves a read-only verification for a later resume.
	Pending bool
	// Declined means the step made no call, because of what an item it
	// depends on gave. The zero Outcome is a call that was made.
	Declined bool
	// JSM is the object the call created or changed. It is saved in the
	// mapping with the item's PagerDuty object, so a create of an item with
	// one must name it: without it, the next plan would create the object
	// again, so the engine looks it up instead, and the item stays unknown
	// until something names it.
	JSM       *review.Ref
	Status    int
	RequestID string
	// Messages are about the item.
	Messages []messages.Message
}

// Result is how an item's turn in a run ended, as the items that depend on
// it see it.
type Result struct {
	State review.EntryState
	// JSM is the item's object: the one its call created or changed, or the
	// one a map item links to. It is nil unless State is review.Done.
	JSM *review.Ref
}

// Results are the results of the items an item depends on, by key.
type Results map[string]Result

// LookupOutcome is what a lookup found.
type LookupOutcome int

const (
	// LookupUnknown means the lookup could not tell. The zero value, so a
	// lookup that says nothing never leads to a second call.
	LookupUnknown LookupOutcome = iota
	// LookupFound means the call took effect; Lookup.JSM is the object.
	LookupFound
	// LookupNotFound means the effect of the call was absent from the lookup.
	// An absent object is insufficient evidence to resend a create.
	LookupNotFound
)

// Lookup is what Step.Lookup found.
type Lookup struct {
	Outcome LookupOutcome
	JSM     *review.Ref
}

// Mapping links one PagerDuty object to the JSM object a run migrated it to.
type Mapping struct {
	PagerDuty, JSM review.Ref
	// Item is the key of the item that made the link.
	Item string
}

// Mappings are the links earlier runs made.
type Mappings struct{ byPD map[mappingKey]Mapping }

type mappingKey struct{ pdType, pdID, jsmType string }

// Find returns the JSM object of type jsmType that pd was migrated to.
func (m Mappings) Find(pd review.Ref, jsmType string) (review.Ref, bool) {
	l, ok := m.byPD[mappingKey{pd.Type, pd.ID, jsmType}]
	return l.JSM, ok
}

// Len is how many links there are.
func (m Mappings) Len() int { return len(m.byPD) }
