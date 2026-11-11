package pd_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

const key = "pd-key-value-1234"

type item struct {
	ID   string `json:"id"`
	Team string `json:"team,omitempty"`
}

func items(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = item{ID: fmt.Sprintf("P%05d", i)}
	}
	return out
}

func clientIn(t *testing.T, f *pdfake.Fake, region httpx.Region) *pd.Client {
	t.Helper()
	h, err := httpx.New(f.Config(region))
	if err != nil {
		t.Fatal(err)
	}
	return pd.New(h)
}

func TestGetSendsAGetWithTheAcceptHeaderOfItsVersion(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/users/P1", map[string]any{"user": map[string]any{"id": "P1"}})
	f.Answer("/v3/schedules/P2", map[string]any{"schedule": map[string]any{"id": "P2"}})
	c := f.Client()
	ctx := context.Background()

	var user struct {
		User item `json:"user"`
	}
	if err := c.Get(ctx, "/users/P1", url.Values{"include[]": {"contact_methods"}}, &user); err != nil {
		t.Fatal(err)
	}
	var schedule struct {
		Schedule item `json:"schedule"`
	}
	if err := c.Get(ctx, "/v3/schedules/P2", nil, &schedule); err != nil {
		t.Fatal(err)
	}
	if user.User.ID != "P1" || schedule.Schedule.ID != "P2" {
		t.Fatalf("decoded %+v and %+v", user, schedule)
	}
	reqs := f.Requests()
	want := []pdfake.Request{
		{Method: http.MethodGet, Host: pdfake.HostUS, Path: "/users/P1", Query: url.Values{"include[]": {"contact_methods"}}, Accept: pdfake.AcceptV2},
		{Method: http.MethodGet, Host: pdfake.HostUS, Path: "/v3/schedules/P2", Query: url.Values{}, Accept: pdfake.AcceptV3},
	}
	if len(reqs) != len(want) {
		t.Fatalf("got %d requests, want %d", len(reqs), len(want))
	}
	for i := range want {
		if fmt.Sprint(reqs[i]) != fmt.Sprint(want[i]) {
			t.Errorf("request %d = %+v, want %+v", i, reqs[i], want[i])
		}
	}
}

func TestAbilities(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/abilities", map[string]any{"abilities": []string{"teams", "urgencies"}})
	got, err := f.Client().Abilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "teams,urgencies" {
		t.Errorf("abilities = %v", got)
	}
}

func TestTheRegionDecidesTheHost(t *testing.T) {
	f := pdfake.New(t, key, pdfake.InRegion(httpx.RegionEU))
	f.Answer("/abilities", map[string]any{"abilities": []string{"teams"}})
	ctx := context.Background()

	if _, err := clientIn(t, f, httpx.RegionUS).Abilities(ctx); !errors.Is(err, httperr.ErrUnauthorized) {
		t.Fatalf("an EU account read from the US host: got %v, want ErrUnauthorized", err)
	}
	if _, err := clientIn(t, f, httpx.RegionEU).Abilities(ctx); err != nil {
		t.Fatalf("an EU account read from the EU host: %v", err)
	}
	reqs := f.Requests()
	if reqs[0].Host != pdfake.HostUS || reqs[1].Host != pdfake.HostEU {
		t.Errorf("hosts = %s, %s", reqs[0].Host, reqs[1].Host)
	}
}

func TestErrorsKeepTheirKind(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, httperr.ErrUnauthorized},
		{http.StatusPaymentRequired, httperr.ErrNotInPlan},
		{http.StatusForbidden, httperr.ErrForbidden},
		{http.StatusNotFound, httperr.ErrNotFound},
		{http.StatusBadRequest, httperr.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			f := pdfake.New(t, key)
			f.Answer("/teams/P1/notification_subscriptions", map[string]any{})
			f.Fail("/teams/P1/notification_subscriptions", pdfake.Fault{Status: c.status})
			var v any
			if err := f.Client().Get(context.Background(), "/teams/P1/notification_subscriptions", nil, &v); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestAWrongKeyIsUnauthorized(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/abilities", map[string]any{"abilities": []string{}})
	cfg := f.Config(httpx.RegionUS)
	cfg.Credentials = cfg.Credentials.With(credentials.PagerDutyKey, "another-key-5678")
	h, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pd.New(h).Abilities(context.Background()); !errors.Is(err, httperr.ErrUnauthorized) {
		t.Errorf("got %v, want ErrUnauthorized", err)
	}
}

func TestARateLimitedReadIsRetried(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/abilities", map[string]any{"abilities": []string{"teams"}})
	f.Fail("/abilities", pdfake.Fault{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"0"}}})
	if _, err := f.Client().Abilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Requests()); n != 2 {
		t.Errorf("sent %d requests, want 2", n)
	}
}

func TestListReadsEveryPage(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/users", "users", items(250))
	got, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/users", Key: "users", Query: url.Values{"include[]": {"teams"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 || got[249].ID != "P00249" {
		t.Fatalf("read %d items", len(got))
	}
	if cov != (pd.Coverage{Total: 250, Read: 250}) || !cov.Complete() {
		t.Errorf("coverage = %+v", cov)
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("sent %d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		q := r.Query
		if q.Get("limit") != "100" || q.Get("offset") != fmt.Sprint(i*100) || q.Get("total") != "true" || q.Get("include[]") != "teams" {
			t.Errorf("request %d query = %v", i, q)
		}
	}
}

