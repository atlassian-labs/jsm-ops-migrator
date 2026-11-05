package credentials_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

const value = "canary-value-7c1d2e"

type exported struct{ Token credentials.Secret }
type unexported struct{ token credentials.Secret }
type withSensitive struct{ Phone credentials.Sensitive }

func renderings(t *testing.T, v any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p"} {
		out[verb] = fmt.Sprintf(verb, v)
	}
	out["Sprint"] = fmt.Sprint(v)
	out["Errorf"] = fmt.Errorf("wrapped: %v", v).Error()
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out["json"] = string(j)
	y, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out["yaml"] = string(y)
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("msg", "v", v)
	out["slog-text"] = buf.String()
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("msg", "v", v)
	out["slog-json"] = buf.String()
	return out
}

func TestSecretNeverPrintsItsValue(t *testing.T) {
	s := credentials.NewSecret(value)
	for name, v := range map[string]any{
		"secret":            s,
		"pointer":           &s,
		"exported field":    exported{Token: s},
		"unexported field":  unexported{token: s},
		"pointer to struct": &unexported{token: s},
		"slice":             []credentials.Secret{s},
		"map":               map[string]credentials.Secret{"k": s},
	} {
		for how, got := range renderings(t, v) {
			if strings.Contains(got, value) {
				t.Errorf("%s via %s leaked the value: %s", name, how, got)
			}
		}
	}
	if got := fmt.Sprint(s); got != credentials.Redacted {
		t.Errorf("Sprint = %q, want %q", got, credentials.Redacted)
	}
}

func TestSensitiveNeverPrintsItsValue(t *testing.T) {
	s := credentials.NewSensitive(value)
	for how, got := range renderings(t, withSensitive{Phone: s}) {
		if strings.Contains(got, value) {
			t.Errorf("via %s leaked the value: %s", how, got)
		}
	}
	if s.Value() != value || s.IsZero() {
		t.Errorf("Value() = %q", s.Value())
	}
}

func TestSecretReveal(t *testing.T) {
	if got := credentials.NewSecret(value).Reveal(); got != value {
		t.Errorf("Reveal() = %q", got)
	}
	var zero credentials.Secret
	if zero.Reveal() != "" || !zero.IsZero() {
		t.Error("the zero Secret must reveal an empty string")
	}
	if !credentials.NewSecret("").IsZero() {
		t.Error("an empty Secret must be zero")
	}
}

func TestKeyLists(t *testing.T) {
	for key, want := range map[string]bool{
		"Authorization": true, "proxy-authorization": true, "api_token": true, "API-Key": true,
		"routing_key": true, "http_cal_url": true, "web_cal_url": true, "atlassian_api_token": true,
		"passphrase": true, "item_key": false, "name": false, "email": false, "phone": false,
	} {
		if got := credentials.IsSecretKey(key); got != want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]bool{
		"phone": true, "phone_number": true, "address": true, "to": true, "device_id": true,
		"token": true, "name": false, "summary": false,
	} {
		if got := credentials.IsSensitiveKey(key); got != want {
			t.Errorf("IsSensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestOSKeychainRoundTrip(t *testing.T) {
	if testingRealKeychain() {
		kc := credentials.OSKeychain()
		account := "test-" + value
		if err := kc.Set(credentials.KeychainService, account, credentials.NewSecret(value)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kc.Delete(credentials.KeychainService, account) })
		got, err := kc.Get(credentials.KeychainService, account)
		if err != nil || got.Reveal() != value {
			t.Fatalf("Get = %v, %v", got, err)
		}
		if err := kc.Delete(credentials.KeychainService, account); err != nil {
			t.Fatal(err)
		}
		if _, err := kc.Get(credentials.KeychainService, account); !errors.Is(err, credentials.ErrKeychainItemNotFound) {
			t.Fatalf("Get after Delete = %v, want ErrKeychainItemNotFound", err)
		}
	} else {
		t.Skip("set JSM_OPS_MIGRATOR_TEST_REAL_KEYCHAIN=1 to use the OS keychain")
	}
}
