package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

var testCredentials = credentials.Set{}.
	With(credentials.PagerDutyKey, "k").
	With(credentials.AtlassianEmail, "e").
	With(credentials.AtlassianToken, "t").
	With(credentials.OrgAPIKey, "o")

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestTargetHosts(t *testing.T) {
	cases := []struct {
		cfg  Config
		req  Request
		want string
	}{
		{Config{}, Request{Family: PagerDuty, Path: "/users", Query: url.Values{"limit": {"100"}, "include[]": {"teams"}}},
			"https://api.pagerduty.com/users?include%5B%5D=teams&limit=100"},
		{Config{PagerDutyRegion: RegionEU}, Request{Family: PagerDuty, Path: "/v3/schedules"}, "https://api.eu.pagerduty.com/v3/schedules"},
		{Config{}, Request{Family: JSMOps, Path: "/jsm/ops/api/c1/v1/teams"}, "https://api.atlassian.com/jsm/ops/api/c1/v1/teams"},
		{Config{Staging: true}, Request{Family: Admin, Path: "/admin/v1/orgs"}, "https://api.stg.atlassian.com/admin/v1/orgs"},
		// The site families take their host from the request, never from the
		// configuration, because login checks other sites than the saved one.
		{Config{Staging: true}, Request{Family: Site, Site: "ACME.jira-dev.com", Path: "/rest/api/3/myself"}, "https://acme.jira-dev.com/rest/api/3/myself"},
		{Config{}, Request{Family: SiteAnonymous, Site: "acme.atlassian.net", Path: "/_edge/tenant_info"}, "https://acme.atlassian.net/_edge/tenant_info"},
	}
	for _, c := range cases {
		c.cfg.Credentials = testCredentials
		client, err := New(c.cfg)
		if err != nil {
			t.Fatal(err)
		}
		u, err := client.target(c.req)
		if err != nil {
			t.Errorf("%s %s: %v", c.req.Family, c.req.Path, err)
			continue
		}
		if u.String() != c.want {
			t.Errorf("got  %s\nwant %s", u, c.want)
		}
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// TestGuard checks the transport's own refusals, which hold even for a
// request that did not come through Do's checks.
func TestGuard(t *testing.T) {
	g := guard{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("the request reached the network")
		return nil, errors.New("unreachable")
	})}
	pd := context.WithValue(context.Background(), targetKey{}, target{host: "api.pagerduty.com", family: PagerDuty})
	events := context.WithValue(context.Background(), targetKey{}, target{host: "events.pagerduty.com", family: JSMOps})
	cases := []struct {
		name        string
		ctx         context.Context
		method, url string
	}{
		{"not built by Do", context.Background(), "GET", "https://api.pagerduty.com/users"},
		{"another host", pd, "GET", "https://evil.example.com/users"},
		{"plain HTTP", pd, "GET", "http://api.pagerduty.com/users"},
		{"a PagerDuty write", pd, "POST", "https://api.pagerduty.com/users"},
		{"a write to any PagerDuty host", events, "POST", "https://events.pagerduty.com/v2/enqueue"},
	}
	for _, c := range cases {
		body := &closeRecorder{Reader: strings.NewReader("{}")}
		r, err := http.NewRequestWithContext(c.ctx, c.method, c.url, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.RoundTrip(r); !errors.Is(err, errGuard) {
			t.Errorf("%s: got %v, want the guard to refuse", c.name, err)
		}
		if !body.closed {
			t.Errorf("%s: a refused request must still close its body", c.name)
		}
	}
}

func TestBackoff(t *testing.T) {
	c, err := New(Config{Retry: Retry{MaxAttempts: 5, MinWait: time.Second, MaxWait: 8 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	cases := []struct {
		name   string
		family Family
		status int
		header http.Header
		want   time.Duration
	}{
		{"Retry-After in seconds", JSMOps, 503, h("Retry-After", "7"), 7 * time.Second},
		{"Retry-After as a date", JSMOps, 429, h("Retry-After", now.Add(30*time.Second).Format(http.TimeFormat)), 30 * time.Second},
		{"Retry-After in the past", JSMOps, 429, h("Retry-After", now.Add(-time.Minute).Format(http.TimeFormat)), 0},
		{"Retry-After too long", PagerDuty, 429, h("Retry-After", "86400"), maxServerWait},
		{"Retry-After before ratelimit-reset", PagerDuty, 429, h("Retry-After", "3", "Ratelimit-Reset", "40"), 3 * time.Second},
		{"ratelimit-reset", PagerDuty, 429, h("Ratelimit-Reset", "12"), 12 * time.Second},
		{"a PagerDuty 429 that says nothing", PagerDuty, 429, nil, time.Minute},
	}
	for _, tc := range cases {
		if got := c.backoff(tc.family, 1, result{status: tc.status, header: tc.header}); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	// Exponential backoff with jitter: a 503 that carries ratelimit-reset, as
	// every PagerDuty answer does, and a JSM 429 that says nothing.
	for _, res := range []result{{status: 503, header: h("Ratelimit-Reset", "12")}, {status: 429}} {
		for attempt, ceiling := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 9: 8 * time.Second, 60: 8 * time.Second} {
			for range 50 {
				if d := c.backoff(JSMOps, attempt, res); d < ceiling/2 || d > ceiling {
					t.Fatalf("attempt %d after %d: %s is outside [%s, %s]", attempt, res.status, d, ceiling/2, ceiling)
				}
			}
		}
	}
}

func TestPacer(t *testing.T) {
	p := &pacer{every: 100 * time.Millisecond}
	t0 := time.Unix(1_800_000_000, 0)
	var waits []time.Duration
	record := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	for range 3 {
		if err := p.wait(context.Background(), func() time.Time { return t0 }, record); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 3 || waits[0] != 0 || waits[1] != 100*time.Millisecond || waits[2] != 200*time.Millisecond {
		t.Errorf("three calls at once wait %v, want 0, 100ms and 200ms", waits)
	}
	waits = nil
	_ = p.wait(context.Background(), func() time.Time { return t0.Add(time.Second) }, record)
	if waits[0] != 0 {
		t.Errorf("a call after a quiet second waits %s", waits[0])
	}
	if l := newLimiter(Limit{PerMinute: 840, Serial: true}); cap(l.serial) != 1 || l.pace.every != time.Minute/840 {
		t.Errorf("the PagerDuty limiter is %+v", l)
	}
}

func TestDoPacesAFamily(t *testing.T) {
	c, err := New(Config{
		Credentials: testCredentials,
		Limits:      map[Family]Limit{PagerDuty: {PerMinute: 600}},
		Transport:   roundTripFunc(func(r *http.Request) (*http.Response, error) { return answer(r, 200, "{}"), nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return t0 }
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	for range 3 {
		if _, err := c.Do(context.Background(), Request{Family: PagerDuty, Path: "/users"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 3 || waits[1] != 100*time.Millisecond || waits[2] != 200*time.Millisecond {
		t.Errorf("waits %v, want 100ms apart", waits)
	}
}

// TestAnAnswerTooLargeIsJudgedByItsStatus: the status says what happened even
// when the body cannot be read, so a write keeps its unknown outcome.
func TestAnAnswerTooLargeIsJudgedByItsStatus(t *testing.T) {
	old := maxBody
	maxBody = 10
	t.Cleanup(func() { maxBody = old })
	cases := []struct {
		method Method
		status int
		want   error
		calls  int
	}{
		{MethodGet, 200, httperr.ErrBadResponse, 1},
		{MethodPost, 201, httperr.ErrUnknownOutcome, 1},
		{MethodPost, 503, httperr.ErrUnknownOutcome, 1},
		{MethodPost, 400, httperr.ErrInvalid, 1},
		{MethodGet, 503, httperr.ErrUnavailable, 3},
	}
	for _, tc := range cases {
		var calls int
		c, err := New(Config{
			Credentials: testCredentials,
			Limits:      map[Family]Limit{},
			Retry:       Retry{MaxAttempts: 3, MinWait: time.Millisecond, MaxWait: 2 * time.Millisecond},
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return answer(r, tc.status, `{"users":[`+strings.Repeat(`{}`, 50)+`]}`), nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(context.Background(), Request{Family: JSMOps, Method: tc.method, Path: "/x"})
		if !errors.Is(err, tc.want) || calls != tc.calls {
			t.Errorf("%s answered %d: got %v after %d calls, want %v after %d", tc.method, tc.status, err, calls, tc.want, tc.calls)
		}
	}
}

func TestErrorBodyIsCut(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := c.errorBody([]byte(strings.Repeat("é", 2000)))
	if len(got) > maxErrorBody+len("…") || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "éé") {
		t.Errorf("a long body is %d bytes: %q…", len(got), got[:10])
	}
}
