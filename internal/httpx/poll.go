package httpx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
)

// Poll calls check every interval until it reports done or fails, for the
// asynchronous calls: enable-ops and alert creation. When timeout passes
// first, Poll returns an error wrapping httperr.ErrUnknownOutcome, because the
// operation was accepted but whether it finished is not known.
//
// check gets a context that ends at the timeout, so a check that is still
// retrying its call stops there too, and no check starts after it.
func Poll(ctx context.Context, interval, timeout time.Duration, check func(context.Context) (done bool, err error)) error {
	if interval <= 0 || timeout <= 0 {
		return errors.New("poll: interval and timeout must be positive")
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// stopped is the error once pctx has ended: the caller's own error when
	// the caller stopped, or the timeout.
	stopped := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("not finished after %s: %w", timeout, httperr.ErrUnknownOutcome)
	}
	for {
		done, err := check(pctx)
		if err != nil {
			if pctx.Err() != nil {
				// The check was cut short, so it says nothing about the
				// operation.
				return stopped()
			}
			return err
		}
		if done {
			return nil
		}
		t := time.NewTimer(interval)
		select {
		case <-pctx.Done():
			t.Stop()
			return stopped()
		case <-t.C:
		}
		if pctx.Err() != nil {
			return stopped()
		}
	}
}
