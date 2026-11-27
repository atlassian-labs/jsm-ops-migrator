package jira_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/jira"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

const site = "acme.atlassian.net"

func fake(t *testing.T) (*jsmfake.Fake, *jira.Client) {
	t.Helper()
	f := jsmfake.New(t, jsmfake.Account{Site: site, CloudID: "cloud-1", OrgID: "org-1", Email: "operator@example.com", Token: "atl-token-value-1234"})
	return f, jira.New(f.Client(), site)
}

func TestGetIsUnderRestAPI3OnTheSite(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodGet, f.JiraPath("/applicationrole/jira-servicedesk"), http.StatusOK, map[string]any{"remainingSeats": 3})

	var got struct {
		RemainingSeats int `json:"remainingSeats"`
	}
	if err := c.Get(context.Background(), "/applicationrole/jira-servicedesk", nil, &got); err != nil {
		t.Fatal(err)
	}
	r := f.Requests()[0]
	if got.RemainingSeats != 3 || r.Host != site || r.Path != "/rest/api/3/applicationrole/jira-servicedesk" {
		t.Fatalf("got %+v from %+v", got, r)
	}
}

func TestWritePostsUnderRestAPI3(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodPost, f.JiraPath("/user"), http.StatusCreated, map[string]any{"accountId": "acc-1"})

	var got struct {
		AccountID string `json:"accountId"`
	}
	res, err := c.Write(context.Background(), jira.Write{Method: jira.MethodPost, Path: "/user",
		Body: map[string]any{"emailAddress": "new@example.com", "products": []string{"jira-servicedesk"}}}, &got)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusCreated || got.AccountID != "acc-1" {
		t.Fatalf("got %+v, %+v", res, got)
	}
}

func TestTokenOwnerUsesVerifiedJiraEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gateway, site int
		want          string
		calls         int
	}{
		{"gateway", 200, 0, "gateway-user", 1},
		{"classic site", 401, 200, "site-user", 2},
		{"missing scope", 401, 401, "", 2},
		{"permission denied", 403, 0, "", 1},
		{"invalid gateway identity", 200, 0, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fake(t)
			path := "/ex/jira/cloud-1" + f.JiraPath("/myself")
			id := "gateway-user"
			if tc.name == "invalid gateway identity" {
				id = ""
			}
			f.Answer(http.MethodGet, path, tc.gateway, map[string]string{"accountId": id})
			if tc.site != 0 {
				f.Answer(http.MethodGet, f.JiraPath("/myself"), tc.site, map[string]string{"accountId": "site-user"})
			}
			got, err := jira.CurrentAccountID(t.Context(), f.Client(), "cloud-1", site)
			reqs := f.Requests()
			if got != tc.want || (err == nil) != (tc.want != "") || len(reqs) != tc.calls {
				t.Fatalf("got %q, %v; %d requests", got, err, len(reqs))
			}
			if reqs[0].Host != jsmfake.APIHost || reqs[0].Path != path {
				t.Fatal(reqs[0])
			}
			if tc.calls == 2 && (reqs[1].Host != site || reqs[1].Path != f.JiraPath("/myself")) {
				t.Fatal(reqs[1])
			}
		})
	}
}
