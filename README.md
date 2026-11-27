# jsm-ops-migrator

[![Atlassian license](https://img.shields.io/badge/license-Apache%202.0-blue.svg?style=flat-square)](LICENSE) [![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg?style=flat-square)](CONTRIBUTING.md)

jsm-ops-migrator is a command-line tool that moves a PagerDuty on-call setup to [Jira Service Management Operations](https://www.atlassian.com/software/jira/service-management) through public, documented APIs.

Rebuilding users, teams, schedules and escalation policies by hand is slow and easy to get wrong. The CLI reads your PagerDuty account, shows you a plan of every change it would make, writes nothing until you confirm, and reports anything it could not move exactly.

> **Status:** early development. Build the CLI from source to try it before the first packaged release. The project follows [semantic versioning](https://semver.org/) and stays below 1.0 while it is in Labs.

## What it migrates

- Users
- Teams
- Schedules
- Escalation policies
- Services
- User contact methods
- Notification rules

These are not migrated, and you set them up in Jira Service Management Operations after the migration:

- Integrations
- Alert and notification policies
- Maintenance windows
- Routing rules
- Alert and incident history

## Usage

The CLI will work in three steps:

1. **Plan.** Read the PagerDuty account and build a plan of the changes, without writing anything.
2. **Confirm.** Review the plan and confirm what should be moved.
3. **Apply.** Make the confirmed changes, then produce a report of what was moved, what was changed to fit, and what was skipped.

Use a separate `--workspace` for each migration. The first read of each side binds the workspace to its PagerDuty account host or its JSM cloud ID and organization ID. You can rotate credentials for those accounts. Each plan version writes its own `acceptance-template.yaml`.

Review the proposed user mappings before you apply. The [user mapping review guide](docs/review-user-mappings.md) walks through exporting the CSV, choosing a decision for each user, approving the plan and checking the receipt.

### Apply approved user access

Set the provisioning policy for the target before running `plan`. Omitting the setting produces a review-only plan. Use `scim` for a site managed by an identity provider. Its plan reports missing access for the identity provider to grant. Use `non-scim` only after confirming the site allows the migration to send invitations. The CLI takes this policy from the operator.

```sh
export JSM_OPS_MIGRATOR_USER_PROVISIONING=non-scim
jsm-ops-migrator plan --workspace "$WORKSPACE"
# Set VERSION to the new plan version printed above.
cp "$WORKSPACE/artifacts/plans/v$VERSION/mapping.csv" reviewed.csv
# Fill in review_decision for every row, then approve with the named admin.
jsm-ops-migrator plan approve-mappings --workspace "$WORKSPACE" --version "$VERSION" --csv reviewed.csv
# Fill in the acceptance file if the plan lists warnings.
jsm-ops-migrator apply --workspace "$WORKSPACE" --accept-file "$WORKSPACE/artifacts/plans/v$VERSION/acceptance-template.yaml"
jsm-ops-migrator resume --workspace "$WORKSPACE"
```

Accepting `propose_invite` authorizes an emailed invitation to the exact login. The address may belong to an account outside the organization. Accepting `propose_access` authorizes the JSM product grant to the named account. Organization invitations require a paid subscription. Each new grant can consume one agent seat. To include individual PagerDuty stakeholders as standard JSM users, pass their IDs to `plan --include-stakeholders P1,P2` and review their seat cost. [Organization invitations API](https://developer.atlassian.com/cloud/admin/organization/rest/api-group-users/)

Before sending invitations, the CLI verifies the target organization and site, rereads the approved identities, and checks remaining seats. The run records the account ID returned for each invitation. Users awaiting acceptance or Operations provisioning appear in the run report. Resume checks readiness and releases dependent work when the Operations account exists. Only ready users appear in the run's mapping CSV. An active JSM user who never becomes ready can still consume a seat.

Use `resume` to reconcile a journaled create by looking up its JSM object. Independent items can continue while dependent items wait. If you verify in JSM that no object was made, wait ten minutes from the recorded attempt and run `resume --skip-create <item-key>`. Repeat `--skip-create` for each verified item in the same run. A fresh lookup must also find no match for each skip. The run leaves skipped creates for a new plan and approval. When JSM has acknowledged a create, resume needs to identify its object.

Reconcile any unfinished write run from a build that predates snapshot coverage before upgrading its workspace. The workspace keeps its original schema until that run is finished.

## Installation

Installation instructions will be added with the first release.

## Documentation

See [Review and approve user mappings](docs/review-user-mappings.md) for a worked example of the identity review.

## Tests

`go test ./...` runs the tests. On every pull request, every push to `main` and nightly, CI builds and
tests the CLI on Linux, macOS and Windows, cross-builds the six release targets, and runs the race
detector, the fuzz targets, the linter, vulnerability and license checks, and a scan of the test data.

## Contributions

Contributions to jsm-ops-migrator are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## License

Copyright (c) 2026 Atlassian US., Inc.
Apache 2.0 licensed, see [LICENSE](LICENSE) file.

<br/>

[![With ❤️ from Atlassian](https://raw.githubusercontent.com/atlassian-internal/oss-assets/master/banner-cheers.png)](https://www.atlassian.com)
