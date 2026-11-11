package httpx_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

const (
	pdKey    = "pd-key-value-1234"
	email    = "operator@example.com"
	apiToken = "atlassian-token-value-1234"
	orgKey   = "org-key-value-1234"
)

func allCredentials() credentials.Set {
	return credentials.Set{}.
		With(credentials.PagerDutyKey, pdKey).
		With(credentials.AtlassianEmail, email).
		With(credentials.AtlassianToken, apiToken).
		With(credentials.OrgAPIKey, orgKey)
}

// fast keeps retries cheap in tests; the backoff itself is tested inside the
// package.
var fast = httpx.Retry{MaxAttempts: 3, MinWait: time.Millisecond, MaxWait: 2 * time.Millisecond}

// serve returns a Client whose every request goes to h, the Router in front
// of it, and the count of requests that reached h. Unless cfg says
// otherwise, the client has every credential, fast retries and no limits.
func serve(t *testing.T, cfg httpx.Config, h http.HandlerFunc) (*httpx.Client, *testkit.Router, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	router := testkit.RouteTo(srv)
	cfg.Transport = router
	if cfg.Credentials.IsZero() {
		cfg.Credentials = allCredentials()
	}
	if cfg.Retry == (httpx.Retry{}) {
		cfg.Retry = fast
	}
	if cfg.Limits == nil {
		cfg.Limits = map[httpx.Family]httpx.Limit{}
	}
	c, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, router, &hits
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }

func TestNewRejectsBadSettings(t *testing.T) {
	cases := map[string]httpx.Config{
		"region":          {PagerDutyRegion: "apac"},
		"timeout":         {Timeout: -time.Second},
		"no attempts":     {Retry: httpx.Retry{MinWait: time.Second}},
		"waits reversed":  {Retry: httpx.Retry{MaxAttempts: 2, MinWait: time.Minute, MaxWait: time.Second}},
		"unknown family":  {Limits: map[httpx.Family]httpx.Limit{"github": {PerMinute: 1}}},
		"negative limit":  {Limits: map[httpx.Family]httpx.Limit{httpx.PagerDuty: {PerMinute: -1}}},
		"empty family id": {Limits: map[httpx.Family]httpx.Limit{"": {}}},
	}
	for name, cfg := range cases {
		if _, err := httpx.New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", name, cfg)
		}
	}
	if _, err := httpx.New(httpx.Config{}); err != nil {
		t.Errorf("the zero Config must be valid: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	if r := httpx.DefaultRetry(); r.MaxAttempts < 2 || r.MinWait <= 0 || r.MaxWait < r.MinWait {
		t.Errorf("DefaultRetry %+v must retry with a real backoff", r)
	}
	pd := httpx.DefaultLimits()[httpx.PagerDuty]
	if !pd.Serial || pd.PerMinute <= 0 || pd.PerMinute >= 960 {
		t.Errorf("PagerDuty limit %+v must be serial and below 960 a minute", pd)
	}
}

