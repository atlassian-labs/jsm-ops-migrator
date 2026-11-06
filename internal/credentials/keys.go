package credentials

import "strings"

// secretKeys are field, header and parameter names whose values are
// credentials. http_cal_url and web_cal_url are PagerDuty calendar feed URLs,
// which are bearer credentials.
var secretKeys = map[string]bool{
	"authorization":       true,
	"proxy_authorization": true,
	"cookie":              true,
	"set_cookie":          true,
	"token":               true,
	"api_token":           true,
	"api_key":             true,
	"apikey":              true,
	"x_api_key":           true,
	"access_token":        true,
	"refresh_token":       true,
	"client_secret":       true,
	"password":            true,
	"passphrase":          true,
	"secret":              true,
	"routing_key":         true,
	"integration_key":     true,
	"http_cal_url":        true,
	"web_cal_url":         true,
}

// Suffixes catch names such as atlassian_api_token, org_api_key or
// org_secret. "_key" alone is not one of them, because names such as item_key
// are ordinary identifiers.
var secretSuffixes = []string{"_token", "_secret", "_password", "_passphrase", "_api_key"}

// personalKeys hold personal contact data: a PagerDuty contact method's
// address, a JSM contact's to, phone numbers and device details.
var personalKeys = map[string]bool{
	"phone":        true,
	"phone_number": true,
	"address":      true,
	"to":           true,
	"device":       true,
	"device_id":    true,
}

func normalizeKey(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
}

// IsSecretKey reports whether a field, header or parameter name holds a
// credential. The match ignores case and treats '-' and '_' alike.
func IsSecretKey(key string) bool {
	k := normalizeKey(key)
	if secretKeys[k] {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// IsSensitiveKey reports whether a name holds a credential or personal contact
// data. Logs and review files redact the values of every such key.
func IsSensitiveKey(key string) bool {
	return IsSecretKey(key) || personalKeys[normalizeKey(key)]
}
