package credentials

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"
)

// Kind names one of the credentials the CLI uses.
type Kind string

const (
	PagerDutyKey   Kind = "pagerduty_api_key"
	AtlassianEmail Kind = "atlassian_email"
	AtlassianToken Kind = "atlassian_api_token"
	OrgAPIKey      Kind = "org_api_key"
)

// Kinds lists every credential, in the order prompts and messages use.
var Kinds = []Kind{PagerDutyKey, AtlassianEmail, AtlassianToken, OrgAPIKey}

var labels = map[Kind]string{
	PagerDutyKey:   "PagerDuty API key",
	AtlassianEmail: "Atlassian account email",
	AtlassianToken: "Atlassian API token",
	OrgAPIKey:      "org API key",
}

// Label is how messages name the credential, for example "PagerDuty API key".
func (k Kind) Label() string { return labels[k] }

// EnvVar is the environment variable that supplies the credential, for
// example JSM_OPS_MIGRATOR_PAGERDUTY_API_KEY. The same names are the keys that
// login --stdin reads.
func (k Kind) EnvVar() string { return "JSM_OPS_MIGRATOR_" + strings.ToUpper(string(k)) }

// Set holds the credentials of one workspace. A zero field is missing.
type Set struct {
	PagerDutyKey   Secret
	AtlassianEmail Sensitive
	AtlassianToken Secret
	OrgAPIKey      Secret
}

// Has reports whether the set holds credential k.
func (s Set) Has(k Kind) bool {
	switch k {
	case PagerDutyKey:
		return !s.PagerDutyKey.IsZero()
	case AtlassianEmail:
		return !s.AtlassianEmail.IsZero()
	case AtlassianToken:
		return !s.AtlassianToken.IsZero()
	case OrgAPIKey:
		return !s.OrgAPIKey.IsZero()
	}
	return false
}

// Missing returns the kinds the set lacks, in the order of Kinds. With no
// arguments it checks every kind.
func (s Set) Missing(kinds ...Kind) []Kind {
	if len(kinds) == 0 {
		kinds = Kinds
	}
	var out []Kind
	for _, k := range kinds {
		if !s.Has(k) {
			out = append(out, k)
		}
	}
	return out
}

// IsZero reports whether the set holds no credential at all.
func (s Set) IsZero() bool { return len(s.Missing()) == len(Kinds) }

// With returns the set with credential k set to value, trimmed of the spaces
// and line breaks a paste brings along. An empty value removes it.
func (s Set) With(k Kind, value string) Set {
	value = strings.TrimSpace(value)
	switch k {
	case PagerDutyKey:
		s.PagerDutyKey = NewSecret(value)
	case AtlassianEmail:
		s.AtlassianEmail = NewSensitive(value)
	case AtlassianToken:
		s.AtlassianToken = NewSecret(value)
	case OrgAPIKey:
		s.OrgAPIKey = NewSecret(value)
	}
	return s
}

// Merge returns s with every credential that o holds replacing its own.
func (s Set) Merge(o Set) Set {
	for _, k := range Kinds {
		if o.Has(k) {
			s = s.copyFrom(o, k)
		}
	}
	return s
}

// Only returns the set with just the given kinds.
func (s Set) Only(kinds ...Kind) Set {
	var out Set
	for _, k := range kinds {
		out = out.copyFrom(s, k)
	}
	return out
}

func (s Set) copyFrom(o Set, k Kind) Set {
	switch k {
	case PagerDutyKey:
		s.PagerDutyKey = o.PagerDutyKey
	case AtlassianEmail:
		s.AtlassianEmail = o.AtlassianEmail
	case AtlassianToken:
		s.AtlassianToken = o.AtlassianToken
	case OrgAPIKey:
		s.OrgAPIKey = o.OrgAPIKey
	}
	return s
}

func (s Set) value(k Kind) string {
	switch k {
	case PagerDutyKey:
		return s.PagerDutyKey.Reveal()
	case AtlassianEmail:
		return s.AtlassianEmail.Value()
	case AtlassianToken:
		return s.AtlassianToken.Reveal()
	case OrgAPIKey:
		return s.OrgAPIKey.Reveal()
	}
	return ""
}

// FromEnv returns the credentials set in the environment, read with getenv,
// and which kinds came from it.
func FromEnv(getenv func(string) string) (Set, []Kind) {
	var s Set
	var found []Kind
	for _, k := range Kinds {
		if v := strings.TrimSpace(getenv(k.EnvVar())); v != "" {
			s = s.With(k, v)
			found = append(found, k)
		}
	}
	return s, found
}

// maxInput bounds what ParseInput reads; four credentials fit many times over.
const maxInput = 64 << 10

// ParseInput reads credentials as NAME=value lines, with the names FromEnv
// uses, for example from a secrets manager piped into login --stdin. Blank
// lines, lines starting with '#' and an "export " prefix are ignored, and a
// value may be quoted. Errors name the line and the name, never the value.
func ParseInput(r io.Reader) (Set, error) {
	// Reading one byte past the limit tells a cut-off input from one that
	// fits, so a value is never silently truncated.
	input, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return Set{}, fmt.Errorf("reading the credentials: %w", err)
	}
	if len(input) > maxInput {
		return Set{}, fmt.Errorf("the input is larger than %d KiB", maxInput>>10)
	}
	byName := map[string]Kind{}
	for _, k := range Kinds {
		byName[k.EnvVar()] = k
	}
	var s Set
	seen := map[Kind]bool{}
	sc := bufio.NewScanner(bytes.NewReader(input))
	sc.Buffer(make([]byte, 0, 4096), maxInput)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return Set{}, fmt.Errorf("line %d: want NAME=value", n)
		}
		name = strings.TrimSpace(name)
		k, known := byName[name]
		switch {
		case !known && isEnvName(name):
			return Set{}, fmt.Errorf("line %d: unknown name %q: want one of %s", n, name, envVarList())
		case !known:
			// A bare secret can contain '=', as Atlassian API tokens do, so
			// what comes before it is never shown unless it looks like a name.
			return Set{}, fmt.Errorf("line %d: want NAME=value, with NAME one of %s", n, envVarList())
		}
		if seen[k] {
			return Set{}, fmt.Errorf("line %d: %s is given twice", n, name)
		}
		seen[k] = true
		s = s.With(k, unquote(strings.TrimSpace(value)))
	}
	if err := sc.Err(); err != nil {
		return Set{}, fmt.Errorf("reading the credentials: %w", err)
	}
	return s, nil
}

// isEnvName reports whether s looks like an environment variable name:
// uppercase letters, digits and underscores, starting with a letter.
func isEnvName(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c == '_' && i > 0, c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func envVarList() string {
	names := make([]string, len(Kinds))
	for i, k := range Kinds {
		names[i] = k.EnvVar()
	}
	return strings.Join(names, ", ")
}

// Equal reports whether two secrets hold the same value, in constant time.
func (s Secret) Equal(o Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.Reveal()), []byte(o.Reveal())) == 1
}

// MinPassphraseLen is the shortest passphrase accepted for a new workspace.
const MinPassphraseLen = 12

// ErrPassphraseTooShort is returned for a new passphrase under
// MinPassphraseLen characters.
var ErrPassphraseTooShort = fmt.Errorf("the passphrase needs at least %d characters", MinPassphraseLen)

// CheckNewPassphrase returns an error if p is too weak to protect a workspace.
func CheckNewPassphrase(p Secret) error {
	if len([]rune(p.Reveal())) < MinPassphraseLen {
		return ErrPassphraseTooShort
	}
	return nil
}
