package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Config is what New needs to build a Client.
type Config struct {
	// Credentials are what the families send. A family whose credential is
	// missing refuses its requests with httperr.ErrRefused.
	Credentials credentials.Set
	// PagerDutyRegion is where the PagerDuty account is hosted; empty means
	// RegionUS.
	PagerDutyRegion Region
	// Staging sends the JSMOps and Admin families to the Atlassian staging
	// API host, api.stg.atlassian.com, for staging sites.
	Staging bool
	// Logger gets a debug line for every attempt; nil discards them.
	Logger *slog.Logger
	// UserAgent is sent with every request; empty means "jsm-ops-migrator".
	UserAgent string
	// Timeout bounds one attempt, from connecting to reading the whole body;
	// zero means DefaultTimeout.
	Timeout time.Duration
	// Retry bounds the retries; the zero value means DefaultRetry.
	Retry Retry
	// Limits bound how fast each family is called; nil means DefaultLimits.
	// A family missing from a non-nil map has no limit.
	Limits map[Family]Limit
	// Transport replaces the network transport, for tests that route
	// requests to a fake. The host checks, the PagerDuty guard and the
	// credentials apply before it, so those tests exercise them too.
	Transport http.RoundTripper
}

// DefaultTimeout bounds one attempt when Config.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// Retry bounds the retries of one request.
type Retry struct {
	// MaxAttempts counts every attempt, the first included; 1 sends a
	// request once.
	MaxAttempts int
	// MinWait and MaxWait bound the exponential backoff used when the API
	// does not say how long to wait.
	MinWait, MaxWait time.Duration
}

// DefaultRetry is the Retry that New uses for a zero Config.Retry. It is
// explicit because a zero retry count silently turns retries off.
func DefaultRetry() Retry { return Retry{MaxAttempts: 5, MinWait: time.Second, MaxWait: time.Minute} }

// Limit bounds how fast one family is called.
type Limit struct {
	// PerMinute is the most attempts started in any minute; 0 means no
	// limit.
	PerMinute int
	// Serial sends one request at a time, its retries included.
	Serial bool
}

// DefaultLimits returns the limits New uses for a nil Config.Limits:
// PagerDuty one request at a time at 840 a minute, below its limit of 960 a
// minute per key, so the CLI's own retries never push it over.
func DefaultLimits() map[Family]Limit {
	return map[Family]Limit{PagerDuty: {PerMinute: 840, Serial: true}}
}

// Client sends every API call the CLI makes. It is safe for concurrent use.
// Build one per credential set and share it, so its limits hold across every
// caller.
type Client struct {
	cfg      Config
	http     *http.Client
	limits   map[Family]*limiter
	redactor *credentials.Redactor
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

// New checks cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	switch cfg.PagerDutyRegion {
	case "", RegionUS, RegionEU:
	default:
		return nil, fmt.Errorf("unknown PagerDuty region %q: want %q or %q", cfg.PagerDutyRegion, RegionUS, RegionEU)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("negative timeout %s", cfg.Timeout)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Retry == (Retry{}) {
		cfg.Retry = DefaultRetry()
	}
	if r := cfg.Retry; r.MaxAttempts < 1 || r.MinWait < 0 || r.MaxWait < r.MinWait {
		return nil, fmt.Errorf("invalid retry settings %+v: want at least one attempt and 0 <= MinWait <= MaxWait", r)
	}
	if cfg.Limits == nil {
		cfg.Limits = DefaultLimits()
	}
	cfg.Limits = maps.Clone(cfg.Limits)
	for f, l := range cfg.Limits {
		if !f.known() {
			return nil, fmt.Errorf("limit for unknown API family %q", f)
		}
		if l.PerMinute < 0 {
			return nil, fmt.Errorf("negative limit for %s", f)
		}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "jsm-ops-migrator"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	base := cfg.Transport
	if base == nil {
		base = defaultTransport()
	}
	creds := cfg.Credentials
	c := &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: guard{next: base},
			Timeout:   cfg.Timeout,
			// A redirect could carry the credential to another host, so none
			// is followed; the 3xx comes back as an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		limits: map[Family]*limiter{},
		redactor: credentials.NewRedactor(creds.PagerDutyKey, creds.AtlassianToken, creds.OrgAPIKey,
			credentials.NewSecret(creds.AtlassianEmail.Value())),
		now:   time.Now,
		sleep: sleep,
	}
	for f, l := range cfg.Limits {
		c.limits[f] = newLimiter(l)
	}
	return c, nil
}

