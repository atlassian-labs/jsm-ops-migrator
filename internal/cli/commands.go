package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

type globalFlags struct {
	workspace      string
	nonInteractive bool
	output         string
	logLevel       string
	noColor        bool
}

// LoginFlags are the login command's own flags.
type LoginFlags struct {
	Clear bool
	// Stdin reads the credentials as NAME=value lines from standard input.
	Stdin bool
}

// PlanFlags are the plan command's own flags.
type PlanFlags struct{ Only string }

// ApplyFlags are the apply command's own flags.
type ApplyFlags struct{}

// PauseFlags are the pause command's own flags.
type PauseFlags struct{}

// ResumeFlags are the resume command's own flags.
type ResumeFlags struct{}

// bodies are the command implementations. Login and pre-flight replaces login,
// and the Run engine replaces the others; tests substitute their own.
type bodies struct {
	login  func(ctx context.Context, env *Env, f LoginFlags) error
	plan   func(ctx context.Context, env *Env, f PlanFlags) error
	apply  func(ctx context.Context, env *Env, f ApplyFlags) error
	pause  func(ctx context.Context, env *Env, f PauseFlags) error
	resume func(ctx context.Context, env *Env, f ResumeFlags) error
}

func defaultBodies() bodies {
	return bodies{login: runLogin, plan: runPlan, apply: runApply, pause: runPause, resume: runResume}
}

var (
	outputValues   = []string{string(OutputText), string(OutputJSON)}
	logLevelValues = []string{"debug", "info", "warn", "error"}
	onlyValues     = []string{"", "pagerduty", "jsm"}
)

func (r *runner) newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "jsm-ops-migrator",
		Short: "Migrate a PagerDuty configuration into JSM Operations",
		Long: "Migrate a PagerDuty configuration into JSM Operations.\n\n" +
			"Run jsm-ops-migrator without a command to choose from a menu. Your files are kept in\n" +
			"~/jsm-ops-migrator unless you give another folder with --workspace.",
		SilenceErrors: true,
		SilenceUsage:  true,
		// Without a command: the menu in a terminal, the help everywhere else,
		// so scripts and CI never wait for input.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !r.interactive() || r.global.output == string(OutputJSON) {
				return cmd.Help()
			}
			r.wantMenu = true
			r.rootFlags = cmd.Flags()
			return nil
		},
		// No Args here: cobra's default check for the root is what reports an
		// unknown command instead of printing the help.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if err := oneOf("--output", r.global.output, outputValues); err != nil {
				return err
			}
			return oneOf("--log-level", r.global.logLevel, logLevelValues)
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&r.global.workspace, "workspace", "", "folder for your files (default: $JSM_OPS_MIGRATOR_WORKSPACE, then ~/jsm-ops-migrator)")
	pf.BoolVar(&r.global.nonInteractive, "non-interactive", false, "never prompt; take every answer from flags or environment variables")
	pf.StringVar(&r.global.output, "output", string(OutputText), "output format: text or json")
	pf.StringVar(&r.global.logLevel, "log-level", "warn", "log level: debug, info, warn or error")
	pf.BoolVar(&r.global.noColor, "no-color", false, "disable colored output")
	for _, k := range configKeys {
		pf.String(k.flag, "", k.usage)
	}

	login := &cobra.Command{
		Use:   "login",
		Short: "Store the credentials and the site and organization to migrate into",
		Long: "Store the credentials and the site and organization to migrate into.\n\n" +
			"In a terminal, login asks for each credential with a masked prompt. Scripts set\n" +
			"them in the environment, or pipe them as NAME=value lines with --stdin:\n\n" +
			envVarHelp() + "\n\n" +
			"Each credential is checked before it is stored in the OS keychain. On a machine\n" +
			"without one, the workspace is protected by a passphrase (" + PassphraseEnvVar + ")\n" +
			"and the credentials are kept in an encrypted file. Credentials are never flags.",
		Args: cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			if r.login.Clear && r.login.Stdin {
				return errors.New("--clear and --stdin cannot be used together")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := specLogin
			if r.login.Clear {
				spec = specLoginClear
			}
			return r.wrap(spec, func(ctx context.Context, env *Env) error {
				return r.bodies.login(ctx, env, r.login)
			})(cmd, args)
		},
	}
	login.Flags().BoolVar(&r.login.Clear, "clear", false, "remove the stored credentials")
	login.Flags().BoolVar(&r.login.Stdin, "stdin", false, "read the credentials as NAME=value lines from standard input")

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Run the pre-flight checks, read both systems and build the migration plan, without writing anything",
		Args:  cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			return oneOf("--only", r.plan.Only, onlyValues)
		},
		RunE: r.wrap(specPlan, func(ctx context.Context, env *Env) error {
			return r.bodies.plan(ctx, env, r.plan)
		}),
	}
	plan.Flags().StringVar(&r.plan.Only, "only", "", "re-read just one side: pagerduty or jsm")

	apply := &cobra.Command{
		Use:   "apply",
		Short: "Write the confirmed plan to JSM Operations",
		Args:  cobra.NoArgs,
		RunE: r.wrap(specApply, func(ctx context.Context, env *Env) error {
			return r.bodies.apply(ctx, env, r.apply)
		}),
	}
	pause := &cobra.Command{
		Use:   "pause",
		Short: "Ask a running apply to stop after the write it is making",
		Args:  cobra.NoArgs,
		RunE: r.wrap(specPause, func(ctx context.Context, env *Env) error {
			return r.bodies.pause(ctx, env, r.pause)
		}),
	}
	resume := &cobra.Command{
		Use:   "resume",
		Short: "Continue a paused or interrupted apply",
		Args:  cobra.NoArgs,
		RunE: r.wrap(specResume, func(ctx context.Context, env *Env) error {
			return r.bodies.resume(ctx, env, r.resume)
		}),
	}
	root.AddCommand(login, plan, apply, pause, resume)

	return root
}

func oneOf(flag, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	var named []string
	for _, a := range allowed {
		if a != "" {
			named = append(named, a)
		}
	}
	return fmt.Errorf("invalid value %q for %s: want one of %v", value, flag, named)
}

func envVarHelp() string {
	lines := make([]string, len(credentials.Kinds))
	for i, k := range credentials.Kinds {
		lines[i] = fmt.Sprintf("  %-34s %s", k.EnvVar(), k.Label())
	}
	return strings.Join(lines, "\n")
}

func notImplemented(command string) error { return fmt.Errorf("%s: %w", command, ErrNotImplemented) }

func runApply(context.Context, *Env, ApplyFlags) error   { return notImplemented("apply") }
func runPause(context.Context, *Env, PauseFlags) error   { return notImplemented("pause") }
func runResume(context.Context, *Env, ResumeFlags) error { return notImplemented("resume") }
