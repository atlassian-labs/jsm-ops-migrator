package teams

import (
	"context"
	"encoding/json"
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

// siteID is the parameter that names the site a request is about.
const siteID = "siteId"

// Client calls the Teams API for one organization, through one site. It is
// safe for concurrent use.
type Client struct {
	c       rest.Client
	cloudID string
}

// New returns a Client that calls the Teams API on site, such as
// acme.atlassian.net, for the organization orgID, about the site with
// cloudID. Paths are under /gateway/api/public/teams/v1/org/<orgId>, so a
// caller passes /teams. An orgID or cloudID that cannot be part of a path or
// a query makes every call fail with httperr.ErrRefused.
func New(h *httpx.Client, site, orgID, cloudID string) *Client {
	prefix, err := rest.Segment("/gateway/api/public/teams/v1/org", "organization id", orgID)
	if err == nil {
		err = rest.CheckSegment("cloudId", cloudID)
	}
	return &Client{
		c: rest.Client{H: h, Family: httpx.Site, Site: site, Prefix: prefix,
			ReadQuery: url.Values{siteID: {cloudID}}, Err: err},
		cloudID: cloudID,
	}
}

// Get reads path, such as /teams/<teamId>, with query, which may be nil, and
// decodes the answer into v. siteId is added to the query.
func (c *Client) Get(ctx context.Context, path string, query url.Values, v any) error {
	return c.c.Get(ctx, path, query, v)
}

// Write sends w and, when v is not nil, decodes the answer into it. The API
// documents no siteId on its other writes (update, delete, adding and
// removing members), so none is added, and a w whose query sets siteId is
// refused, because the client owns it. A write that may have taken effect
// without an answer that says so is httperr.ErrUnknownOutcome; it is never
// sent again here.
func (c *Client) Write(ctx context.Context, w Write, v any) (Result, error) {
	return c.c.Do(ctx, w, v)
}

// CreateTeam creates a team (POST /teams) from body, which must encode as a
// JSON object, and decodes the answer into v when it is not nil. siteId goes
// in the body, where the API takes it on this call. The API allows two teams
// with one name, so after httperr.ErrUnknownOutcome a lookup by name can find
// several.
func (c *Client) CreateTeam(ctx context.Context, body any, v any) (Result, error) {
	if c.c.Err != nil {
		return Result{}, c.c.Err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("teams: create team: %w: encoding the body: %w", httperr.ErrRefused, err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Result{}, fmt.Errorf("teams: create team: %w: the body must be a JSON object", httperr.ErrRefused)
	}
	if _, ok := fields[siteID]; ok {
		return Result{}, fmt.Errorf("teams: create team: %w: the client sets %s itself", httperr.ErrRefused, siteID)
	}
	site, err := json.Marshal(c.cloudID)
	if err != nil {
		return Result{}, err
	}
	fields[siteID] = site
	return c.c.Do(ctx, Write{Method: MethodPost, Path: "/teams", Body: fields}, v)
}

// ListRequest is a list paged by cursor, such as the organization's teams.
type ListRequest struct {
	// Path is the list's path, such as /teams.
	Path string
	// Query holds the list's own parameters. List sets cursor and siteId
	// itself.
	Query url.Values
	// Key is the field of the answer that holds the items, such as
	// "entities".
	Key string
}

// List reads every page of a list that the answer's cursor pages, until the
// cursor is null, and decodes its items into T. A failure is returned with
// the items read before it.
func List[T any](ctx context.Context, c *Client, lr ListRequest) ([]T, error) {
	if lr.Key == "" {
		return nil, fmt.Errorf("teams: list %s: %w: want the key that holds the items", lr.Path, httperr.ErrRefused)
	}
	req, err := c.c.Request(lr.Path, lr.Query)
	if err != nil {
		return nil, err
	}
	return rest.Collect(ctx, c.c.H, req, httpx.Cursor[T](lr.Key))
}

// MembersRequest is a list read with a POST whose body asks for the next
// page, such as a team's members.
type MembersRequest struct {
	// Path is the list's path, such as /teams/<teamId>/members.
	Path string
	// Key is the field of the answer that holds the items.
	Key string
	// First is the page size.
	First int
}

// Members reads every page of a list paged by pageInfo and decodes its items
// into T. The POST only reads, so it is sent again like any read. A failure
// is returned with the items read before it.
func Members[T any](ctx context.Context, c *Client, mr MembersRequest) ([]T, error) {
	if mr.First < 1 || mr.Key == "" {
		return nil, fmt.Errorf("teams: list %s: %w: want the key that holds the items and a page size of at least 1", mr.Path, httperr.ErrRefused)
	}
	req, err := c.c.Request(mr.Path, nil)
	if err != nil {
		return nil, err
	}
	return rest.Collect(ctx, c.c.H, req, httpx.PageInfo[T](mr.Key, mr.First))
}