// Do sends req and returns the answer when its status is 2xx. Anything else
// is an *httperr.Error that wraps the sentinel classifying it. A request that
// breaks the family's rules is refused before anything is sent. Reads are
// retried; a write is sent again only when it cannot have reached the server,
// or the API answered 429.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	u, err := c.target(req)
	if err != nil {
		return nil, err
	}
	var body []byte
	if req.Body != nil {
		if body, err = json.Marshal(req.Body); err != nil {
			return nil, c.fail(req, httperr.ErrRefused, result{err: fmt.Errorf("encoding the body: %w", err)})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, c.fail(req, httperr.ErrUnavailable, result{err: err})
	}
	lim := c.limits[req.Family]
	if lim != nil && lim.serial != nil {
		select {
		case lim.serial <- struct{}{}:
			defer func() { <-lim.serial }()
		case <-ctx.Done():
			return nil, c.fail(req, httperr.ErrUnavailable, result{err: ctx.Err()})
		}
	}
	for n := 1; ; n++ {
		if lim != nil && lim.pace != nil {
			if err := lim.pace.wait(ctx, c.now, c.sleep); err != nil {
				return nil, c.fail(req, httperr.ErrUnavailable, result{err: err})
			}
		}
		start := c.now()
		res := c.attempt(ctx, req, u, body)
		kind, retry := judge(ctx, req, res)
		c.log(ctx, req, n, res, c.now().Sub(start))
		if kind == nil {
			return &Response{Status: res.status, Body: res.body, RequestID: requestID(res.header), header: res.header, req: req}, nil
		}
		if !retry || n >= c.cfg.Retry.MaxAttempts {
			return nil, c.fail(req, kind, res)
		}
		wait := c.backoff(req.Family, n, res)
		attrs := []slog.Attr{slog.String("family", string(req.Family)), slog.String("method", string(req.method())),
			slog.String("path", req.Path), slog.Duration("wait", wait)}
		if res.status == http.StatusTooManyRequests && req.write() {
			// The API refused the change without acting on it, so it is safe
			// to send again. The user still sees it at the default log level,
			// because a change sent twice should never be a surprise.
			c.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "the API's rate limit refused a change; sending it again", attrs...)
		} else {
			c.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "retrying API call", attrs...)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, c.fail(req, httperr.ErrUnavailable, result{status: res.status, header: res.header, err: err})
		}
	}
}

// DoJSON sends req and decodes a 2xx answer's body into v. The response is
// returned with a decoding error too, for its request id.
func (c *Client) DoJSON(ctx context.Context, req Request, v any) (*Response, error) {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, resp.Decode(v)
}

// target checks req against its family's rules and returns where it goes.
func (c *Client) target(req Request) (*url.URL, error) {
	refuse := func(format string, args ...any) error {
		return c.fail(req, httperr.ErrRefused, result{err: fmt.Errorf(format, args...)})
	}
	if !req.Family.known() {
		return nil, refuse("unknown API family %q", req.Family)
	}
	m := req.method()
	if !m.known() {
		return nil, refuse("unknown method %q", m)
	}
	// The first of two guards that keep PagerDuty read-only; the transport
	// guard checks again by host. tools/pdseed is the only code that may
	// write there, and it does not use this package.
	if req.Family == PagerDuty && m != MethodGet {
		return nil, refuse("PagerDuty is read-only, so only GET is sent")
	}
	if !validPath(req.Path) {
		return nil, refuse("path %q must start with / and hold no host, query, fragment or dot segment", req.Path)
	}
	for _, k := range needs[req.Family] {
		if !c.cfg.Credentials.Has(k) {
			return nil, refuse("no %s", k.Label())
		}
	}
	host, err := c.host(req.Family, req.Site)
	if err != nil {
		return nil, refuse("%w", err)
	}
	return &url.URL{Scheme: "https", Host: host, Path: req.Path, RawQuery: req.Query.Encode()}, nil
}

func (c *Client) fail(req Request, kind error, res result) error {
	e := &httperr.Error{
		Family: string(req.Family), Method: string(req.method()), Path: req.Path,
		Status: res.status, RequestID: requestID(res.header), Kind: kind, Cause: c.cause(res.err),
	}
	if len(res.body) > 0 {
		e.Body = c.errorBody(res.body)
	}
	return e
}

// log writes one line per attempt. It leaves out the query, which can hold
// personal data, and the body.
func (c *Client) log(ctx context.Context, req Request, n int, res result, took time.Duration) {
	attrs := []slog.Attr{
		slog.String("family", string(req.Family)), slog.String("method", string(req.method())),
		slog.String("path", req.Path), slog.Int("attempt", n), slog.Duration("took", took),
	}
	if res.status != 0 {
		attrs = append(attrs, slog.Int("status", res.status))
	}
	if id := requestID(res.header); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if res.err != nil {
		attrs = append(attrs, slog.String("error", c.cause(res.err).Error()))
	}
	c.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "API call", attrs...)
}
