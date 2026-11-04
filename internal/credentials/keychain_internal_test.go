package credentials

import (
	"errors"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// The OS keychain cannot be used on CI runners, so these tests drive the
// wrapper through go-keyring's in-memory mock and check the error mapping.
func TestOSKeychainWithTheMock(t *testing.T) {
	keyring.MockInit()
	kc := OSKeychain()
	if err := kc.Set(KeychainService, "a", NewSecret("v1")); err != nil {
		t.Fatal(err)
	}
	got, err := kc.Get(KeychainService, "a")
	if err != nil || got.Reveal() != "v1" {
		t.Fatalf("Get = %q, %v", got.Reveal(), err)
	}
	if err := kc.Delete(KeychainService, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := kc.Get(KeychainService, "a"); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
	if err := kc.Delete(KeychainService, "a"); err != nil {
		t.Errorf("deleting a missing item must succeed: %v", err)
	}

	keyring.MockInitWithError(errors.New("org.freedesktop.secrets was not provided"))
	if _, err := kc.Get(KeychainService, "a"); !errors.Is(err, ErrKeychainUnavailable) {
		t.Errorf("a failing keychain must be unavailable: %v", err)
	}
}

func TestKeychainErrorMapping(t *testing.T) {
	if mapKeyringError(nil) != nil {
		t.Error("nil must stay nil")
	}
	if !errors.Is(mapKeyringError(keyring.ErrNotFound), ErrKeychainItemNotFound) {
		t.Error("ErrNotFound must map to ErrKeychainItemNotFound")
	}
	if err := mapKeyringError(keyring.ErrSetDataTooBig); errors.Is(err, ErrKeychainUnavailable) || !errors.Is(err, keyring.ErrSetDataTooBig) {
		t.Errorf("too big: %v", err)
	}
}

func TestKeychainTimeout(t *testing.T) {
	kc := osKeychain{timeout: 20 * time.Millisecond}
	err := kc.call(func() error { time.Sleep(time.Second); return nil })
	if !errors.Is(err, ErrKeychainUnavailable) {
		t.Errorf("a keychain that does not answer must time out as unavailable: %v", err)
	}
}
