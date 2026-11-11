package admin

import (
	"context"
	"fmt"
	"net/url"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
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

// Client calls the Admin API with the org API key. It is safe for concurrent
// use.
type Client struct{ c rest.Client }

// New returns a Client that sends its calls through h. Paths are under
// /admin, so a caller passes /v1/orgs.
func New(h *httpx.Client) *Client {
	return &Client{c: rest.Client{H: h, Family: httpx.Admin, Prefix: "/admin"}}
}

// Get reads path, such as /v1/orgs/<orgId>, with query, which may be nil,
// and decodes the answer into v.
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	return c.c.Get(ctx, path, query, v)
}

// Search sends a read that the API takes as a POST, such as a user search,
// and decodes the answer into v. It only reads, so it is sent again like any
// read. It reads the answer's first page; a caller that needs the next one
// asks for it with the API's own parameters.
func (c *Client) Search(ctx context.Context, path string, body, v any) error {
	_, err := c.c.Do(ctx, Write{Method: MethodPost, Path: path, Body: body, Idempotent: true}, v)
	return err
}

// Write sends w and, when v is not nil, decodes the answer into it. A 206,
// such as an invite that worked for some emails only, is a success whose
// Result.Status says so. A write that may have taken effect without an
// answer that says so is httperr.ErrUnknownOutcome; it is never sent again
// here.
func (c *Client) Write(ctx context.Context, w Write, v any) (Result, error) {
	return c.c.Do(ctx, w, v)
}

// ListRequest is a list to read.
type ListRequest struct {
	// Path is the list's path, such as /v1/orgs.
	Path string
	// Query holds the list's own parameters. List sets cursor itself.
	Query url.Values
	// Key is the field of the answer that holds the items, such as "data".
	Key string
}

// List reads every page of a list that links.next pages and decodes its
// items into T. Only the cursor is taken from the link, never its host or
// path. A failure is returned with the items read before it.
func List[T any](ctx context.Context, c *Client, lr ListRequest) ([]T, error) {
	if lr.Key == "" {
		return nil, fmt.Errorf("admin: list %s: %w: want the key that holds the items", lr.Path, httperr.ErrRefused)
	}
	req, err := c.c.Request(lr.Path, lr.Query)
	if err != nil {
		return nil, err
	}
	return rest.Collect(ctx, c.c.H, req, httpx.Link[T](lr.Key))
}
