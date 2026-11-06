package testkit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

// FakeAPIs answers the pre-flight calls the way PagerDuty, the site and the
// Admin API would. A credential field that is set is the only value accepted;
// a call made with any other value is rejected with httperr.ErrUnauthorized.
// The *Err fields make a call fail as set.
type FakeAPIs struct {
	PagerDutyKey, AtlassianEmail, AtlassianToken, OrgAPIKey string

	AbilitiesErr error
	// Sites maps a site to its tenant; an unknown site is httperr.ErrNotFound.
	Sites     map[string]preflight.Tenant
	TenantErr error
	AccessErr error
	// APIs lists the new public APIs the site has.
	APIs    map[preflight.API]bool
	APIErr  error
	Orgs    []preflight.Org
	OrgsErr error
	// Members maps an organization to the emails of its accounts.
	Members map[string][]string
	FindErr error

	mu    sync.Mutex
	calls []string
}

// ClientsFor returns the clients for the given credentials, as the factory in
// cmd/jsm-ops-migrator does.
func (f *FakeAPIs) ClientsFor(creds credentials.Set) preflight.Clients {
	c := fakeClients{f: f, creds: creds}
	return preflight.Clients{PagerDuty: c, Atlassian: c, Admin: c}
}

// Calls lists the calls made, in order.
func (f *FakeAPIs) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *FakeAPIs) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

type fakeClients struct {
	f     *FakeAPIs
	creds credentials.Set
}

func accepted(want, got string) bool { return want == "" || want == got }

func (c fakeClients) Abilities(context.Context) ([]string, error) {
	c.f.record("pagerduty abilities")
	if !accepted(c.f.PagerDutyKey, c.creds.PagerDutyKey.Reveal()) {
		return nil, fmt.Errorf("GET /abilities: 401: %w", httperr.ErrUnauthorized)
	}
	if c.f.AbilitiesErr != nil {
		return nil, c.f.AbilitiesErr
	}
	return []string{"teams"}, nil
}

func (c fakeClients) TenantInfo(_ context.Context, site string) (preflight.Tenant, error) {
	c.f.record("tenant_info " + site)
	if c.f.TenantErr != nil {
		return preflight.Tenant{}, c.f.TenantErr
	}
	t, ok := c.f.Sites[site]
	if !ok {
		return preflight.Tenant{}, fmt.Errorf("GET https://%s/_edge/tenant_info: 404: %w", site, httperr.ErrNotFound)
	}
	return t, nil
}

func (c fakeClients) atlassianOK() bool {
	return accepted(c.f.AtlassianEmail, c.creds.AtlassianEmail.Value()) &&
		accepted(c.f.AtlassianToken, c.creds.AtlassianToken.Reveal())
}

func (c fakeClients) CheckAccess(_ context.Context, cloudID string) error {
	c.f.record("ops teams " + cloudID)
	if !c.atlassianOK() {
		return fmt.Errorf("GET /v1/teams: 401: %w", httperr.ErrUnauthorized)
	}
	return c.f.AccessErr
}

func (c fakeClients) HasAPI(_ context.Context, cloudID string, api preflight.API) (bool, error) {
	c.f.record("api " + string(api) + " " + cloudID)
	if !c.atlassianOK() {
		return false, fmt.Errorf("401: %w", httperr.ErrUnauthorized)
	}
	if c.f.APIErr != nil {
		return false, c.f.APIErr
	}
	return c.f.APIs[api], nil
}

func (c fakeClients) Orgs(context.Context) ([]preflight.Org, error) {
	c.f.record("admin orgs")
	if !accepted(c.f.OrgAPIKey, c.creds.OrgAPIKey.Reveal()) {
		return nil, fmt.Errorf("GET /admin/v1/orgs: 401: %w", httperr.ErrUnauthorized)
	}
	if c.f.OrgsErr != nil {
		return nil, c.f.OrgsErr
	}
	return c.f.Orgs, nil
}

func (c fakeClients) FindUser(_ context.Context, orgID string, email credentials.Sensitive) (preflight.Account, error) {
	c.f.record("admin find user " + orgID)
	if !accepted(c.f.OrgAPIKey, c.creds.OrgAPIKey.Reveal()) {
		return preflight.Account{}, fmt.Errorf("401: %w", httperr.ErrUnauthorized)
	}
	if c.f.FindErr != nil {
		return preflight.Account{}, c.f.FindErr
	}
	for _, m := range c.f.Members[orgID] {
		if m == email.Value() {
			return preflight.Account{AccountID: "account-of-" + orgID}, nil
		}
	}
	return preflight.Account{}, fmt.Errorf("user search: %w", httperr.ErrNotFound)
}

// HealthyAPIs returns fake APIs where everything works: the site acme has
// cloudId cloud-1 and a clock that matches now, and the org API key manages
// org-1, which has operator@example.com.
func HealthyAPIs(now time.Time) *FakeAPIs {
	return &FakeAPIs{
		Sites:   map[string]preflight.Tenant{"acme.atlassian.net": {CloudID: "cloud-1", ServerTime: now}},
		APIs:    map[preflight.API]bool{preflight.ServicesAPI: true, preflight.UserNotificationsAPI: true, preflight.GlobalEscalationsAPI: true},
		Orgs:    []preflight.Org{{ID: "org-1", Name: "Acme"}},
		Members: map[string][]string{"org-1": {"operator@example.com"}},
	}
}

// AllCredentials is a complete set of test credentials, matching what
// HealthyAPIs accepts once its credential fields are set from it.
func AllCredentials() credentials.Set {
	return credentials.Set{}.
		With(credentials.PagerDutyKey, "pd-canary-0123456789").
		With(credentials.AtlassianEmail, "operator@example.com").
		With(credentials.AtlassianToken, "atl-canary-0123456789").
		With(credentials.OrgAPIKey, "org-canary-0123456789")
}
