package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func runApproveMappings(ctx context.Context, env *Env, version int, path string) error {
	st, err := env.OpenStore(ctx, store.OpenExisting)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	e := env.newEngine(st)
	v, err := e.Latest(ctx)
	if err != nil {
		return err
	}
	if v == nil || v.Version != version {
		return messages.ValidationFailed(fmt.Errorf("plan v%d is not the latest plan in this workspace", version))
	}
	if err := e.CheckPlan(ctx, v); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	decisions, err := review.ImportMappings(file, v.Plan, v.Hash)
	if err != nil {
		return messages.ValidationFailed(err)
	}
	creds, err := env.credentials(st)
	if err != nil {
		return err
	}
	report, err := runChecks(ctx, env, creds, preflight.Login, false)
	if err != nil {
		return err
	}
	if err := writePreflightError("approve-mappings", report); err != nil {
		return err
	}
	e.Accounts = migrationAccounts(report, env.Config.OrgID)
	if err := e.CheckAccounts(v); err != nil {
		return err
	}
	if report.Operator == nil || !report.Operator.ActiveOrgAdmin {
		return errors.New("approval requires an active account with the organization admin role")
	}
	clients, err := env.Clients(creds.set)
	if err != nil {
		return err
	}
	id, err := clients.Atlassian.CurrentAccountID(ctx, report.CloudID)
	if err != nil {
		return fmt.Errorf("checking the approver's authenticated identity: %w", err)
	}
	if id == "" || id != report.Operator.AccountID {
		return errors.New("the authenticated account differs from the organization admin")
	}
	receipt, err := e.SaveMappingApproval(ctx, v, id, decisions)
	if err != nil {
		return err
	}
	if env.Output == OutputJSON {
		return writeJSON(env.Stdout, receipt)
	}
	_, err = fmt.Fprintf(env.Stdout, "Recorded %d mapping decisions for plan v%d, approved by %s. See %s.\n", len(decisions), version, review.Quote(id), receipt.Artifact)
	return err
}
