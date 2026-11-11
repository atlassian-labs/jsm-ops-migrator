package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// maxBody bounds how much of an answer is read, so a broken server cannot
// exhaust memory; the largest pages the CLI reads are far below it.
var maxBody int64 = 32 << 20

// maxErrorBody bounds the part of an error answer kept in httperr.Error.Body.
const maxErrorBody = 1024

var errTooLarge = errors.New("the answer is larger than the CLI reads")

func defaultTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		// A custom TLS config turns HTTP/2 off unless it is asked for.
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
}

type targetKey struct{}

// target is where Do meant a request to go.
type target struct {
	host   string
	family Family
}

// guard is the last check before the network, so the rules hold for every
// request whatever built it: HTTPS only, only the host Do chose, and only GET
// to PagerDuty.
type guard struct{ next http.RoundTripper }

var errGuard = errors.New("the transport refused the request")

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	t, ok := r.Context().Value(targetKey{}).(target)
	var why string
	switch {
	case !ok:
		why = "it was not built by Do"
	case r.URL.Scheme != "https" || r.URL.Host != t.host:
		why = fmt.Sprintf("%s://%s is not the host chosen for %s", r.URL.Scheme, r.URL.Host, t.family)
	case isPagerDutyHost(r.URL.Hostname()) && r.Method != http.MethodGet:
		why = "PagerDuty is read-only"
	default:
		return g.next.RoundTrip(r)
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, fmt.Errorf("%w: %s", errGuard, why)
}

// result is how one attempt ended.
type result struct {
	status int
	header http.Header
	body   []byte
	// err is set when no complete answer arrived.
	err error
	// sent reports whether the request may have reached the server, which
	// decides whether a write can be sent again. It is set once the attempt
	// has a connection, before any byte of it can be written.
	sent bool
}

func (c *Client) attempt(ctx context.Context, req Request, u *url.URL, body []byte) result {
	var sent atomic.Bool
	ctx = context.WithValue(ctx, targetKey{}, target{host: u.Host, family: req.Family})
	// Not WroteHeaders: HTTP/2 writes and flushes the HEADERS frame before it
	// fires that hook, on another goroutine, so a timeout in between returns
	// with a bodyless write already on the wire and the hook not yet run.
	// GotConn fires before any byte can be written, so a write is counted as
	// possibly sent too early, never too late.
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { sent.Store(true) }})
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, string(req.method()), u.String(), rd)
	if err != nil {
		return result{err: err}
	}
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	hr.Header.Set("Accept", accept)
	hr.Header.Set("User-Agent", c.cfg.UserAgent)
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	c.authorize(hr, req.Family)

	resp, err := c.http.Do(hr)
	if err != nil {
		return result{err: err, sent: sent.Load()}
	}
	defer func() { _ = resp.Body.Close() }()
	res := result{status: resp.StatusCode, header: resp.Header, sent: true}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		res.err = err
	case int64(len(data)) > maxBody:
		res.err = errTooLarge
	default:
		res.body = data
	}
	return res
}

func requestID(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "Atl-Traceid"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// emailRe finds email addresses. An API's error text can name any user, and
// an email address is personal data, so it never reaches an error message.
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)

// errorBody redacts an error answer before cutting it short, so a cut can
// never leave part of a secret behind.
func (c *Client) errorBody(b []byte) string {
	var s string
	if json.Valid(b) {
		s = string(c.redactor.JSON(b))
	} else {
		s = c.redactor.String(string(b))
	}
	s = emailRe.ReplaceAllString(s, credentials.Redacted)
	if len(s) > maxErrorBody {
		s = strings.ToValidUTF8(s[:maxErrorBody], "") + "…"
	}
	return s
}

// cleanCause drops the URL that net/http puts in its errors, because the
// query can hold personal data such as the email address of a user search.
func cleanCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// cause is the cause an httperr.Error carries. Its text is redacted, because
// a transport error can name a proxy's credentials or the value of a header,
// and the error reaches messages and review files that nothing else redacts.
// errors.Is still matches the original, such as context.Canceled.
func (c *Client) cause(err error) error {
	if err == nil {
		return nil
	}
	err = cleanCause(err)
	if msg := c.scrub(err.Error()); msg != err.Error() {
		return &scrubbed{msg: msg, err: err}
	}
	return err
}

// scrub removes the client's credentials, what the redactor's patterns find
// and email addresses from text.
func (c *Client) scrub(s string) string {
	return emailRe.ReplaceAllString(c.redactor.String(s), credentials.Redacted)
}

// scrubbed is an error whose text was redacted.
type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.err }

// decodeCause says why an answer did not decode without quoting any of it.
// The errors from encoding/json, and from the types it decodes into such as
// time.Time, can repeat the value they could not read, and that value can be
// a phone number, an email address or a calendar URL. Redacting would miss
// some of those, so none of the value is kept.
func decodeCause(err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var invalid *json.InvalidUnmarshalError
	switch {
	case errors.As(err, &syntax):
		return fmt.Errorf("not valid JSON at byte %d", syntax.Offset)
	case errors.As(err, &typ):
		// Not Field: it holds the map keys on the path as well as the struct
		// field names, and a map can be keyed by an email address or a phone
		// number. Struct is the Go type the value sits in. Value starts with
		// the JSON kind; anything after it would be the value itself.
		kind, _, _ := strings.Cut(typ.Value, " ")
		in := "the answer"
		if typ.Struct != "" {
			in = typ.Struct
		}
		return fmt.Errorf("a value in %s is a JSON %s, not a %s", in, kind, typ.Type)
	case errors.As(err, &invalid):
		// A bug in the caller; the message names only Go types.
		return invalid
	}
	return errors.New("a value in the answer does not have the expected form")
}
