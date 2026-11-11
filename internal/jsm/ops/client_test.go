package ops_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/ops"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

func account() jsmfake.Account {
	return jsmfake.Account{
		Site: "acme.atlassian.net", CloudID: "cloud-1", OrgID: "org-1",
		Email: "operator@example.com", Token: "atl-token-value-1234", OrgKey: "org-key-value-1234",
	}
}

type schedule struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func schedules(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = schedule{ID: fmt.Sprintf("s%03d", i)}
	}
	return out
}

func TestGetIsUnderTheSitesPrefix(t *testing.T) {
	f := jsmfake.New(t, account())
	f.Answer(http.MethodGet, f.OpsPath("/v1/schedules/s1"), http.StatusOK, schedule{ID: "s1", Name: "Primary"})
	c := ops.New(f.Client(), "cloud-1")

	var got schedule
	if err := c.Get(context.Background(), "/v1/schedules/s1", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "Primary" {
		t.Fatalf("got %+v", got)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || reqs[0].Host != jsmfake.APIHost || reqs[0].Path != "/jsm/ops/api/cloud-1/v1/schedules/s1" {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestStagingUsesTheStagingHost(t *testing.T) {
	a := account()
	a.Staging = true
	f := jsmfake.New(t, a)
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams"), http.StatusOK, []any{})
	if err := ops.New(f.Client(), "cloud-1").Get(context.Background(), "/v1/teams", nil, new([]any)); err != nil {
		t.Fatal(err)
	}
	if h := f.Requests()[0].Host; h != jsmfake.APIHostStaging {
		t.Fatalf("host %s", h)
	}
}

func TestWrongCredentialIsUnauthorized(t *testing.T) {
	f := jsmfake.New(t, account())
	f.Answer(http.MethodGet, f.OpsPath("/v1/teams"), http.StatusOK, []any{})
	cfg := f.Config()
	cfg.Credentials = cfg.Credentials.With(credentials.AtlassianToken, "another-token-5678")
	h, err := httpx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = ops.New(h, "cloud-1").Get(context.Background(), "/v1/teams", nil, new([]any))
	if !errors.Is(err, httperr.ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
}

func TestBadCloudIDRefusesEveryCall(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"", "..", "cloud/1", "cloud?x", "cloud#1"} {
		f := jsmfake.New(t, account())
		c := ops.New(f.Client(), id)
		if err := c.Get(ctx, "/v1/teams", nil, new([]any)); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%q: Get: %v", id, err)
		}
		if _, err := c.Write(ctx, ops.Write{Method: ops.MethodPost, Path: "/v1/schedules"}, nil); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%q: Write: %v", id, err)
		}
		if _, err := ops.List[schedule](ctx, c, ops.ListRequest{Path: "/v1/schedules"}); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%q: List: %v", id, err)
		}
		if _, err := c.EnableOps(ctx, "team-1", []string{"acc-1"}); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%q: EnableOps: %v", id, err)
		}
		if n := len(f.Requests()); n != 0 {
			t.Errorf("%q: %d requests sent", id, n)
		}
	}
}

