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
	"github.com/atlassian-labs/jsm-ops-migrator/internal/pd"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

// clientsFactory builds the API clients a command uses from the credentials
// it checks.
type clientsFactory func(env *cli.Env, creds credentials.Set) (preflight.Clients, error)

// apiClients returns the factory for the real API clients, each built on base
// with the credentials and the configured PagerDuty region. The PagerDuty
// client is real; the Atlassian ones are placeholders until the JSM client
// replaces them. The binary's tests pass a base that reaches a fake.
func apiClients(base httpx.Config) clientsFactory {
	return func(env *cli.Env, creds credentials.Set) (preflight.Clients, error) {
		cfg := base
		cfg.Credentials = creds
		cfg.PagerDutyRegion = httpx.Region(env.Config.PagerDutyRegion)
		cfg.Logger = env.Logger
		h, err := httpx.New(cfg)
		if err != nil {
			return preflight.Clients{}, err
		}
		clients := preflight.Placeholders()
		clients.PagerDuty = pd.New(h)
		return clients, nil
	}
}

func main() { os.Exit(run(credentials.OSKeychain(), apiClients(httpx.Config{}))) }

// run is main with the keychain and the API clients as parameters, so the
// binary's tests can give it ones that are not the operator's.
func run(kc credentials.Keychain, newClients clientsFactory) int {
	// Libraries that log through the standard log package would print around
	// the redactor; everything the CLI logs goes through its own slog logger.
	log.SetOutput(io.Discard)

	// The CLI handles Ctrl+C itself, so apply can finish the write it is
	// making instead of being killed halfway through.
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)

	return cli.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Options{
		Keychain:   kc,
		NewClients: newClients,
		Interrupts: interrupts,
		// The Ctrl+C after the command was canceled: the operator wants out.
		ForceExit: func() { os.Exit(130) },
	})
}
