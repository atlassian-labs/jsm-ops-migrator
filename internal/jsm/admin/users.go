package admin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
)

const searchLimit = 100

// UserMatch describes an exact email in the organization's visible directories.
// An empty match means there was no directory result. An account outside the
// organization can still own that email.
type UserMatch struct {
	AccountID        string `json:"account_id,omitempty"`
	AccountStatus    string `json:"account_status,omitempty"`
	MembershipStatus string `json:"membership_status,omitempty"`
	Status           string `json:"status,omitempty"`
	Incomplete       bool   `json:"incomplete,omitempty"`
	Ambiguous        bool   `json:"ambiguous,omitempty"`
	SiteAccess       string `json:"site_access,omitempty"` // "yes", "no", or empty when the expansion was unavailable.
}

// Active reports whether the directory identified one active account.
func (m UserMatch) Active() bool {
	return !m.Incomplete && !m.Ambiguous && m.AccountID != "" && m.AccountStatus == "active" &&
		(m.MembershipStatus == "active" || m.Status == "active") &&
		(m.MembershipStatus == "" || m.MembershipStatus == "active") && (m.Status == "" || m.Status == "active")
}

// FindUsers returns one match per input email in input order. It reads every
// page before returning any matches: partial results must never be used to
// decide which accounts are missing.
func (c *Client) FindUsers(ctx context.Context, orgID string, emails []credentials.Sensitive) ([]UserMatch, error) {
	return c.findUsers(ctx, orgID, "", emails)
}

// FindSiteUsers also reads each user's product access on the target JSM site.
func (c *Client) FindSiteUsers(ctx context.Context, orgID, cloudID string, emails []credentials.Sensitive) ([]UserMatch, error) {
	if err := rest.CheckSegment("cloud ID", cloudID); err != nil {
		return nil, err
	}
	return c.findUsers(ctx, orgID, "ari:cloud:jira-servicedesk::site/"+cloudID, emails)
}

func (c *Client) findUsers(ctx context.Context, orgID, resource string, emails []credentials.Sensitive) ([]UserMatch, error) {
	if err := rest.CheckSegment("organization id", orgID); err != nil {
		return nil, err
	}
	path := "/v2/orgs/" + orgID + "/directories/-/users/search"
	matches := make(map[string]UserMatch)
	var unique []string
	for _, email := range emails {
		key := email.Value()
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("user search: %w: email is empty", httperr.ErrRefused)
		}
		if _, exists := matches[key]; !exists {
			matches[key] = UserMatch{}
			unique = append(unique, key)
		}
	}
	for batch := range slices.Chunk(unique, searchLimit) {
		if err := c.searchEmailBatch(ctx, path, resource, batch, matches); err != nil {
			return nil, err
		}
	}
	result := make([]UserMatch, len(emails))
	for i, email := range emails {
		result[i] = matches[email.Value()]
	}
	return result, nil
}

func (c *Client) searchEmailBatch(ctx context.Context, path, resource string, emails []string, matches map[string]UserMatch) error {
	body := struct {
		Emails []string `json:"emails"`
		Limit  int      `json:"limit"`
		Cursor string   `json:"cursor,omitempty"`
		Expand []string `json:"expand,omitempty"`
	}{Emails: emails, Limit: searchLimit}
	if resource != "" {
		body.Expand = []string{"productAccess"}
	}
	seen := map[string]bool{}
	for {
		var page struct {
			Data []struct {
				Email            string `json:"email"`
				AccountID        string `json:"accountId"`
				AccountStatus    string `json:"accountStatus"`
				MembershipStatus string `json:"membershipStatus"`
				Status           string `json:"status"`
				ProductAccess    []struct {
					ID string `json:"id"`
				} `json:"productAccess"`
			} `json:"data"`
			Links struct{ Next string } `json:"links"`
		}
		if err := c.Search(ctx, path, body, &page); err != nil {
			return err
		}
		if page.Data == nil {
			return fmt.Errorf("user search: %w: missing directory results", httperr.ErrBadResponse)
		}
		for _, user := range page.Data {
			if user.Email == "" {
				// An unattributable row could conflict with any match in this batch.
				// Isolate each address before trusting either a match or a miss.
				if len(emails) == 1 {
					matches[emails[0]] = UserMatch{Incomplete: true}
					return nil
				}
				for _, email := range emails {
					matches[email] = UserMatch{}
					if err := c.searchEmailBatch(ctx, path, resource, []string{email}, matches); err != nil {
						return err
					}
				}
				return nil
			}
			match := UserMatch{AccountID: user.AccountID, AccountStatus: user.AccountStatus, MembershipStatus: user.MembershipStatus, Status: user.Status}
			if resource != "" && user.ProductAccess != nil {
				match.SiteAccess = "no"
				for _, product := range user.ProductAccess {
					if product.ID == "" {
						match.SiteAccess = ""
						continue
					}
					if product.ID == resource {
						match.SiteAccess = "yes"
						break
					}
				}
			}
			if rest.CheckSegment("account ID", user.AccountID) != nil {
				match = UserMatch{Incomplete: true}
			}
			for _, email := range emails {
				if strings.EqualFold(email, user.Email) {
					if old := matches[email]; old != (UserMatch{}) && old != match {
						matches[email] = UserMatch{Ambiguous: true}
					} else {
						matches[email] = match
					}
				}
			}
		}
		body.Cursor = page.Links.Next
		if body.Cursor == "" {
			return nil
		}
		if seen[body.Cursor] {
			return fmt.Errorf("user search: %w: repeated page cursor", httperr.ErrBadResponse)
		}
		seen[body.Cursor] = true
	}
}
