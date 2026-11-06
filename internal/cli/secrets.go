package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/tui"
)

// PassphraseEnvVar supplies the passphrase of a workspace without a keychain,
// for scripts and CI.
const PassphraseEnvVar = envPrefix + "_PASSPHRASE"

// defaultKeyProvider is the provider state.db records. A new workspace uses
// the OS keychain when it works. Without one it uses a passphrase, but only
// when JSM_OPS_MIGRATOR_PASSPHRASE is set or the operator agrees, so a keychain
// that is merely slow to answer never moves a workspace to a passphrase
// without anyone noticing.
func (e *Env) defaultKeyProvider(ctx context.Context) (store.KeyProvider, error) {
	info, err := store.ReadInfo(ctx, e.Workspace)
	if err != nil {
		return nil, err
	}
	switch info.KeyProvider {
	case credentials.KeychainProviderID:
		return credentials.NewKeychainKeyProvider(e.keychain), nil
	case credentials.PassphraseProviderID:
		return credentials.NewPassphraseKeyProvider(e.askPassphrase), nil
	case "":
	default:
		return nil, fmt.Errorf("%w: state.db's key is protected by %q, which this jsm-ops-migrator does not know",
			store.ErrKeyUnavailable, info.KeyProvider)
	}

	reason := credentials.KeychainAvailable(e.keychain)
	if reason == nil {
		return credentials.NewKeychainKeyProvider(e.keychain), nil
	}
	if os.Getenv(PassphraseEnvVar) == "" {
		if !e.Interactive {
			return nil, awaitingInput(fmt.Errorf("this machine's keychain cannot be used (%s): set %s to protect the workspace with a passphrase instead",
				credentials.RedactText(reason.Error()), PassphraseEnvVar))
		}
		choice, err := e.Choose(ctx, "This machine's keychain cannot be used",
			credentials.RedactText(reason.Error())+"\n\n"+
				"jsm-ops-migrator can protect this workspace with a passphrase instead. The credentials\n"+
				"then go in an encrypted file, and every command asks for the passphrase.",
			[]tui.Option{{Key: "passphrase", Label: "Use a passphrase"}, {Key: "cancel", Label: "Cancel"}}, "passphrase")
		if err != nil {
			return nil, err
		}
		if choice != "passphrase" {
			return nil, tui.ErrCanceled
		}
	}
	return credentials.NewPassphraseKeyProvider(e.askPassphrase), nil
}

// askPassphrase takes JSM_OPS_MIGRATOR_PASSPHRASE, or asks in the terminal: twice
// for a new workspace.
func (e *Env) askPassphrase(ctx context.Context, isNew bool) (credentials.Secret, error) {
	if v := os.Getenv(PassphraseEnvVar); v != "" {
		return credentials.NewSecret(v), nil
	}
	if !e.Interactive {
		return credentials.Secret{}, awaitingInput(fmt.Errorf("this workspace is protected by a passphrase: set %s", PassphraseEnvVar))
	}
	e.passPrompted = true
	if !isNew {
		answers, err := e.Ask(ctx, "Unlock "+displayPath(e.Workspace.Dir()), []tui.Question{
			{Title: "Passphrase", Masked: true, Description: "The passphrase that protects this workspace."},
		})
		if err != nil {
			return credentials.Secret{}, err
		}
		return credentials.NewSecret(answers[0]), nil
	}
	for try := 1; ; try++ {
		answers, err := e.Ask(ctx, "Choose a passphrase for "+displayPath(e.Workspace.Dir()), []tui.Question{
			{
				Title:  "Passphrase",
				Masked: true,
				Description: fmt.Sprintf("At least %d characters. Keep it safe: nothing can recover the workspace without it.",
					credentials.MinPassphraseLen),
				Validate: func(v string) error { return credentials.CheckNewPassphrase(credentials.NewSecret(v)) },
			},
			{Title: "The same passphrase again", Masked: true},
		})
		if err != nil {
			return credentials.Secret{}, err
		}
		first, second := credentials.NewSecret(answers[0]), credentials.NewSecret(answers[1])
		if first.Equal(second) {
			return first, nil
		}
		if try == maxPassphraseTries {
			return credentials.Secret{}, errors.New("the passphrases did not match")
		}
		_, _ = io.WriteString(e.Stderr, "The passphrases do not match. Try again.\n")
	}
}

func (e *Env) passphrasePrompted() bool { return e.passPrompted }

// vault is where the opened workspace keeps its credentials: the OS keychain,
// or credentials.enc for a workspace protected by a passphrase.
func (e *Env) vault(st *store.Store) credentials.Vault {
	if st.KeyProviderID() == credentials.PassphraseProviderID {
		return credentials.NewFileVault(e.Workspace, fileSealer{st.Cipher()})
	}
	return credentials.NewKeychainVault(e.keychain, st.WorkspaceID())
}

// credentialsFile seals credentials.enc with the workspace's data key, bound
// to that file.
var credentialsFile = store.FieldRef{Table: "file", Item: credentials.CredentialsFile, Column: "credentials"}

type fileSealer struct{ c store.Cipher }

func (f fileSealer) Seal(p []byte) ([]byte, error) { return f.c.Seal(credentialsFile, p) }
func (f fileSealer) Open(s []byte) ([]byte, error) { return f.c.Open(credentialsFile, s) }

// resolveCredentials returns the stored credentials with the ones set in the
// environment on top, and the source of each one the environment gave.
func resolveCredentials(v credentials.Vault) (credentials.Set, map[credentials.Kind]string, error) {
	stored, err := v.Load()
	if err != nil {
		return credentials.Set{}, nil, err
	}
	fromEnv, kinds := credentials.FromEnv(os.Getenv)
	sources := map[credentials.Kind]string{}
	for _, k := range kinds {
		sources[k] = "from " + k.EnvVar()
	}
	return stored.Merge(fromEnv), sources, nil
}

// credentialsStatus tells the menu whether every credential is available,
// without the workspace lock, its key or a passphrase. It returns nil when it
// cannot tell.
func credentialsStatus(ctx context.Context, ws *store.Workspace, kc credentials.Keychain) *bool {
	fromEnv, _ := credentials.FromEnv(os.Getenv)
	info, err := store.ReadInfo(ctx, ws)
	if err != nil {
		return nil
	}
	var stored credentials.Set
	switch {
	case info.WorkspaceID == "":
	case info.KeyProvider == credentials.PassphraseProviderID:
		// The file cannot be read without the passphrase; that it exists has
		// to do.
		if _, err := ws.ReadFile(credentials.CredentialsFile); err == nil {
			ok := true
			return &ok
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	default:
		if stored, err = credentials.NewKeychainVault(kc, info.WorkspaceID).Load(); err != nil {
			return nil
		}
	}
	ok := len(stored.Merge(fromEnv).Missing()) == 0
	return &ok
}

// noKeychain stands in when no keychain is configured.
type noKeychain struct{}

var errNoKeychain = fmt.Errorf("%w: no keychain is configured", credentials.ErrKeychainUnavailable)

func (noKeychain) Get(string, string) (credentials.Secret, error) {
	return credentials.Secret{}, errNoKeychain
}
func (noKeychain) Set(string, string, credentials.Secret) error { return errNoKeychain }
func (noKeychain) Delete(string, string) error                  { return errNoKeychain }
