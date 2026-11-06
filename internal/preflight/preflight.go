// Package preflight holds the checks login makes before it stores anything
// and plan runs before it reads anything: the credentials work, the site
// still has the saved cloudId, the operator's account is in the organization,
// the stated JSM plan, the local clock, and which new public APIs the site
// has. A check that fails never stops the others, so one run reports every
// problem.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Status is a check's result.
type Status string

const (
	OK      Status = "ok"
	Warning Status = "warning"
	Failed  Status = "failed"
	// Missing means a credential or an answer the check needs was not
	// given. The command then exits with the code for awaiting input.
	Missing Status = "missing"
	// NotChecked means the check could not run, for example because the
	// API could not be reached.
	NotChecked Status = "not_checked"
)

// ID names a check.
type ID string

const (
	SecretStorage     ID = "secret_storage"
	PagerDutyKey      ID = "pagerduty_api_key"
	Site              ID = "site"
	AtlassianToken    ID = "atlassian_api_token"
	OrgKey            ID = "org_api_key"
	OrgAdmin          ID = "org_admin"
	JSMPlan           ID = "jsm_plan"
	Clock             ID = "clock"
	Services          ID = "services_api"
	UserNotifications ID = "user_notifications_api"
	GlobalEscalations ID = "global_escalations_api"
)

var titles = map[ID]string{
	SecretStorage:     "Secret storage",
	PagerDutyKey:      "PagerDuty API key",
	Site:              "Site",
	AtlassianToken:    "Atlassian email and API token",
	OrgKey:            "Org API key",
	OrgAdmin:          "Org admin",
	JSMPlan:           "JSM plan",
	Clock:             "Clock",
	Services:          "Services API",
	UserNotifications: "Contacts and notification rules API",
	GlobalEscalations: "Global escalations API",
}

// Title is how the check is named in the report.
func (id ID) Title() string { return titles[id] }

// The checks in the order they run: each one may use what an earlier one
// found, such as the cloudId from the site check.
var (
	// All is every check, the set plan runs.
	All = []ID{SecretStorage, PagerDutyKey, Site, AtlassianToken, OrgKey, OrgAdmin, JSMPlan, Clock,
		Services, UserNotifications, GlobalEscalations}
	// Login is the set login runs before it stores anything.
	Login = []ID{PagerDutyKey, Site, AtlassianToken, OrgKey, OrgAdmin}
	// PagerDutySide and JSMSide split All by the system they check, for
	// plan --only.
	PagerDutySide = []ID{SecretStorage, PagerDutyKey}
	JSMSide       = []ID{SecretStorage, Site, AtlassianToken, OrgKey, OrgAdmin, JSMPlan, Clock,
		Services, UserNotifications, GlobalEscalations}
)

var apiChecks = map[ID]API{
	Services:          ServicesAPI,
	UserNotifications: UserNotificationsAPI,
	GlobalEscalations: GlobalEscalationsAPI,
}

// JSMPlans are the plans an operator can state. No API reports a site's plan.
var JSMPlans = []string{"free", "standard", "premium", "enterprise"}

// Clock limits. JSM rejects an override that starts more than 10 minutes in
// the past, so a clock off by minutes can make apply's writes fail.
const (
	ClockWarnAfter = time.Minute
	ClockFailAfter = 5 * time.Minute
)

// Input is what the checks run on.
type Input struct {
	Credentials credentials.Set
	// Sources says where each credential came from, for example "from
	// JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY", and is added to its check's detail.
	Sources map[credentials.Kind]string
	// SecretStorage names where the workspace keeps its secrets.
	SecretStorage string
	Site          string
	// CloudID is the saved cloudId; empty when there is none yet.
	CloudID string
	OrgID   string
	JSMPlan string
	// Checks are the checks to run, in any order; nil runs All.
	Checks []ID
	// Now is the local clock; nil means time.Now.
	Now func() time.Time
}

