package httpx

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Method is an HTTP method. The API clients may not import net/http, so the
// methods they can send are declared here.
type Method string

const (
	MethodGet    Method = http.MethodGet
	MethodPost   Method = http.MethodPost
	MethodPut    Method = http.MethodPut
	MethodPatch  Method = http.MethodPatch
	MethodDelete Method = http.MethodDelete
)

func (m Method) known() bool {
	switch m {
	case MethodGet, MethodPost, MethodPut, MethodPatch, MethodDelete:
		return true
	}
	return false
}

// Request is one API call.
type Request struct {
	Family Family
	// Method is empty for GET.
	Method Method
	// Site is the site's host, such as acme.atlassian.net, for the Site and
	// SiteAnonymous families; the others ignore it.
	Site string
	// Path is the path under the family's host, such as /users or
	// /jsm/ops/api/<cloudId>/v1/teams. It never holds a host or a query.
	Path  string
	Query url.Values
	// Body is sent as JSON; nil sends no body.
	Body any
	// Accept is the Accept header; empty means application/json.
	Accept string
	// Idempotent marks a write that may safely be sent again, such as a
	// search sent as a POST or a PUT that replaces an object. Any other
	// write that may have reached the server is reported as
	// httperr.ErrUnknownOutcome instead of being sent again.
	Idempotent bool
}

func (r Request) method() Method {
	if r.Method == "" {
		return MethodGet
	}
	return r.Method
}

// write reports whether r may change something, so it must not be sent again
// once it may have reached the server.
func (r Request) write() bool { return r.method() != MethodGet && !r.Idempotent }

// validPath reports whether p is a path that stays on its family's host and
// in its own part of it: no host, query or fragment, and no dot segments.
func validPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "?#\\") {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Response is the answer to a call that succeeded (2xx).
type Response struct {
	Status int
	Body   []byte
	// RequestID is the API's id for the request, for support cases, or empty
	// when the API sent none.
	RequestID string

	header http.Header
	req    Request
}

// Header returns the first value of a response header.
func (r *Response) Header(name string) string { return r.header.Get(name) }

// Decode unmarshals the body into v. A body that does not decode is an error
// wrapping httperr.ErrBadResponse, or httperr.ErrUnknownOutcome for a write.
// The error never quotes the body.
func (r *Response) Decode(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return r.bad(decodeCause(err))
	}
	return nil
}

// bad is the error for an answer that is not what the call expects. err must
// not quote the answer.
func (r *Response) bad(err error) error {
	kind := httperr.ErrBadResponse
	if r.req.write() {
		// The write was answered 2xx, so it took effect, but what it did
		// cannot be read from the answer and has to be looked up.
		kind = httperr.ErrUnknownOutcome
	}
	return &httperr.Error{
		Family: string(r.req.Family), Method: string(r.req.method()), Path: r.req.Path,
		Status: r.Status, RequestID: r.RequestID, Kind: kind, Cause: err,
	}
}
