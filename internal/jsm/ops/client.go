package ops

import (
	"context"
	"fmt"
	"net/url"
	"time"

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

// MaxSize is the largest page JSM Operations lists allow, and the size List
// asks for unless told otherwise.
const MaxSize = 50

// Client calls JSM Operations on one site. It is safe for concurrent use.
type Client struct {
	c       rest.Client
	cloudID string
	timing  timing
}

// New returns a Client for the site with cloudID, sending its calls through
// h. Paths are under /jsm/ops/api/<cloudId>, so a caller passes /v1/teams.
// A cloudID that cannot be part of a path makes every call fail with
// httperr.ErrRefused.
func New(h *httpx.Client, cloudID string) *Client {
	prefix, err := rest.Segment("/jsm/ops/api", "cloudId", cloudID)
	return &Client{
		c:       rest.Client{H: h, Family: httpx.JSMOps, Prefix: prefix, Err: err},
		cloudID: cloudID,
		timing:  defaultTiming(),
	}
}

// Get reads path, such as /v1/schedules/<id>, with query, which may be nil,
// and decodes the answer into v. An error wraps the httperr sentinel that
// classifies it, and never quotes the answer.
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	return c.c.Get(ctx, path, query, v)
}

// Write sends w and, when v is not nil, decodes the answer into it. A write
// that may have taken effect without an answer that says so, such as one that
// timed out, is httperr.ErrUnknownOutcome; it is never sent again here.
func (c *Client) Write(ctx context.Context, w Write, v any) (Result, error) {
	return c.c.Do(ctx, w, v)
}

// ListRequest is a list to read.
type ListRequest struct {
	// Path is the list's path, such as /v1/schedules.
	Path string
	// Query holds the list's own parameters, such as query or expand. List
	// sets offset and size itself.
	Query url.Values
	// Key is the field of the answer that holds the items; empty means
	// "values", which every JSM Operations list uses.
	Key string
	// Size is the page size, at most MaxSize; zero means MaxSize.
	Size int
}

// List reads every page of a list and decodes its items into T. JSM
// Operations pages by offset and size, and its links.next is a relative path
// without the cloudId, so the offset is counted here and the link is never
// followed. A failure is returned with the items read before it.
func List[T any](ctx context.Context, c *Client, lr ListRequest) ([]T, error) {
	size, key := lr.Size, lr.Key
	if size == 0 {
		size = MaxSize
	}
	if key == "" {
		key = "values"
	}
	if size < 0 || size > MaxSize {
		return nil, fmt.Errorf("ops: list %s: %w: the size must be between 1 and %d", lr.Path, httperr.ErrRefused, MaxSize)
	}
	req, err := c.c.Request(lr.Path, lr.Query)
	if err != nil {
		return nil, err
	}
	return rest.Collect(ctx, c.c.H, req, httpx.OffsetSize[T](key, size))
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
