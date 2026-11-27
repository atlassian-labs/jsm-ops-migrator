package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// ErrInviteRejected means the response explicitly refused the requested role.
var ErrInviteRejected = errors.New("the invitation did not grant the requested JSM role")

// Invite one reviewed login per journal entry so a partial response never
// attributes another user's access grant to this source identity.
func (c *Client) Invite(ctx context.Context, orgID, cloudID string, email credentials.Sensitive) (string, Result, error) {
	path, err := rest.Segment("/v2/orgs", "organization ID", orgID)
	if err != nil {
		return "", Result{}, err
	}
	if err := rest.CheckSegment("cloud ID", cloudID); err != nil {
		return "", Result{}, err
	}
	resource := "ari:cloud:jira-servicedesk::site/" + cloudID
	body := map[string]any{
		"emails":           []string{email.Value()},
		"permissionRules":  []map[string]string{{"resource": resource, "role": "atlassian/user"}},
		"sendNotification": true,
		"notificationText": "Please accept this invitation to access Jira Service Management for your organization's migration.",
	}
	var reply struct {
		Data []struct {
			ID, Email string
			Results   json.RawMessage
		}
	}
	res, err := c.Write(ctx, Write{Method: MethodPost, Path: path + "/users/invite", Body: body}, &reply)
	switch {
	case errors.Is(err, httperr.ErrNotInPlan):
		return "", res, fmt.Errorf("organization invitations require a paid subscription: %w", err)
	case errors.Is(err, httperr.ErrConflict):
		return "", res, fmt.Errorf("invitation conflicted with the current site state; recheck access and available agent seats: %w", err)
	case err != nil:
		return "", res, err
	}
	if (res.Status != 200 && res.Status != 206) || len(reply.Data) != 1 || !strings.EqualFold(reply.Data[0].Email, email.Value()) {
		return "", res, fmt.Errorf("invitation did not confirm one account for the reviewed email: %w", httperr.ErrUnknownOutcome)
	}
	status, reason := invitationStatus(reply.Data[0].Results, resource)
	if status == "ERROR" {
		if reason == "" {
			return "", res, ErrInviteRejected
		}
		return "", res, fmt.Errorf("%w: %s", ErrInviteRejected, messages.Clean(reason))
	}
	if status != "INVITED" || res.Status != 200 || rest.CheckSegment("account ID", reply.Data[0].ID) != nil {
		return "", res, fmt.Errorf("invitation did not confirm the requested JSM role on the target site: %w", httperr.ErrUnknownOutcome)
	}
	return reply.Data[0].ID, res, nil
}

// The API returns one assignment result for our one requested role. Both the
// single-result object and the documented one-element list identify that role.
func invitationStatus(raw json.RawMessage, resource string) (string, string) {
	type result struct {
		RoleAssignmentResult []struct{ Resource, Role, Status, StatusReason string }
	}
	var single result
	if len(raw) > 0 && raw[0] == '[' {
		var list []result
		if json.Unmarshal(raw, &list) != nil || len(list) != 1 {
			return "", ""
		}
		single = list[0]
	} else if json.Unmarshal(raw, &single) != nil {
		return "", ""
	}
	if len(single.RoleAssignmentResult) != 1 {
		return "", ""
	}
	a := single.RoleAssignmentResult[0]
	if a.Resource != resource || (a.Role != "atlassian/user" && a.Role != "ari:cloud:jira-servicedesk::role/product/member") {
		return "", ""
	}
	return a.Status, a.StatusReason
}
