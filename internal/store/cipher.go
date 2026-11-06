package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// KeyProvider protects state.db's data key. Both providers live in
// internal/credentials: the keychain one, and the passphrase one for machines
// without a keychain. The wrapped value is opaque to the store, so a provider
// can keep its own parameters (a salt, for example) inside it.
type KeyProvider interface {
	// ID names the provider; it is saved with the wrapped key, and a
	// workspace never opens with a different provider.
	ID() string
	WrapKey(ctx context.Context, workspaceID string, dataKey []byte) ([]byte, error)
	UnwrapKey(ctx context.Context, workspaceID string, wrapped []byte) ([]byte, error)
}

// FieldRef names where a sealed value lives. A value sealed for one
// table, item and column cannot be opened for any other.
type FieldRef struct{ Table, Item, Column string }

// Cipher seals payload columns and computes keyed fingerprints.
type Cipher interface {
	Seal(ref FieldRef, plaintext []byte) ([]byte, error)
	Open(ref FieldRef, sealed []byte) ([]byte, error)
	// Fingerprint is an HMAC-SHA-256 under a key derived from the workspace's
	// data key. Use it for values kept in clear for lookups: unlike a plain
	// hash, it cannot be reversed by hashing guesses such as phone numbers.
	Fingerprint(domain string, data []byte) [32]byte
}

// ErrDecrypt means a sealed value could not be opened: it was changed, it
// belongs to another place, or another workspace's key sealed it.
var ErrDecrypt = errors.New("store: value cannot be decrypted")

const (
	dataKeyLen   = 32
	sealedFormat = 0x01
	fieldAADTag  = "jsm-ops-migrator field v1"
	fpKeyLabel   = "jsm-ops-migrator fingerprint v1"
)

type fieldCipher struct {
	aead  cipher.AEAD
	fpKey []byte
}

func newFieldCipher(dataKey []byte) (*fieldCipher, error) {
	if len(dataKey) != dataKeyLen {
		return nil, fmt.Errorf("store: data key must be %d bytes, got %d", dataKeyLen, len(dataKey))
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, dataKey)
	mac.Write([]byte(fpKeyLabel))
	return &fieldCipher{aead: aead, fpKey: mac.Sum(nil)}, nil
}

func (c *fieldCipher) Seal(ref FieldRef, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{sealedFormat}, nonce...)
	return c.aead.Seal(out, nonce, plaintext, fieldAAD(ref)), nil
}

func (c *fieldCipher) Open(ref FieldRef, sealed []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(sealed) < 1+n+c.aead.Overhead() || sealed[0] != sealedFormat {
		return nil, ErrDecrypt
	}
	plaintext, err := c.aead.Open(nil, sealed[1:1+n], sealed[1+n:], fieldAAD(ref))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func (c *fieldCipher) Fingerprint(domain string, data []byte) [32]byte {
	mac := hmac.New(sha256.New, c.fpKey)
	mac.Write(lengthPrefixed(domain))
	mac.Write(data)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// zero overwrites the fingerprint key. The AES key inside the AEAD cannot be
// reached, so this is best effort.
func (c *fieldCipher) zero() { clear(c.fpKey) }

// fieldAAD length-prefixes each part, so ("a|b", "c") and ("a", "b|c") never
// produce the same additional data.
func fieldAAD(ref FieldRef) []byte {
	out := []byte(fieldAADTag)
	for _, part := range []string{ref.Table, ref.Item, ref.Column} {
		out = append(out, lengthPrefixed(part)...)
	}
	return out
}

func lengthPrefixed(s string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(s)))
	return append(out, s...)
}
