package admin_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

func TestInviteBindsLoginAndSiteAndDoesNotRetryPartialWrites(t *testing.T) {
	for _, tc := range []struct {
		status     int
		email      string
		roleStatus string
		list       bool
		want       bool
	}{
		{200, "user@example.com", "INVITED", false, true},
		{200, "user@example.com", "INVITED", true, true},
		{200, "user@example.com", "ERROR", false, false},
		{206, "user@example.com", "ERROR", true, false},
		{200, "different@example.com", "INVITED", false, false},
		{206, "user@example.com", "INVITED", false, false},
		{500, "user@example.com", "", false, false},
	} {
		f, c := fake(t)
		var result any = map[string]any{"roleAssignmentResult": []map[string]string{{
			"resource": "ari:cloud:jira-servicedesk::site/cloud-1", "role": "atlassian/user", "status": tc.roleStatus,
		}}}
		if tc.list {
			result = []any{result}
		}
		f.Answer("POST", f.AdminPath("/v2/orgs/org-1/users/invite"), tc.status, map[string]any{"data": []any{map[string]any{
			"id": "target", "email": tc.email, "results": result,
		}}})
		id, _, err := c.Invite(t.Context(), "org-1", "cloud-1", credentials.NewSensitive("user@example.com"))
		reqs := f.Requests()
		wantErr := httperr.ErrUnknownOutcome
		if tc.roleStatus == "ERROR" {
			wantErr = admin.ErrInviteRejected
		}
		if (id == "target") != tc.want || (err == nil) != tc.want || (!tc.want && !errors.Is(err, wantErr)) || len(reqs) != 1 {
			t.Fatalf("status=%d, id=%q, err=%v, calls=%d", tc.status, id, err, len(reqs))
		}
		var b struct {
			Emails           []string
			PermissionRules  []struct{ Resource, Role string }
			SendNotification bool
		}
		if err := json.Unmarshal(reqs[0].Body, &b); err != nil || len(b.Emails) != 1 || b.Emails[0] != "user@example.com" || len(b.PermissionRules) != 1 || b.PermissionRules[0].Resource != "ari:cloud:jira-servicedesk::site/cloud-1" || b.PermissionRules[0].Role != "atlassian/user" || !b.SendNotification {
			t.Fatal("wrong invitation body")
		}
	}
}

func TestWorkspaceSearchSecondPageUsesOnlyCursor(t *testing.T) {
	f, c := fake(t)
	path := f.AdminPath("/v2/orgs/org-1/workspaces")
	f.Fail("POST", path, jsmfake.Fault{Status: 200, Body: map[string]any{"data": []any{}, "links": map[string]string{"next": "https://api.atlassian.com/admin/v2/orgs/org-1/workspaces?cursor=abc%2Bdef"}}})
	f.Answer("POST", path, 200, map[string]any{"data": []any{map[string]any{"id": "ari:cloud:jira-servicedesk::site/cloud-1", "attributes": map[string]string{"typeKey": "jira-servicedesk"}}}})
	if err := c.CheckJSMWorkspace(t.Context(), "org-1", "cloud-1"); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[1].Body, &body); err != nil || len(body) != 1 || body["cursor"] != "abc+def" {
		t.Fatalf("second page: %s", reqs[1].Body)
	}
}
