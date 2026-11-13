package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/internal/rest"
)

// ErrEnableFailed means JSM reported that enabling Operations on the team
// failed. Nothing is retried after it: JSM finished the request and said no.
var ErrEnableFailed = errors.New("JSM reported that enabling Operations failed")

// What JSM's answers to enable-ops say, matched in lower case. JSM holds one
// lock for the whole site while it enables a team, and a second call fails at
// once with the lock message. A team that the Teams API has just created can
// also take a moment to reach JSM, which answers that it does not know the
// team: "Team not found with identifier: …" at runtime, "The team is not
// found." in the documentation. Neither call did anything, so both are sent
// again. Only these messages are, so a refusal about something else, such as
// an admin account JSM does not know, fails at once. "already exists" means
// Operations is already on for the team, which is what the caller wanted.
const (
	lockText          = "already being processed"
	alreadyExistsText = "already exists"
)

var teamNotFoundTexts = []string{"team not found", "team is not found"}

// timing bounds enable-ops. Until its latency is measured on a staging site,
// the defaults are first guesses. Ten attempts and two minutes of polling are
// enough for a customer's other enable-ops call to finish, without stalling a
// run when JSM never will. A team that JSM still does not know after five
// attempts, about half a minute, is taken as gone, because a deleted team
// answers the same as one that has not arrived yet.
type timing struct {
	attempts, teamAttempts    int
	minWait, maxWait          time.Duration
	pollInterval, pollTimeout time.Duration
	sleep                     func(context.Context, time.Duration) error
}

func defaultTiming() timing {
	return timing{
		attempts: 10, teamAttempts: 5, minWait: 2 * time.Second, maxWait: 30 * time.Second,
		pollInterval: 5 * time.Second, pollTimeout: 2 * time.Minute, sleep: sleep,
	}
}

// wait is how long to wait before attempt n+1.
func (t timing) wait(n int) time.Duration {
	d := t.minWait
	for i := 1; i < n && d < t.maxWait; i++ {
		d *= 2
	}
	return min(d, t.maxWait)
}

// EnableResult is how enable-ops ended.
type EnableResult struct {
	// RequestID is JSM's id for the request, or empty when Operations was
	// already on.
	RequestID string
	// AlreadyEnabled reports that Operations was already on for the team.
	AlreadyEnabled bool
}

// sites holds one slot per cloudId, so only one enable-ops call runs on a
// site at a time, whichever Client sends it.
var sites sync.Map

func siteSlot(cloudID string) chan struct{} {
	s, _ := sites.LoadOrStore(cloudID, make(chan struct{}, 1))
	return s.(chan struct{})
}

