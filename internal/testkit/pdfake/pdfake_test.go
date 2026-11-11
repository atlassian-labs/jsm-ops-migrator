package pdfake_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/pdfake"
)

// spy records what the fake reports instead of failing the test.
type spy struct{ errs []string }

func (s *spy) Helper() {}
func (s *spy) Errorf(format string, args ...any) {
	s.errs = append(s.errs, fmt.Sprintf(format, args...))
}

const key = "pd-key-value-1234"

func get(f *pdfake.Fake, method, host, target, accept string) (int, string) {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.Header.Set("Authorization", "Token token="+key)
	r.Header.Set("Accept", accept)
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

func TestAWriteFailsTheTest(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		s := &spy{}
		f := pdfake.New(s, key)
		f.Answer("/users", map[string]any{})
		if code, _ := get(f, m, pdfake.HostUS, "/users", pdfake.AcceptV2); code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d", m, code)
		}
		if len(s.errs) != 1 || !strings.Contains(s.errs[0], "must only be read") {
			t.Errorf("%s reported %q", m, s.errs)
		}
	}
}

func TestAnotherHostFailsTheTest(t *testing.T) {
	s := &spy{}
	f := pdfake.New(s, key)
	if code, _ := get(f, http.MethodGet, "example.com", "/users", pdfake.AcceptV2); code != http.StatusMisdirectedRequest || len(s.errs) != 1 {
		t.Errorf("answered %d, reported %q", code, s.errs)
	}
}

func TestTheFakeChecksKeyRegionAndAccept(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/v3/schedules", map[string]any{})
	cases := []struct {
		host, accept string
		want         int
	}{
		{pdfake.HostUS, pdfake.AcceptV3, http.StatusOK},
		{pdfake.HostEU, pdfake.AcceptV3, http.StatusUnauthorized},
		{pdfake.HostUS, pdfake.AcceptV2, http.StatusNotAcceptable},
	}
	for _, c := range cases {
		if code, _ := get(f, http.MethodGet, c.host, "/v3/schedules", c.accept); code != c.want {
			t.Errorf("%s with %s: %d, want %d", c.host, c.accept, code, c.want)
		}
	}
}

func TestListPagesLikePagerDuty(t *testing.T) {
	f := pdfake.New(t, key)
	f.List("/users", "users", []any{"a", "b", "c"})
	var page struct {
		Users  []string `json:"users"`
		More   bool     `json:"more"`
		Limit  int      `json:"limit"`
		Offset int      `json:"offset"`
		Total  *int     `json:"total"`
	}
	code, body := get(f, http.MethodGet, pdfake.HostUS, "/users?limit=2&offset=0", pdfake.AcceptV2)
	if err := json.Unmarshal([]byte(body), &page); err != nil || code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Join(page.Users, "") != "ab" || !page.More || page.Limit != 2 || page.Total != nil {
		t.Errorf("first page %+v", page)
	}
	page.Users = nil
	_, body = get(f, http.MethodGet, pdfake.HostUS, "/users?limit=2&offset=2&total=true", pdfake.AcceptV2)
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if strings.Join(page.Users, "") != "c" || page.More || page.Total == nil || *page.Total != 3 {
		t.Errorf("second page %+v", page)
	}
	// The default page is 25, the most is 100, and nothing past the window
	// is answered.
	for target, want := range map[string]int{
		"/users":                       http.StatusOK,
		"/users?limit=101":             http.StatusBadRequest,
		"/users?offset=9900&limit=100": http.StatusOK,
		"/users?offset=9901&limit=100": http.StatusBadRequest,
	} {
		if code, _ := get(f, http.MethodGet, pdfake.HostUS, target, pdfake.AcceptV2); code != want {
			t.Errorf("%s: %d, want %d", target, code, want)
		}
	}
}

func TestFaultsComeInOrder(t *testing.T) {
	f := pdfake.New(t, key)
	f.Answer("/abilities", map[string]any{"abilities": []string{}})
	f.Fail("/abilities", pdfake.Fault{Status: 429, Header: http.Header{"Ratelimit-Reset": {"1"}}}, pdfake.Fault{Status: 503})
	var codes []int
	for range 3 {
		code, _ := get(f, http.MethodGet, pdfake.HostUS, "/abilities", pdfake.AcceptV2)
		codes = append(codes, code)
	}
	if fmt.Sprint(codes) != "[429 503 200]" {
		t.Errorf("codes = %v", codes)
	}
	if code, _ := get(f, http.MethodGet, pdfake.HostUS, "/nothing", pdfake.AcceptV2); code != http.StatusNotFound {
		t.Errorf("an unknown path answered %d", code)
	}
}