// TestDoRefuses covers the requests that must never leave the machine.
func TestDoRefuses(t *testing.T) {
	all := allCredentials()
	cases := []struct {
		name  string
		creds credentials.Set
		req   httpx.Request
	}{
		{"PagerDuty POST", all, httpx.Request{Family: httpx.PagerDuty, Method: httpx.MethodPost, Path: "/users"}},
		{"PagerDuty PUT", all, httpx.Request{Family: httpx.PagerDuty, Method: httpx.MethodPut, Path: "/users/P1"}},
		{"PagerDuty PATCH", all, httpx.Request{Family: httpx.PagerDuty, Method: httpx.MethodPatch, Path: "/users/P1"}},
		{"PagerDuty DELETE", all, httpx.Request{Family: httpx.PagerDuty, Method: httpx.MethodDelete, Path: "/users/P1"}},
		{"PagerDuty idempotent POST", all, httpx.Request{Family: httpx.PagerDuty, Method: httpx.MethodPost, Path: "/users", Idempotent: true}},
		{"unknown method", all, httpx.Request{Family: httpx.JSMOps, Method: "TRACE", Path: "/v1/teams"}},
		{"unknown family", all, httpx.Request{Family: "github", Path: "/repos"}},
		{"no family", all, httpx.Request{Path: "/users"}},
		{"relative path", all, httpx.Request{Family: httpx.PagerDuty, Path: "users"}},
		{"host in path", all, httpx.Request{Family: httpx.PagerDuty, Path: "//evil.example.com/users"}},
		{"query in path", all, httpx.Request{Family: httpx.PagerDuty, Path: "/users?limit=1"}},
		{"fragment in path", all, httpx.Request{Family: httpx.PagerDuty, Path: "/users#x"}},
		{"dot segment", all, httpx.Request{Family: httpx.JSMOps, Path: "/jsm/ops/api/c1/../../../admin/v1/orgs"}},
		{"backslash", all, httpx.Request{Family: httpx.JSMOps, Path: `/jsm\ops`}},
		{"no PagerDuty key", all.Only(credentials.AtlassianEmail, credentials.AtlassianToken, credentials.OrgAPIKey),
			httpx.Request{Family: httpx.PagerDuty, Path: "/abilities"}},
		{"no API token", all.Only(credentials.AtlassianEmail, credentials.OrgAPIKey),
			httpx.Request{Family: httpx.JSMOps, Path: "/jsm/ops/api/c1/v1/teams"}},
		{"no email", all.Only(credentials.AtlassianToken), httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/rest/api/3/myself"}},
		{"no org key", all.Only(credentials.PagerDutyKey), httpx.Request{Family: httpx.Admin, Path: "/admin/v1/orgs"}},
		{"no site", all, httpx.Request{Family: httpx.Site, Path: "/rest/api/3/myself"}},
		{"not an Atlassian site", all, httpx.Request{Family: httpx.Site, Site: "acme.atlasian.net", Path: "/rest/api/3/myself"}},
		{"bare suffix", all, httpx.Request{Family: httpx.Site, Site: "atlassian.net", Path: "/rest/api/3/myself"}},
		{"suffix inside", all, httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net.example.com", Path: "/x"}},
		{"site with port", all, httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net:8443", Path: "/x"}},
		{"site with user", all, httpx.Request{Family: httpx.Site, Site: "me@acme.atlassian.net", Path: "/x"}},
		{"site with path", all, httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net/x", Path: "/x"}},
		{"site with scheme", all, httpx.Request{Family: httpx.SiteAnonymous, Site: "https://acme.atlassian.net", Path: "/_edge/tenant_info"}},
		{"anonymous site elsewhere", all, httpx.Request{Family: httpx.SiteAnonymous, Site: "example.com", Path: "/_edge/tenant_info"}},
		{"body that cannot be encoded", all, httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x", Body: map[string]any{"f": func() {}}}},
	}
	for _, c := range cases {
		client, router, _ := serve(t, httpx.Config{Credentials: c.creds}, ok)
		_, err := client.Do(context.Background(), c.req)
		if !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%s: got %v, want ErrRefused", c.name, err)
			continue
		}
		if n := len(router.Requests()); n != 0 {
			t.Errorf("%s: %d requests reached the transport", c.name, n)
		}
		var e *httperr.Error
		if !errors.As(err, &e) || e.Family != string(c.req.Family) || e.Path != c.req.Path {
			t.Errorf("%s: the error must name the request: %+v", c.name, e)
		}
		for _, secret := range []string{pdKey, apiToken, orgKey} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error shows a credential: %s", c.name, err)
			}
		}
	}
}

func TestDoSendsEachFamilyToItsHostWithItsCredential(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+apiToken))
	cases := []struct {
		name               string
		cfg                httpx.Config
		req                httpx.Request
		host, auth, accept string
	}{
		{"PagerDuty", httpx.Config{}, httpx.Request{Family: httpx.PagerDuty, Path: "/abilities", Accept: "application/vnd.pagerduty+json;version=2"},
			"api.pagerduty.com", "Token token=" + pdKey, "application/vnd.pagerduty+json;version=2"},
		{"PagerDuty EU", httpx.Config{PagerDutyRegion: httpx.RegionEU}, httpx.Request{Family: httpx.PagerDuty, Path: "/v3/schedules"},
			"api.eu.pagerduty.com", "Token token=" + pdKey, "application/json"},
		{"JSM Operations", httpx.Config{}, httpx.Request{Family: httpx.JSMOps, Path: "/jsm/ops/api/c1/v1/teams"},
			"api.atlassian.com", basic, "application/json"},
		{"Admin", httpx.Config{}, httpx.Request{Family: httpx.Admin, Path: "/admin/v1/orgs"},
			"api.atlassian.com", "Bearer " + orgKey, "application/json"},
		{"Admin on staging", httpx.Config{Staging: true}, httpx.Request{Family: httpx.Admin, Path: "/admin/v1/orgs"},
			"api.stg.atlassian.com", "Bearer " + orgKey, "application/json"},
		{"site", httpx.Config{}, httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/rest/api/3/myself"},
			"acme.atlassian.net", basic, "application/json"},
		{"site without a credential", httpx.Config{}, httpx.Request{Family: httpx.SiteAnonymous, Site: "acme.atlassian.net", Path: "/_edge/tenant_info"},
			"acme.atlassian.net", "", "application/json"},
	}
	for _, c := range cases {
		var got *http.Request
		client, _, _ := serve(t, c.cfg, func(w http.ResponseWriter, r *http.Request) {
			got = r
			w.Header().Set("X-Request-Id", "req-1")
			ok(w, r)
		})
		resp, err := client.Do(context.Background(), c.req)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Host != c.host || got.Header.Get("Authorization") != c.auth || got.Header.Get("Accept") != c.accept {
			t.Errorf("%s: host %q, accept %q, auth set %v", c.name, got.Host, got.Header.Get("Accept"), got.Header.Get("Authorization") != "")
		}
		if got.Header.Get("User-Agent") != "jsm-ops-migrator" || got.Header.Get("Content-Type") != "" {
			t.Errorf("%s: user agent %q, content type %q", c.name, got.Header.Get("User-Agent"), got.Header.Get("Content-Type"))
		}
		if resp.RequestID != "req-1" || resp.Header("X-Request-Id") != "req-1" || resp.Status != 200 {
			t.Errorf("%s: response %+v", c.name, resp)
		}
	}
}

