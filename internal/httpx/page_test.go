package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

type item struct {
	ID string `json:"id"`
}

func items(from, to int) []item {
	var out []item
	for i := from; i < to; i++ {
		out = append(out, item{ID: strconv.Itoa(i)})
	}
	return out
}

func ids(pages []httpx.Page[item]) []string {
	var out []string
	for _, p := range pages {
		for _, it := range p.Items {
			out = append(out, it.ID)
		}
	}
	return out
}

func collect[T any](seq iter.Seq2[httpx.Page[T], error]) ([]httpx.Page[T], error) {
	var pages []httpx.Page[T]
	for p, err := range seq {
		if err != nil {
			return pages, err
		}
		pages = append(pages, p)
	}
	return pages, nil
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

// pagerDuty serves n items the way PagerDuty's classic list endpoints do,
// applying at most maxLimit per page.
func pagerDuty(t *testing.T, n, maxLimit int, offsets *[]int) http.HandlerFunc {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		if q.Get("total") != "true" || q.Get("include[]") != "teams" {
			t.Errorf("query %s lost a parameter", r.URL.RawQuery)
		}
		mu.Lock()
		*offsets = append(*offsets, offset)
		mu.Unlock()
		limit = min(limit, maxLimit)
		end := min(offset+limit, n)
		writeJSON(w, map[string]any{"users": items(offset, end), "limit": limit, "offset": offset, "more": end < n, "total": n})
	}
}

func TestOffsetPages(t *testing.T) {
	var offsets []int
	client, _, _ := serve(t, httpx.Config{}, pagerDuty(t, 250, 100, &offsets))
	req := httpx.Request{Family: httpx.PagerDuty, Path: "/users", Query: url.Values{"include[]": {"teams"}}}
	pages, err := collect(httpx.Pages(context.Background(), client, req, httpx.Offset[item]("users", 100, 10000)))
	if err != nil || len(pages) != 3 || len(ids(pages)) != 250 || ids(pages)[249] != "249" {
		t.Fatalf("got %d pages, %d items, %v", len(pages), len(ids(pages)), err)
	}
	for _, p := range pages {
		if p.Total != 250 {
			t.Errorf("a page says the total is %d", p.Total)
		}
	}
	if !slices.Equal(offsets, []int{0, 100, 200}) {
		t.Errorf("offsets %v", offsets)
	}
	if req.Query.Get("limit") != "" {
		t.Errorf("Pages changed the caller's query")
	}

	// When PagerDuty applies a smaller limit than asked for, the next offset
	// follows the limit it applied.
	offsets = nil
	client, _, _ = serve(t, httpx.Config{}, pagerDuty(t, 250, 100, &offsets))
	pages, err = collect(httpx.Pages(context.Background(), client, req, httpx.Offset[item]("users", 1000, 10000)))
	if err != nil || len(ids(pages)) != 250 || !slices.Equal(offsets, []int{0, 100, 200}) {
		t.Errorf("got %d items at offsets %v, %v", len(ids(pages)), offsets, err)
	}
}

func TestOffsetStopsAtTheWindow(t *testing.T) {
	var offsets []int
	client, _, hits := serve(t, httpx.Config{}, pagerDuty(t, 12000, 100, &offsets))
	req := httpx.Request{Family: httpx.PagerDuty, Path: "/users", Query: url.Values{"include[]": {"teams"}}}
	var pages []httpx.Page[item]
	var last error
	for p, err := range httpx.Pages(context.Background(), client, req, httpx.Offset[item]("users", 100, 300)) {
		if err != nil {
			last = err
			if p.Total != 12000 {
				t.Errorf("the window error must keep the total, got %d", p.Total)
			}
			continue
		}
		pages = append(pages, p)
	}
	if !errors.Is(last, httpx.ErrWindow) || len(ids(pages)) != 300 || hits.Load() != 3 {
		t.Errorf("got %d items in %d calls, then %v", len(ids(pages)), hits.Load(), last)
	}
}

func TestOffsetSizePages(t *testing.T) {
	var offsets []string
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		offsets = append(offsets, q.Get("offset"))
		if q.Get("size") != "3" {
			t.Errorf("size %q", q.Get("size"))
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		// The API gives fewer than asked for; the CLI counts what came.
		end := min(offset+2, 7)
		links := map[string]string{}
		if end < 7 {
			links["next"] = "/v1/schedules?offset=" + strconv.Itoa(end) + "&size=3"
		}
		writeJSON(w, map[string]any{"values": items(offset, end), "links": links})
	})
	pages, err := collect(httpx.Pages(context.Background(), client,
		httpx.Request{Family: httpx.JSMOps, Path: "/jsm/ops/api/c1/v1/schedules"}, httpx.OffsetSize[item]("values", 3)))
	if err != nil || len(ids(pages)) != 7 || !slices.Equal(offsets, []string{"0", "2", "4", "6"}) {
		t.Errorf("got %v at offsets %v, %v", ids(pages), offsets, err)
	}
}

