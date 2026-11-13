// Package jsmfake is a fake of the Atlassian APIs the JSM clients call. It
// answers as one site in one organization, reached with one Atlassian account
// and one org API key: JSM Operations and the Admin API on the Atlassian API
// host, and the Teams REST API, the Jira REST API and tenant_info on the site.
// Each API checks its own credential and pages its lists its own way, and the
// Teams API rejects a request without the siteId it needs. The tests that use
// it register the paths they call, with the answers or the list items to page
// through, as for pdfake.
package jsmfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
)

// The hosts the Atlassian API is served from.
const (
	APIHost        = "api.atlassian.com"
	APIHostStaging = "api.stg.atlassian.com"
)

// The path prefixes of each API.
const (
	opsPrefix    = "/jsm/ops/api/"
	adminPrefix  = "/admin/"
	teamsPrefix  = "/gateway/api/public/teams/"
	jiraPrefix   = "/rest/api/3/"
	tenantInfo   = "/_edge/tenant_info"
	siteIDParam  = "siteId"
	maxOpsSize   = 50
	defaultPage  = 25
	maxBodyBytes = 1 << 20
)

// T is what the fake reports a wrong request to, such as a *testing.T.
type T interface {
	Helper()
	Errorf(format string, args ...any)
}

// Account is the site, organization and credentials the fake answers as.
type Account struct {
	// Site is the site's host, such as acme.atlassian.net.
	Site    string
	CloudID string
	OrgID   string
	// Email and Token are the Atlassian account the JSM Operations, Teams
	// and Jira calls must carry.
	Email, Token string
	// OrgKey is the org API key the Admin API calls must carry.
	OrgKey string
	// Staging serves the Atlassian API on APIHostStaging instead of APIHost.
	Staging bool
	// Now is the site's clock, sent in tenant_info's Date header; nil means
	// time.Now.
	Now func() time.Time
}

// Request is one request the fake received.
type Request struct {
	Method string
	// Host is the host the request was sent to.
	Host  string
	Path  string
	Query url.Values
	// Body is the request's body, or nil when it had none.
	Body json.RawMessage
}

// Fault is an answer the fake gives instead of the path's own answer.
type Fault struct {
	Status int
	// Header is added to the answer, such as Retry-After.
	Header http.Header
	// Body is the answer, encoded as JSON; nil means an error body for the
	// status.
	Body any
	// Raw, when set, is the answer as it is, such as one that is not JSON.
	Raw []byte
	// Delay holds the answer back, so a test can tell whether calls overlap
	// (see Peak).
	Delay time.Duration
}

type route struct {
	status int
	answer any
	list   *list
}

type list struct {
	key   string
	items []any
}

// Option changes how a Fake answers.
type Option func(*Fake)

// PageSize is the page size of the lists that the API pages itself: the
// Teams API's cursor lists and the Admin API's linked lists. JSM Operations
// and Teams member lists use the size the request asks for.
func PageSize(n int) Option { return func(f *Fake) { f.pageSize = n } }

// Fake is a fake site and organization. It is safe for concurrent use.
type Fake struct {
	t        T
	a        Account
	pageSize int

	mu             sync.Mutex
	routes         map[string]route
	faults         map[string][]Fault
	log            []Request
	inFlight, peak int
}

// New returns a fake for a that answers only tenant_info until paths are
// registered.
func New(t T, a Account, opts ...Option) *Fake {
	f := &Fake{t: t, a: a, pageSize: defaultPage, routes: map[string]route{}, faults: map[string][]Fault{}}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Account is what the fake answers as.
func (f *Fake) Account() Account { return f.a }

// OpsPath is the full path of a JSM Operations path, such as /v1/teams.
func (f *Fake) OpsPath(p string) string { return opsPrefix + f.a.CloudID + p }

// TeamsPath is the full path of a Teams API path, such as /teams.
func (f *Fake) TeamsPath(p string) string {
	return teamsPrefix + "v1/org/" + f.a.OrgID + p
}

// AdminPath is the full path of an Admin API path, such as /v1/orgs.
func (f *Fake) AdminPath(p string) string { return "/admin" + p }

// JiraPath is the full path of a Jira REST API path, such as /myself.
func (f *Fake) JiraPath(p string) string { return "/rest/api/3" + p }

func routeKey(method, path string) string { return method + " " + path }

// Answer makes method path, a full path such as OpsPath("/v1/teams"), answer
// status with body encoded as JSON.
func (f *Fake) Answer(method, path string, status int, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[routeKey(method, path)] = route{status: status, answer: body}
}

// List makes method path a list of items under key, paged the way its API
// pages: JSM Operations by offset and size with links.next, the Teams API by
// cursor (GET) or pageInfo (POST), and the Admin API by links.next.
func (f *Fake) List(method, path, key string, items []any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[routeKey(method, path)] = route{list: &list{key: key, items: items}}
}

// Fail makes the next requests for method path answer with faults, one per
// request in order, before the path answers as registered again.
func (f *Fake) Fail(method, path string, faults ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := routeKey(method, path)
	f.faults[k] = append(f.faults[k], faults...)
}

// Requests returns every request the fake received, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.log...)
}

