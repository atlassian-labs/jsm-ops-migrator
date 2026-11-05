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
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

func main() {
	// Libraries that log through the standard log package would print around
	// the redactor; everything the CLI logs goes through its own slog logger.
	log.SetOutput(io.Discard)

	// The CLI handles Ctrl+C itself, so apply can finish the write it is
	// making instead of being killed halfway through.
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)

	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Options{
		NewKeyProvider: func(*cli.Env) (store.KeyProvider, error) {
			return credentials.NewKeychainKeyProvider(credentials.OSKeychain()), nil
		},
		Interrupts: interrupts,
		// The Ctrl+C after the command was canceled: the operator wants out.
		ForceExit: func() { os.Exit(130) },
	}))
}
