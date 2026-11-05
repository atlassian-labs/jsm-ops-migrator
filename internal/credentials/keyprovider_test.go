package credentials_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/testkit"
)

var _ store.KeyProvider = (*credentials.KeychainKeyProvider)(nil)

func TestKeychainKeyProviderRoundTrip(t *testing.T) {
	ctx := context.Background()
	kc := testkit.NewFakeKeychain()
	p := credentials.NewKeychainKeyProvider(kc)
	dataKey := bytes.Repeat([]byte{7}, 32)

	wrapped, err := p.WrapKey(ctx, "ws1", dataKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped, dataKey) {
		t.Fatal("the wrapped key contains the data key in clear")
	}
	if kc.Len() != 1 {
		t.Fatalf("keychain items = %d, want 1", kc.Len())
	}
	got, err := p.UnwrapKey(ctx, "ws1", wrapped)
	if err != nil || !bytes.Equal(got, dataKey) {
		t.Fatalf("UnwrapKey = %x, %v", got, err)
	}

	// A second wrap reuses the key-encryption key instead of making another.
	again, err := p.WrapKey(ctx, "ws1", dataKey)
	if err != nil || kc.Len() != 1 {
		t.Fatalf("second WrapKey: err=%v items=%d", err, kc.Len())
	}
	if got, err := p.UnwrapKey(ctx, "ws1", again); err != nil || !bytes.Equal(got, dataKey) {
		t.Fatalf("unwrapping the second wrap = %x, %v", got, err)
	}
}

func TestKeychainKeyProviderRefusesWrongInput(t *testing.T) {
	ctx := context.Background()
	kc := testkit.NewFakeKeychain()
	p := credentials.NewKeychainKeyProvider(kc)
	wrapped, err := p.WrapKey(ctx, "ws1", bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Clone(wrapped)
	tampered[len(tampered)-1] ^= 1
	if _, err := p.UnwrapKey(ctx, "ws1", tampered); err == nil {
		t.Error("a changed byte must fail")
	}
	// Give ws2 the very same key-encryption key: only the workspace ID in
	// the wrap's additional data can then refuse it.
	kek, _ := kc.Get(credentials.KeychainService, credentials.WorkspaceKeyAccount("ws1"))
	_ = kc.Set(credentials.KeychainService, credentials.WorkspaceKeyAccount("ws2"), kek)
	if _, err := p.UnwrapKey(ctx, "ws2", wrapped); err == nil {
		t.Error("a wrap made for one workspace must not open for another")
	}
	if err := kc.Delete(credentials.KeychainService, credentials.WorkspaceKeyAccount("ws1")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UnwrapKey(ctx, "ws1", wrapped); !errors.Is(err, credentials.ErrKeychainItemNotFound) {
		t.Errorf("missing entry: err = %v, want ErrKeychainItemNotFound", err)
	}
	// Through the store, a missing keychain item is a key the store cannot get.
	ws, _ := store.OpenWorkspace(t.TempDir(), true)
	st, err := store.Open(ctx, ws, p, store.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	id := st.WorkspaceID()
	_ = st.Close()
	_ = kc.Delete(credentials.KeychainService, credentials.WorkspaceKeyAccount(id))
	if _, err := store.Open(ctx, ws, p, store.OpenExisting); !errors.Is(err, store.ErrKeyUnavailable) || !errors.Is(err, credentials.ErrKeychainItemNotFound) {
		t.Errorf("store open without the keychain item: %v", err)
	}
	kc.SetUnavailable(true)
	if _, err := p.WrapKey(ctx, "ws3", bytes.Repeat([]byte{1}, 32)); !errors.Is(err, credentials.ErrKeychainUnavailable) {
		t.Errorf("unavailable keychain: err = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.WrapKey(canceled, "ws4", bytes.Repeat([]byte{1}, 32)); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: err = %v", err)
	}
}
