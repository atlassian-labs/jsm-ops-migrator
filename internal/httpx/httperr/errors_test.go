package httperr_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		httperr.ErrUnauthorized, httperr.ErrForbidden, httperr.ErrNotFound, httperr.ErrUnavailable,
		httperr.ErrNotInPlan, httperr.ErrConflict, httperr.ErrInvalid, httperr.ErrBadResponse,
		httperr.ErrUnknownOutcome, httperr.ErrRefused, httperr.ErrPartial,
	}
	for i, a := range all {
		for _, b := range all[i+1:] {
			if errors.Is(a, b) || a.Error() == b.Error() {
				t.Errorf("%q and %q cannot be told apart", a, b)
			}
		}
	}
}

func TestErrorMatchesKindAndCause(t *testing.T) {
	e := &httperr.Error{Family: "pagerduty", Method: "GET", Path: "/users", Status: 403,
		Kind: httperr.ErrForbidden, Cause: io.ErrUnexpectedEOF}
	wrapped := fmt.Errorf("reading users: %w", e)
	if !errors.Is(wrapped, httperr.ErrForbidden) || !errors.Is(wrapped, io.ErrUnexpectedEOF) {
		t.Errorf("errors.Is must match both the kind and the cause")
	}
	if errors.Is(wrapped, httperr.ErrUnavailable) {
		t.Errorf("matched a sentinel it does not wrap")
	}
	var got *httperr.Error
	if !errors.As(wrapped, &got) || got.Status != 403 {
		t.Errorf("errors.As = %v", got)
	}
	if (&httperr.Error{}).Unwrap() != nil {
		t.Errorf("an empty error must unwrap to nothing")
	}
}

func TestErrorText(t *testing.T) {
	cases := []struct {
		err  *httperr.Error
		want string
	}{
		{&httperr.Error{Family: "pagerduty", Method: "GET", Path: "/users", Status: 403, Kind: httperr.ErrForbidden, RequestID: "abc"},
			"GET pagerduty /users: 403: the credential is not allowed to make this call (request abc)"},
		{&httperr.Error{Family: "jsm_ops", Method: "POST", Path: "/v1/teams", Kind: httperr.ErrUnknownOutcome, Cause: errors.New("timeout")},
			"POST jsm_ops /v1/teams: the write may or may not have taken effect: timeout"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

func TestPartial(t *testing.T) {
	p := &httperr.Partial{Done: []int{0, 2}, Failed: []httperr.ItemError{{Index: 1, Err: httperr.ErrConflict}}}
	err := fmt.Errorf("adding members: %w", p)
	if !errors.Is(err, httperr.ErrPartial) {
		t.Errorf("a partial result must match ErrPartial")
	}
	var got *httperr.Partial
	if !errors.As(err, &got) || len(got.Failed) != 1 || !errors.Is(got.Failed[0].Err, httperr.ErrConflict) {
		t.Errorf("errors.As = %+v", got)
	}
	if want := "the call partly succeeded: 2 done, 1 failed"; p.Error() != want {
		t.Errorf("got %q, want %q", p.Error(), want)
	}
}
