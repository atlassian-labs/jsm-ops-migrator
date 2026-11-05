package credentials

import (
	"fmt"
	"log/slog"
)

// Redacted is what every secret and sensitive value prints as.
const Redacted = "[redacted]"

// Secret holds a credential such as an API token.
//
// The value is held inside a closure. fmt cannot call methods through an
// unexported field, so a Secret nested in one is printed field by field; a
// pointer there would still show its target under verbs such as %s, but a func
// prints only as an address under every verb.
type Secret struct{ v func() string }

// NewSecret wraps a credential value.
func NewSecret(v string) Secret { return Secret{v: func() string { return v }} }

// Reveal returns the value. The repository rules allow it only in
// internal/credentials and internal/httpx, the two places that must hand the
// value to a keychain or an API.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return s.v()
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.Reveal() == "" }

func (Secret) String() string               { return Redacted }
func (Secret) GoString() string             { return Redacted }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (Secret) MarshalYAML() (any, error)    { return Redacted, nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }

// Sensitive holds personal data, such as an email address or a phone number,
// that must reach an API or a sealed store column but never a log or a file.
// It prints exactly like Secret. Unlike Secret.Reveal, Value is not restricted
// by the repository rules, because the code that builds API requests and
// sealed payloads needs it in many places.
type Sensitive struct{ v func() string }

// NewSensitive wraps a personal value.
func NewSensitive(v string) Sensitive { return Sensitive{v: func() string { return v }} }

// Value returns the value.
func (s Sensitive) Value() string {
	if s.v == nil {
		return ""
	}
	return s.v()
}

// IsZero reports whether the value is empty.
func (s Sensitive) IsZero() bool { return s.Value() == "" }

func (Sensitive) String() string               { return Redacted }
func (Sensitive) GoString() string             { return Redacted }
func (Sensitive) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (Sensitive) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (Sensitive) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (Sensitive) MarshalYAML() (any, error)    { return Redacted, nil }
func (Sensitive) LogValue() slog.Value         { return slog.StringValue(Redacted) }
