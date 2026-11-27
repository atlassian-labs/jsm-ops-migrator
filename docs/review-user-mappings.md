# Review and approve user mappings

Use this guide to check which Atlassian account each PagerDuty user should map to. The plan exports one row per PagerDuty user, including users the tool could not match and users excluded by policy. Keeping all the rows lets the approver account for everyone. Approval saves the decisions locally. Sending invitations or changing site access is a separate operation.

## 1. Export the proposed mappings

Use the same workspace throughout. If you previously used `--workspace`, set `WORKSPACE` to that directory. The example below uses the CLI default. If you are testing from a source checkout, build with `go build -o ./jsm-ops-migrator ./cmd/jsm-ops-migrator` and use `./jsm-ops-migrator` for the commands below.

```sh
WORKSPACE="${JSM_OPS_MIGRATOR_WORKSPACE:-$HOME/jsm-ops-migrator}"
# On first use, sign in to PagerDuty and the destination Atlassian site.
# Skip login if you already signed in to this workspace.
jsm-ops-migrator login --workspace "$WORKSPACE"
jsm-ops-migrator plan --workspace "$WORKSPACE"
```

Read the plan version and review directory printed by `plan`. Resolve any blockers it reports. Use the file under `artifacts/plans/` for review. The similarly named file under `artifacts/runs/` lists mappings recorded during a run.

## 2. Edit the last column on every user row

In this example the plan is v1. Set `VERSION` to the number from your own output, then make a working copy.

```sh
VERSION=1
PLAN_DIR="$WORKSPACE/artifacts/plans/v$VERSION"
REVIEWED="$PLAN_DIR/reviewed-mapping.csv"
cp -i "$PLAN_DIR/mapping.csv" "$REVIEWED"
```

You can rename or move the working copy. Open it in a text editor or spreadsheet.

| Row | What to do |
| --- | --- |
| 1 | Keep the `plan_hash` record. Its second cell identifies the exact plan that produced the file. Leave the remaining cells empty. |
| 2 | Keep the column names in their original order. |
| 3 onward | For every user, replace the entire prompt in the last column, `review_decision`, with **one word** from the choices shown for that user. You can reorder the user rows. |

| What you see in `proposed_action` | What to check | Example decision to type |
| --- | --- | --- |
| `map_existing` | The PagerDuty email matches an active Atlassian account with JSM access on the destination site. Confirm the proposed account ID belongs to the person. | `accept` if correct |
| `propose_access` | The email matches an active account that needs JSM access on the destination site. Accepting approves the proposed mapping for this plan. | `accept` if intended |
| `review_directory_miss` | No directory user matched this email. Confirm the person's Atlassian login email. | `defer` while you check |
| Another review or exclusion reason | Read `reason`. For example, an ambiguous match needs investigation, and a pending PagerDuty invitation may account for a user who should be excluded. | `defer` to leave unresolved, or `skip` to exclude from this plan |

For example, if Taylor's row shows `map_existing` and you verified the account, replace `Replace with accept / defer / skip` with `accept`. If Sam's row shows `review_directory_miss`, replace `Replace with defer / skip` with `defer` while you check the login email. A blank cell or a remaining prompt stops approval.

**Keep every user row and edit only `review_decision`.** The other columns explain the proposal and let the CLI check the file against the original plan.

| Columns | Why they are in the file |
| --- | --- |
| `pd_user_id`, `pd_email`, `pd_role` | Identify the source user, show the login email used for matching, and explain role-based exclusions. |
| `proposed_account_id`, `target_jsm_access` | Identify the proposed destination account and show whether the directory confirmed access to JSM on this site. A blank means the value is unknown. |
| `proposed_action`, `reason` | Show the proposed outcome and the evidence or next step for this user. |
| `review_decision` | Record the reviewer's choice for this user. |

Save as UTF-8 CSV. Comma and semicolon separators are supported. In a spreadsheet, import the columns as text to preserve IDs. If you have an older export that starts with column names, run `plan` with the current CLI and review a fresh copy of its export.

## 3. Approve the reviewed plan

An active organization admin runs this in the original workspace with their own Atlassian email and API token. Someone else can prepare the CSV. If a different admin approves it, supply that admin's credentials through `JSM_OPS_MIGRATOR_ATLASSIAN_EMAIL` and `JSM_OPS_MIGRATOR_ATLASSIAN_API_TOKEN` for the approval command. The workspace also needs the destination organization API key. A scoped personal token needs access to Jira's current-user endpoint. [Jira current-user API](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-myself/#api-rest-api-3-myself-get)

```sh
jsm-ops-migrator plan approve-mappings --workspace "$WORKSPACE" --version "$VERSION" --csv "$REVIEWED"
```

If the CLI reports an invalid decision, use the record number to find the row in your CSV and choose one listed value. If the version is stale or the file’s plan identifier differs, copy `mapping.csv` from the latest plan directory and review its proposals again. A previous export can have the same users and still belong to a different plan.

## 4. Check the receipt

```sh
cat "$PLAN_DIR/mapping-approval.json"
```

Check that `plan_version` matches the version you approved and `plan_hash` matches the second cell in row 1 of your reviewed CSV. Check `approved_by` for the admin's account ID and `approved_at` for the time. Under `decisions`, check each PagerDuty user ID and its decision. Each accepted mapping includes its destination account ID.

Repeating the same approval with the same admin preserves the receipt. Before apply starts, a different decision or approver adds the earlier revision to `previous_approvals`. A new plan version needs its own review.