func TestCursorPages(t *testing.T) {
	next := map[string]any{"": "c1", "c1": "c2", "c2": nil}
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		writeJSON(w, map[string]any{"entities": []item{{ID: "after-" + c}}, "cursor": next[c]})
	})
	pages, err := collect(httpx.Pages(context.Background(), client,
		httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/gateway/api/public/teams/v1/org/o1/teams"}, httpx.Cursor[item]("entities")))
	if got := ids(pages); err != nil || !slices.Equal(got, []string{"after-", "after-c1", "after-c2"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestPageInfoPages(t *testing.T) {
	var bodies []map[string]any
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		more := b["after"] == nil
		writeJSON(w, map[string]any{"results": []item{{ID: "m" + strconv.Itoa(len(bodies))}},
			"pageInfo": map[string]any{"hasNextPage": more, "endCursor": "e1"}})
	})
	pages, err := collect(httpx.Pages(context.Background(), client,
		httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/gateway/api/public/teams/v1/org/o1/teams/t1/members"},
		httpx.PageInfo[item]("results", 50)))
	if err != nil || !slices.Equal(ids(pages), []string{"m1", "m2"}) || len(bodies) != 2 {
		t.Fatalf("got %v, %v", ids(pages), err)
	}
	if bodies[0]["first"] != 50.0 || bodies[0]["after"] != nil || bodies[1]["after"] != "e1" {
		t.Errorf("bodies %v", bodies)
	}
}

// TestLinkPages also checks that a link in an answer cannot move the request
// to another host or path: only its cursor is used.
func TestLinkPages(t *testing.T) {
	next := map[string]string{"": "abc", "abc": "https://evil.example.com/collect?cursor=def", "def": ""}
	client, router, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		writeJSON(w, map[string]any{"data": []item{{ID: "p" + c}}, "links": map[string]string{"next": next[c]}})
	})
	pages, err := collect(httpx.Pages(context.Background(), client, httpx.Request{Family: httpx.Admin, Path: "/admin/v1/orgs"}, httpx.Link[item]("data")))
	if err != nil || !slices.Equal(ids(pages), []string{"p", "pabc", "pdef"}) {
		t.Fatalf("got %v, %v", ids(pages), err)
	}
	for _, r := range router.Requests() {
		if !strings.HasPrefix(r.URL, "https://api.atlassian.com/admin/v1/orgs") {
			t.Errorf("a request went to %s", r.URL)
		}
	}
}

func TestPagesRejectsAnswersThatWouldNotEnd(t *testing.T) {
	cases := []struct {
		name  string
		req   httpx.Request
		style httpx.Style[item]
		body  string
	}{
		{"the same cursor again", httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/t"}, httpx.Cursor[item]("entities"),
			`{"entities":[{"id":"1"}],"cursor":"c1"}`},
		{"an empty page with more to come", httpx.Request{Family: httpx.PagerDuty, Path: "/users"}, httpx.Offset[item]("users", 100, 10000),
			`{"users":[],"more":true,"limit":100}`},
		{"an empty JSM page with a next link", httpx.Request{Family: httpx.JSMOps, Path: "/s"}, httpx.OffsetSize[item]("values", 50),
			`{"values":[],"links":{"next":"/v1/s?offset=50"}}`},
		{"no list under the key", httpx.Request{Family: httpx.PagerDuty, Path: "/users"}, httpx.Offset[item]("users", 100, 10000),
			`{"teams":[]}`},
		{"a link without a cursor", httpx.Request{Family: httpx.Admin, Path: "/admin/v1/orgs"}, httpx.Link[item]("data"),
			`{"data":[{"id":"1"}],"links":{"next":"/admin/v1/orgs?page=2"}}`},
		{"a next page without a cursor", httpx.Request{Family: httpx.Site, Site: "acme.atlassian.net", Path: "/m"}, httpx.PageInfo[item]("results", 50),
			`{"results":[{"id":"1"}],"pageInfo":{"hasNextPage":true}}`},
		{"a style not made by a constructor", httpx.Request{Family: httpx.PagerDuty, Path: "/users"}, httpx.Style[item]{},
			`{"":[]}`},
	}
	for _, c := range cases {
		client, _, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
			// The cursor style is asked again with the cursor it was given.
			_, _ = w.Write([]byte(c.body))
		})
		_, err := collect(httpx.Pages(context.Background(), client, c.req, c.style))
		if err == nil || hits.Load() > 2 {
			t.Errorf("%s: got %v after %d calls, want an error", c.name, err, hits.Load())
		}
		if c.name != "a style not made by a constructor" && !errors.Is(err, httperr.ErrBadResponse) {
			t.Errorf("%s: got %v, want ErrBadResponse", c.name, err)
		}
	}
}

func TestPagesStopsWhenTheCallerDoes(t *testing.T) {
	var offsets []int
	client, _, hits := serve(t, httpx.Config{}, pagerDuty(t, 1000, 100, &offsets))
	req := httpx.Request{Family: httpx.PagerDuty, Path: "/users", Query: url.Values{"include[]": {"teams"}}}
	for _, err := range httpx.Pages(context.Background(), client, req, httpx.Offset[item]("users", 100, 10000)) {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if hits.Load() != 1 {
		t.Errorf("%d calls after the caller stopped at the first page", hits.Load())
	}
}

func TestPagesEndsAtTheFirstError(t *testing.T) {
	client, _, hits := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	var pages, errs int
	for page, err := range httpx.Pages(context.Background(), client, httpx.Request{Family: httpx.PagerDuty, Path: "/users"}, httpx.Offset[item]("users", 100, 10000)) {
		if err != nil {
			errs++
			if !errors.Is(err, httperr.ErrForbidden) || page.Total != -1 || len(page.Items) != 0 {
				t.Errorf("got %v with page %+v", err, page)
			}
			continue
		}
		pages++
	}
	if pages != 0 || errs != 1 || hits.Load() != 1 {
		t.Errorf("got %d pages and %d errors in %d calls, want the one error", pages, errs, hits.Load())
	}
}
