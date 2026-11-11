// Package decode holds the tolerant decoding the API clients use for fields
// whose shape varies, so an unexpected value becomes a reported item instead
// of a failed read.
package decode

import (
	"encoding/json"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
)

// Unknown is a value that no decoder accepted. It holds the value redacted,
// so it can be reported and stored.
type Unknown struct{ JSON json.RawMessage }

// OneOf tries each decoder on raw, in order, and returns the index of the
// first one that accepts it. When none does, it returns -1 and the redacted
// value.
func OneOf(raw json.RawMessage, decoders ...func(json.RawMessage) error) (int, *Unknown) {
	for i, d := range decoders {
		if d(raw) == nil {
			return i, nil
		}
	}
	return -1, &Unknown{JSON: credentials.RedactJSON(raw)}
}
