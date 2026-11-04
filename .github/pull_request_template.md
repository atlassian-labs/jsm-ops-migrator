## Summary

## How it was tested

## Checklist (the rules in AGENTS.md)

- [ ] PagerDuty is still only read: no non-GET request outside `tools/pdseed`
- [ ] Network calls only from `internal/pd` or `internal/jsm`, through `internal/httpx`
- [ ] Credentials are `credentials.Secret` values and never come from a flag
- [ ] No token, routing key, phone number, device detail or calendar URL in logs, messages, review files, test data or stored payloads
- [ ] Stored payloads go through the store's field encryption; values kept in clear are keyed fingerprints
- [ ] No create is retried blindly after a timeout or 5xx
- [ ] Calls follow the declared order; a failure is recorded and the run carries on; no call is skipped or changed by the CLI
- [ ] Schema changes are new migration files; merged ones are untouched
- [ ] Ported code is recorded in `docs/provenance.md`; no MPL-headed file is copied
- [ ] No internal Atlassian code, no PagerDuty api-schema or code generated from it, no internal links, ticket keys or customer data
- [ ] New dependencies are listed below with their license
- [ ] Comments explain why, not what

## New dependencies and licenses
