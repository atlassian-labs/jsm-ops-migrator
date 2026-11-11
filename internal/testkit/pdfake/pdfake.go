// Package pdfake is a fake PagerDuty REST API for tests. It answers as one
// account, hosted in one region and reached with one API key, the way
// PagerDuty answers: lists are paged by limit and offset up to the 10,000
// window, and the classic and v3 paths each want their own Accept header. The
// tests that use it register the paths they read, with the answers or the
// list items to page through. Any request other than a GET fails the test,
// because the CLI must never write to PagerDuty.
package pdfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
)

// The hosts PagerDuty serves each region from.
const (
	HostUS = "api.pagerduty.com"
	HostEU = "api.eu.pagerduty.com"
)

// The Accept headers PagerDuty's classic (v2) and v3 paths take.
const (
	AcceptV2 = "application/vnd.pagerduty+json;version=2"
	AcceptV3 = "application/json"
)

// T is what the fake reports a wrong request to, such as a *testing.T. A
// program that serves the fake, as the binary's tests do, passes anything
// that makes the run fail.
type T interface {
	Helper()
	Errorf(format string, args ...any)
}

// Fake is a fake PagerDuty account. It is safe for concurrent use.
type Fake struct {
	t      T
	key    string
	region httpx.Region

	mu     sync.Mutex
	routes map[string]route
	faults map[string][]Fault
	log    []Request
}

// Request is one request the fake received.
type Request struct {
	Method string
	// Host is the host the request was sent to, such as api.pagerduty.com.
	Host   string
	Path   string
	Query  url.Values
	Accept string
}

// Fault is a failure the fake answers with instead of the path's answer.
type Fault struct {
	Status int
	// Header is added to the answer, such as Retry-After or ratelimit-reset
	// on a 429.
	Header http.Header
	// Body is the answer; nil means PagerDuty's error body for the status.
	Body any
}

type route struct {
	answer any
	list   *list
}

type list struct {
	key      string
	items    []any
	maxLimit int
	keep     func(url.Values, any) bool
}

// Option changes how a Fake answers.
type Option func(*Fake)

// InRegion hosts the account in region, httpx.RegionUS when not given. A
// request to the other region's host is answered 401, as PagerDuty answers a
// key it does not know there.
func InRegion(region httpx.Region) Option { return func(f *Fake) { f.region = region } }

// New returns an account reached with key that answers nothing until paths
// are registered.
func New(t T, key string, opts ...Option) *Fake {
	f := &Fake{t: t, key: key, region: httpx.RegionUS, routes: map[string]route{}, faults: map[string][]Fault{}}
	for _, o := range opts {
		o(f)
	}
	return f
}

// Key is the API key the account accepts.
func (f *Fake) Key() string { return f.key }

// Answer makes GET path answer 200 with body, encoded as JSON.
func (f *Fake) Answer(path string, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = route{answer: body}
}

// ListOption changes how a list is paged.
type ListOption func(*list)

// MaxLimit is the largest page the list allows; the classic endpoints allow
// 100, and the v3 rotation and event lists 1,000.
func MaxLimit(n int) ListOption { return func(l *list) { l.maxLimit = n } }

// Filter answers only the items keep accepts for the request's query, such
// as the members of the team in team_ids[], or the names that hold query.
func Filter(keep func(query url.Values, item any) bool) ListOption {
	return func(l *list) { l.keep = keep }
}

// List makes GET path a list of items under key, paged as PagerDuty pages:
// limit (25 by default), offset, more, and total when the request asks with
// total=true. A request for a page past the 10,000 window is answered 400,
// because what PagerDuty answers there is not known and the CLI must never
// ask.
func (f *Fake) List(path, key string, items []any, opts ...ListOption) {
	l := &list{key: key, items: items, maxLimit: 100}
	for _, o := range opts {
		o(l)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = route{list: l}
}

// Fail makes the next GETs of path answer with faults, one per request in
// order, before the path answers normally again.
func (f *Fake) Fail(path string, faults ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[path] = append(f.faults[path], faults...)
}

// Requests returns every request the fake received, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.log...)
}

// Transport returns a RoundTripper that serves requests in-process, so a test
// that sends thousands of them opens no socket. The HTTP layer's host and
// GET-only guards run before it.
func (f *Fake) Transport() http.RoundTripper { return transport{f} }

