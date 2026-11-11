package jsm_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

const site = "acme.atlassian.net"

var clock = time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)

func fake(t *testing.T, opts ...jsmfake.Option) (*jsmfake.Fake, *jsm.Client) {
	t.Helper()
	f := jsmfake.New(t, jsmfake.Account{
		Site: site, CloudID: "cloud-1", OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-token-value-1234", OrgKey: "org-key-value-1234",
		Now: func() time.Time { return clock },
	}, opts...)
	return f, jsm.New(f.Client())
}

func TestTenantInfoReadsTheCloudIDAndTheClock(t *testing.T) {
	f, c := fake(t)
	got, err := c.TenantInfo(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	if got != (preflight.Tenant{CloudID: "cloud-1", ServerTime: clock}) {
		t.Fatalf("got %+v", got)
	}
	r := f.Requests()[0]
	if r.Host != site || r.Path != "/_edge/tenant_info" {
		t.Fatalf("request %+v", r)
	}
}

func TestTenantInfoWithoutADateHasNoServerTime(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodGet, "/_edge/tenant_info", http.StatusOK, map[string]any{"cloudId": "cloud-1"})
	got, err := c.TenantInfo(context.Background(), site)
	if err != nil || got.CloudID != "cloud-1" || !got.ServerTime.IsZero() {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestTenantInfoErrors(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		answer any
		status int
		want   error
	}{
		"no cloudId":            {map[string]any{"hostname": site}, http.StatusOK, httperr.ErrBadResponse},
		"cloudId not a segment": {map[string]any{"cloudId": "a/b"}, http.StatusOK, httperr.ErrBadResponse},
		"unknown site":          {map[string]any{"message": "no"}, http.StatusNotFound, httperr.ErrNotFound},
	} {
		f, c := fake(t)
		f.Answer(http.MethodGet, "/_edge/tenant_info", tc.status, tc.answer)
		if _, err := c.TenantInfo(ctx, site); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	_, c := fake(t)
	if _, err := c.TenantInfo(ctx, "acme.example.com"); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("a host that is not an Atlassian site: %v", err)
	}
}

func TestCheckAccessTakesEitherTeamsShape(t *testing.T) {
	for name, answer := range map[string]any{
		"bare array": []any{map[string]any{"teamId": "t1", "teamName": "Ops"}},
		"documented": map[string]any{"platformTeams": []any{}},
	} {
		f, c := fake(t)
		f.Answer(http.MethodGet, f.OpsPath("/v1/teams"), http.StatusOK, answer)
		if err := c.CheckAccess(context.Background(), "cloud-1"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckAccessReportsARejectedToken(t *testing.T) {
	f, c := fake(t)
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams"), http.StatusUnauthorized, map[string]any{"code": 401, "message": "Unauthorized"})
	if err := c.CheckAccess(context.Background(), "cloud-1"); !errors.Is(err, httperr.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestHasAPIReportsEveryAPIAsMissingWithoutACall(t *testing.T) {
	f, c := fake(t)
	for _, api := range []preflight.API{preflight.ServicesAPI, preflight.UserNotificationsAPI, preflight.GlobalEscalationsAPI} {
		ok, err := c.HasAPI(context.Background(), "cloud-1", api)
		if ok || err != nil {
			t.Errorf("%s: got %v, %v", api, ok, err)
		}
	}
	if _, err := c.HasAPI(context.Background(), "cloud-1", "teleport"); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("unknown API: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestOrgsReadsEveryPage(t *testing.T) {
	f, c := fake(t, jsmfake.PageSize(1))
	f.List(http.MethodGet, f.AdminPath("/v1/orgs"), "data", []any{
		map[string]any{"id": "org-1", "type": "orgs", "attributes": map[string]any{"name": "Acme"}},
		map[string]any{"id": "org-2", "type": "orgs", "attributes": map[string]any{"name": "Acme Labs"}},
	})
	got, err := c.Orgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != (preflight.Org{ID: "org-2", Name: "Acme Labs"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFindUserMatchesTheEmail(t *testing.T) {
	f, c := fake(t)
	path := f.AdminPath("/v2/orgs/org-1/directories/-/users/search")
	f.Answer(http.MethodPost, path, http.StatusOK, map[string]any{"data": []any{
		map[string]any{"accountId": "acc-1", "email": "Operator@Example.com"},
	}, "links": map[string]any{"next": nil}})

	got, err := c.FindUser(context.Background(), "org-1", credentials.NewSensitive("operator@example.com"))
	if err != nil || got.AccountID != "acc-1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	r := f.Requests()[0]
	if r.Method != http.MethodPost || string(r.Body) != `{"emails":["operator@example.com"]}` {
		t.Fatalf("request %s %s", r.Method, r.Body)
	}
}

func TestFindUserWithoutAMatchIsNotFound(t *testing.T) {
	for name, data := range map[string][]any{
		"empty":         {},
		"another email": {map[string]any{"accountId": "acc-1", "email": "someone@example.com"}},
		"no account id": {map[string]any{"accountId": "", "email": "operator@example.com"}},
	} {
		f, c := fake(t)
		f.Answer(http.MethodPost, f.AdminPath("/v2/orgs/org-1/directories/-/users/search"), http.StatusOK, map[string]any{"data": data})
		_, err := c.FindUser(context.Background(), "org-1", credentials.NewSensitive("operator@example.com"))
		if !errors.Is(err, httperr.ErrNotFound) {
			t.Errorf("%s: got %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "operator@") {
			t.Errorf("%s: the error quotes the email: %v", name, err)
		}
	}
}

func TestFindUserRefusesAnOrgIDThatIsNotASegment(t *testing.T) {
	f, c := fake(t)
	if _, err := c.FindUser(context.Background(), "org-1/../x", credentials.NewSensitive("operator@example.com")); !errors.Is(err, httperr.ErrRefused) {
		t.Fatalf("got %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestPollIsTheHTTPLayersPoll(t *testing.T) {
	err := jsm.Poll(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) { return false, nil })
	if !errors.Is(err, httperr.ErrUnknownOutcome) {
		t.Fatalf("got %v", err)
	}
	n := 0
	err = jsm.Poll(context.Background(), time.Millisecond, time.Second, func(context.Context) (bool, error) { n++; return n == 3, nil })
	if err != nil || n != 3 {
		t.Fatalf("got %v after %d checks", err, n)
	}
}
