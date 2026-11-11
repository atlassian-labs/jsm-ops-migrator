# Provenance

Every file or behavior that comes from another project is listed here.

| File in this repository | Source repository and commit | Source path | License | What changed |
| --- | --- | --- | --- | --- |
| `internal/httpx/client.go`, `request.go`, `family.go` | `atlassian/terraform-provider-atlassian-operations` v2.0.6 (`eb15128c`) | `internal/httpClient/request.go`, `internal/httpClient/response.go`, `internal/httpClient/httpClientHelpers/helpers.go` | Apache-2.0 (the files have no license header) | Re-implemented, not copied: the request and response types, Basic auth for JSM Operations and the site APIs, Bearer auth for the Admin API, and the API base paths. Changed: one shared client, a context on every call, typed errors, no retry of an answer that does not decode, explicit retry defaults and nothing logged to the console. Retries use the standard library instead of `go-retryablehttp`. |
| `internal/httpx/page.go` (`OffsetSize`, `PageInfo`) | `atlassian/terraform-provider-atlassian-operations` v2.0.6 (`eb15128c`) | `internal/dto/list_response.go`; `internal/provider/team_resource.go` | Apache-2.0 (no header); MPL-2.0 (header) | Only the API shapes: JSM Operations lists (`values`, `links.next`) and Teams member paging (a `{"first", "after"}` body answered with `pageInfo`). Re-implemented; nothing from the MPL-headed file is copied. |
