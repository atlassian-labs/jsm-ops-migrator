package credentials_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

func allCredentials() credentials.Set { return testkit.AllCredentials() }

func sameSet(t *testing.T, got, want credentials.Set) {
	t.Helper()
	if !got.PagerDutyKey.Equal(want.PagerDutyKey) || got.AtlassianEmail.Value() != want.AtlassianEmail.Value() ||
		!got.AtlassianToken.Equal(want.AtlassianToken) || !got.OrgAPIKey.Equal(want.OrgAPIKey) {
		t.Errorf("credentials differ: missing %v, want missing %v", got.Missing(), want.Missing())
	}
}

func TestKeychainVault(t *testing.T) {
	kc := testkit.NewFakeKeychain()
	v := credentials.NewKeychainVault(kc, "ws1")
	if got, err := v.Load(); err != nil || !got.IsZero() {
		t.Fatalf("empty vault: %v, %v", got.Missing(), err)
	}
	if err := v.Save(allCredentials()); err != nil {
		t.Fatal(err)
	}
	if kc.Len() != 1 {
		t.Errorf("the credentials must be one keychain item, got %d", kc.Len())
	}
	item, _ := kc.Get(credentials.KeychainService, credentials.CredentialsAccount("ws1"))
	if strings.Contains(item.Reveal(), credentials.Redacted) {
		t.Errorf("the item holds redacted placeholders instead of values")
	}
	got, err := v.Load()
	if err != nil {
		t.Fatal(err)
	}
	sameSet(t, got, allCredentials())

	// Another workspace has its own item.
	if got, _ := credentials.NewKeychainVault(kc, "ws2").Load(); !got.IsZero() {
		t.Error("workspaces share credentials")
	}
	// Saving an empty set clears, and clearing twice is fine.
	if err := v.Save(credentials.Set{}); err != nil || kc.Len() != 0 {
		t.Errorf("empty save: %v, items %d", err, kc.Len())
	}
	if err := v.Clear(); err != nil {
		t.Error(err)
	}
	kc.SetUnavailable(true)
	if _, err := v.Load(); !errors.Is(err, credentials.ErrKeychainUnavailable) {
		t.Errorf("unavailable keychain: %v", err)
	}
}

func TestKeychainVaultCorruptItemIsNotShown(t *testing.T) {
	kc := testkit.NewFakeKeychain()
	_ = kc.Set(credentials.KeychainService, credentials.CredentialsAccount("ws1"), credentials.NewSecret("canary-not-json"))
	_, err := credentials.NewKeychainVault(kc, "ws1").Load()
	if err == nil || strings.Contains(err.Error(), "canary") {
		t.Errorf("err = %v", err)
	}
}

type memFiles map[string][]byte

func (m memFiles) ReadFile(name string) ([]byte, error) {
	b, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", name, fs.ErrNotExist)
	}
	return b, nil
}
func (m memFiles) WriteFileAtomic(name string, data []byte) error { m[name] = data; return nil }
func (m memFiles) RemoveFile(name string) error                   { delete(m, name); return nil }

type gcmSealer struct{ aead cipher.AEAD }

func newSealer(t *testing.T) gcmSealer {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, _ := cipher.NewGCM(block)
	return gcmSealer{aead}
}

func (s gcmSealer) Seal(p []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, p, nil), nil
}

func (s gcmSealer) Open(b []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(b) < n {
		return nil, errors.New("short")
	}
	return s.aead.Open(nil, b[:n], b[n:], nil)
}

func TestFileVault(t *testing.T) {
	files, sealer := memFiles{}, newSealer(t)
	v := credentials.NewFileVault(files, sealer)
	if err := v.Save(allCredentials()); err != nil {
		t.Fatal(err)
	}
	data := files[credentials.CredentialsFile]
	for _, value := range []string{"canary", "operator@example.com"} {
		if bytes.Contains(data, []byte(value)) {
			t.Errorf("%s holds %q in clear", credentials.CredentialsFile, value)
		}
	}
	got, err := v.Load()
	if err != nil {
		t.Fatal(err)
	}
	sameSet(t, got, allCredentials())

	// A file sealed with another workspace's key does not open.
	if _, err := credentials.NewFileVault(files, newSealer(t)).Load(); err == nil {
		t.Error("another key opened the file")
	}
	if err := v.Clear(); err != nil || len(files) != 0 {
		t.Errorf("clear: %v, files %v", err, files)
	}
	if got, err := v.Load(); err != nil || !got.IsZero() {
		t.Errorf("after clear: %v, %v", got.Missing(), err)
	}
}

// The workspace is what the file vault writes through in the CLI.
var _ credentials.Files = (*store.Workspace)(nil)
