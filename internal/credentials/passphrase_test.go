package credentials_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/store"
)

var _ store.KeyProvider = (*credentials.PassphraseKeyProvider)(nil)

type asker struct {
	answers []string
	asked   []bool // isNew for each call
}

func (a *asker) ask(_ context.Context, isNew bool) (credentials.Secret, error) {
	a.asked = append(a.asked, isNew)
	if len(a.answers) == 0 {
		return credentials.Secret{}, errors.New("no more answers")
	}
	v := a.answers[0]
	a.answers = a.answers[1:]
	return credentials.NewSecret(v), nil
}

func TestPassphraseKeyProvider(t *testing.T) {
	defer credentials.FastPassphraseKDF()()
	ctx := context.Background()
	dataKey := bytes.Repeat([]byte{9}, 32)
	a := &asker{answers: []string{"correct horse battery staple"}}
	p := credentials.NewPassphraseKeyProvider(a.ask)

	wrapped, err := p.WrapKey(ctx, "ws1", dataKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped, dataKey) {
		t.Fatal("the wrapped key holds the data key in clear")
	}
	// The same command reuses the passphrase instead of asking again.
	got, err := p.UnwrapKey(ctx, "ws1", wrapped)
	if err != nil || !bytes.Equal(got, dataKey) || len(a.asked) != 1 || !a.asked[0] {
		t.Fatalf("unwrap: %v, asked %v", err, a.asked)
	}

	// A later command asks once, not as a new passphrase.
	b := &asker{answers: []string{"wrong passphrase!!", "correct horse battery staple"}}
	later := credentials.NewPassphraseKeyProvider(b.ask)
	if _, err := later.UnwrapKey(ctx, "ws1", wrapped); !errors.Is(err, credentials.ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	// The wrong one is forgotten, so the next try asks again.
	if got, err := later.UnwrapKey(ctx, "ws1", wrapped); err != nil || !bytes.Equal(got, dataKey) {
		t.Fatalf("second try: %v", err)
	}
	if len(b.asked) != 2 || b.asked[0] || b.asked[1] {
		t.Errorf("asked %v, want twice for an existing passphrase", b.asked)
	}

	// The key is bound to its workspace.
	other := credentials.NewPassphraseKeyProvider((&asker{answers: []string{"correct horse battery staple"}}).ask)
	if _, err := other.UnwrapKey(ctx, "ws2", wrapped); err == nil {
		t.Error("the key opened for another workspace")
	}
}

func TestPassphraseKeyProviderRejectsAWeakNewPassphrase(t *testing.T) {
	defer credentials.FastPassphraseKDF()()
	p := credentials.NewPassphraseKeyProvider((&asker{answers: []string{"short"}}).ask)
	if _, err := p.WrapKey(context.Background(), "ws1", make([]byte, 32)); !errors.Is(err, credentials.ErrPassphraseTooShort) {
		t.Errorf("err = %v", err)
	}
}

// A tampered state.db must not make the derivation use any amount of memory.
func TestPassphraseKeyProviderBoundsTheParameters(t *testing.T) {
	defer credentials.FastPassphraseKDF()()
	ctx := context.Background()
	p := credentials.NewPassphraseKeyProvider((&asker{answers: []string{"correct horse battery staple"}}).ask)
	wrapped, err := p.WrapKey(ctx, "ws1", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), wrapped...)
	copy(tampered[5:9], []byte{0xff, 0xff, 0xff, 0xff}) // memory
	asked := &asker{}
	q := credentials.NewPassphraseKeyProvider(asked.ask)
	if _, err := q.UnwrapKey(ctx, "ws1", tampered); err == nil || len(asked.asked) != 0 {
		t.Errorf("tampered parameters: err %v, asked %v", err, asked.asked)
	}
}

func TestKeychainAvailable(t *testing.T) {
	if err := credentials.KeychainAvailable(nil); !errors.Is(err, credentials.ErrKeychainUnavailable) {
		t.Errorf("nil keychain: %v", err)
	}
}
