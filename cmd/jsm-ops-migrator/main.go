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
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

// The API clients for the checks. Each one is a placeholder until the HTTP
// layer and the PagerDuty and JSM clients replace it.
func placeholders(credentials.Set) preflight.Clients { return preflight.Placeholders() }

func main() { os.Exit(run(credentials.OSKeychain(), placeholders)) }

// run is main with the keychain and the API clients as parameters, so the
// binary's tests can give it ones that are not the operator's.
func run(kc credentials.Keychain, newClients func(credentials.Set) preflight.Clients) int {
	// Libraries that log through the standard log package would print around
	// the redactor; everything the CLI logs goes through its own slog logger.
	log.SetOutput(io.Discard)

	// The CLI handles Ctrl+C itself, so apply can finish the write it is
	// making instead of being killed halfway through.
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)

	return cli.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Options{
		Keychain: kc,
		NewClients: func(_ *cli.Env, creds credentials.Set) (preflight.Clients, error) {
			return newClients(creds), nil
		},
		Interrupts: interrupts,
		// The Ctrl+C after the command was canceled: the operator wants out.
		ForceExit: func() { os.Exit(130) },
	})
}
