package jira

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
)

// Gateway routes scoped user tokens to the verified Jira cloud ID.
func Gateway(h *httpx.Client, cloudID string) *Client {
	prefix, err := rest.Segment("/ex/jira", "cloud ID", cloudID)
	return &Client{c: rest.Client{H: h, Family: httpx.JiraGateway, Prefix: prefix + "/rest/api/3", Err: err}}
}

type Seats struct {
	Remaining int  `json:"remaining"`
	Unlimited bool `json:"unlimited"`
}

func (c *Client) Seats(ctx context.Context) (Seats, error) {
	var role struct {
		Key               string
		RemainingSeats    *int
		HasUnlimitedSeats *bool
	}
	if err := c.Get(ctx, "/applicationrole/jira-servicedesk", nil, &role); err != nil {
		return Seats{}, err
	}
	if role.Key != "jira-servicedesk" || role.HasUnlimitedSeats == nil {
		return Seats{}, fmt.Errorf("missing JSM seat capacity: %w", httperr.ErrBadResponse)
	}
	if *role.HasUnlimitedSeats {
		return Seats{Unlimited: true}, nil
	}
	if role.RemainingSeats == nil || *role.RemainingSeats < 0 {
		return Seats{}, fmt.Errorf("missing remaining JSM seats: %w", httperr.ErrBadResponse)
	}
	return Seats{Remaining: *role.RemainingSeats}, nil
}

// HasAccess distinguishes an invitation awaiting acceptance from a grant that
// has reached Jira but has yet to create its Operations user.
func (c *Client) HasAccess(ctx context.Context, id string) (bool, error) {
	var user struct {
		AccountID        string
		ApplicationRoles *struct{ Items []struct{ Key string } }
	}
	err := c.Get(ctx, "/user", url.Values{"accountId": {id}, "expand": {"applicationRoles"}}, &user)
	if errors.Is(err, httperr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if user.AccountID != id || user.ApplicationRoles == nil || user.ApplicationRoles.Items == nil {
		return false, httperr.ErrBadResponse
	}
	for _, role := range user.ApplicationRoles.Items {
		if role.Key == "jira-servicedesk" {
			return true, nil
		}
	}
	return false, nil
}
