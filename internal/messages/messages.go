package messages

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Class says what the operator has to do about a message.
type Class string

const (
	// Blocker must be fixed before apply can run. During apply it reports a
	// call that no later call can get past, such as a rejected credential;
	// the run still carries on with the calls after it.
	Blocker Class = "blocker"
	// Warning is accepted by name, with a reason, before apply runs. During
	// apply it reports an item whose call failed.
	Warning Class = "warning"
	// ManualTask is something the operator does by hand, such as checking
	// whether a write whose outcome is unknown took effect.
	ManualTask Class = "manual_task"
	// Information needs nothing from the operator.
	Information Class = "information"
)

// Classes are the four classes, the most urgent first: the order every
// report lists them in.
var Classes = []Class{Blocker, Warning, ManualTask, Information}

var labels = map[Class][2]string{
	Blocker:     {"blocker", "blockers"},
	Warning:     {"warning", "warnings"},
	ManualTask:  {"manual task", "manual tasks"},
	Information: {"information", "information"},
}

// Label is the class in words, as reports print it. An unknown class is
// printed as it is, so a report never shows a message without one.
func (c Class) Label() string {
	if l, ok := labels[c]; ok {
		return l[0]
	}
	return OneLine(string(c))
}

// Plural is the label for a count other than one.
func (c Class) Plural() string {
	if l, ok := labels[c]; ok {
		return l[1]
	}
	return OneLine(string(c))
}

// Known reports whether c is one of the four classes.
func (c Class) Known() bool {
	_, ok := labels[c]
	return ok
}

// Message is one thing a step or the engine reports.
type Message struct {
	Class Class `json:"class"`
	// Code names the kind of message, such as "member_dropped". With Item it
	// forms the message's ID, which stays the same in every plan version that
	// still raises it, so an acceptance file can name a warning.
	Code string `json:"code"`
	// Item is the key of the plan item the message is about, or empty for a
	// message about the whole plan or run, such as an incomplete snapshot.
	Item string `json:"item,omitempty"`
	Text string `json:"text"`
}

// codeRe keeps codes free of '/', so the code is always the last part of an
// ID whatever the item key holds.
var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ID identifies the message within a plan: "<item>/<code>", or the code
// alone for a message without an item.
func (m Message) ID() string {
	if m.Item == "" {
		return m.Code
	}
	return m.Item + "/" + m.Code
}

// Check reports a message that no report can show correctly: an unknown
// class, or a code that is not snake_case. The error does not name the
// message: its ID holds an item key, which only the caller knows is safe to
// show.
func (m Message) Check() error {
	if !m.Class.Known() {
		return fmt.Errorf("unknown class %s", strconv.Quote(OneLine(string(m.Class))))
	}
	if !codeRe.MatchString(m.Code) {
		return errors.New("code must be snake_case")
	}
	return nil
}

// Redacted returns the message with its text as every report writes it: on
// one line and redacted (see Clean).
func (m Message) Redacted() Message {
	m.Text = Clean(m.Text)
	return m
}

// Clean is s as every report writes it. It is put on one line first, because
// the redactor does not see a phone number split by a line break that
// OneLine would then join, and it is redacted until nothing more changes,
// because removing one value can make the text next to it match a rule.
func Clean(s string) string {
	s = OneLine(s)
	for range maxRedactPasses {
		r := credentials.RedactText(s)
		if r == s {
			break
		}
		s = r
	}
	return s
}

// maxRedactPasses bounds Clean. Each pass only replaces values with
// "[redacted]", so a text settles after a few.
const maxRedactPasses = 8

// Op says which kind of call an error came from.
type Op int

const (
	// Read is a call plan makes to read PagerDuty or JSM.
	Read Op = iota
	// Write is a call apply makes for an item.
	Write
)

// Classify returns the class of a failed call.
//
// A failed read is a blocker, except a feature the account's plan does not
// include: a list that could not be read in full must never look empty, so
// the plan cannot be applied until it is read again. A failed write is a
// warning on its item, because the run carries on and each later step
// decides from the result; it is a blocker only when no later call can get
// past it either, such as a rejected credential. A write whose outcome is
// unknown is a manual task until a lookup settles it.
//
// An error that wraps no httperr declaration is classed by what failed: on
// a read, the list is no more complete than for any other failure, so it is
// a blocker; on a write, the item failed, as when JSM reports that enabling
// Operations failed, so it is a warning.
func Classify(err error, op Op) Class {
	if err == nil {
		return ""
	}
	switch {
	// First: the cause underneath an unknown outcome can match another
	// declaration, but the write may have taken effect all the same.
	case errors.Is(err, httperr.ErrUnknownOutcome):
		if op == Read {
			return Blocker
		}
		return ManualTask
	case errors.Is(err, httperr.ErrRefused),
		errors.Is(err, httperr.ErrUnauthorized),
		errors.Is(err, httperr.ErrForbidden):
		return Blocker
	case errors.Is(err, httperr.ErrNotInPlan):
		if op == Read {
			return Information
		}
		return Blocker
	case errors.Is(err, httperr.ErrPartial),
		errors.Is(err, httperr.ErrNotFound),
		errors.Is(err, httperr.ErrConflict),
		errors.Is(err, httperr.ErrInvalid),
		errors.Is(err, httperr.ErrUnavailable),
		errors.Is(err, httperr.ErrBadResponse):
		if op == Read {
			return Blocker
		}
		return Warning
	}
	if op == Read {
		return Blocker
	}
	return Warning
}
