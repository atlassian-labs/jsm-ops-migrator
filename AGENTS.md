# AGENTS.md: jsm-ops-migrator

Rules for anyone changing this repository, people and coding agents alike.
`CLAUDE.md` imports this file; keep the rules here, not in tool-specific files.

## What this is

`jsm-ops-migrator` is a command-line tool that a customer's org admin runs on their
own machine to migrate a PagerDuty configuration into JSM Operations. It reads
PagerDuty through its read-only API, sends each change to the right Atlassian
API in dependency order, and saves every result as review files in a local,
encrypted workspace. Commands: `login`, `plan`, `apply`, `pause`, `resume`.

The CLI owns the calls, not the migration rules. What is migrated, and how each
entity is mapped, lives in the entity steps that plug into `plan` and `apply`.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...                 # includes the repository rules and the testdata scan
golangci-lint run ./...       # v2; same import and call rules as internal/testkit/rules_test.go
(cd tools/pdseed && go build ./... && go vet ./... && golangci-lint run ./...)
```

The checks CI runs, which you can run locally too (versions are pinned in
`.github/workflows/ci.yml`):

```bash
go test -race ./...                                           # needs cgo
govulncheck -test ./...                                       # golang.org/x/vuln/cmd/govulncheck
osv-scanner scan source --lockfile=go.mod --lockfile=tools/pdseed/go.mod
go-licenses check ./... --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT
go test ./internal/testkit -run '^TestTestdataIsClean$'       # secrets and phone numbers in testdata/
```

CI also runs the tests on macOS, Windows and Linux, builds six targets without
cgo and fuzzes every `Fuzz` function. Set `JSM_OPS_MIGRATOR_TEST_REAL_KEYCHAIN=1` to
run the keychain test against the real OS keychain. No other test may touch
it: the binary's tests in `cmd/jsm-ops-migrator` use a file-backed keychain from
`testkit`, and the `internal` tests use `testkit.FakeKeychain`. The scripts reach
no network: a script that sets `JSM_OPS_MIGRATOR_TEST_APIS=healthy` runs the
binary against `testkit`'s fake APIs, and reads PagerDuty with the real client
from a `testkit/pdfake` account hosted in the region that
`JSM_OPS_MIGRATOR_TEST_PAGERDUTY_REGION` names (`us` when empty); the others
get placeholders.

Credentials come from masked prompts, `login --stdin` or these environment
variables, never from flags: `JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY`,
`JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL`, `JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN` and
`JSM_OPS_MIGRATOR_ORG_API_KEY`. On a machine without a keychain,
`JSM_OPS_MIGRATOR_PASSPHRASE` protects the workspace instead. The PagerDuty
client is real: `login` checks the key and finds where the account is hosted,
`us` or `eu`, which it saves as `pagerduty_region` with the key. Until the JSM
client exists, `cmd/jsm-ops-migrator` wires `preflight.Placeholders` for the
Atlassian side, so every check that needs an Atlassian API reports "not
checked". `login` stores only a credential, and saves only a setting, whose
check passed, so for now it stores only the PagerDuty key and saves only its
region. A site in `config.yaml` without its cloudId was never checked and
counts as not saved.

## Rules

1. **PagerDuty is read-only.** The CLI sends only GET requests to PagerDuty, and
   a test proves it. Only `tools/pdseed`, a separate module the CLI never
   imports, may write to PagerDuty.
2. **Network boundary.** Only `internal/pd` and `internal/jsm/...` make network
   calls, and only through `internal/httpx`, so every call gets the same
   retries, limits and redaction. Code that reads PagerDuty data defines its
   own paths and types and reads them with `pd.Client.Get` and `pd.List`.
3. **Secrets.** Credentials are `credentials.Secret` values, which print as
   `[redacted]`. They live only in the OS keychain, environment variables or
   the encrypted credentials file, and are never taken as flag values.
4. **Sensitive data never leaves its place.** Tokens, routing keys, phone
   numbers, device details and PagerDuty calendar URLs never appear in logs,
   messages, review files, test data or stored payloads.
5. **Stored payloads are encrypted.** Payload columns go through the store's
   field encryption; a value kept in clear for lookups is a keyed fingerprint,
   never a plain hash.
6. **No blind retries.** A create that timed out or returned a 5xx is an
   unknown outcome. It is never sent again without first checking whether it
   took effect.
7. **Calls in declared order.** Entity steps declare what each item depends on.
   The CLI makes the calls in that order, records every result, and on a
   failure records it and carries on. It never skips or changes a call on its
   own; each later step decides from the recorded results.
8. **Schema changes are new migration files.** Merged migration files are never
   edited.
9. **Provenance.** From `atlassian/terraform-provider-atlassian-operations`
   (Apache-2.0) copy only files without a license header, from
   `internal/httpClient/*` and `internal/dto/*`. Files with an MPL-2.0 header
   are re-implemented, not copied. Record every ported piece in
   `docs/provenance.md`.
10. **What never goes in.** No code from internal Atlassian tools. Neither the
    PagerDuty `api-schema` spec nor code generated from it (its repository has
    no license). `go-pagerduty` is a reference, not a dependency. No non-public
    data: no internal links, ticket keys or customer data.
11. **Dependencies.** Every new dependency is listed with its license in the
    pull request. MPL-2.0 is accepted only for `hashicorp/go-retryablehttp` and
    `pgregory.net/rapid`, pending the open-source review.
12. **Comments explain why, not what.**
