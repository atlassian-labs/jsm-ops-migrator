package ops

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx/httperr"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit/jsmfake"
)

func TestWaitForUserRecognizesOnlyTheExpectedMissingAccount(t *testing.T) {
	f, c := enableSite(t, "cloud-1")
	path := f.OpsPath("/v1/users/contacts")
	missing := jsmfake.Fault{Status: 400, Body: map[string]any{"errors": []map[string]string{{"title": "No user found with the target account Id: acc-1"}}}}
	c.timing.sleep = func(context.Context, time.Duration) error { return nil }
	f.Fail(http.MethodGet, path, missing)
	f.Answer(http.MethodGet, path, 200, map[string]any{"values": []any{}})
	if ready, err := c.WaitForUser(t.Context(), "acc-1"); err != nil || !ready || len(f.Requests()) != 2 {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
	f.Fail(http.MethodGet, path, missing)
	if ready, err := c.WaitForUser(t.Context(), "other"); ready || !errors.Is(err, httperr.ErrInvalid) {
		t.Fatalf("unexpected 400: %t %v", ready, err)
	}
	f.Answer(http.MethodGet, path, 400, missing.Body)
	c.timing.pollTimeout = 20 * time.Millisecond
	c.timing.sleep = sleep
	if ready, err := c.WaitForUser(t.Context(), "acc-1"); err != nil || ready {
		t.Fatalf("timeout: %t %v", ready, err)
	}
}
