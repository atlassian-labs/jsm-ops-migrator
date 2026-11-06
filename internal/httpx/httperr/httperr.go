// Package httperr declares the errors every API client returns, so callers
// can tell a rejected credential from a call that could not be made without
// knowing which API answered. The HTTP layer wraps its typed errors around
// these; callers test them with errors.Is.
package httperr

import "errors"

var (
	// ErrUnauthorized means the API rejected the credential (401).
	ErrUnauthorized = errors.New("the credential was rejected")
	// ErrForbidden means the credential is valid but may not make the call
	// (403).
	ErrForbidden = errors.New("the credential is not allowed to make this call")
	// ErrNotFound means the API has no such object or endpoint (404).
	ErrNotFound = errors.New("not found")
	// ErrUnavailable means the call could not be made or gave no answer: a
	// network failure, a timeout, a 5xx after the retries, or a client that
	// is not built yet.
	ErrUnavailable = errors.New("the API could not be reached")
)
