package main

import (
	"net/http"
	"os"
	"strconv"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

// Each CLI invocation starts a new process. Register the invite route only
// for the apply that should send it. A repeat during resume would receive 404.
func configureAccessFake(site *jsmfake.Fake) {
	gateway := "/ex/jira/cloud-1" + site.JiraPath("")
	seats := 5
	if n, err := strconv.Atoi(os.Getenv("TEST_JSM_SEATS")); err == nil {
		seats = n
	}
	site.Answer(http.MethodGet, gateway+"/applicationrole/jira-servicedesk", 200, map[string]any{"key": "jira-servicedesk", "remainingSeats": seats, "hasUnlimitedSeats": false})
	site.Answer(http.MethodPost, site.AdminPath("/v2/orgs/org-1/workspaces"), 200, map[string]any{"data": []any{map[string]any{"id": "ari:cloud:jira-servicedesk::site/cloud-1", "attributes": map[string]string{"typeKey": "jira-servicedesk"}}}})
	switch os.Getenv("TEST_ACCESS_PHASE") {
	case "invite":
		site.Answer(http.MethodPost, site.AdminPath("/v2/orgs/org-1/users/invite"), 200, map[string]any{"data": []any{map[string]any{
			"id": "invited-user", "email": "missing@example.com",
			"results": map[string]any{"roleAssignmentResult": []any{map[string]string{
				"resource": "ari:cloud:jira-servicedesk::site/cloud-1", "role": "ari:cloud:jira-servicedesk::role/product/member", "status": "INVITED",
			}}},
		}}})
	case "ready":
		site.Answer(http.MethodGet, gateway+"/user", 200, map[string]any{"accountId": "invited-user", "applicationRoles": map[string]any{"items": []any{map[string]string{"key": "jira-servicedesk"}}}})
		site.Answer(http.MethodGet, site.OpsPath("/v1/users/contacts"), 200, map[string]any{"values": []any{}})
	}
}
