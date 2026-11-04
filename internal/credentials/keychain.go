package credentials

import (
	"errors"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
)

var (
	// ErrKeychainItemNotFound means the keychain has no item for the service
	// and account.
	ErrKeychainItemNotFound = errors.New("keychain item not found")
	// ErrKeychainUnavailable means the OS keychain could not be used, for
	// example on a headless Linux machine without a Secret Service.
	ErrKeychainUnavailable = errors.New("keychain unavailable")
)

// Keychain stores secrets in the OS keychain: macOS Keychain, Windows
// Credential Manager or the Linux Secret Service.
type Keychain interface {
	// Get returns ErrKeychainItemNotFound when there is no such item.
	Get(service, account string) (Secret, error)
	Set(service, account string, value Secret) error
	// Delete succeeds when there is no such item.
	Delete(service, account string) error
}

// keychainTimeout bounds every keychain call. On Linux a Secret Service that
// waits for an unlock prompt nobody will answer would otherwise hang the CLI.
const keychainTimeout = 10 * time.Second

// OSKeychain returns the OS keychain.
func OSKeychain() Keychain { return osKeychain{timeout: keychainTimeout} }

type osKeychain struct{ timeout time.Duration }

func (k osKeychain) Get(service, account string) (Secret, error) {
	var v string
	err := k.call(func() (err error) { v, err = keyring.Get(service, account); return err })
	if err != nil {
		return Secret{}, err
	}
	return NewSecret(v), nil
}

func (k osKeychain) Set(service, account string, value Secret) error {
	return k.call(func() error { return keyring.Set(service, account, value.Reveal()) })
}

// Delete removes the item. Removing an item that does not exist succeeds, so
// login --clear can run twice.
func (k osKeychain) Delete(service, account string) error {
	err := k.call(func() error { return keyring.Delete(service, account) })
	if errors.Is(err, ErrKeychainItemNotFound) {
		return nil
	}
	return err
}

// call runs fn with the timeout. A call that times out keeps running in its
// goroutine, because the keychain APIs cannot be canceled; the CLI only stops
// waiting for it.
func (k osKeychain) call(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return mapKeyringError(err)
	case <-time.After(k.timeout):
		return fmt.Errorf("%w: no answer after %s", ErrKeychainUnavailable, k.timeout)
	}
}

func mapKeyringError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrKeychainItemNotFound
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return fmt.Errorf("keychain: %w", err)
	default:
		return fmt.Errorf("%w: %w", ErrKeychainUnavailable, err)
	}
}
