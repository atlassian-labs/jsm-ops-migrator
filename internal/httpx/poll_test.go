package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

func TestPoll(t *testing.T) {
	ctx := context.Background()
	var calls int
	err := httpx.Poll(ctx, time.Millisecond, time.Minute, func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil || calls != 3 {
		t.Errorf("done on the third check: got %v after %d calls", err, calls)
	}

	failed := errors.New("FAILED")
	if err := httpx.Poll(ctx, time.Millisecond, time.Minute, func(context.Context) (bool, error) { return false, failed }); !errors.Is(err, failed) {
		t.Errorf("a failed check: got %v", err)
	}

	calls = 0
	err = httpx.Poll(ctx, time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, httperr.ErrUnknownOutcome) || calls < 2 {
		t.Errorf("past the timeout: got %v after %d calls, want ErrUnknownOutcome", err, calls)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := httpx.Poll(canceled, time.Hour, time.Hour, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled context: got %v", err)
	}

	if err := httpx.Poll(ctx, 0, time.Second, func(context.Context) (bool, error) { return true, nil }); err == nil {
		t.Errorf("a zero interval must be rejected")
	}
}

// TestPollStopsAtItsTimeout: a check still running at the timeout is cut
// short, and no check starts after it.
func TestPollStopsAtItsTimeout(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	err := httpx.Poll(ctx, time.Millisecond, 20*time.Millisecond, func(ctx context.Context) (bool, error) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Second):
			return true, nil
		}
	})
	if !errors.Is(err, httperr.ErrUnknownOutcome) || time.Since(start) > time.Second {
		t.Errorf("a slow check: got %v after %s, want ErrUnknownOutcome at the timeout", err, time.Since(start))
	}

	// A check whose call is waiting to retry, as the API asked.
	client, _, _ := serve(t, httpx.Config{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	start = time.Now()
	err = httpx.Poll(ctx, time.Millisecond, 20*time.Millisecond, func(ctx context.Context) (bool, error) {
		_, err := client.Do(ctx, httpx.Request{Family: httpx.JSMOps, Path: "/status"})
		return false, err
	})
	if !errors.Is(err, httperr.ErrUnknownOutcome) || time.Since(start) > time.Second {
		t.Errorf("a retrying check: got %v after %s, want ErrUnknownOutcome at the timeout", err, time.Since(start))
	}

	// A check that ignores its context and ends past the timeout.
	var calls int
	err = httpx.Poll(ctx, time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) {
		calls++
		time.Sleep(40 * time.Millisecond)
		return false, nil
	})
	if !errors.Is(err, httperr.ErrUnknownOutcome) || calls != 1 {
		t.Errorf("past the timeout: got %v after %d checks, want ErrUnknownOutcome after 1", err, calls)
	}

	// The caller's own deadline is the caller's error, not the poll's.
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err = httpx.Poll(short, time.Millisecond, time.Hour, func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, httperr.ErrUnknownOutcome) {
		t.Errorf("the caller's deadline: got %v, want context.DeadlineExceeded", err)
	}
}
