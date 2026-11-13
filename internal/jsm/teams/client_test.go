package teams_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/teams"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

const site = "acme.atlassian.net"

func fake(t *testing.T, opts ...jsmfake.Option) (*jsmfake.Fake, *teams.Client) {
	t.Helper()
	f := jsmfake.New(t, jsmfake.Account{
		Site: site, CloudID: "cloud-1", OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-token-value-1234",
	}, opts...)
	return f, teams.New(f.Client(), site, "org-1", "cloud-1")
}

type team struct {
	ID          string `json:"teamId"`
	DisplayName string `json:"displayName,omitempty"`
}

func teamsOf(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = team{ID: fmt.Sprintf("t%02d", i)}
	}
	return out
}

func TestGetAddsTheSiteID(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodGet, f.TeamsPath("/teams/t1"), http.StatusOK, team{ID: "t1", DisplayName: "Ops"})

	var got team
	if err := c.Get(context.Background(), "/teams/t1", nil, &got); err != nil {
		t.Fatal(err)
	}
	r := f.Requests()[0]
	if got.DisplayName != "Ops" || r.Host != site || r.Path != "/gateway/api/public/teams/v1/org/org-1/teams/t1" || r.Query.Get("siteId") != "cloud-1" {
		t.Fatalf("got %+v from %+v", got, r)
	}
}

func TestACallerCannotSetTheSiteID(t *testing.T) {
	f, c := fake(t)
	ctx := context.Background()
	q := url.Values{"siteId": {"cloud-1"}}
	if err := c.Get(ctx, "/teams/t1", q, new(team)); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("Get: %v", err)
	}
	if _, err := teams.List[team](ctx, c, teams.ListRequest{Path: "/teams", Query: q, Key: "entities"}); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("List: %v", err)
	}
	if _, err := c.CreateTeam(ctx, map[string]any{"displayName": "Ops", "siteId": "cloud-1"}, nil); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("CreateTeam: %v", err)
	}
	for _, siteID := range []string{"cloud-1", "another-cloud"} {
		w := teams.Write{Method: teams.MethodPost, Path: "/teams/t1/members/add", Query: url.Values{"siteId": {siteID}}, Body: map[string]any{}}
		if _, err := c.Write(ctx, w, nil); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("Write with siteId %s: %v", siteID, err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestListPagesByCursor(t *testing.T) {
	f, c := fake(t, jsmfake.PageSize(2))
	f.List(http.MethodGet, f.TeamsPath("/teams"), "entities", teamsOf(5))

	got, err := teams.List[team](context.Background(), c, teams.ListRequest{Path: "/teams", Key: "entities"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4].ID != "t04" {
		t.Fatalf("got %+v", got)
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for _, r := range reqs {
		if r.Query.Get("siteId") != "cloud-1" {
			t.Errorf("query %v", r.Query)
		}
	}
}

func TestMembersPagesByPageInfo(t *testing.T) {
	f, c := fake(t)
	path := "/teams/t1/members"
	f.List(http.MethodPost, f.TeamsPath(path), "results", teamsOf(5))

	got, err := teams.Members[team](context.Background(), c, teams.MembersRequest{Path: path, Key: "results", First: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d members", len(got))
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for _, r := range reqs {
		if r.Method != http.MethodPost || r.Query.Get("siteId") != "cloud-1" {
			t.Errorf("request %+v", r)
		}
	}
}

func TestListsNeedAKeyAndAPageSize(t *testing.T) {
	f, c := fake(t)
	ctx := context.Background()
	if _, err := teams.List[team](ctx, c, teams.ListRequest{Path: "/teams"}); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("List without key: %v", err)
	}
	if _, err := teams.Members[team](ctx, c, teams.MembersRequest{Path: "/teams/t1/members", Key: "results"}); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("Members without page size: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestCreateTeamPutsTheSiteIDInTheBody(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodPost, f.TeamsPath("/teams"), http.StatusCreated, team{ID: "t9", DisplayName: "Ops"})

	var got team
	res, err := c.CreateTeam(context.Background(), map[string]any{"displayName": "Ops", "description": "", "teamType": "MEMBER_INVITE"}, &got)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusCreated || got.ID != "t9" {
		t.Fatalf("got %+v, %+v", res, got)
	}
	r := f.Requests()[0]
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["siteId"] != "cloud-1" || body["displayName"] != "Ops" || r.Query.Has("siteId") {
		t.Fatalf("request %s %v", r.Body, r.Query)
	}
}

func TestCreateTeamTakesAStructBody(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodPost, f.TeamsPath("/teams"), http.StatusCreated, team{ID: "t9"})
	type create struct {
		DisplayName string `json:"displayName"`
		TeamType    string `json:"teamType"`
	}
	if _, err := c.CreateTeam(context.Background(), create{DisplayName: "Ops", TeamType: "OPEN"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTeamRefusesABodyThatIsNotAnObject(t *testing.T) {
	f, c := fake(t)
	for _, body := range []any{nil, []string{"a"}, "team", 7, func() {}} {
		if _, err := c.CreateTeam(context.Background(), body, nil); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%T: %v", body, err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestWriteSendsNoSiteID(t *testing.T) {
	f, c := fake(t)
	add := f.TeamsPath("/teams/t1/members/add")
	f.Answer(http.MethodPost, add, http.StatusOK, map[string]any{"members": []any{}, "errors": []any{}})

	_, err := c.Write(context.Background(), teams.Write{Method: teams.MethodPost, Path: "/teams/t1/members/add",
		Body: map[string]any{"members": []map[string]string{{"accountId": "acc-1"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Requests()[0]
	if r.Query.Has("siteId") || string(r.Body) != `{"members":[{"accountId":"acc-1"}]}` {
		t.Fatalf("request %+v", r)
	}
}

func TestBadIDsRefuseEveryCall(t *testing.T) {
	f := jsmfake.New(t, jsmfake.Account{Site: site, CloudID: "cloud-1", OrgID: "org-1", Email: "operator@example.com", Token: "t"})
	ctx := context.Background()
	for _, c := range []*teams.Client{
		teams.New(f.Client(), site, "org/1", "cloud-1"),
		teams.New(f.Client(), site, "org-1", ""),
		teams.New(f.Client(), site, "org-1", "cloud?1"),
	} {
		if err := c.Get(ctx, "/teams", nil, new(team)); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("Get: %v", err)
		}
		if _, err := c.CreateTeam(ctx, map[string]any{}, nil); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("CreateTeam: %v", err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}