func TestListStopsAtTheWindow(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/users", "users", items(pd.Window+50))
	got, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/users", Key: "users"})
	if err != nil {
		t.Fatal(err)
	}
	// The fake answers 400 past the window, so getting here proves List
	// never asked past it.
	if len(got) != pd.Window || cov != (pd.Coverage{Total: pd.Window + 50, Read: pd.Window, Capped: true}) || cov.Complete() {
		t.Errorf("read %d items, coverage %+v", len(got), cov)
	}
}

func TestAListThatEndsAtTheWindowIsComplete(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/users", "users", items(pd.Window))
	_, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/users", Key: "users"})
	if err != nil || !cov.Complete() {
		t.Errorf("err %v, coverage %+v", err, cov)
	}
}

func TestListKeepsWhatItReadBeforeAFailure(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/services", "services", items(300))
	// The first page answers, then every attempt at the second fails.
	f.Fail("/services", pdfake.Fault{Status: http.StatusOK, Body: map[string]any{"services": items(100), "more": true, "limit": 100, "offset": 0, "total": 300}},
		pdfake.Fault{Status: 503}, pdfake.Fault{Status: 503}, pdfake.Fault{Status: 503})
	got, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/services", Key: "services"})
	if !errors.Is(err, httperr.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if len(got) != 100 || cov != (pd.Coverage{Total: 300, Read: 100}) || cov.Complete() {
		t.Errorf("read %d items, coverage %+v", len(got), cov)
	}
}

func TestListTakesALargerLimit(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/v3/schedules/P1/rotations", "rotations", items(1500), pdfake.MaxLimit(1000))
	got, _, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/v3/schedules/P1/rotations", Key: "rotations", Limit: 1000})
	if err != nil || len(got) != 1500 || len(f.Requests()) != 2 {
		t.Errorf("err %v, %d items in %d requests", err, len(got), len(f.Requests()))
	}
}

func TestListWithoutATotal(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/teams/P1/tags", map[string]any{"tags": items(2), "more": false})
	got, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/teams/P1/tags", Key: "tags"})
	if err != nil || len(got) != 2 || cov != (pd.Coverage{Total: -1, Read: 2}) || !cov.Complete() {
		t.Errorf("err %v, %d items, coverage %+v", err, len(got), cov)
	}
}

func TestListFilters(t *testing.T) {
	f := pdfake.New(t, key)
	all := []any{item{ID: "P1", Team: "T1"}, item{ID: "P2", Team: "T2"}, item{ID: "P3", Team: "T1"}}
	f.List("/users", "users", all, pdfake.Filter(func(q url.Values, it any) bool {
		return q.Get("team_ids[]") == "" || it.(item).Team == q.Get("team_ids[]")
	}))
	got, cov, err := pd.List[item](context.Background(), f.Client(), pd.ListRequest{Path: "/users", Key: "users", Query: url.Values{"team_ids[]": {"T1"}}})
	if err != nil || len(got) != 2 || got[1].ID != "P3" || cov.Total != 2 {
		t.Errorf("err %v, items %+v, coverage %+v", err, got, cov)
	}
}

func TestListRejectsABadRequestWithoutSendingIt(t *testing.T) {
	f := pdfake.New(t, key)
	for _, lr := range []pd.ListRequest{{Path: "/users"}, {Path: "/users", Key: "users", Limit: pd.Window + 1}, {Path: "/users", Key: "users", Limit: -1}} {
		if _, _, err := pd.List[item](context.Background(), f.Client(), lr); err == nil {
			t.Errorf("%+v: no error", lr)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("sent %d requests", n)
	}
}

func TestCalendarURLsNeverReachTheCaller(t *testing.T) {
	const feed = "https://acme.pagerduty.com/private/FEEDTOKEN0123456789/feed"
	type schedule struct {
		ID         string `json:"id"`
		HTTPCalURL string `json:"http_cal_url"`
		WebCalURL  string `json:"web_cal_url"`
	}
	f := pdfake.New(t, key)
	f.Answer("/schedules/P1", map[string]any{"schedule": map[string]any{"id": "P1", "http_cal_url": feed, "web_cal_url": feed}})
	f.List("/schedules", "schedules", []any{map[string]any{"id": "P1", "http_cal_url": feed}})
	c := f.Client()

	var one struct {
		Schedule schedule `json:"schedule"`
	}
	if err := c.Get(context.Background(), "/schedules/P1", nil, &one); err != nil {
		t.Fatal(err)
	}
	listed, _, err := pd.List[schedule](context.Background(), c, pd.ListRequest{Path: "/schedules", Key: "schedules"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(listed, one.Schedule) {
		if s.ID != "P1" || s.HTTPCalURL != "" || s.WebCalURL != "" {
			t.Errorf("decoded %+v", s)
		}
	}
}
