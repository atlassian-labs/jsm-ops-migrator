package pd

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
)

// Window is the furthest PagerDuty lets an offset reach: offset + limit may
// not pass 10,000 on the classic endpoints. List never asks past it, on any
// endpoint, because what PagerDuty answers beyond it is not known.
const Window = 10_000

// DefaultLimit is the page size List uses when ListRequest.Limit is zero, the
// most the classic endpoints allow.
const DefaultLimit = 100

// ListRequest is a list to read.
type ListRequest struct {
	// Path is the list's path, such as /users or /v3/schedules/P1/rotations.
	Path string
	// Query holds the list's own parameters, such as include[] or
	// team_ids[]. List sets limit, offset and total itself.
	Query url.Values
	// Key is the field of the answer that holds the items, such as "users".
	Key string
	// Limit is the page size; zero means DefaultLimit. The v3 rotation and
	// event lists allow up to 1,000.
	Limit int
}

// Coverage is how much of a list was read.
type Coverage struct {
	// Total is how many items PagerDuty says the list holds, or -1 when it
	// did not say.
	Total int
	// Read is how many items were read.
	Read int
	// Capped reports that the list goes past the window, so the items after
	// it were not read. The caller reads them by splitting the list with the
	// endpoint's filters, such as team_ids[] or query.
	Capped bool
}

// Complete reports whether every item of the list was read.
func (c Coverage) Complete() bool {
	return !c.Capped && (c.Total < 0 || c.Read >= c.Total)
}

// List reads every page of a list and decodes its items into T. It returns
// the items with how much of the list they cover. A list longer than the
// window is not an error: List returns what the window holds, with
// Coverage.Capped set. Any other failure is returned together with the items
// read before it, so the caller can record a partial read; a failed page is
// never an empty list.
func List[T any](ctx context.Context, c *Client, lr ListRequest) ([]T, Coverage, error) {
	cov := Coverage{Total: -1}
	limit := lr.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 0 || limit > Window || lr.Key == "" {
		return nil, cov, fmt.Errorf("pd: list %s: want a key and a limit between 1 and %d", lr.Path, Window)
	}
	var items []T
	for page, err := range httpx.Pages(ctx, c.h, request(lr.Path, lr.Query), httpx.Offset[T](lr.Key, limit, Window)) {
		if page.Total >= 0 && cov.Total < 0 {
			cov.Total = page.Total
		}
		if errors.Is(err, httpx.ErrWindow) {
			cov.Capped = true
			break
		}
		if err != nil {
			return items, cov, err
		}
		items = append(items, page.Items...)
		cov.Read = len(items)
	}
	return items, cov, nil
}