// Check is one check's result.
type Check struct {
	ID     ID     `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the result of a run.
type Report struct {
	Checks []Check `json:"checks"`
	// CloudID is the cloudId the site gave, or empty when it was not looked
	// up.
	CloudID string `json:"cloud_id,omitempty"`
	// Orgs are the organizations the org API key can manage, when it was
	// checked.
	Orgs []Org `json:"orgs,omitempty"`
	// Operator is the operator's account, when the org admin check found it.
	// The confirmation before apply records it.
	Operator *Account `json:"operator,omitempty"`
}

// Find returns the result of check id.
func (r Report) Find(id ID) (Check, bool) {
	for _, c := range r.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

// With returns the checks with the given status.
func (r Report) With(s Status) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == s {
			out = append(out, c)
		}
	}
	return out
}

// Run makes the checks in in.Checks. It returns an error only when ctx ends;
// every other problem is a check result.
func Run(ctx context.Context, c Clients, in Input) (Report, error) {
	if in.Now == nil {
		in.Now = time.Now
	}
	want := in.Checks
	if want == nil {
		want = All
	}
	r := &run{ctx: ctx, c: c, in: in}
	for _, id := range All {
		if !slices.Contains(want, id) {
			continue
		}
		status, detail := r.check(id)
		if err := ctx.Err(); err != nil {
			return r.report, err
		}
		r.report.Checks = append(r.report.Checks, Check{ID: id, Title: id.Title(), Status: status, Detail: detail})
	}
	return r.report, nil
}

type run struct {
	ctx    context.Context
	c      Clients
	in     Input
	report Report

	tenant    *Tenant
	tenantErr error
}

func (r *run) check(id ID) (Status, string) {
	switch id {
	case SecretStorage:
		return r.secretStorage()
	case PagerDutyKey:
		return r.pagerDutyKey()
	case Site:
		return r.site()
	case AtlassianToken:
		return r.atlassianToken()
	case OrgKey:
		return r.orgKey()
	case OrgAdmin:
		return r.orgAdmin()
	case JSMPlan:
		return r.jsmPlan()
	case Clock:
		return r.clock()
	}
	if api, ok := apiChecks[id]; ok {
		return r.api(api)
	}
	return NotChecked, "unknown check"
}

func (r *run) secretStorage() (Status, string) {
	if r.in.SecretStorage == "" {
		return NotChecked, "unknown"
	}
	return OK, "kept in " + r.in.SecretStorage
}

func (r *run) pagerDutyKey() (Status, string) {
	if !r.in.Credentials.Has(credentials.PagerDutyKey) {
		return Missing, missing(credentials.PagerDutyKey)
	}
	_, err := r.c.PagerDuty.Abilities(r.ctx)
	switch {
	case err == nil:
		// PagerDuty does not say whether a key is read-only, so this is a
		// reminder, not a check.
		return OK, "works" + r.source(credentials.PagerDutyKey) +
			"; PagerDuty cannot tell whether it is read-only, and jsm-ops-migrator only reads with it"
	case errors.Is(err, httperr.ErrUnauthorized):
		return Failed, "PagerDuty rejected the key"
	case errors.Is(err, httperr.ErrForbidden):
		return Failed, "PagerDuty does not allow this key to read the account"
	}
	return notChecked(err)
}

func (r *run) lookUpTenant() (*Tenant, error) {
	if r.tenant == nil && r.tenantErr == nil {
		t, err := r.c.Atlassian.TenantInfo(r.ctx, r.in.Site)
		if err != nil {
			r.tenantErr = err
		} else {
			r.tenant = &t
		}
	}
	return r.tenant, r.tenantErr
}

func (r *run) site() (Status, string) {
	if r.in.Site == "" {
		return Missing, "no site: run jsm-ops-migrator login, or pass --site"
	}
	t, err := r.lookUpTenant()
	switch {
	case errors.Is(err, httperr.ErrNotFound):
		return Failed, "there is no Atlassian site at " + r.in.Site
	case err != nil:
		return notChecked(err)
	case t.CloudID == "":
		return Failed, r.in.Site + " did not give a cloudId"
	}
	r.report.CloudID = t.CloudID
	if r.in.CloudID != "" && r.in.CloudID != t.CloudID {
		return Failed, fmt.Sprintf("%s now has cloudId %s, but %s is saved: it may have been moved or recreated, so run jsm-ops-migrator login again",
			r.in.Site, t.CloudID, r.in.CloudID)
	}
	return OK, fmt.Sprintf("%s, cloudId %s", r.in.Site, t.CloudID)
}

// cloudID is the one the site gave, or else the saved one.
func (r *run) cloudID() string {
	if r.report.CloudID != "" {
		return r.report.CloudID
	}
	return r.in.CloudID
}

func (r *run) atlassianToken() (Status, string) {
	if m := r.in.Credentials.Missing(credentials.AtlassianEmail, credentials.AtlassianToken); len(m) > 0 {
		return Missing, missing(m...)
	}
	cloudID := r.cloudID()
	if cloudID == "" {
		return NotChecked, "needs the site's cloudId"
	}
	err := r.c.Atlassian.CheckAccess(r.ctx, cloudID)
	switch {
	case err == nil:
		return OK, "works on the site" + r.source(credentials.AtlassianToken)
	case errors.Is(err, httperr.ErrUnauthorized):
		return Failed, "the site rejected the email and API token"
	case errors.Is(err, httperr.ErrForbidden):
		return Failed, "the account may not use JSM Operations on this site"
	}
	return notChecked(err)
}

func (r *run) orgKey() (Status, string) {
	if !r.in.Credentials.Has(credentials.OrgAPIKey) {
		return Missing, missing(credentials.OrgAPIKey)
	}
	orgs, err := r.c.Admin.Orgs(r.ctx)
	switch {
	case errors.Is(err, httperr.ErrUnauthorized):
		return Failed, "the Admin API rejected the key"
	case err != nil:
		return notChecked(err)
	}
	r.report.Orgs = orgs
	if r.in.OrgID == "" {
		return Missing, "the key works, but no organization is set: run jsm-ops-migrator login, or pass --org-id"
	}
	if !slices.ContainsFunc(orgs, func(o Org) bool { return o.ID == r.in.OrgID }) {
		return Failed, "the key cannot manage organization " + r.in.OrgID
	}
	return OK, "works for organization " + r.in.OrgID + r.source(credentials.OrgAPIKey)
}

func (r *run) orgAdmin() (Status, string) {
	switch {
	case !r.in.Credentials.Has(credentials.OrgAPIKey) || !r.in.Credentials.Has(credentials.AtlassianEmail):
		return NotChecked, "needs the org API key and the Atlassian account email"
	case r.in.OrgID == "":
		return NotChecked, "needs the organization"
	}
	acct, err := r.c.Admin.FindUser(r.ctx, r.in.OrgID, r.in.Credentials.AtlassianEmail)
	switch {
	case errors.Is(err, httperr.ErrNotFound):
		return Failed, "the Atlassian account is not in organization " + r.in.OrgID
	case errors.Is(err, httperr.ErrUnauthorized), errors.Is(err, httperr.ErrForbidden):
		return Failed, "the org API key may not search the organization's users"
	case err != nil:
		return notChecked(err)
	}
	r.report.Operator = &acct
	// No API is known to return the org admin role, so finding the account is
	// as far as the check can go.
	return Warning, "the account is in the organization, but no API shows whether it is an org admin: make sure it is"
}

func (r *run) jsmPlan() (Status, string) {
	switch {
	case r.in.JSMPlan == "":
		return Missing, "not stated: pass --jsm-plan " + strings.Join(JSMPlans, ", ")
	case !slices.Contains(JSMPlans, r.in.JSMPlan):
		return Failed, fmt.Sprintf("unknown plan %q: want one of %s", r.in.JSMPlan, strings.Join(JSMPlans, ", "))
	}
	return OK, r.in.JSMPlan + ", as stated; no API reports a site's plan"
}

func (r *run) clock() (Status, string) {
	if r.in.Site == "" {
		return NotChecked, "needs the site"
	}
	t, err := r.lookUpTenant()
	if err != nil {
		return notChecked(err)
	}
	if t.ServerTime.IsZero() {
		return NotChecked, "the site's answer did not give its time"
	}
	skew := r.in.Now().Sub(t.ServerTime)
	off := skew.Abs().Round(time.Second)
	direction := "ahead of"
	if skew < 0 {
		direction = "behind"
	}
	switch {
	case off <= ClockWarnAfter:
		return OK, "matches the site's clock"
	case off <= ClockFailAfter:
		return Warning, fmt.Sprintf("%s %s the site's clock; set it right before apply", off, direction)
	}
	return Failed, fmt.Sprintf("%s %s the site's clock; JSM rejects override starts more than 10 minutes in the past, so set it right first", off, direction)
}

func (r *run) api(api API) (Status, string) {
	switch {
	case len(r.in.Credentials.Missing(credentials.AtlassianEmail, credentials.AtlassianToken)) > 0:
		return NotChecked, "needs the Atlassian email and API token"
	case r.cloudID() == "":
		return NotChecked, "needs the site's cloudId"
	}
	ok, err := r.c.Atlassian.HasAPI(r.ctx, r.cloudID(), api)
	switch {
	case err != nil:
		return notChecked(err)
	case ok:
		return OK, "available"
	}
	return Warning, "not available on this site yet"
}

func (r *run) source(k credentials.Kind) string {
	if s := r.in.Sources[k]; s != "" {
		return " (" + s + ")"
	}
	return ""
}

func missing(kinds ...credentials.Kind) string {
	var labels, vars []string
	for _, k := range kinds {
		labels = append(labels, k.Label())
		vars = append(vars, k.EnvVar())
	}
	return fmt.Sprintf("no %s: run jsm-ops-migrator login, or set %s", strings.Join(labels, " or "), strings.Join(vars, " and "))
}

// notChecked is the result of a check whose call failed for a reason that
// says nothing about the credential. The status already says the check did
// not run, so the detail is only the reason.
func notChecked(err error) (Status, string) {
	return NotChecked, credentials.RedactText(err.Error())
}
