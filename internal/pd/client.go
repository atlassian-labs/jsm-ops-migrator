package pd

import (
	"context"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
)

// Accept headers. The classic REST API (v2) wants its version in the Accept
// header; the /v3/ paths take plain JSON.
const (
	acceptV2 = "application/vnd.pagerduty+json;version=2"
	acceptV3 = "application/json"
)

// Client reads one PagerDuty account. It is safe for concurrent use; the HTTP
// layer sends its requests one at a time, below PagerDuty's rate limit.
type Client struct{ h *httpx.Client }

// New returns a Client that sends its requests through h, which decides the
// key and the region.
func New(h *httpx.Client) *Client { return &Client{h: h} }

// request is the one place that builds a request. It takes no method and no
// body, so nothing built here can be anything but a GET.
func request(path string, query url.Values) httpx.Request {
	accept := acceptV2
	if strings.HasPrefix(path, "/v3/") {
		accept = acceptV3
	}
	return httpx.Request{Family: httpx.PagerDuty, Path: path, Query: query, Accept: accept}
}

// Get reads path, such as /users/P123 or /v3/schedules/P456, with the given
// query, which may be nil, and decodes the answer into v. An error wraps the
// httperr sentinel that classifies it: for example httperr.ErrNotInPlan when
// the account's plan lacks the feature (402), or httperr.ErrForbidden when
// the key may not read it (403). It never quotes the answer.
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	_, err := c.h.DoJSON(ctx, request(path, query), v)
	return err
}

// Abilities lists the account's abilities (GET /abilities), which depend on
// its pricing plan. It is the cheapest call that proves a key works, so the
// pre-flight checks and login use it.
func (c *Client) Abilities(ctx context.Context) ([]string, error) {
	var answer struct {
		Abilities []string `json:"abilities"`
	}
	if err := c.Get(ctx, "/abilities", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Abilities, nil
}
