// Command jsm-ops-migrator migrates a PagerDuty configuration into JSM Operations.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"

	// Embedded so time zone handling is identical on Windows, where the
	// system may not provide a time zone database.
	_ "time/tzdata"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/cli"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/httpx"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/jsm/admin"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
	engine "github.com/atlassian-labs/jsm-ops-migrator/internal/run"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/users"
)

// clientsFactory builds the API clients a command uses from the credentials
// it checks.
type clientsFactory func(env *cli.Env, creds credentials.Set) (preflight.Clients, error)

// apiClients returns the factory for the real API clients, all built on one
// HTTP layer client from base with the credentials, the configured PagerDuty
// region and, for a staging site, the staging API host, so its limits hold
// across them. The binary's tests pass a base that reaches fakes.
func apiClients(base httpx.Config) clientsFactory {
	return func(env *cli.Env, creds credentials.Set) (preflight.Clients, error) {
		h, err := httpClient(base, env, creds)
		if err != nil {
			return preflight.Clients{}, err
		}
		atlassian := jsm.New(h)
		return preflight.Clients{PagerDuty: pd.New(h), Atlassian: atlassian, Admin: atlassian}, nil
	}
}

func httpClient(base httpx.Config, env *cli.Env, creds credentials.Set) (*httpx.Client, error) {
	cfg := base
	cfg.Credentials = creds
	cfg.PagerDutyRegion = httpx.Region(env.Config.PagerDutyRegion)
	cfg.Staging = httpx.IsStagingSite(env.Config.Site)
	cfg.Logger = env.Logger
	return httpx.New(cfg)
}

func main() { os.Exit(run(credentials.OSKeychain(), &httpx.Config{})) }

// run is main with the keychain and the API clients as parameters, so the
// binary's tests can give it ones that are not the operator's.
func run(kc credentials.Keychain, base *httpx.Config) int {
	// Libraries that log through the standard log package would print around
	// the redactor; everything the CLI logs goes through its own slog logger.
	log.SetOutput(io.Discard)

	// The CLI handles Ctrl+C itself, so apply can finish the write it is
	// making instead of being killed halfway through.
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)

	opts := cli.Options{
		Keychain:   kc,
		Interrupts: interrupts,
		// The Ctrl+C after the command was canceled: the operator wants out.
		ForceExit: func() { os.Exit(130) },
	}
	if base != nil {
		opts.NewClients = apiClients(*base)
		opts.NewSteps = func(env *cli.Env, creds credentials.Set) ([]engine.Step, error) {
			h, err := httpClient(*base, env, creds)
			if err != nil {
				return nil, err
			}
			return []engine.Step{users.Step{PD: pd.New(h), Admin: admin.New(h), OrgID: env.Config.OrgID, CloudID: env.Config.CloudID}}, nil
		}
	}
	return cli.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, opts)
}
