package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

const feedToken = "FEEDTOKEN0123456789"

// scheduleAnswer is shaped like a PagerDuty schedule read: both feed fields
// at the top, the feed URL inside a description and a list, and fields that
// must come through untouched.
const scheduleAnswer = `{"schedule":{"id":"P1","name":"Ops <primary> & co","big":12345678901234567890,` +
	`"http_cal_url":"https://acme.pagerduty.com/private/` + feedToken + `/feed",` +
	`"web_cal_url":"webcal://acme.pagerduty.com/private/` + feedToken + `/feed",` +
	`"description":"Primary rota. Feed: https://acme.pagerduty.com/private/` + feedToken + `/feed (ask ops)",` +
	`"links":["https://acme.pagerduty.com/private/` + feedToken + `/feed","https://acme.pagerduty.com/schedules/P1"],` +
	`"layers":[{"id":"L1","web_cal_url":"webcal://x.pagerduty.com/private/` + feedToken + `"}]}}`

func TestPagerDutyAnswersHoldNoCalendarURL(t *testing.T) {
	c, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/schedules" {
			_, _ = io.WriteString(w, `{"schedules":[{"id":"P1","http_cal_url":"https://a.pagerduty.com/private/`+feedToken+`"}],"more":false,"limit":100,"offset":0,"total":1}`)
			return
		}
		_, _ = io.WriteString(w, scheduleAnswer)
	})
	ctx := context.Background()

	resp, err := c.Do(ctx, httpx.Request{Family: httpx.PagerDuty, Path: "/schedules/P1"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(resp.Body)
	if strings.Contains(body, feedToken) || strings.Contains(body, "cal_url") {
		t.Errorf("the answer still holds a feed URL: %s", body)
	}
	for _, kept := range []string{`"name":"Ops <primary> & co"`, `"big":12345678901234567890`, `"https://acme.pagerduty.com/schedules/P1"`, `"id":"L1"`} {
		if !strings.Contains(body, kept) {
			t.Errorf("the answer lost %s: %s", kept, body)
		}
	}
	// Text around a feed URL is kept, with a marker where the URL was, so
	// the migration carries it over; a list keeps its length.
	for _, kept := range []string{
		`"description":"Primary rota. Feed: [redacted] (ask ops)"`,
		`"links":["[redacted]","https://acme.pagerduty.com/schedules/P1"]`,
	} {
		if !strings.Contains(body, kept) {
			t.Errorf("the answer misses %s: %s", kept, body)
		}
	}

	var got struct {
		Schedule struct {
			HTTPCalURL string `json:"http_cal_url"`
			WebCalURL  string `json:"web_cal_url"`
		} `json:"schedule"`
	}
	if _, err := c.DoJSON(ctx, httpx.Request{Family: httpx.PagerDuty, Path: "/schedules/P1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schedule.HTTPCalURL != "" || got.Schedule.WebCalURL != "" {
		t.Error("DoJSON decoded a feed URL")
	}

	for page, err := range httpx.Pages(ctx, c, httpx.Request{Family: httpx.PagerDuty, Path: "/schedules"}, httpx.Offset[map[string]any]("schedules", 100, 10000)) {
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Items {
			if _, ok := s["http_cal_url"]; ok {
				t.Error("a paged item kept its feed URL")
			}
		}
	}
}

func TestAnswersWithoutACalendarURLAreUntouched(t *testing.T) {
	const plain = `{"users":[{"id":"P1","name":"A & B","n":1.50}],"more":false}`
	const jsm = `{"web_cal_url":"https://acme.pagerduty.com/private/` + feedToken + `"}`
	c, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jsm" {
			_, _ = io.WriteString(w, jsm)
			return
		}
		_, _ = io.WriteString(w, plain)
	})
	resp, err := c.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/users"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != plain {
		t.Errorf("an answer without a feed URL changed: %s", resp.Body)
	}
	// Feed URLs are PagerDuty's, so other families' answers are left as
	// they are.
	resp, err = c.Do(context.Background(), httpx.Request{Family: httpx.JSMOps, Path: "/jsm"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != jsm {
		t.Errorf("a JSM Operations answer changed: %s", resp.Body)
	}
}

func TestAnUnreadablePagerDutyAnswerHoldsNoCalendarURL(t *testing.T) {
	c, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"http_cal_url":"https://acme.pagerduty.com/private/`+feedToken+`"`)
	})
	resp, err := c.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/schedules/P1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), feedToken) {
		t.Errorf("a cut-short answer still holds the feed URL: %s", resp.Body)
	}
}

// A feed URL used as a field name cannot be removed without what the field
// holds, so the answer is refused instead of silently losing it.
func TestAFeedURLAsAFieldNameIsUnreadable(t *testing.T) {
	c, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"by_feed":{"https://acme.pagerduty.com/private/`+feedToken+`":{"id":"P1"}}}`)
	})
	_, err := c.Do(context.Background(), httpx.Request{Family: httpx.PagerDuty, Path: "/schedules"})
	if !errors.Is(err, httperr.ErrBadResponse) {
		t.Fatalf("got %v, want ErrBadResponse", err)
	}
	if strings.Contains(err.Error(), feedToken) {
		t.Errorf("the error quotes the feed URL: %v", err)
	}
}