func TestDoSendsTheQueryAndTheBody(t *testing.T) {
	var got struct {
		method, query, contentType string
		body                       map[string]any
	}
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.query, got.contentType = r.Method, r.URL.RawQuery, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"t1"}`)
	})
	var out struct{ ID string }
	resp, err := client.DoJSON(context.Background(), httpx.Request{
		Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/jsm/ops/api/c1/v1/teams",
		Query: url.Values{"expand": {"members"}}, Body: map[string]string{"name": "ops"},
	}, &out)
	if err != nil || resp.Status != 201 || out.ID != "t1" {
		t.Fatalf("DoJSON = %v, %+v", err, out)
	}
	if got.method != "POST" || got.query != "expand=members" || got.contentType != "application/json" || got.body["name"] != "ops" {
		t.Errorf("sent %+v", got)
	}
}

// TestErrorTaxonomy covers each line of the error taxonomy: what every answer
// maps to, and whether it is tried again.
func TestErrorTaxonomy(t *testing.T) {
	get, post := httpx.MethodGet, httpx.MethodPost
	cases := []struct {
		method     httpx.Method
		idempotent bool
		status     int
		want       error
		hits       int32
	}{
		{get, false, 400, httperr.ErrInvalid, 1},
		{get, false, 401, httperr.ErrUnauthorized, 1},
		{get, false, 402, httperr.ErrNotInPlan, 1},
		{get, false, 403, httperr.ErrForbidden, 1},
		{get, false, 404, httperr.ErrNotFound, 1},
		{get, false, 405, httperr.ErrInvalid, 1},
		{get, false, 410, httperr.ErrNotFound, 1},
		{post, false, 409, httperr.ErrConflict, 1},
		{post, false, 422, httperr.ErrInvalid, 1},
		{get, false, 302, httperr.ErrBadResponse, 1},
		{get, false, 429, httperr.ErrUnavailable, 3},
		{post, false, 429, httperr.ErrUnavailable, 3},
		{get, false, 500, httperr.ErrUnavailable, 3},
		{get, false, 501, httperr.ErrUnavailable, 1},
		{get, false, 503, httperr.ErrUnavailable, 3},
		{post, false, 500, httperr.ErrUnknownOutcome, 1},
		{post, false, 503, httperr.ErrUnknownOutcome, 1},
		{httpx.MethodPut, false, 502, httperr.ErrUnknownOutcome, 1},
		{httpx.MethodDelete, false, 504, httperr.ErrUnknownOutcome, 1},
		{post, true, 503, httperr.ErrUnavailable, 3},
	}
	for _, c := range cases {
		client, _, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Location", "https://evil.example.com/steal")
			w.WriteHeader(c.status)
		})
		_, err := client.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: c.method, Path: "/jsm/ops/api/c1/v1/teams", Idempotent: c.idempotent})
		if !errors.Is(err, c.want) || hits.Load() != c.hits {
			t.Errorf("%s %d (idempotent %v): got %v after %d calls, want %v after %d", c.method, c.status, c.idempotent, err, hits.Load(), c.want, c.hits)
		}
		var e *httperr.Error
		if errors.As(err, &e) && e.Status != c.status {
			t.Errorf("%s %d: the error says status %d", c.method, c.status, e.Status)
		}
	}
}

func TestRetriesUntilTheAPIAnswers(t *testing.T) {
	var n atomic.Int32
	client, _, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ok(w, r)
	})
	if _, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"}); err != nil || hits.Load() != 3 {
		t.Errorf("GET: got %v after %d calls", err, hits.Load())
	}

	// A 429 means the API did not act, so even a create goes again.
	n.Store(1)
	client, _, hits = serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	if _, err := client.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x"}); err != nil || hits.Load() != 2 {
		t.Errorf("POST after a 429: got %v after %d calls", err, hits.Load())
	}
}

func TestAChangeSentAgainAfterA429IsShown(t *testing.T) {
	cases := []struct {
		name  string
		req   httpx.Request
		warns int
	}{
		{"a create", httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x"}, 1},
		{"a read", httpx.Request{Family: httpx.JSMOps, Path: "/x"}, 0},
		{"a search sent as a POST", httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x", Idempotent: true}, 0},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		var n atomic.Int32
		client, _, _ := serve(t, httpx.Config{Logger: logger}, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			ok(w, r)
		})
		if _, err := client.Do(context.Background(), c.req); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out := buf.String()
		if got := strings.Count(out, "level=WARN"); got != c.warns {
			t.Errorf("%s: %d warnings, want %d: %s", c.name, got, c.warns, out)
		}
		if c.warns > 0 && !strings.Contains(out, "method=POST path=/x") {
			t.Errorf("%s: the warning does not name the call: %s", c.name, out)
		}
	}
}

// TestATimedOutWriteIsSentOnce is the no-blind-retry rule: once a create may
// have reached the server, its outcome is unknown and it is not sent again.
func TestATimedOutWriteIsSentOnce(t *testing.T) {
	release := make(chan struct{})
	hang := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	writes, _, writeHits := serve(t, httpx.Config{Timeout: 50 * time.Millisecond}, hang)
	reads, _, readHits := serve(t, httpx.Config{Timeout: 50 * time.Millisecond}, hang)
	// Registered last, so it runs before the servers close and wait.
	t.Cleanup(func() { close(release) })

	_, err := writes.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/jsm/ops/api/c1/v1/alerts", Body: map[string]string{"message": "test"}})
	if !errors.Is(err, httperr.ErrUnknownOutcome) || writeHits.Load() != 1 {
		t.Errorf("timed-out POST: got %v after %d calls, want ErrUnknownOutcome after 1", err, writeHits.Load())
	}
	_, err = reads.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"})
	if !errors.Is(err, httperr.ErrUnavailable) || readHits.Load() != 3 {
		t.Errorf("timed-out GET: got %v after %d calls, want ErrUnavailable after 3", err, readHits.Load())
	}
}

func TestAWriteThatNeverLeftIsSentAgain(t *testing.T) {
	client, router, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	var failures atomic.Int32
	router.FailWith(func(*http.Request) error {
		if failures.Add(1) <= 2 {
			return errors.New("connect: connection refused")
		}
		return nil
	})
	_, err := client.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x", Body: map[string]int{"a": 1}})
	if err != nil || hits.Load() != 1 || len(router.Requests()) != 3 {
		t.Errorf("got %v with %d calls received and %d attempted", err, hits.Load(), len(router.Requests()))
	}

	router.FailWith(func(*http.Request) error { return errors.New("connect: connection refused") })
	_, err = client.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x"})
	if !errors.Is(err, httperr.ErrUnavailable) || errors.Is(err, httperr.ErrUnknownOutcome) {
		t.Errorf("a create that never left is unavailable, not unknown: %v", err)
	}
}

// connectedThenTimedOut fails every request the way HTTP/2 can: the
// connection is there and the request may be on the wire, but the attempt's
// timeout fired before WroteHeaders ran.
type connectedThenTimedOut struct{ calls atomic.Int32 }

func (c *connectedThenTimedOut) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{})
	}
	return nil, context.DeadlineExceeded
}

func TestAWriteThatHadAConnectionIsNotSentAgain(t *testing.T) {
	cases := []struct {
		name string
		req  httpx.Request
		want error
		sent int32
	}{
		{"a delete", httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodDelete, Path: "/x"}, httperr.ErrUnknownOutcome, 1},
		{"a post without a body", httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x"}, httperr.ErrUnknownOutcome, 1},
		{"a post with a body", httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x", Body: map[string]int{"a": 1}}, httperr.ErrUnknownOutcome, 1},
		{"a read", httpx.Request{Family: httpx.JSMOps, Path: "/x"}, httperr.ErrUnavailable, int32(fast.MaxAttempts)},
	}
	for _, c := range cases {
		rt := &connectedThenTimedOut{}
		client, err := httpx.New(httpx.Config{Credentials: allCredentials(), Retry: fast, Limits: map[httpx.Family]httpx.Limit{}, Transport: rt})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Do(context.Background(), c.req)
		if !errors.Is(err, c.want) || rt.calls.Load() != c.sent {
			t.Errorf("%s: got %v after %d sends, want %v after %d", c.name, err, rt.calls.Load(), c.want, c.sent)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	client, router, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/collect", http.StatusFound)
	})
	_, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"})
	if !errors.Is(err, httperr.ErrBadResponse) {
		t.Errorf("got %v, want ErrBadResponse", err)
	}
	for _, r := range router.Requests() {
		if !strings.HasPrefix(r.URL, "https://api.pagerduty.com/") {
			t.Errorf("a request went to %s", r.URL)
		}
	}
}

func TestErrorsAreRedacted(t *testing.T) {
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Atl-Traceid", "trace-9")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"key `+pdKey+` from `+email+` and bob@example.org, call +44 20 7946 0958","routing_key":"R0123456789"}`)
	})
	_, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users", Query: url.Values{"query": {"alice@example.com"}}})
	var e *httperr.Error
	if !errors.As(err, &e) || e.RequestID != "trace-9" || !strings.Contains(e.Body, "message") {
		t.Fatalf("got %v", err)
	}
	for _, leak := range []string{pdKey, email, "bob@example.org", "7946", "R0123456789", "alice@example.com"} {
		if strings.Contains(e.Body, leak) || strings.Contains(err.Error(), leak) {
			t.Errorf("the error shows %q: %s / %s", leak, err, e.Body)
		}
	}
}

