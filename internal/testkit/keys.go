package testkit

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

// MemoryKeyProvider wraps data keys with a random key that lives only in
// memory, for tests that need a store without a keychain.
type MemoryKeyProvider struct{ aead cipher.AEAD }

// NewMemoryKeyProvider returns a key provider with a fresh random key.
func NewMemoryKeyProvider() *MemoryKeyProvider {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &MemoryKeyProvider{aead: aead}
}

// ID is "memory".
func (p *MemoryKeyProvider) ID() string { return "memory" }

// WrapKey encrypts the data key with the in-memory key.
func (p *MemoryKeyProvider) WrapKey(_ context.Context, workspaceID string, dataKey []byte) ([]byte, error) {
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return p.aead.Seal(nonce, nonce, dataKey, []byte(workspaceID)), nil
}

// UnwrapKey decrypts the data key.
func (p *MemoryKeyProvider) UnwrapKey(_ context.Context, workspaceID string, wrapped []byte) ([]byte, error) {
	n := p.aead.NonceSize()
	if len(wrapped) < n {
		return nil, errors.New("memory key provider: wrapped key too short")
	}
	return p.aead.Open(nil, wrapped[:n], wrapped[n:], []byte(workspaceID))
}

// FakeKeychain is an in-memory credentials.Keychain.
type FakeKeychain struct {
	mu          sync.Mutex
	items       map[string]string
	unavailable bool
}

// NewFakeKeychain returns an empty fake keychain.
func NewFakeKeychain() *FakeKeychain { return &FakeKeychain{items: map[string]string{}} }

// SetUnavailable makes every call fail with credentials.ErrKeychainUnavailable,
// as on a machine without a keychain.
func (k *FakeKeychain) SetUnavailable(v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unavailable = v
}

// Len is the number of stored items.
func (k *FakeKeychain) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

func (k *FakeKeychain) Get(service, account string) (credentials.Secret, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unavailable {
		return credentials.Secret{}, credentials.ErrKeychainUnavailable
	}
	v, ok := k.items[service+"\x00"+account]
	if !ok {
		return credentials.Secret{}, credentials.ErrKeychainItemNotFound
	}
	return credentials.NewSecret(v), nil
}

func (k *FakeKeychain) Set(service, account string, value credentials.Secret) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unavailable {
		return credentials.ErrKeychainUnavailable
	}
	k.items[service+"\x00"+account] = value.Reveal()
	return nil
}

func (k *FakeKeychain) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unavailable {
		return credentials.ErrKeychainUnavailable
	}
	delete(k.items, service+"\x00"+account)
	return nil
}

// NewStore opens a store in a fresh temporary workspace with an in-memory key
// provider, and closes it when the test ends.
func NewStore(t testing.TB) *store.Store {
	t.Helper()
	ws, err := store.OpenWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), ws, NewMemoryKeyProvider(), store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// FileKeychain is a credentials.Keychain kept in a JSON file. Tests that run
// the binary as separate processes use it, since a FakeKeychain lives in one
// process and the real keychain must never be touched by a test.
type FileKeychain struct{ path string }

// NewFileKeychain returns a keychain stored at path, which need not exist yet.
func NewFileKeychain(path string) *FileKeychain { return &FileKeychain{path: path} }

func (k *FileKeychain) load() (map[string]string, error) {
	items := map[string]string{}
	data, err := os.ReadFile(k.path)
	if errors.Is(err, fs.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	return items, json.Unmarshal(data, &items)
}

func (k *FileKeychain) save(items map[string]string) error {
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return os.WriteFile(k.path, data, 0o600)
}

func (k *FileKeychain) Get(service, account string) (credentials.Secret, error) {
	items, err := k.load()
	if err != nil {
		return credentials.Secret{}, err
	}
	v, ok := items[service+"/"+account]
	if !ok {
		return credentials.Secret{}, credentials.ErrKeychainItemNotFound
	}
	return credentials.NewSecret(v), nil
}

func (k *FileKeychain) Set(service, account string, value credentials.Secret) error {
	items, err := k.load()
	if err != nil {
		return err
	}
	items[service+"/"+account] = value.Reveal()
	return k.save(items)
}

func (k *FileKeychain) Delete(service, account string) error {
	items, err := k.load()
	if err != nil {
		return err
	}
	delete(items, service+"/"+account)
	return k.save(items)
}
