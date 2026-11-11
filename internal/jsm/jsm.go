// Package jsm makes the Atlassian calls the CLI itself needs: the site's
// tenant_info, and the pre-flight checks' calls to JSM Operations and the
// Admin API. Client implements preflight.Atlassian and preflight.Admin. The
// clients for every other call are its subpackages: ops (JSM Operations),
// teams (the Teams REST API), admin (the Admin API) and jira (the Jira REST
// API), through which the migration's steps call the paths they define.
package jsm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/ops"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

// httpDate is the format of the Date header. The HTTP package's parser cannot
// be used here, because only the HTTP layer imports the network stack.
const httpDate = "Mon, 02 Jan 2006 15:04:05 GMT"

// Client makes the pre-flight calls to the site, JSM Operations and the
// Admin API. It is safe for concurrent use.
type Client struct{ h *httpx.Client }

// New returns a Client that sends its calls through h, which holds the
// credentials.
func New(h *httpx.Client) *Client { return &Client{h: h} }

var (
	_ preflight.Atlassian = (*Client)(nil)
	_ preflight.Admin     = (*Client)(nil)
)

// TenantInfo looks the site's cloudId up with GET https://<site>/_edge/tenant_info,
// which needs no credential, and reads the site's clock from the answer's
// Date header. A Date header that is missing or unreadable leaves
// Tenant.ServerTime zero, which the clock check reports as not checked.
func (c *Client) TenantInfo(ctx context.Context, site string) (preflight.Tenant, error) {
	var answer struct {
		CloudID string `json:"cloudId"`
	}
	resp, err := c.h.DoJSON(ctx, httpx.Request{Family: httpx.SiteAnonymous, Site: site, Path: "/_edge/tenant_info"}, &answer)
	if err != nil {
		return preflight.Tenant{}, err
	}
	if rest.CheckSegment("cloudId", answer.CloudID) != nil {
		return preflight.Tenant{}, fmt.Errorf("jsm: tenant_info: %w: the answer gave no cloudId that can be used", httperr.ErrBadResponse)
	}
	t := preflight.Tenant{CloudID: answer.CloudID}
	if d, err := time.Parse(httpDate, resp.Header("Date")); err == nil {
		t.ServerTime = d
	}
	return t, nil
}

// CheckAccess reads the site's JSM Operations teams (GET /v1/teams), the
// cheapest call that proves the Atlassian email and API token work there.
// The answer's shape is not read: the API answers a bare array where its
// documentation gives an object, and only the status matters here.
func (c *Client) CheckAccess(ctx context.Context, cloudID string) error {
	var teams json.RawMessage
	return ops.New(c.h, cloudID).Get(ctx, "/v1/teams", nil, &teams)
}

// HasAPI reports whether the site has one of the public APIs that are still
// being built. None of them is public yet, so HasAPI makes no call and reports
// each as missing; when one ships, its check goes here.
func (c *Client) HasAPI(_ context.Context, _ string, api preflight.API) (bool, error) {
	switch api {
	case preflight.ServicesAPI, preflight.UserNotificationsAPI, preflight.GlobalEscalationsAPI:
		return false, nil
	}
	return false, fmt.Errorf("jsm: %w: unknown API %q", httperr.ErrRefused, api)
}

// Orgs lists the organizations the org API key can manage (GET
// /admin/v1/orgs).
func (c *Client) Orgs(ctx context.Context) ([]preflight.Org, error) {
	type org struct {
		ID         string `json:"id"`
		Attributes struct {
			Name string `json:"name"`
		} `json:"attributes"`
	}
	items, err := admin.List[org](ctx, admin.New(c.h), admin.ListRequest{Path: "/v1/orgs", Key: "data"})
	if err != nil {
		return nil, err
	}
	out := make([]preflight.Org, len(items))
	for i, o := range items {
		out[i] = preflight.Org{ID: o.ID, Name: o.Attributes.Name}
	}
	return out, nil
}

// FindUser looks up the account with email in the organization's directories
// with the Admin API's exact-email search. It returns an error wrapping
// httperr.ErrNotFound when the organization has no such account. Neither the
// error nor the request's log line holds the email.
func (c *Client) FindUser(ctx context.Context, orgID string, email credentials.Sensitive) (preflight.Account, error) {
	if err := rest.CheckSegment("organization id", orgID); err != nil {
		return preflight.Account{}, err
	}
	var answer struct {
		Data []struct {
			AccountID string `json:"accountId"`
			Email     string `json:"email"`
		} `json:"data"`
	}
	path := "/v2/orgs/" + orgID + "/directories/-/users/search"
	if err := admin.New(c.h).Search(ctx, path, map[string]any{"emails": []string{email.Value()}}, &answer); err != nil {
		return preflight.Account{}, err
	}
	for _, u := range answer.Data {
		if u.AccountID != "" && strings.EqualFold(u.Email, email.Value()) {
			return preflight.Account{AccountID: u.AccountID}, nil
		}
	}
	return preflight.Account{}, fmt.Errorf("jsm: user search: %w: the organization has no account with that email", httperr.ErrNotFound)
}

// Poll calls check every interval until it reports done or fails, for the
// asynchronous calls such as alert creation. When timeout passes first, the
// error wraps httperr.ErrUnknownOutcome, because the operation was accepted
// but whether it finished is not known. check gets a context that ends at the
// timeout. The steps that poll cannot import the HTTP layer, so it is
// offered here.
func Poll(ctx context.Context, interval, timeout time.Duration, check func(context.Context) (done bool, err error)) error {
	return httpx.Poll(ctx, interval, timeout, check)
}
