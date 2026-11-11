package admin_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

func fake(t *testing.T, opts ...jsmfake.Option) (*jsmfake.Fake, *admin.Client) {
	t.Helper()
	f := jsmfake.New(t, jsmfake.Account{Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1", OrgKey: "org-key-value-1234"}, opts...)
	return f, admin.New(f.Client())
}

type org struct {
	ID string `json:"id"`
}

func orgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = org{ID: fmt.Sprintf("org-%d", i)}
	}
	return out
}

func TestGetUsesTheOrgKeyUnderAdmin(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodGet, f.AdminPath("/v1/orgs/org-1"), http.StatusOK, map[string]any{"data": org{ID: "org-1"}})

	var got struct{ Data org }
	if err := c.Get(context.Background(), "/v1/orgs/org-1", nil, &got); err != nil {
		t.Fatal(err)
	}
	r := f.Requests()[0]
	if got.Data.ID != "org-1" || r.Host != jsmfake.APIHost || r.Path != "/admin/v1/orgs/org-1" {
		t.Fatalf("got %+v from %+v", got, r)
	}
}

func TestAnotherOrgKeyIsUnauthorized(t *testing.T) {
	f, _ := fake(t)
	f.Answer(http.MethodGet, f.AdminPath("/v1/orgs"), http.StatusOK, map[string]any{"data": []any{}})
	cfg := f.Config()
	cfg.Credentials = cfg.Credentials.With(credentials.OrgAPIKey, "another-org-key-5678")
	h, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.New(h).Get(context.Background(), "/v1/orgs", nil, new(any)); !errors.Is(err, httperr.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestListFollowsLinksNext(t *testing.T) {
	f, c := fake(t, jsmfake.PageSize(2))
	f.List(http.MethodGet, f.AdminPath("/v1/orgs"), "data", orgs(5))

	got, err := admin.List[org](context.Background(), c, admin.ListRequest{Path: "/v1/orgs", Key: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4].ID != "org-4" {
		t.Fatalf("got %+v", got)
	}
	reqs := f.Requests()
	if len(reqs) != 3 || reqs[1].Query.Get("cursor") == "" || reqs[1].Path != "/admin/v1/orgs" {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestListNeedsAKey(t *testing.T) {
	f, c := fake(t)
	if _, err := admin.List[org](context.Background(), c, admin.ListRequest{Path: "/v1/orgs"}); !errors.Is(err, httperr.ErrRefused) {
		t.Fatalf("got %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestSearchIsAReadSentAgainAfterA5xx(t *testing.T) {
	f, c := fake(t)
	path := f.AdminPath("/v2/orgs/org-1/directories/-/users/search")
	f.Answer(http.MethodPost, path, http.StatusOK, map[string]any{"data": []any{map[string]any{"accountId": "acc-1"}}})
	f.Fail(http.MethodPost, path, jsmfake.Fault{Status: http.StatusBadGateway})

	var got struct {
		Data []struct {
			AccountID string `json:"accountId"`
		} `json:"data"`
	}
	if err := c.Search(context.Background(), "/v2/orgs/org-1/directories/-/users/search", map[string]any{"emails": []string{"a@example.com"}}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Data[0].AccountID != "acc-1" {
		t.Fatalf("got %+v", got)
	}
	if n := len(f.Requests()); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

func TestWriteReportsAPartialAnswer(t *testing.T) {
	f, c := fake(t)
	path := f.AdminPath("/v2/orgs/org-1/users/invite")
	f.Answer(http.MethodPost, path, http.StatusPartialContent, map[string]any{"data": []any{}})

	res, err := c.Write(context.Background(), admin.Write{Method: admin.MethodPost, Path: "/v2/orgs/org-1/users/invite",
		Body: map[string]any{"emails": []string{"a@example.com", "b@example.com"}}}, new(any))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusPartialContent {
		t.Fatalf("status %d", res.Status)
	}
}

func TestErrorsNeverQuoteAnEmail(t *testing.T) {
	f, c := fake(t)
	path := f.AdminPath("/v2/orgs/org-1/users/invite")
	f.Answer(http.MethodPost, path, http.StatusBadRequest, map[string]any{"message": "Invalid email: someone.private@example.com"})

	_, err := c.Write(context.Background(), admin.Write{Method: admin.MethodPost, Path: "/v2/orgs/org-1/users/invite",
		Body: map[string]any{"emails": []string{"someone.private@example.com"}}}, nil)
	var e *httperr.Error
	if !errors.As(err, &e) || !errors.Is(err, httperr.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if s := err.Error() + e.Body; strings.Contains(s, "someone.private") {
		t.Fatalf("the error quotes the email: %s | %s", err, e.Body)
	}
}
