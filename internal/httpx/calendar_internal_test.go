package httpx

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// holdsCalendarURL reports whether any key or string in a decoded JSON value
// holds a feed URL, and, with fields, whether it has a feed field at all.
func holdsCalendarURL(v any, fields bool) bool {
	switch t := v.(type) {
	case string:
		return credentials.HasCalendarURL(t)
	case map[string]any:
		for k, x := range t {
			if fields && calendarKeys[k] || credentials.HasCalendarURL(k) || holdsCalendarURL(x, fields) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if holdsCalendarURL(x, fields) {
				return true
			}
		}
	}
	return false
}

func FuzzDropCalendarURLs(f *testing.F) {
	for _, seed := range []string{
		`{"schedule":{"id":"P1","http_cal_url":"https://a.pagerduty.com/private/T0K3N"}}`,
		`{"web_cal_url":null,"x":["webcal://a.pagerduty.com/private/T0K3N",1]}`,
		`{"d":"https:\/\/a.pagerduty.com\/private\/T0K3N"}`,
		`{"https://a.pagerduty.com/private/T0K3N":1}`,
		`{"users":[{"id":"P1","name":"A é","n":12345678901234567890}],"more":false}`,
		`{"cut":"https://a.pagerduty.com/private/T0K3N`,
		"{\"\":{\"http_cal_url\":\"\x1e\"}}",
		`0 http://pAgerdutY.Com/privAte/000`,
		`"http://pAgerdutY.Com/privAte/"`,
		`{"description":"Primary rota. Feed: https://a.pagerduty.com/private/T0K3N/feed (ask ops)"}`,
		`[]`, `null`, ``,
	} {
		f.Add([]byte(seed))
	}
	c, err := New(Config{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := c.dropCalendarURLs(bytes.Clone(in))
		var before, after any
		if json.Unmarshal(in, &before) != nil {
			// An answer that will not decode loses only its URLs, as text.
			if err != nil || credentials.HasCalendarURL(string(out)) || json.Unmarshal(out, &after) == nil && holdsCalendarURL(after, false) {
				t.Fatalf("%q became %q (%v), which holds a feed URL", in, out, err)
			}
			return
		}
		if hasFeedKey(before) {
			if err == nil {
				t.Fatalf("%q uses a feed URL as a field name but was read as %q", in, out)
			}
			return
		}
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if err := json.Unmarshal(out, &after); err != nil {
			t.Fatalf("%q became %q, which does not decode: %v", in, out, err)
		}
		if holdsCalendarURL(after, true) {
			t.Fatalf("%q kept a feed URL: %q", in, out)
		}
		// Only the feed fields go, and only the URL in any other string:
		// everything else is kept as it was.
		if want := expected(before); !reflect.DeepEqual(after, want) {
			t.Fatalf("%q became %q, want %v", in, out, want)
		}
		if !holdsCalendarURL(before, true) && !bytes.Equal(in, out) {
			t.Fatalf("%q holds no feed URL but became %q", in, out)
		}
	})
}

// hasFeedKey reports whether a field name anywhere in v holds a feed URL.
func hasFeedKey(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if credentials.HasCalendarURL(k) || hasFeedKey(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if hasFeedKey(x) {
				return true
			}
		}
	}
	return false
}

// expected is v as dropCalendarURLs should leave it: without the feed
// fields, and with each feed URL in any other string replaced by the marker.
func expected(v any) any {
	switch t := v.(type) {
	case string:
		return credentials.RedactCalendarURLs(t)
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if !calendarKeys[k] {
				out[k] = expected(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = expected(x)
		}
		return out
	}
	return v
}