// Peak is the most requests the fake was answering at one time.
func (f *Fake) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// Transport returns a RoundTripper that serves requests in-process. The HTTP
// layer's host checks and credentials apply before it.
func (f *Fake) Transport() http.RoundTripper { return transport{f} }

// Config returns HTTP layer settings for a client that reaches f with its
// account's credentials, retries fast and has no rate limit.
func (f *Fake) Config() httpx.Config {
	return httpx.Config{
		Credentials: credentials.Set{}.
			With(credentials.AtlassianEmail, f.a.Email).
			With(credentials.AtlassianToken, f.a.Token).
			With(credentials.OrgAPIKey, f.a.OrgKey),
		Staging:   f.a.Staging,
		Transport: f.Transport(),
		Retry:     httpx.Retry{MaxAttempts: 3, MinWait: time.Millisecond, MaxWait: 2 * time.Millisecond},
		Limits:    map[httpx.Family]httpx.Limit{},
	}
}

// Client returns an HTTP layer client built from Config.
func (f *Fake) Client() *httpx.Client {
	h, err := httpx.New(f.Config())
	if err != nil {
		panic(err)
	}
	return h
}

type transport struct{ f *Fake }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	in := r.Clone(r.Context())
	in.Host = r.URL.Host
	in.RequestURI = r.URL.RequestURI()
	in.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	t.f.ServeHTTP(rec, in)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

type family int

const (
	unknownFamily family = iota
	opsFamily
	adminFamily
	teamsFamily
	jiraFamily
	tenantFamily
)

func (f *Fake) apiHost() string {
	if f.a.Staging {
		return APIHostStaging
	}
	return APIHost
}

// familyOf names the API a request is for, from its host and path.
func (f *Fake) familyOf(host, path string) family {
	switch {
	case host == f.apiHost() && strings.HasPrefix(path, opsPrefix):
		return opsFamily
	case host == f.apiHost() && strings.HasPrefix(path, adminPrefix):
		return adminFamily
	case host == f.a.Site && strings.HasPrefix(path, teamsPrefix):
		return teamsFamily
	case host == f.a.Site && strings.HasPrefix(path, jiraPrefix):
		return jiraFamily
	case host == f.a.Site && path == tenantInfo:
		return tenantFamily
	}
	return unknownFamily
}

// ServeHTTP answers r as the site and organization would.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.Query()}
	if len(body) > 0 {
		req.Body = body
	}
	f.mu.Lock()
	f.log = append(f.log, req)
	f.inFlight++
	f.peak = max(f.peak, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	fam := f.familyOf(r.Host, r.URL.Path)
	switch {
	case fam == unknownFamily && (r.Host == APIHost || r.Host == APIHostStaging):
		// The other environment's API host does not know this account.
		fail(w, http.StatusUnauthorized)
		return
	case fam == unknownFamily && r.Host != f.a.Site:
		f.t.Helper()
		f.t.Errorf("jsmfake: got a request for %s, which is not this site or the Atlassian API host", r.Host)
		fail(w, http.StatusMisdirectedRequest)
		return
	case !f.authorized(fam, r):
		fail(w, http.StatusUnauthorized)
		return
	case fam == teamsFamily && !f.hasSiteID(r, body):
		write(w, http.StatusBadRequest, map[string]any{"code": "SITE_ID_REQUIRED_FOR_TEAM_API", "message": "siteId is required"})
		return
	}

	k := routeKey(r.Method, r.URL.Path)
	f.mu.Lock()
	var fault *Fault
	if q := f.faults[k]; len(q) > 0 {
		fault, f.faults[k] = &q[0], q[1:]
	}
	rt, ok := f.routes[k]
	f.mu.Unlock()

	switch {
	case fault != nil:
		if fault.Delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(fault.Delay):
			}
		}
		for name, vs := range fault.Header {
			for _, v := range vs {
				w.Header().Add(name, v)
			}
		}
		switch {
		case fault.Raw != nil:
			w.WriteHeader(fault.Status)
			_, _ = w.Write(fault.Raw)
		case fault.Body == nil:
			fail(w, fault.Status)
		default:
			write(w, fault.Status, fault.Body)
		}
	case ok && rt.list != nil:
		f.page(w, fam, r, body, rt.list)
	case ok:
		write(w, rt.status, rt.answer)
	case fam == tenantFamily && r.Method == http.MethodGet:
		f.tenantInfo(w)
	default:
		fail(w, http.StatusNotFound)
	}
}

// authorized checks the credential each API takes. tenant_info takes none,
// and a credential sent there fails the test, because it would reach the site
// for nothing.
func (f *Fake) authorized(fam family, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	switch fam {
	case tenantFamily:
		if auth != "" {
			f.t.Helper()
			f.t.Errorf("jsmfake: tenant_info was sent a credential")
		}
		return true
	case adminFamily:
		return f.a.OrgKey != "" && auth == "Bearer "+f.a.OrgKey
	default:
		email, token, ok := r.BasicAuth()
		return ok && f.a.Email != "" && email == f.a.Email && token == f.a.Token
	}
}

