package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// calendarKeys are the fields PagerDuty puts a schedule's calendar feed URL
// in.
var calendarKeys = map[string]bool{"http_cal_url": true, "web_cal_url": true}

// mayHoldFeedRe finds answers that need parsing. Every feed URL has /private/
// in its path, so most answers are passed through as they are. Case is
// ignored, as the redactor's feed URL pattern ignores it, and an answer with a
// \u escape is parsed anyway, since the escape could spell either word.
var mayHoldFeedRe = regexp.MustCompile(`(?i)cal_url|private|\\u`)

// errFeedKey is an answer that uses a feed URL as a field name.
var errFeedKey = errors.New("a field name in the answer holds a calendar feed URL")

// dropCalendarURLs removes every PagerDuty calendar feed URL from an answer
// before anything can decode it. A feed URL is a bearer credential, since
// anyone holding it can read the schedule, and no step needs it, so it never
// leaves this package.
//
// The http_cal_url and web_cal_url fields are deleted, because they hold
// nothing else. In any other string, such as a schedule's description, only
// the URL is replaced with "[redacted]": the migration carries that text over,
// and losing all of it without a sign would be worse than a marker where the
// URL was. An answer that uses a feed URL as a field name cannot be read
// without the URL, so it is an error instead of a silent loss of what the
// field holds. An answer without a feed URL comes back unchanged.
func (c *Client) dropCalendarURLs(body []byte) ([]byte, error) {
	if !mayHoldFeedRe.Match(body) {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	// Numbers stay as written, so an ID that is a large integer keeps every
	// digit.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || !errors.Is(dec.Decode(new(any)), io.EOF) {
		// It will not decode anyway; the URLs go from the text, so the
		// body holds none in any form.
		return []byte(credentials.RedactCalendarURLs(string(body))), nil
	}
	v, changed, err := dropCalendar(v)
	if err != nil || !changed {
		return body, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// dropCalendar removes feed URLs from a decoded JSON value, as
// dropCalendarURLs describes, and reports whether it changed anything.
func dropCalendar(v any) (any, bool, error) {
	changed := false
	switch t := v.(type) {
	case string:
		if credentials.HasCalendarURL(t) {
			return credentials.RedactCalendarURLs(t), true, nil
		}
	case map[string]any:
		for k, x := range t {
			if credentials.HasCalendarURL(k) {
				return nil, false, errFeedKey
			}
			if calendarKeys[k] {
				delete(t, k)
				changed = true
				continue
			}
			x, c, err := dropCalendar(x)
			if err != nil {
				return nil, false, err
			}
			t[k], changed = x, changed || c
		}
	case []any:
		for i, x := range t {
			x, c, err := dropCalendar(x)
			if err != nil {
				return nil, false, err
			}
			t[i], changed = x, changed || c
		}
	}
	return v, changed, nil
}
