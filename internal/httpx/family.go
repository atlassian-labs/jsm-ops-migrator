package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// Family is a group of endpoints that share a host and a credential. The
// family decides where a request goes and what it carries, so a request can
// never pick its own host or credential.
type Family string

const (
	// PagerDuty is the PagerDuty REST API, v2 and v3, called with the
	// PagerDuty API key. Only GET is ever sent, and its answers come back
	// without calendar feed URLs.
	PagerDuty Family = "pagerduty"
	// JSMOps is the JSM Operations REST API on the Atlassian API host (paths
	// start with /jsm/ops/api/<cloudId>), called with the Atlassian account
	// email and API token.
	JSMOps Family = "jsm_ops"
	// Admin is the Atlassian Admin API on the Atlassian API host (paths start
	// with /admin), called with the org API key.
	Admin Family = "admin"
	// Site is a site's own APIs, the Teams REST API under
	// /gateway/api/public/teams and the Jira REST API under /rest/api/3,
	// called with the Atlassian account email and API token.
	Site Family = "site"
	// SiteAnonymous is a site called without a credential, for
	// /_edge/tenant_info.
	SiteAnonymous Family = "site_anonymous"
)

// Region is where a PagerDuty account is hosted.
type Region string

const (
	RegionUS Region = "us"
	RegionEU Region = "eu"
)

const (
	pagerDutyHostUS         = "api.pagerduty.com"
	pagerDutyHostEU         = "api.eu.pagerduty.com"
	atlassianAPIHost        = "api.atlassian.com"
	atlassianAPIHostStaging = "api.stg.atlassian.com"
)

// stagingSiteSuffix is the domain Atlassian's staging sites live under.
const stagingSiteSuffix = ".jira-dev.com"

// siteSuffixes are the domains Atlassian sites live under. The site families
// refuse any other host, so a mistyped site never receives the API token.
var siteSuffixes = []string{".atlassian.net", stagingSiteSuffix}

// IsStagingSite reports whether site, a host name such as acme.jira-dev.com,
// is a staging site. A staging site's JSM Operations and Admin calls go to
// the staging API host, so a client for one is built with Config.Staging.
func IsStagingSite(site string) bool {
	s := strings.ToLower(site)
	return strings.HasSuffix(s, stagingSiteSuffix) && len(s) > len(stagingSiteSuffix)
}

// needs lists the credentials each family sends; a family missing from it is
// unknown.
var needs = map[Family][]credentials.Kind{
	PagerDuty:     {credentials.PagerDutyKey},
	JSMOps:        {credentials.AtlassianEmail, credentials.AtlassianToken},
	Admin:         {credentials.OrgAPIKey},
	Site:          {credentials.AtlassianEmail, credentials.AtlassianToken},
	SiteAnonymous: nil,
}

func (f Family) known() bool { _, ok := needs[f]; return ok }

// host returns the host a request of family f goes to.
func (c *Client) host(f Family, site string) (string, error) {
	switch f {
	case PagerDuty:
		if c.cfg.PagerDutyRegion == RegionEU {
			return pagerDutyHostEU, nil
		}
		return pagerDutyHostUS, nil
	case JSMOps, Admin:
		if c.cfg.Staging {
			return atlassianAPIHostStaging, nil
		}
		return atlassianAPIHost, nil
	default:
		return siteHost(site)
	}
}

// authorize adds the family's credential to r. It is the one place where a
// credential leaves its Secret.
func (c *Client) authorize(r *http.Request, f Family) {
	creds := c.cfg.Credentials
	switch f {
	case PagerDuty:
		r.Header.Set("Authorization", "Token token="+creds.PagerDutyKey.Reveal())
	case JSMOps, Site:
		r.SetBasicAuth(creds.AtlassianEmail.Value(), creds.AtlassianToken.Reveal())
	case Admin:
		r.Header.Set("Authorization", "Bearer "+creds.OrgAPIKey.Reveal())
	}
}

// isPagerDutyHost covers every PagerDuty host, not only the API's, so the
// guard holds even for a host this package does not know about.
func isPagerDutyHost(host string) bool {
	h := strings.ToLower(host)
	return h == "pagerduty.com" || strings.HasSuffix(h, ".pagerduty.com")
}

// siteHost checks that site is a bare host name under one of siteSuffixes.
// The errors never repeat the site: a mistyped one can hold a user name and
// password, or a pasted API token.
func siteHost(site string) (string, error) {
	host := strings.ToLower(site)
	u, err := url.Parse("https://" + host)
	if host == "" || err != nil || u.Host != host || u.Port() != "" {
		return "", errors.New("the site is not a host name such as acme.atlassian.net: it must have no scheme, user name, password, port or path")
	}
	for _, s := range siteSuffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return host, nil
		}
	}
	return "", fmt.Errorf("the site is not an Atlassian site: want a host under %s", strings.Join(siteSuffixes, " or "))
}