func TestListPagesByOffsetAndSize(t *testing.T) {
	f := jsmfake.New(t, account())
	f.List(http.MethodGet, f.OpsPath("/v1/schedules"), "values", schedules(120))
	c := ops.New(f.Client(), "cloud-1")

	got, err := ops.List[schedule](context.Background(), c, ops.ListRequest{Path: "/v1/schedules"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 || got[119].ID != "s119" {
		t.Fatalf("got %d items", len(got))
	}
	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.Query.Get("size") != strconv.Itoa(ops.MaxSize) || r.Query.Get("offset") != strconv.Itoa(i*ops.MaxSize) {
			t.Errorf("request %d query %v", i, r.Query)
		}
	}
}

func TestListKeepsItsQueryAndSize(t *testing.T) {
	f := jsmfake.New(t, account())
	f.List(http.MethodGet, f.OpsPath("/v1/schedules"), "values", schedules(5))
	c := ops.New(f.Client(), "cloud-1")

	got, err := ops.List[schedule](context.Background(), c,
		ops.ListRequest{Path: "/v1/schedules", Query: map[string][]string{"query": {"primary"}}, Size: 2})
	if err != nil || len(got) != 5 {
		t.Fatalf("got %d, %v", len(got), err)
	}
	for _, r := range f.Requests() {
		if r.Query.Get("query") != "primary" || r.Query.Get("size") != "2" {
			t.Errorf("query %v", r.Query)
		}
	}
}

func TestListRefusesAPageTooLarge(t *testing.T) {
	f := jsmfake.New(t, account())
	c := ops.New(f.Client(), "cloud-1")
	for _, size := range []int{-1, ops.MaxSize + 1} {
		if _, err := ops.List[schedule](context.Background(), c, ops.ListRequest{Path: "/v1/schedules", Size: size}); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("size %d: %v", size, err)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestListKeepsTheItemsBeforeAFailure(t *testing.T) {
	f := jsmfake.New(t, account())
	path := f.OpsPath("/v1/schedules")
	f.List(http.MethodGet, path, "values", schedules(120))
	c := ops.New(f.Client(), "cloud-1")

	// The first page is answered; then every attempt at the second fails.
	ctx := context.Background()
	f.Fail(http.MethodGet, path, jsmfake.Fault{Status: http.StatusOK, Body: map[string]any{"values": schedules(50), "links": map[string]any{"next": "/v1/schedules?offset=50&size=50"}}},
		jsmfake.Fault{Status: http.StatusServiceUnavailable}, jsmfake.Fault{Status: http.StatusServiceUnavailable}, jsmfake.Fault{Status: http.StatusServiceUnavailable})
	got, err := ops.List[schedule](ctx, c, ops.ListRequest{Path: "/v1/schedules"})
	if !errors.Is(err, httperr.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d items, want the first page's 50", len(got))
	}
}

func TestWriteSendsTheBodyAndReportsTheAnswer(t *testing.T) {
	f := jsmfake.New(t, account())
	path := f.OpsPath("/v1/schedules")
	f.Fail(http.MethodPost, path, jsmfake.Fault{Status: http.StatusCreated, Header: http.Header{"X-Request-Id": {"req-1"}}, Body: schedule{ID: "s9", Name: "New"}})
	c := ops.New(f.Client(), "cloud-1")

	var got schedule
	res, err := c.Write(context.Background(), ops.Write{Method: ops.MethodPost, Path: "/v1/schedules", Body: schedule{Name: "New"}}, &got)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusCreated || res.RequestID != "req-1" || got.ID != "s9" {
		t.Fatalf("result %+v, answer %+v", res, got)
	}
	if b := string(f.Requests()[0].Body); b != `{"id":"","name":"New"}` {
		t.Fatalf("body %s", b)
	}
}

func TestWriteRefusesReadsAndBadPaths(t *testing.T) {
	f := jsmfake.New(t, account())
	c := ops.New(f.Client(), "cloud-1")
	ctx := context.Background()
	for _, w := range []ops.Write{{Path: "/v1/schedules"}, {Method: "GET", Path: "/v1/schedules"}, {Method: ops.MethodPost, Path: "v1/schedules"}} {
		if _, err := c.Write(ctx, w, nil); !errors.Is(err, httperr.ErrRefused) {
			t.Errorf("%+v: %v", w, err)
		}
	}
	if err := c.Get(ctx, "v1/teams", nil, new([]any)); !errors.Is(err, httperr.ErrRefused) {
		t.Errorf("relative Get path: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestOnlyAnIdempotentWriteIsSentAgainAfterA5xx(t *testing.T) {
	ctx := context.Background()
	for _, idempotent := range []bool{false, true} {
		f := jsmfake.New(t, account())
		path := f.OpsPath("/v1/schedules/s1/overrides/a1")
		f.Answer(http.MethodPut, path, http.StatusOK, map[string]any{})
		f.Fail(http.MethodPut, path, jsmfake.Fault{Status: http.StatusBadGateway})
		c := ops.New(f.Client(), "cloud-1")

		_, err := c.Write(ctx, ops.Write{Method: ops.MethodPut, Path: "/v1/schedules/s1/overrides/a1", Body: map[string]any{}, Idempotent: idempotent}, nil)
		n := len(f.Requests())
		switch {
		case idempotent && (err != nil || n != 2):
			t.Errorf("idempotent: %v after %d requests, want success after 2", err, n)
		case !idempotent && (!errors.Is(err, httperr.ErrUnknownOutcome) || n != 1):
			t.Errorf("not idempotent: %v after %d requests, want ErrUnknownOutcome after 1", err, n)
		}
	}
}

func TestAWriteAnsweredWithoutAReadableBodyIsAnUnknownOutcome(t *testing.T) {
	f := jsmfake.New(t, account())
	f.Fail(http.MethodPost, f.OpsPath("/v1/schedules"), jsmfake.Fault{Status: http.StatusCreated, Raw: []byte("not json")})
	c := ops.New(f.Client(), "cloud-1")

	res, err := c.Write(context.Background(), ops.Write{Method: ops.MethodPost, Path: "/v1/schedules", Body: schedule{Name: "x"}}, new(schedule))
	if !errors.Is(err, httperr.ErrUnknownOutcome) || res.Status != http.StatusCreated {
		t.Fatalf("got %+v, %v", res, err)
	}
}
