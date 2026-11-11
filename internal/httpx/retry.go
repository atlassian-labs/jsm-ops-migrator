package httpx

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// maxServerWait caps a wait the API asks for, so a wrong header cannot stall
// a run for hours.
const maxServerWait = 5 * time.Minute

// pagerDutyQuietWait is the wait after a PagerDuty 429 that does not say when
// to come back. PagerDuty's limit is per minute, so a minute always clears it.
const pagerDutyQuietWait = time.Minute

// judge turns how an attempt ended into the sentinel Do returns, nil for a
// success, and whether to try again.
func judge(ctx context.Context, req Request, res result) (kind error, retry bool) {
	write := req.write()
	// An answer too large to read still has a status, and the status decides
	// what happened, so only a 2xx is judged by its size.
	tooLarge := errors.Is(res.err, errTooLarge)
	switch {
	case errors.Is(res.err, errGuard):
		return httperr.ErrRefused, false
	case res.err != nil && !tooLarge:
		if write && res.sent {
			return httperr.ErrUnknownOutcome, false
		}
		// A write that never left can be sent again: nothing reached the
		// server.
		return httperr.ErrUnavailable, ctx.Err() == nil
	case res.status >= 200 && res.status < 300:
		switch {
		case !tooLarge:
			return nil, false
		case write:
			// The write took effect, but what it did cannot be read from
			// the answer and has to be looked up.
			return httperr.ErrUnknownOutcome, false
		}
		return httperr.ErrBadResponse, false
	case res.status == http.StatusTooManyRequests:
		// The API refused the call without acting on it, so even a write may
		// be sent again.
		return httperr.ErrUnavailable, true
	case res.status >= 500:
		if write {
			return httperr.ErrUnknownOutcome, false
		}
		return httperr.ErrUnavailable, res.status != http.StatusNotImplemented
	}
	return statusKind(res.status), false
}

func statusKind(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return httperr.ErrUnauthorized
	case http.StatusPaymentRequired:
		return httperr.ErrNotInPlan
	case http.StatusForbidden:
		return httperr.ErrForbidden
	case http.StatusNotFound, http.StatusGone:
		return httperr.ErrNotFound
	case http.StatusConflict:
		return httperr.ErrConflict
	}
	if status >= 300 && status < 400 {
		// Redirects are never followed, so one is an answer the CLI cannot use.
		return httperr.ErrBadResponse
	}
	return httperr.ErrInvalid
}

// backoff is how long to wait before attempt+1: what the API asks for first,
// then PagerDuty's quiet minute, then exponential backoff.
func (c *Client) backoff(f Family, attempt int, res result) time.Duration {
	if d, ok := retryAfter(res.header.Get("Retry-After"), c.now()); ok {
		return min(d, maxServerWait)
	}
	if res.status == http.StatusTooManyRequests {
		// PagerDuty sends ratelimit-reset on every answer, so it only counts
		// once the limit is hit.
		if s, err := strconv.Atoi(strings.TrimSpace(res.header.Get("Ratelimit-Reset"))); err == nil && s >= 0 {
			return min(time.Duration(s)*time.Second, maxServerWait)
		}
		if f == PagerDuty {
			return pagerDutyQuietWait
		}
	}
	return c.exponential(attempt)
}

func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(s, 0)) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// exponential doubles MinWait for each attempt up to MaxWait, then picks a
// random wait between half of that and all of it, so callers that failed
// together do not all come back together.
func (c *Client) exponential(attempt int) time.Duration {
	r := c.cfg.Retry
	d := r.MaxWait
	if attempt <= 32 {
		if e := r.MinWait << (attempt - 1); e > 0 && e < d {
			d = e
		}
	}
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
}

// limiter applies one family's Limit.
type limiter struct {
	// serial holds a token while a call is in flight, retries included.
	serial chan struct{}
	pace   *pacer
}

func newLimiter(l Limit) *limiter {
	lim := &limiter{}
	if l.Serial {
		lim.serial = make(chan struct{}, 1)
	}
	if l.PerMinute > 0 {
		lim.pace = &pacer{every: time.Minute / time.Duration(l.PerMinute)}
	}
	return lim
}

// pacer spaces the starts of attempts at least every apart. A caller reserves
// its slot under the lock and waits outside it, so callers start in the order
// they arrived.
type pacer struct {
	every time.Duration
	mu    sync.Mutex
	next  time.Time
}

func (p *pacer) wait(ctx context.Context, now func() time.Time, sleep func(context.Context, time.Duration) error) error {
	p.mu.Lock()
	t := now()
	at := p.next
	if at.Before(t) {
		at = t
	}
	p.next = at.Add(p.every)
	p.mu.Unlock()
	return sleep(ctx, at.Sub(t))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
