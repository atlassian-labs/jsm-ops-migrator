# jsm-ops-migrator

[![Atlassian license](https://img.shields.io/badge/license-Apache%202.0-blue.svg?style=flat-square)](LICENSE) [![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg?style=flat-square)](CONTRIBUTING.md)

jsm-ops-migrator is a command-line tool that moves a PagerDuty on-call setup to [Jira Service Management Operations](https://www.atlassian.com/software/jira/service-management) through public, documented APIs.

Rebuilding users, teams, schedules and escalation policies by hand is slow and easy to get wrong. The CLI reads your PagerDuty account, shows you a plan of every change it would make, writes nothing until you confirm, and reports anything it could not move exactly.

> **Status:** early development. There is no release yet, so the tool cannot be installed or run. The project follows [semantic versioning](https://semver.org/) and stays below 1.0 while it is in Labs.

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

Commands and examples will be added with the first release.

## Installation

Installation instructions will be added with the first release.

## Documentation

For now, this README is the documentation. A post-migration guide for the areas you set up by hand (integrations, policies, maintenance windows and routing) will be added in a `docs` folder.

## Tests

Tests and a CI pipeline will be added together with the code.

## Contributions

Contributions to jsm-ops-migrator are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## License

Copyright (c) 2026 Atlassian US., Inc.
Apache 2.0 licensed, see [LICENSE](LICENSE) file.

<br/>

[![With ❤️ from Atlassian](https://raw.githubusercontent.com/atlassian-internal/oss-assets/master/banner-cheers.png)](https://www.atlassian.com)
