package admin

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
)

// CheckJSMWorkspace proves the target site is a JSM workspace of the
// organization whose key will send the invitations.
func (c *Client) CheckJSMWorkspace(ctx context.Context, orgID, cloudID string) error {
	path, err := rest.Segment("/v2/orgs", "organization ID", orgID)
	if err != nil {
		return err
	}
	body := map[string]any{"limit": 1000}
	want := "ari:cloud:jira-servicedesk::site/" + cloudID
	seen := map[string]bool{}
	for {
		var page struct {
			Data []struct {
				ID         string
				Attributes struct{ TypeKey string }
			}
			Links struct{ Next string }
		}
		if err := c.Search(ctx, path+"/workspaces", body, &page); err != nil {
			return err
		}
		for _, w := range page.Data {
			if w.ID == want && w.Attributes.TypeKey == "jira-servicedesk" {
				return nil
			}
		}
		next := page.Links.Next
		if next == "" {
			return fmt.Errorf("the organization did not list the target JSM workspace: %w", httperr.ErrNotFound)
		}
		if strings.Contains(next, "?") || strings.Contains(next, "://") {
			link, err := url.Parse(next)
			if err != nil || link.Query().Get("cursor") == "" {
				return fmt.Errorf("workspace next link has no usable cursor: %w", httperr.ErrBadResponse)
			}
			next = link.Query().Get("cursor")
		}
		if seen[next] {
			return fmt.Errorf("repeated workspace cursor: %w", httperr.ErrBadResponse)
		}
		body = map[string]any{"cursor": next}
		seen[next] = true
	}
}