// hasSiteID checks the siteId the Teams API needs: in the body of a team
// create, and in the query of a read. The API documents none on its other
// writes.
func (f *Fake) hasSiteID(r *http.Request, body []byte) bool {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/teams"):
		var b map[string]any
		return json.Unmarshal(body, &b) == nil && b[siteIDParam] == f.a.CloudID
	case r.Method == http.MethodGet, r.Method == http.MethodPost && strings.HasSuffix(path, "/members"):
		return r.URL.Query().Get(siteIDParam) == f.a.CloudID
	}
	return true
}

func (f *Fake) tenantInfo(w http.ResponseWriter) {
	now := time.Now
	if f.a.Now != nil {
		now = f.a.Now
	}
	w.Header().Set("Date", now().UTC().Format(http.TimeFormat))
	write(w, http.StatusOK, map[string]any{"cloudId": f.a.CloudID, "hostname": f.a.Site})
}

// page answers one page of l the way fam pages it.
func (f *Fake) page(w http.ResponseWriter, fam family, r *http.Request, body []byte, l *list) {
	q := r.URL.Query()
	switch {
	case fam == opsFamily:
		size, err := strconv.Atoi(q.Get("size"))
		if err != nil || size < 1 || size > maxOpsSize {
			write(w, http.StatusBadRequest, map[string]any{"message": fmt.Sprintf("size must be between 1 and %d", maxOpsSize)})
			return
		}
		offset, err := strconv.Atoi(q.Get("offset"))
		if err != nil || offset < 0 {
			write(w, http.StatusBadRequest, map[string]any{"message": "offset must be 0 or more"})
			return
		}
		items, end := window(l.items, offset, size)
		links := map[string]any{}
		if end < len(l.items) {
			// A relative path without the cloudId, as JSM Operations gives it.
			rel := strings.TrimPrefix(r.URL.Path, opsPrefix+f.a.CloudID)
			links["next"] = fmt.Sprintf("%s?offset=%d&size=%d", rel, end, size)
		}
		write(w, http.StatusOK, map[string]any{l.key: items, "links": links})
	case fam == teamsFamily && r.Method == http.MethodPost:
		var b struct {
			First int    `json:"first"`
			After string `json:"after"`
		}
		if json.Unmarshal(body, &b) != nil || b.First < 1 {
			write(w, http.StatusBadRequest, map[string]any{"message": "first must be at least 1"})
			return
		}
		offset, ok := cursorOffset(b.After)
		if !ok {
			write(w, http.StatusBadRequest, map[string]any{"message": "unknown cursor"})
			return
		}
		items, end := window(l.items, offset, b.First)
		write(w, http.StatusOK, map[string]any{l.key: items,
			"pageInfo": map[string]any{"hasNextPage": end < len(l.items), "endCursor": cursorOf(end)}})
	case fam == teamsFamily:
		offset, ok := cursorOffset(q.Get("cursor"))
		if !ok {
			write(w, http.StatusBadRequest, map[string]any{"message": "unknown cursor"})
			return
		}
		items, end := window(l.items, offset, f.pageSize)
		var next any
		if end < len(l.items) {
			next = cursorOf(end)
		}
		write(w, http.StatusOK, map[string]any{l.key: items, "cursor": next})
	case fam == adminFamily:
		offset, ok := cursorOffset(q.Get("cursor"))
		if !ok {
			write(w, http.StatusBadRequest, map[string]any{"message": "unknown cursor"})
			return
		}
		items, end := window(l.items, offset, f.pageSize)
		links := map[string]any{"self": "https://" + f.apiHost() + r.URL.Path}
		if end < len(l.items) {
			links["next"] = "https://" + f.apiHost() + r.URL.Path + "?cursor=" + cursorOf(end)
		}
		write(w, http.StatusOK, map[string]any{l.key: items, "links": links})
	default:
		f.t.Helper()
		f.t.Errorf("jsmfake: %s %s is registered as a list, but this API has no list paging", r.Method, r.URL.Path)
		fail(w, http.StatusInternalServerError)
	}
}

// window returns items[offset:offset+size], clamped, and where it ends.
func window(items []any, offset, size int) ([]any, int) {
	end := min(offset+size, len(items))
	if offset >= len(items) {
		return []any{}, len(items)
	}
	return items[offset:end], end
}

func cursorOf(offset int) string { return "c" + strconv.Itoa(offset) }

func cursorOffset(c string) (int, bool) {
	if c == "" {
		return 0, true
	}
	n, err := strconv.Atoi(strings.TrimPrefix(c, "c"))
	return n, err == nil && strings.HasPrefix(c, "c") && n >= 0
}

// fail writes an error body for status.
func fail(w http.ResponseWriter, status int) {
	write(w, status, map[string]any{"code": status, "message": http.StatusText(status)})
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