// TestErrorsNeverQuoteTheAnswer covers values that encoding/json and
// time.Time repeat in their errors. The redactor's patterns would miss some of
// them, such as an email address or a phone number without a +.
func TestErrorsNeverQuoteTheAnswer(t *testing.T) {
	type schedule struct {
		At      time.Time                  `json:"at"`
		Phone   string                     `json:"phone"`
		ByPhone map[string]int             `json:"by_phone"`
		ByEmail map[string]struct{ N int } `json:"by_email"`
	}
	cases := map[string]string{
		// A type error inside a map names the keys on its path.
		"a map keyed by phone": `{"by_phone":{"+15551234567":"x"}}`,
		"a map keyed by email": `{"by_email":{"alice@example.com":{"N":"x"}}}`,
		"a calendar URL":       `{"at":"webcal://acme.pagerduty.com/private/CALTOKEN123"}`,
		"an email address":     `{"at":"alice@example.com"}`,
		"a phone number":       `{"at":"555 010 9999"}`,
		"a number":             `{"phone":15551234567}`,
		"JSON cut short":       `{"phone":"555 010 9999`,
		"an email in a key":    `{"alice@example.com"}`,
	}
	leaks := []string{"CALTOKEN123", "alice", "555 010 9999", "15551234567"}
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, httperr.ErrBadResponse) {
			t.Errorf("%s: got %v, want ErrBadResponse", name, err)
		}
		for _, leak := range leaks {
			if err != nil && strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error quotes %q: %v", name, leak, err)
			}
		}
	}
	for name, body := range cases {
		client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		for _, family := range []httpx.Family{httpx.PagerDuty, httpx.JSMOps} {
			var v schedule
			_, err := client.DoJSON(context.Background(), httpx.Request{Family: family, Path: "/schedules"}, &v)
			check(name+" from "+string(family), err)
		}
	}

	// Pages decodes the list items itself.
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"schedules":[{"at":"alice@example.com"}],"more":false,"limit":100}`)
	})
	_, err := collect(httpx.Pages(context.Background(), client,
		httpx.Request{Family: httpx.PagerDuty, Path: "/schedules"}, httpx.Offset[schedule]("schedules", 100, 10000)))
	check("a list item", err)

	client, _, _ = serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"phone":15551234567}`) })
	var v schedule
	if _, err := client.DoJSON(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/schedules"}, &v); err == nil ||
		!strings.Contains(err.Error(), "a value in schedule is a JSON number, not a string") {
		t.Errorf("a type error still names the Go type and the JSON kind: %v", err)
	}

	// A list of maps decodes its items itself, and a map at the top has no
	// Go type around it.
	client, _, _ = serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"alice@example.com":"x"}],"more":false,"limit":100}`)
	})
	_, err = collect(httpx.Pages(context.Background(), client,
		httpx.Request{Family: httpx.PagerDuty, Path: "/items"}, httpx.Offset[map[string]int]("items", 100, 10000)))
	check("a map in a list", err)
}

func TestRefusalsNeverQuoteTheSite(t *testing.T) {
	for _, site := range []string{
		"admin:Hunter2Secret@acme.atlassian.net",
		"https://admin:Hunter2Secret@acme.atlassian.net",
		"acme.atlassian.net/Hunter2Secret",
		"hunter2secret.example.com",
	} {
		client, router, _ := serve(t, httpx.Config{}, ok)
		_, err := client.Do(context.Background(), httpx.Request{Family: httpx.Site, Site: site, Path: "/rest/api/3/myself"})
		if !errors.Is(err, httperr.ErrRefused) || len(router.Requests()) != 0 {
			t.Errorf("%s: got %v after %d requests, want ErrRefused before any", site, err, len(router.Requests()))
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "hunter2secret") {
			t.Errorf("the refusal quotes the site: %v", err)
		}
	}
}

func TestATransportErrorIsRedacted(t *testing.T) {
	client, router, _ := serve(t, httpx.Config{}, ok)
	router.FailWith(func(*http.Request) error {
		return fmt.Errorf("proxy refused %s for %s: %w", pdKey, email, context.Canceled)
	})
	_, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"})
	if !errors.Is(err, httperr.ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want ErrUnavailable that still matches its cause", err)
	}
	for _, leak := range []string{pdKey, email} {
		if err != nil && strings.Contains(err.Error(), leak) {
			t.Errorf("the error shows %q: %v", leak, err)
		}
	}
}

func TestASerialFamilySendsOneRequestAtATime(t *testing.T) {
	var inFlight, most atomic.Int32
	client, _, hits := serve(t, httpx.Config{Limits: map[httpx.Family]httpx.Limit{httpx.PagerDuty: {Serial: true}}},
		func(w http.ResponseWriter, r *http.Request) {
			raise(&most, inFlight.Add(1))
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			ok(w, r)
		})
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if most.Load() != 1 || hits.Load() != 6 {
		t.Errorf("%d requests at once, %d in all", most.Load(), hits.Load())
	}
}

// raise sets a to n if n is larger.
func raise(a *atomic.Int32, n int32) {
	for {
		m := a.Load()
		if n <= m || a.CompareAndSwap(m, n) {
			return
		}
	}
}

func TestLogsLeaveOutTheQueryAndSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: credentials.RedactAttr}))
	client, _, _ := serve(t, httpx.Config{Logger: logger}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-7")
		ok(w, r)
	})
	if _, err := client.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users", Query: url.Values{"query": {"alice@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"family=pagerduty", "path=/users", "attempt=1", "status=200", "request_id=req-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q: %s", want, out)
		}
	}
	for _, leak := range []string{pdKey, "alice"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log shows %q: %s", leak, out)
		}
	}
}

func TestDoStopsOnACanceledContext(t *testing.T) {
	client, router, _ := serve(t, httpx.Config{}, ok)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Do(ctx, httpx.Request{Family: httpx.PagerDuty, Path: "/abilities"})
	if !errors.Is(err, context.Canceled) || len(router.Requests()) != 0 {
		t.Errorf("got %v after %d requests, want context.Canceled before any", err, len(router.Requests()))
	}
}

func TestDoJSONRejectsABodyItCannotRead(t *testing.T) {
	client, _, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"users":`) })
	var v any
	if _, err := client.DoJSON(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"}, &v); !errors.Is(err, httperr.ErrBadResponse) || hits.Load() != 1 {
		t.Errorf("got %v after %d calls, want ErrBadResponse after 1", err, hits.Load())
	}

	// A create answered 201 took effect, so an answer it cannot read leaves
	// what it did to be looked up, not a failure to retry.
	client, _, hits = serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":`)
	})
	if _, err := client.DoJSON(context.Background(), httpx.Request{Family: httpx.JSMOps, Method: httpx.MethodPost, Path: "/x"}, &v); !errors.Is(err, httperr.ErrUnknownOutcome) || hits.Load() != 1 {
		t.Errorf("a create: got %v after %d calls, want ErrUnknownOutcome after 1", err, hits.Load())
	}
}

func TestResponseDecode(t *testing.T) {
	var v struct{ Name string }
	r := &httpx.Response{Status: 200, Body: []byte(`{"name":"x"}`)}
	if err := r.Decode(&v); err != nil || v.Name != "x" {
		t.Errorf("Decode = %v, %+v", err, v)
	}
	r.Body = []byte(`{"name":`)
	if err := r.Decode(&v); !errors.Is(err, httperr.ErrBadResponse) {
		t.Errorf("a body that does not decode: got %v, want ErrBadResponse", err)
	}
	if r.Header("X-Request-Id") != "" {
		t.Errorf("a response without headers has none")
	}
}
