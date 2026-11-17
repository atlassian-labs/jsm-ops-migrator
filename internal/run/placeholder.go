package run

import (
	"context"
	"errors"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/discover"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// Placeholders are the steps until the entity steps are built: one that
// reads nothing, plans nothing and says so, so plan and apply already write
// every review file and run record, and nothing is ever sent.
func Placeholders() []Step { return []Step{placeholder{}} }

type placeholder struct{}

func (placeholder) Name() string { return "placeholder" }

func (placeholder) Read(context.Context, review.Side, *discover.Writer) error { return nil }

func (placeholder) Plan(context.Context, PlanInput) (Planned, error) {
	return Planned{Messages: []messages.Message{{
		Class: messages.Information, Code: "placeholder_no_steps",
		Text: "The migration steps are not built yet, so this plan migrates nothing.",
	}}}, nil
}

// errNoCalls is never returned: the placeholder plans no item that makes a
// call.
var errNoCalls = errors.New("the placeholder step makes no calls")

func (placeholder) Apply(context.Context, review.Item, Results) (Outcome, error) {
	return Outcome{}, errNoCalls
}

func (placeholder) Lookup(context.Context, review.Item, Results, *discover.Snapshot) (Lookup, error) {
	return Lookup{}, errNoCalls
}
