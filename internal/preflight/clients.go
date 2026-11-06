package preflight

import (
	"context"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Clients are the calls the checks make. cmd/jsm-ops-migrator builds them from the
// credentials being checked. Each API client implements its part, and until it
// exists cmd/jsm-ops-migrator passes Placeholders.
//
// Errors wrap the httperr declarations: ErrUnauthorized when the credential
// is rejected, ErrForbidden when it may not make the call, ErrNotFound when
// the object is not there, and anything else when the call could not be made.
type Clients struct {
	PagerDuty PagerDuty
	Atlassian Atlassian
	Admin     Admin
}

// PagerDuty is the PagerDuty REST API, called with the PagerDuty API key.
type PagerDuty interface {
	// Abilities lists the account's abilities (GET /abilities), the
	// cheapest call that proves the key works.
	Abilities(ctx context.Context) ([]string, error)
}

// Atlassian is the site and JSM Operations, called with the Atlassian account
// email and API token.
type Atlassian interface {
	// TenantInfo looks up the site's cloudId (GET
	// https://<site>/_edge/tenant_info, which needs no credential) and reads
	// the server's clock from the response.
	TenantInfo(ctx context.Context, site string) (Tenant, error)
	// CheckAccess makes a read-only JSM Operations call (GET /v1/teams) to
	// prove that the email and API token work on the site.
	CheckAccess(ctx context.Context, cloudID string) error
	// HasAPI reports whether the site has one of the public APIs that are
	// still being built.
	HasAPI(ctx context.Context, cloudID string, api API) (bool, error)
}

// Admin is the Atlassian Admin API, called with the org API key.
type Admin interface {
	// Orgs lists the organizations the key can manage (GET /admin/v1/orgs).
	Orgs(ctx context.Context) ([]Org, error)
	// FindUser looks the operator's account up in the organization by its
	// email, with the Admin API user search. It returns an error wrapping
	// httperr.ErrNotFound when the organization has no such account.
	FindUser(ctx context.Context, orgID string, email credentials.Sensitive) (Account, error)
}

// Tenant is what a site's tenant_info gives.
type Tenant struct {
	CloudID string
	// ServerTime is when the site sent its answer (the Date header), or zero
	// when the answer did not say.
	ServerTime time.Time
}

// Org is an organization the org API key can manage.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Account is the operator's Atlassian account.
type Account struct {
	AccountID string `json:"account_id"`
}

// API names one of the public JSM APIs that are still being built. The
// steps that need one decide what to do when a site does not have it.
type API string

const (
	// ServicesAPI creates services.
	ServicesAPI API = "services"
	// UserNotificationsAPI lets an admin set other users' contacts and
	// notification rules.
	UserNotificationsAPI API = "user_notifications"
	// GlobalEscalationsAPI creates escalations that no team owns.
	GlobalEscalationsAPI API = "global_escalations"
)

// Placeholders returns clients whose every call fails with
// httperr.ErrUnavailable, for builds made before the PagerDuty and JSM
// clients exist. Every check that needs one reports that it could not run,
// all for the same reason, so the output can say it once.
func Placeholders() Clients {
	return Clients{PagerDuty: placeholder{}, Atlassian: placeholder{}, Admin: placeholder{}}
}

type placeholder struct{}

func (placeholder) err() error { return notBuilt{} }

// notBuilt is an httperr.ErrUnavailable that says why.
type notBuilt struct{}

func (notBuilt) Error() string        { return "the API clients are not built yet" }
func (notBuilt) Is(target error) bool { return target == httperr.ErrUnavailable }

func (p placeholder) Abilities(context.Context) ([]string, error)        { return nil, p.err() }
func (p placeholder) TenantInfo(context.Context, string) (Tenant, error) { return Tenant{}, p.err() }
func (p placeholder) CheckAccess(context.Context, string) error          { return p.err() }
func (p placeholder) HasAPI(context.Context, string, API) (bool, error)  { return false, p.err() }
func (p placeholder) Orgs(context.Context) ([]Org, error)                { return nil, p.err() }
func (p placeholder) FindUser(context.Context, string, credentials.Sensitive) (Account, error) {
	return Account{}, p.err()
}
