// Package rest is what the four Atlassian API clients share: requests built
// under each family's path prefix, reads, paged lists and writes. Each client
// wraps it with its own prefix and paging style, so the code that calls an
// Atlassian API names only the path under that prefix and its own types, and
// never sees the HTTP layer.
package rest

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Method is the method of a write. The callers may not import the HTTP layer,
// so the clients re-declare these.
type Method = httpx.Method

const (
	MethodPost   = httpx.MethodPost
	MethodPut    = httpx.MethodPut
	MethodPatch  = httpx.MethodPatch
	MethodDelete = httpx.MethodDelete
)

// Write is one call that may change something.
type Write struct {
	// Method is POST, PUT, PATCH or DELETE.
	Method Method
	// Path is under the client's prefix, such as /v1/schedules.
	Path  string
	Query url.Values
	// Body is sent as JSON; nil sends no body.
	Body any
	// Idempotent marks a write that may safely be sent again, such as a
	// search sent as a POST or a PUT that replaces an object. Any other write
	// that may have reached the server ends in httperr.ErrUnknownOutcome
	// instead of being sent again.
	Idempotent bool
}

// Result is how a write that succeeded was answered.
type Result struct {
	// Status is the 2xx status, so a caller can tell a partial answer, such
	// as a 206, from a complete one.
	Status int
	// RequestID is the API's id for the request, for support cases, or empty
	// when the API sent none.
	RequestID string
}

// Client sends one API family's calls under one path prefix.
type Client struct {
	H      *httpx.Client
	Family httpx.Family
	// Site is the site's host for the families that call a site.
	Site string
	// Prefix comes before every path, such as /jsm/ops/api/<cloudId>.
	Prefix string
	// ReadQuery is added to every read, such as the Teams API's siteId. A
	// caller that sets one of its parameters itself is refused, on a read or
	// a write alike, so each value has one owner.
	ReadQuery url.Values
	// Err refuses every call, for a client built with an id that cannot be
	// part of a path.
	Err error
}

// Segment builds the client prefix base/id, or an error wrapping
// httperr.ErrRefused when id could change the path, such as one holding a
// slash or a dot segment. what names id in the error.
func Segment(base, what, id string) (string, error) {
	if err := CheckSegment(what, id); err != nil {
		return "", err
	}
	return base + "/" + id, nil
}

// CheckSegment reports, as an error wrapping httperr.ErrRefused, an id that
// cannot be one path segment.
func CheckSegment(what, id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/?#\\%") {
		return fmt.Errorf("%w: the %s is empty or is not one path segment", httperr.ErrRefused, what)
	}
	return nil
}

// Request is a GET of path under the prefix, with the read query added.
func (c Client) Request(path string, query url.Values) (httpx.Request, error) {
	if c.Err != nil {
		return httpx.Request{}, c.Err
	}
	if !strings.HasPrefix(path, "/") {
		return httpx.Request{}, fmt.Errorf("%w: path %q must start with /", httperr.ErrRefused, path)
	}
	q := maps.Clone(query)
	for k, vs := range c.ReadQuery {
		if _, ok := q[k]; ok {
			return httpx.Request{}, fmt.Errorf("%w: the client sets %s itself", httperr.ErrRefused, k)
		}
		if q == nil {
			q = url.Values{}
		}
		q[k] = vs
	}
	return httpx.Request{Family: c.Family, Site: c.Site, Path: c.Prefix + path, Query: q}, nil
}

// Get reads path with query, which may be nil, and decodes the answer into v.
func (c Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	req, err := c.Request(path, query)
	if err != nil {
		return err
	}
	_, err = c.H.DoJSON(ctx, req, v)
	return err
}

// Do sends w and, when v is not nil, decodes the answer into it. A write
// answered 2xx whose answer does not decode is httperr.ErrUnknownOutcome:
// it took effect, but what it did has to be looked up.
func (c Client) Do(ctx context.Context, w Write, v any) (Result, error) {
	if c.Err != nil {
		return Result{}, c.Err
	}
	switch w.Method {
	case MethodPost, MethodPut, MethodPatch, MethodDelete:
	default:
		return Result{}, fmt.Errorf("%w: a write is POST, PUT, PATCH or DELETE, not %q", httperr.ErrRefused, w.Method)
	}
	if !strings.HasPrefix(w.Path, "/") {
		return Result{}, fmt.Errorf("%w: path %q must start with /", httperr.ErrRefused, w.Path)
	}
	for k := range c.ReadQuery {
		if _, ok := w.Query[k]; ok {
			return Result{}, fmt.Errorf("%w: the client sets %s itself", httperr.ErrRefused, k)
		}
	}
	resp, err := c.H.Do(ctx, httpx.Request{
		Family: c.Family, Site: c.Site, Method: w.Method, Path: c.Prefix + w.Path,
		Query: w.Query, Body: w.Body, Idempotent: w.Idempotent,
	})
	if err != nil {
		return Result{}, err
	}
	res := Result{Status: resp.Status, RequestID: resp.RequestID}
	if v != nil {
		return res, resp.Decode(v)
	}
	return res, nil
}

// Collect reads every page of a list. The items read before a failure come
// back with it, so a partial read is never taken for an empty list.
func Collect[T any](ctx context.Context, h *httpx.Client, first httpx.Request, style httpx.Style[T]) ([]T, error) {
	var items []T
	for page, err := range httpx.Pages(ctx, h, first, style) {
		if err != nil {
			return items, err
		}
		items = append(items, page.Items...)
	}
	return items, nil
}
