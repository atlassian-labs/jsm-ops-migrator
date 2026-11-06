package credentials

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/argon2"
)

// ErrWrongPassphrase means the passphrase does not unlock the workspace.
var ErrWrongPassphrase = errors.New("the passphrase is wrong")

// PassphraseProviderID is the ID state.db records for a workspace protected
// by a passphrase.
const PassphraseProviderID = "passphrase"

// The argon2id parameters are RFC 9106's second recommended option, for
// machines where 2 GiB of memory per derivation is too much. They are saved
// with each wrapped key, so they can change later without breaking existing
// workspaces.
var (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
)

// Bounds on what a wrapped key may ask for, so a tampered state.db cannot make
// the derivation take all the machine's memory or time.
const (
	maxArgonTime   = 16
	maxArgonMemory = 1024 * 1024 // KiB
	maxArgonThread = 64
)

const (
	saltLen          = 16
	passphraseFormat = 0x01
	passphraseHeader = 1 + 4 + 4 + 1 + saltLen
	passphraseAAD    = "jsm-ops-migrator data key v1|passphrase|"
)

// AskPassphrase returns the passphrase. With isNew it is choosing one for a
// new workspace, so a prompt should ask twice and check its strength.
type AskPassphrase func(ctx context.Context, isNew bool) (Secret, error)

// PassphraseKeyProvider protects state.db's data key with a key derived from
// a passphrase with argon2id, for machines without a keychain such as
// headless Linux and CI. It asks for the passphrase the first time it needs
// it and remembers it for the rest of the command.
//
// It satisfies store.KeyProvider structurally, so this package does not
// import store.
type PassphraseKeyProvider struct {
	ask  AskPassphrase
	mu   sync.Mutex
	pass Secret
}

// NewPassphraseKeyProvider returns a key provider that gets its passphrase
// from ask.
func NewPassphraseKeyProvider(ask AskPassphrase) *PassphraseKeyProvider {
	return &PassphraseKeyProvider{ask: ask}
}

// ID is "passphrase".
func (p *PassphraseKeyProvider) ID() string { return PassphraseProviderID }

func (p *PassphraseKeyProvider) passphrase(ctx context.Context, isNew bool) (Secret, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pass.IsZero() {
		return p.pass, nil
	}
	pass, err := p.ask(ctx, isNew)
	if err != nil {
		return Secret{}, err
	}
	if pass.IsZero() {
		return Secret{}, errors.New("no passphrase given")
	}
	if isNew {
		if err := CheckNewPassphrase(pass); err != nil {
			return Secret{}, err
		}
	}
	p.pass = pass
	return pass, nil
}

// WrapKey encrypts the data key with a key derived from the passphrase and a
// fresh salt.
func (p *PassphraseKeyProvider) WrapKey(ctx context.Context, workspaceID string, dataKey []byte) ([]byte, error) {
	pass, err := p.passphrase(ctx, true)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, passphraseHeader)
	header = append(header, passphraseFormat)
	header = binary.BigEndian.AppendUint32(header, argonTime)
	header = binary.BigEndian.AppendUint32(header, argonMemory)
	header = append(header, argonThreads)
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	header = append(header, salt...)

	kek := argon2.IDKey([]byte(pass.Reveal()), salt, argonTime, argonMemory, argonThreads, keyEncryptionKeyLen)
	defer clear(kek)
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(header, nonce...)
	return aead.Seal(out, nonce, dataKey, passphraseAADFor(workspaceID, header)), nil
}

// UnwrapKey derives the key with the saved parameters and decrypts the data
// key. A wrong passphrase returns ErrWrongPassphrase and is forgotten, so the
// next call asks again.
func (p *PassphraseKeyProvider) UnwrapKey(ctx context.Context, workspaceID string, wrapped []byte) ([]byte, error) {
	if len(wrapped) < passphraseHeader || wrapped[0] != passphraseFormat {
		return nil, errBadWrappedKey
	}
	header := wrapped[:passphraseHeader]
	t := binary.BigEndian.Uint32(header[1:5])
	m := binary.BigEndian.Uint32(header[5:9])
	threads := header[9]
	salt := header[10:passphraseHeader]
	if t == 0 || t > maxArgonTime || m < 8*uint32(threads) || m > maxArgonMemory || threads == 0 || threads > maxArgonThread {
		return nil, fmt.Errorf("%w: unexpected key derivation parameters", errBadWrappedKey)
	}

	pass, err := p.passphrase(ctx, false)
	if err != nil {
		return nil, err
	}
	kek := argon2.IDKey([]byte(pass.Reveal()), salt, t, m, threads, keyEncryptionKeyLen)
	defer clear(kek)
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	rest := wrapped[passphraseHeader:]
	n := aead.NonceSize()
	if len(rest) < n+aead.Overhead() {
		return nil, errBadWrappedKey
	}
	dataKey, err := aead.Open(nil, rest[:n], rest[n:], passphraseAADFor(workspaceID, header))
	if err != nil {
		p.mu.Lock()
		p.pass = Secret{}
		p.mu.Unlock()
		return nil, ErrWrongPassphrase
	}
	return dataKey, nil
}

func passphraseAADFor(workspaceID string, header []byte) []byte {
	return append([]byte(passphraseAAD+workspaceID+"|"), header...)
}

// KeychainAvailable returns nil when kc can be used, and otherwise an error
// that wraps ErrKeychainUnavailable. It looks up an item that never exists, so
// it changes nothing and, on macOS, shows no prompt.
func KeychainAvailable(kc Keychain) error {
	if kc == nil {
		return fmt.Errorf("%w: none configured", ErrKeychainUnavailable)
	}
	_, err := kc.Get(KeychainService, "availability-check")
	if err == nil || errors.Is(err, ErrKeychainItemNotFound) {
		return nil
	}
	return err
}
