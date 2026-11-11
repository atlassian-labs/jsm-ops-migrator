package httperr

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotInPlan means the account's plan does not include the feature
	// behind the call (402). For a read it is information, not a failure:
	// the account cannot hold that kind of object.
	ErrNotInPlan = errors.New("the account's plan does not include this feature")
	// ErrConflict means the object conflicts with one that exists, such as a
	// name already taken (409).
	ErrConflict = errors.New("the object conflicts with one that exists")
	// ErrInvalid means the API rejected the request as invalid (400, 422).
	ErrInvalid = errors.New("the API rejected the request as invalid")
	// ErrBadResponse means the API answered with a body that could not be
	// read. It is never retried, because the same request gets the same
	// answer.
	ErrBadResponse = errors.New("the API's answer could not be read")
	// ErrUnknownOutcome means what a write did is not known: it timed out or
	// got a 5xx after it may have reached the server, it was answered 2xx
	// with an answer too large or malformed to read, or an accepted operation
	// did not finish in time. The write is never sent again before the caller
	// checks what happened.
	ErrUnknownOutcome = errors.New("the write may or may not have taken effect")
	// ErrRefused means the HTTP layer refused to send the request: a write to
	// PagerDuty, a host that is not allowed, or a missing credential. It is a
	// bug or a configuration error, and never retried.
	ErrRefused = errors.New("the request was refused before it was sent")
	// ErrPartial means the call did some of what it was asked. The error is a
	// *Partial, which says which items failed.
	ErrPartial = errors.New("the call partly succeeded")
)

// Error is a call that did not succeed: what was called and how it ended. It
// wraps the sentinel that classifies it and, when there is one, the cause
// underneath, such as a network error.
type Error struct {
	// Family is the API family, such as "pagerduty".
	Family string
	Method string
	// Path is the request path. The query is left out, because it can hold
	// personal data such as the email address of a user search.
	Path string
	// Status is the HTTP status, or 0 when no answer arrived.
	Status int
	// RequestID is the API's id for the request, for support cases.
	RequestID string
	// Body is the start of the response body, redacted.
	Body string
	// Kind is the sentinel that classifies the error.
	Kind error
	// Cause is what went wrong underneath, or nil.
	Cause error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", e.Method, e.Family, e.Path)
	if e.Status != 0 {
		fmt.Fprintf(&b, ": %d", e.Status)
	}
	for _, err := range []error{e.Kind, e.Cause} {
		if err != nil {
			b.WriteString(": " + err.Error())
		}
	}
	if e.RequestID != "" {
		b.WriteString(" (request " + e.RequestID + ")")
	}
	return b.String()
}

// Unwrap returns the sentinel and the cause, so errors.Is matches either.
func (e *Error) Unwrap() []error {
	var out []error
	for _, err := range []error{e.Kind, e.Cause} {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

// Partial is a call that did part of what it was asked: Teams members/add
// answers 200 with an error for each member it could not add, and an Admin
// API invite answers 206. Items are named by their position in the request,
// so a partial result never carries an email address.
type Partial struct {
	// Done holds the positions of the items the call handled.
	Done []int
	// Failed holds an error for each item it did not handle.
	Failed []ItemError
}

// ItemError is why one item of a partial call failed.
type ItemError struct {
	// Index is the item's position in the request.
	Index int
	// Err wraps a sentinel, such as ErrConflict, when the API's reason maps
	// to one.
	Err error
}

func (p *Partial) Error() string {
	return fmt.Sprintf("%s: %d done, %d failed", ErrPartial, len(p.Done), len(p.Failed))
}

// Unwrap returns ErrPartial.
func (p *Partial) Unwrap() error { return ErrPartial }
