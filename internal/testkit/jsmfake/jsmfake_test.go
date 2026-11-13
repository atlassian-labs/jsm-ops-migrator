package jsmfake_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

// recorder is a T that keeps what the fake reports, so a test can check it.
type recorder struct{ errs []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func account() jsmfake.Account {
	return jsmfake.Account{Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-token-value-1234", OrgKey: "org-key-value-1234"}
}

func TestTheOtherEnvironmentsHostRejectsTheAccount(t *testing.T) {
	a := account()
	a.Staging = true
	f := jsmfake.New(t, a)
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams"), http.StatusOK, []any{})
	cfg := f.Config()
	cfg.Staging = false
	h, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Path: f.OpsPath("/v1/teams")})
	if !errors.Is(err, httperr.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestTeamsRequestsWithoutTheSiteIDAreRejected(t *testing.T) {
	f := jsmfake.New(t, account())
	f.Answer(http.MethodGet, f.TeamsPath("/teams/t1"), http.StatusOK, map[string]any{})
	f.Answer(http.MethodPost, f.TeamsPath("/teams"), http.StatusCreated, map[string]any{})
	h := f.Client()
	ctx := context.Background()
	for _, req := range []httpx.Request{
		{Family: httpx.Site, Site: "acme.atlassian.net", Path: f.TeamsPath("/teams/t1")},
		{Family: httpx.Site, Site: "acme.atlassian.net", Method: httpx.MethodPost, Path: f.TeamsPath("/teams"), Body: map[string]any{"displayName": "Ops"}},
	} {
		_, err := h.Do(ctx, req)
		var e *httperr.Error
		if !errors.As(err, &e) || e.Status != http.StatusBadRequest || !strings.Contains(e.Body, "SITE_ID_REQUIRED_FOR_TEAM_API") {
			t.Errorf("%s %s: got %v", req.Method, req.Path, err)
		}
	}
}

func TestACredentialSentToTenantInfoFailsTheTest(t *testing.T) {
	rec := &recorder{}
	f := jsmfake.New(rec, account())
	r, err := http.NewRequest(http.MethodGet, "https://acme.atlassian.net/_edge/tenant_info", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetBasicAuth("operator@example.com", "atl-token-value-1234")
	resp, err := f.Transport().RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(rec.errs) != 1 {
		t.Fatalf("status %d, reports %q", resp.StatusCode, rec.errs)
	}
}

func TestAnotherHostFailsTheTest(t *testing.T) {
	rec := &recorder{}
	f := jsmfake.New(rec, account())
	r, err := http.NewRequest(http.MethodGet, "https://other.atlassian.net/rest/api/3/myself", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Transport().RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest || len(rec.errs) != 1 {
		t.Fatalf("status %d, reports %q", resp.StatusCode, rec.errs)
	}
}
