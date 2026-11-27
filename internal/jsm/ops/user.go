package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// WaitForUser bounds the wait for asynchronous Operations provisioning.
// Contact details are discarded. Only the specific missing-user response is pending.
func (c *Client) WaitForUser(ctx context.Context, id string) (bool, error) {
	wait, cancel := context.WithTimeout(ctx, c.timing.pollTimeout)
	defer cancel()
	for {
		var page struct{ Values []struct{} }
		err := c.Get(wait, "/v1/users/contacts", url.Values{"targetAccountId": {id}}, &page)
		if err == nil {
			if page.Values == nil {
				return false, httperr.ErrBadResponse
			}
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var response *httperr.Error
		var body struct{ Errors []struct{ Title string } }
		if !errors.As(err, &response) || response.Status != 400 || json.Unmarshal([]byte(response.Body), &body) != nil || len(body.Errors) != 1 || body.Errors[0].Title != "No user found with the target account Id: "+id {
			return false, err
		}
		if err := c.timing.sleep(wait, 15*time.Second); err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return false, nil
			}
			return false, err
		}
	}
}
