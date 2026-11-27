package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// UserReady checks once per pass. A later resume checks pending users again.
func (c *Client) UserReady(ctx context.Context, id string) (bool, error) {
	var page struct{ Values []struct{} }
	err := c.Get(ctx, "/v1/users/contacts", url.Values{"targetAccountId": {id}}, &page)
	if err == nil {
		if page.Values == nil {
			return false, httperr.ErrBadResponse
		}
		return true, nil
	}
	var response *httperr.Error
	var body struct{ Errors []struct{ Title string } }
	if errors.As(err, &response) && response.Status == 400 && json.Unmarshal([]byte(response.Body), &body) == nil && len(body.Errors) == 1 && body.Errors[0].Title == "No user found with the target account Id: "+id {
		return false, nil
	}
	return false, err
}