// Config returns HTTP layer settings for a client in region that reaches f
// with its key, retries fast and has no rate limit.
func (f *Fake) Config(region httpx.Region) httpx.Config {
	return httpx.Config{
		Credentials:     credentials.Set{}.With(credentials.PagerDutyKey, f.key),
		PagerDutyRegion: region,
		Transport:       f.Transport(),
		Retry:           httpx.Retry{MaxAttempts: 3, MinWait: time.Millisecond, MaxWait: 2 * time.Millisecond},
		Limits:          map[httpx.Family]httpx.Limit{},
	}
}

// Client returns a pd.Client for f's account, in its own region.
func (f *Fake) Client() *pd.Client {
	h, err := httpx.New(f.Config(f.region))
	if err != nil {
		panic(err)
	}
	return pd.New(h)
}

type transport struct{ f *Fake }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	in := r.Clone(r.Context())
	in.Host = r.URL.Host
	in.RequestURI = r.URL.RequestURI()
	rec := httptest.NewRecorder()
	t.f.ServeHTTP(rec, in)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// ServeHTTP answers r as the account would.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.log = append(f.log, Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.Query(), Accept: r.Header.Get("Accept")})
	f.mu.Unlock()

	switch {
	case r.Method != http.MethodGet:
		f.t.Helper()
		f.t.Errorf("pdfake: got %s %s, but PagerDuty must only be read", r.Method, r.URL.Path)
		fail(w, http.StatusMethodNotAllowed, 2100, "Only GET is allowed")
		return
	case r.Host != HostUS && r.Host != HostEU:
		f.t.Helper()
		f.t.Errorf("pdfake: got a request for %s, which is not a PagerDuty API host", r.Host)
		fail(w, http.StatusMisdirectedRequest, 2100, "Not a PagerDuty API host")
		return
	case r.Host != f.host() || r.Header.Get("Authorization") != "Token token="+f.key:
		fail(w, http.StatusUnauthorized, 2006, "Invalid token")
		return
	case r.Header.Get("Accept") != accept(r.URL.Path):
		fail(w, http.StatusNotAcceptable, 2001, "Wrong Accept header for this path")
		return
	}

	f.mu.Lock()
	var fault *Fault
	if q := f.faults[r.URL.Path]; len(q) > 0 {
		fault, f.faults[r.URL.Path] = &q[0], q[1:]
	}
	rt, ok := f.routes[r.URL.Path]
	f.mu.Unlock()

	switch {
	case fault != nil:
		for k, vs := range fault.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if fault.Body == nil {
			fail(w, fault.Status, 0, http.StatusText(fault.Status))
			return
		}
		write(w, fault.Status, fault.Body)
	case !ok:
		fail(w, http.StatusNotFound, 2100, "Not Found")
	case rt.list != nil:
		f.page(w, r.URL.Query(), rt.list)
	default:
		write(w, http.StatusOK, rt.answer)
	}
}

func (f *Fake) host() string {
	if f.region == httpx.RegionEU {
		return HostEU
	}
	return HostUS
}

func accept(path string) string {
	if strings.HasPrefix(path, "/v3/") {
		return AcceptV3
	}
	return AcceptV2
}

func (f *Fake) page(w http.ResponseWriter, q url.Values, l *list) {
	limit, offset := 25, 0
	var err error
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > l.maxLimit {
			fail(w, http.StatusBadRequest, 2001, fmt.Sprintf("limit must be between 1 and %d", l.maxLimit))
			return
		}
	}
	if v := q.Get("offset"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			fail(w, http.StatusBadRequest, 2001, "offset must be 0 or more")
			return
		}
	}
	if offset+limit > pd.Window {
		fail(w, http.StatusBadRequest, 2001, "offset + limit must not pass 10000")
		return
	}
	items := l.items
	if l.keep != nil {
		items = nil
		for _, it := range l.items {
			if l.keep(q, it) {
				items = append(items, it)
			}
		}
	}
	end := min(offset+limit, len(items))
	pageItems := []any{}
	if offset < len(items) {
		pageItems = items[offset:end]
	}
	answer := map[string]any{l.key: pageItems, "limit": limit, "offset": offset, "more": end < len(items), "total": nil}
	if q.Get("total") == "true" {
		answer["total"] = len(items)
	}
	write(w, http.StatusOK, answer)
}

// fail writes PagerDuty's error body.
func fail(w http.ResponseWriter, status, code int, message string) {
	write(w, status, map[string]any{"error": map[string]any{"message": message, "code": code}})
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
