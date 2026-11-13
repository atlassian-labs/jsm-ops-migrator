// Package ops is the JSM Operations REST API client: the general part of
// every call under /jsm/ops/api/<cloudId>, and enable-ops, whose locking and
// retries every step that creates a team shares. The packages that call JSM
// Operations define the paths they call and the types they send and decode;
// ops knows no other endpoint.
//
// The HTTP layer attaches the Atlassian account email and API token, sends
// the calls to the Atlassian API host, retries what is safe to retry and
// redacts what it reports.
package ops
