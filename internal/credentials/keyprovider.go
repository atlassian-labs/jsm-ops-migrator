package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeychainService is the service name of every keychain item the CLI stores.
const KeychainService = "jsm-ops-migrator"

const (
	keyEncryptionKeyLen = 32
	wrappedFormat       = 0x01
	wrapAADPrefix       = "jsm-ops-migrator data key v1|"
)

// KeychainKeyProvider protects state.db's data key with a key-encryption key
// held in the OS keychain, under the account workspace-key-<workspace id>.
//
// It satisfies store.KeyProvider structurally, so this package does not
// import store.
type KeychainKeyProvider struct{ kc Keychain }

// NewKeychainKeyProvider returns a key provider backed by kc.
func NewKeychainKeyProvider(kc Keychain) *KeychainKeyProvider {
	return &KeychainKeyProvider{kc: kc}
}

// KeychainProviderID is the ID state.db records for a workspace whose key is
// in the OS keychain.
const KeychainProviderID = "keychain"

// ID names the provider in state.db, so a workspace is never opened with a
// different provider than the one that wrapped its key.
func (p *KeychainKeyProvider) ID() string { return KeychainProviderID }

// WorkspaceKeyAccount is the keychain account that holds a workspace's
// key-encryption key.
func WorkspaceKeyAccount(workspaceID string) string { return "workspace-key-" + workspaceID }

// WrapKey encrypts the data key. It creates the key-encryption key on first
// use and reuses an existing one, so a crash between storing the keychain item
// and saving the wrapped key does not strand a second key.
func (p *KeychainKeyProvider) WrapKey(ctx context.Context, workspaceID string, dataKey []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kek, err := p.keyEncryptionKey(workspaceID, true)
	if err != nil {
		return nil, err
	}
	return wrap(kek, workspaceID, dataKey)
}

// UnwrapKey decrypts the data key.
func (p *KeychainKeyProvider) UnwrapKey(ctx context.Context, workspaceID string, wrapped []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kek, err := p.keyEncryptionKey(workspaceID, false)
	if err != nil {
		return nil, err
	}
	dataKey, err := unwrap(kek, workspaceID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("the keychain item %s/%s does not match this state.db: %w",
			KeychainService, WorkspaceKeyAccount(workspaceID), err)
	}
	return dataKey, nil
}

func (p *KeychainKeyProvider) keyEncryptionKey(workspaceID string, create bool) ([]byte, error) {
	account := WorkspaceKeyAccount(workspaceID)
	s, err := p.kc.Get(KeychainService, account)
	switch {
	case err == nil:
		kek, err := base64.StdEncoding.DecodeString(s.Reveal())
		if err != nil || len(kek) != keyEncryptionKeyLen {
			return nil, fmt.Errorf("the keychain item %s/%s is not a valid key", KeychainService, account)
		}
		return kek, nil
	case errors.Is(err, ErrKeychainItemNotFound) && create:
		kek := make([]byte, keyEncryptionKeyLen)
		if _, err := rand.Read(kek); err != nil {
			return nil, err
		}
		if err := p.kc.Set(KeychainService, account, NewSecret(base64.StdEncoding.EncodeToString(kek))); err != nil {
			return nil, fmt.Errorf("storing the keychain item %s/%s: %w", KeychainService, account, err)
		}
		return kek, nil
	default:
		return nil, fmt.Errorf("reading the keychain item %s/%s: %w", KeychainService, account, err)
	}
}

func wrap(kek []byte, workspaceID string, dataKey []byte) ([]byte, error) {
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{wrappedFormat}, nonce...)
	return aead.Seal(out, nonce, dataKey, []byte(wrapAADPrefix+workspaceID)), nil
}

var errBadWrappedKey = errors.New("wrapped key cannot be decrypted")

func unwrap(kek []byte, workspaceID string, wrapped []byte) ([]byte, error) {
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(wrapped) < 1+n+aead.Overhead() || wrapped[0] != wrappedFormat {
		return nil, errBadWrappedKey
	}
	dataKey, err := aead.Open(nil, wrapped[1:1+n], wrapped[1+n:], []byte(wrapAADPrefix+workspaceID))
	if err != nil {
		return nil, errBadWrappedKey
	}
	return dataKey, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
