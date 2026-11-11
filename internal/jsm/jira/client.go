package jira

import (
	"context"
	"net/url"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
)

type (
	// Write is one call that may change something.
	Write = rest.Write
	// Result is how a write that succeeded was answered.
	Result = rest.Result
	// Method is the method of a write.
	Method = rest.Method
)

const (
	MethodPost   = rest.MethodPost
	MethodPut    = rest.MethodPut
	MethodPatch  = rest.MethodPatch
	MethodDelete = rest.MethodDelete
)

// Client calls the Jira REST API on one site. It is safe for concurrent use.
type Client struct{ c rest.Client }

// New returns a Client that calls Jira on site, such as acme.atlassian.net,
// through h. Paths are under /rest/api/3, so a caller passes /myself.
func New(h *httpx.Client, site string) *Client {
	return &Client{c: rest.Client{H: h, Family: httpx.Site, Site: site, Prefix: "/rest/api/3"}}
}

// Get reads path with query, which may be nil, and decodes the answer into v.
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	return c.c.Get(ctx, path, query, v)
}

// Write sends w and, when v is not nil, decodes the answer into it. A write
// that may have taken effect without an answer that says so is
// httperr.ErrUnknownOutcome; it is never sent again here.
func (c *Client) Write(ctx context.Context, w Write, v any) (Result, error) {
	return c.c.Do(ctx, w, v)
}
