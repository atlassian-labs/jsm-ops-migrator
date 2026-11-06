package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
)

// Vault keeps one workspace's credentials.
type Vault interface {
	// Load returns the stored credentials; one never stored is zero.
	Load() (Set, error)
	// Save replaces the stored credentials with s. Saving an empty set
	// clears them.
	Save(s Set) error
	// Clear removes the stored credentials. Clearing nothing succeeds.
	Clear() error
	// Where names the storage in messages, for example "the OS keychain".
	Where() string
}

// storedSet is how a Set is serialized inside a vault: the only place its
// values are written out.
type storedSet struct {
	Version        int    `json:"version"`
	PagerDutyKey   string `json:"pagerduty_api_key,omitempty"`
	AtlassianEmail string `json:"atlassian_email,omitempty"`
	AtlassianToken string `json:"atlassian_api_token,omitempty"`
	OrgAPIKey      string `json:"org_api_key,omitempty"`
}

const storedSetVersion = 1

func encodeSet(s Set) ([]byte, error) {
	return json.Marshal(storedSet{
		Version:        storedSetVersion,
		PagerDutyKey:   s.value(PagerDutyKey),
		AtlassianEmail: s.value(AtlassianEmail),
		AtlassianToken: s.value(AtlassianToken),
		OrgAPIKey:      s.value(OrgAPIKey),
	})
}

// errCorrupt keeps the stored bytes out of the message: they hold secrets.
var errCorrupt = errors.New("the stored credentials cannot be read")

func decodeSet(data []byte) (Set, error) {
	var st storedSet
	if err := json.Unmarshal(data, &st); err != nil {
		return Set{}, errCorrupt
	}
	if st.Version != storedSetVersion {
		return Set{}, fmt.Errorf("the stored credentials have format %d; this jsm-ops-migrator reads %d", st.Version, storedSetVersion)
	}
	return Set{}.
		With(PagerDutyKey, st.PagerDutyKey).
		With(AtlassianEmail, st.AtlassianEmail).
		With(AtlassianToken, st.AtlassianToken).
		With(OrgAPIKey, st.OrgAPIKey), nil
}

// CredentialsAccount is the keychain account that holds a workspace's
// credentials, next to the workspace key under WorkspaceKeyAccount.
func CredentialsAccount(workspaceID string) string { return "credentials-" + workspaceID }

// NewKeychainVault keeps the credentials of the workspace with the given ID
// in kc, as one item, so the operator answers at most one keychain prompt for
// all of them.
func NewKeychainVault(kc Keychain, workspaceID string) Vault {
	return keychainVault{kc: kc, account: CredentialsAccount(workspaceID)}
}

type keychainVault struct {
	kc      Keychain
	account string
}

func (v keychainVault) Where() string { return "the OS keychain" }

func (v keychainVault) Load() (Set, error) {
	item, err := v.kc.Get(KeychainService, v.account)
	if errors.Is(err, ErrKeychainItemNotFound) {
		return Set{}, nil
	}
	if err != nil {
		return Set{}, fmt.Errorf("reading the keychain item %s/%s: %w", KeychainService, v.account, err)
	}
	return decodeSet([]byte(item.Reveal()))
}

func (v keychainVault) Save(s Set) error {
	if s.IsZero() {
		return v.Clear()
	}
	data, err := encodeSet(s)
	if err != nil {
		return err
	}
	if err := v.kc.Set(KeychainService, v.account, NewSecret(string(data))); err != nil {
		return fmt.Errorf("storing the keychain item %s/%s: %w", KeychainService, v.account, err)
	}
	return nil
}

func (v keychainVault) Clear() error {
	if err := v.kc.Delete(KeychainService, v.account); err != nil {
		return fmt.Errorf("removing the keychain item %s/%s: %w", KeychainService, v.account, err)
	}
	return nil
}

// CredentialsFile is the workspace file that holds the credentials on a
// machine without a keychain.
const CredentialsFile = "credentials.enc"

// Files reads, replaces and removes a workspace root file. *store.Workspace
// implements it.
type Files interface {
	// ReadFile returns an error that wraps fs.ErrNotExist for a missing file.
	ReadFile(name string) ([]byte, error)
	WriteFileAtomic(name string, data []byte) error
	// RemoveFile succeeds when the file does not exist.
	RemoveFile(name string) error
}

// Sealer encrypts and authenticates the file's content.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// NewFileVault keeps the credentials in credentials.enc, sealed by s. The
// workspace seals it with state.db's data key, which the passphrase protects,
// so one passphrase unlocks both.
func NewFileVault(files Files, s Sealer) Vault { return fileVault{files: files, sealer: s} }

type fileVault struct {
	files  Files
	sealer Sealer
}

func (v fileVault) Where() string { return CredentialsFile }

func (v fileVault) Load() (Set, error) {
	sealed, err := v.files.ReadFile(CredentialsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Set{}, nil
	}
	if err != nil {
		return Set{}, err
	}
	data, err := v.sealer.Open(sealed)
	if err != nil {
		return Set{}, fmt.Errorf("%s cannot be decrypted with this workspace's key: %w", CredentialsFile, err)
	}
	defer clear(data)
	return decodeSet(data)
}

func (v fileVault) Save(s Set) error {
	if s.IsZero() {
		return v.Clear()
	}
	data, err := encodeSet(s)
	if err != nil {
		return err
	}
	defer clear(data)
	sealed, err := v.sealer.Seal(data)
	if err != nil {
		return err
	}
	return v.files.WriteFileAtomic(CredentialsFile, sealed)
}

func (v fileVault) Clear() error { return v.files.RemoveFile(CredentialsFile) }