// EnableOps turns Operations on for teamID, the Teams API's id for the team,
// naming adminAccountIDs as its admins, and waits until JSM has finished.
//
// Only one call runs on a site at a time, because JSM locks the whole site
// while it enables a team. A call that JSM refused because of that lock, or
// because it does not know the team yet, is sent again; nothing else is.
// When Operations is already on, the result says so and the call succeeds.
//
// An error wraps ErrEnableFailed when JSM reports that the request failed,
// and httperr.ErrUnknownOutcome when the request was accepted but whether it
// finished is not known, such as when the poll timed out; the caller then
// checks the team before calling again. Such an error also wraps what
// stopped the poll, such as the caller's own context or a 403 on the status
// read, but ErrUnknownOutcome is the one to act on.
func (c *Client) EnableOps(ctx context.Context, teamID string, adminAccountIDs []string) (EnableResult, error) {
	if c.c.Err != nil {
		return EnableResult{}, c.c.Err
	}
	if err := rest.CheckSegment("team id", teamID); err != nil {
		return EnableResult{}, err
	}
	if len(adminAccountIDs) == 0 {
		return EnableResult{}, fmt.Errorf("ops: enable-ops: %w: JSM needs at least one admin account", httperr.ErrRefused)
	}
	slot := siteSlot(c.cloudID)
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return EnableResult{}, fmt.Errorf("ops: enable-ops: waiting for the site's other enable-ops call: %w: %w", httperr.ErrUnavailable, ctx.Err())
	}

	t := c.timing
	team := "/v1/teams/" + teamID
	var answer struct {
		RequestID string `json:"requestId"`
	}
	misses := 0
	for n := 1; ; n++ {
		_, err := c.c.Do(ctx, Write{Method: MethodPost, Path: team + "/enable-ops",
			Body: map[string]any{"adminAccountIds": adminAccountIDs}}, &answer)
		if err == nil {
			break
		}
		switch judgeEnable(err) {
		case alreadyOn:
			return EnableResult{AlreadyEnabled: true}, nil
		case teamUnknown:
			if misses++; misses >= t.teamAttempts || n >= t.attempts {
				return EnableResult{}, err
			}
		case locked:
			if n >= t.attempts {
				return EnableResult{}, err
			}
		default:
			return EnableResult{}, err
		}
		if err := t.sleep(ctx, t.wait(n)); err != nil {
			return EnableResult{}, fmt.Errorf("ops: enable-ops: %w: %w", httperr.ErrUnavailable, err)
		}
	}

	id := answer.RequestID
	if rest.CheckSegment("request id", id) != nil {
		return EnableResult{}, fmt.Errorf("ops: enable-ops: %w: the answer gave no request id to follow", httperr.ErrUnknownOutcome)
	}
	err := httpx.Poll(ctx, t.pollInterval, t.pollTimeout, func(pctx context.Context) (bool, error) {
		var st struct {
			Status requestStatus `json:"status"`
		}
		if err := c.c.Get(pctx, team+"/requests/"+id, nil, &st); err != nil {
			return false, err
		}
		switch strings.ToLower(string(st.Status)) {
		case "successful":
			return true, nil
		// JSM sends "in-progress", its enum's JSON value; the spec documents
		// the enum's name, IN_PROGRESS.
		case "in-progress", "in_progress":
			return false, nil
		case "failed":
			return false, fmt.Errorf("ops: enable-ops request %s: %w", id, ErrEnableFailed)
		}
		return false, fmt.Errorf("ops: enable-ops request %s: %w: a status this client does not know", id, httperr.ErrBadResponse)
	})
	switch {
	case err == nil, errors.Is(err, ErrEnableFailed), errors.Is(err, httperr.ErrUnknownOutcome):
		return EnableResult{RequestID: id}, err
	}
	// JSM accepted the request, so a status that could not be read leaves
	// its outcome open, whatever stopped the read. What stopped it stays
	// matchable, as in the HTTP layer's own errors.
	return EnableResult{RequestID: id}, fmt.Errorf("ops: enable-ops request %s was accepted, but whether it finished is not known: %w: %w",
		id, httperr.ErrUnknownOutcome, err)
}

// verdict is what a refused enable-ops call says.
type verdict int

const (
	// refused is any other refusal; it is not sent again.
	refused verdict = iota
	locked
	teamUnknown
	alreadyOn
)

// judgeEnable reads a refused enable-ops call. Only the statuses JSM refuses
// a request with are read, so a 5xx is never taken for a lock, an unknown
// team or Operations already being on.
func judgeEnable(err error) verdict {
	var e *httperr.Error
	if !errors.As(err, &e) {
		return refused
	}
	body := strings.ToLower(e.Body)
	refusal := e.Status == 400 || e.Status == 409 || e.Status == 422 || e.Status == 423
	switch {
	case refusal && strings.Contains(body, alreadyExistsText):
		return alreadyOn
	case (e.Status == 404 || e.Status == 422) && containsAny(body, teamNotFoundTexts):
		return teamUnknown
	case refusal && strings.Contains(body, lockText):
		return locked
	}
	return refused
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// requestStatus is a team request's status. The API documents it as an
// object holding the value, and answers it as a plain string, so both are
// read.
type requestStatus string

func (s *requestStatus) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = requestStatus(v)
		return nil
	}
	var o struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*s = requestStatus(o.Value)
	return nil
}
