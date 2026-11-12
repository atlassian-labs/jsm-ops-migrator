package review

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/messages"
)

// ErrAcceptance means an acceptance file does not accept the plan it is
// given with: it is for another version, or it does not accept each warning
// exactly once, with a reason.
var ErrAcceptance = errors.New("the acceptance file does not accept this plan")

// Acceptance is the file a non-interactive apply reads instead of asking: the
// plan version it is for, and a reason for each of that version's warnings.
type Acceptance struct {
	PlanVersion int        `yaml:"plan_version"`
	PlanHash    string     `yaml:"plan_hash"`
	Warnings    []Accepted `yaml:"warnings"`
}

// Accepted is one warning accepted, with why.
type Accepted struct {
	ID     string `yaml:"id" json:"id"`
	Reason string `yaml:"reason" json:"reason"`
}

// ParseAcceptance reads an acceptance file. An unknown key is an error, so a
// typo never leaves a warning unaccepted without a word, and so is a second
// YAML document, which would otherwise be ignored.
func ParseAcceptance(data []byte) (Acceptance, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var a Acceptance
	if err := dec.Decode(&a); err != nil {
		if errors.Is(err, io.EOF) {
			return Acceptance{}, fmt.Errorf("%w: the file is empty", ErrAcceptance)
		}
		return Acceptance{}, fmt.Errorf("%w: %w", ErrAcceptance, err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return Acceptance{}, fmt.Errorf("%w: the file holds more than one YAML document", ErrAcceptance)
	}
	return a, nil
}

// maxListed keeps an error readable when a file misses thousands of warnings.
const maxListed = 10

// Accept checks a against the plan whose plan.json has the given hash, and
// returns the accepted warnings in the plan's order. Every warning must be
// accepted, once and with a reason, and nothing else: there is no wildcard,
// so each warning is a decision someone made.
func (a Acceptance) Accept(p Plan, hash string) ([]Accepted, error) {
	var problems []string
	if a.PlanVersion != p.Version {
		problems = append(problems, fmt.Sprintf("it is for plan v%d, not v%d", a.PlanVersion, p.Version))
	}
	if a.PlanHash != hash {
		problems = append(problems, "its plan_hash is not the hash of this plan's plan.json")
	}
	given := map[string]Accepted{}
	var unknown, twice, noReason []string
	warnings := map[string]bool{}
	for _, m := range p.Messages {
		if m.Class == messages.Warning {
			warnings[m.ID()] = true
		}
	}
	for _, w := range a.Warnings {
		if !warnings[w.ID] {
			unknown = append(unknown, w.ID)
			continue
		}
		if _, dup := given[w.ID]; dup {
			twice = append(twice, w.ID)
			continue
		}
		// A reason of control or invisible characters shows as nothing in the
		// confirmation record, so it is no reason.
		if messages.OneLine(w.Reason) == "" {
			noReason = append(noReason, w.ID)
		}
		given[w.ID] = w
	}
	var missing []string
	var out []Accepted
	for _, m := range p.Messages {
		if m.Class != messages.Warning {
			continue
		}
		if w, ok := given[m.ID()]; ok {
			out = append(out, w)
		} else {
			missing = append(missing, m.ID())
		}
	}
	for _, l := range []struct {
		what string
		ids  []string
	}{
		{"not accepted", missing},
		{"not a warning of this plan", unknown},
		{"accepted twice", twice},
		{"accepted without a reason", noReason},
	} {
		if len(l.ids) > 0 {
			problems = append(problems, l.what+": "+listIDs(l.ids))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrAcceptance, strings.Join(problems, "; "))
	}
	return out, nil
}

// listIDs names up to maxListed IDs, each quoted: an ID from the file can
// hold anything, including a control character that would reach the
// terminal with the error.
func listIDs(ids []string) string {
	shown := ids[:min(len(ids), maxListed)]
	quoted := make([]string, len(shown))
	for i, id := range shown {
		quoted[i] = quote(id)
	}
	out := strings.Join(quoted, ", ")
	if len(ids) > maxListed {
		out += fmt.Sprintf(" and %d more", len(ids)-maxListed)
	}
	return out
}

// AcceptanceTemplate is an acceptance file for p with every warning listed
// and every reason empty, for the operator to fill in. Each warning's text is
// a comment above it.
func AcceptanceTemplate(p Plan, hash string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Acceptance file for plan v%d. Give a reason for every warning, then run:\n", p.Version)
	b.WriteString("#   jsm-ops-migrator apply --accept-file <this file>\n")
	b.WriteString("# It accepts only this plan version: a new version needs a new file.\n")
	fmt.Fprintf(&b, "plan_version: %d\n", p.Version)
	fmt.Fprintf(&b, "plan_hash: %s\n", strconv.Quote(hash))
	var warnings []messages.Message
	for _, m := range p.Messages {
		if m.Class == messages.Warning {
			warnings = append(warnings, m)
		}
	}
	if len(warnings) == 0 {
		b.WriteString("warnings: []\n")
		return []byte(b.String())
	}
	b.WriteString("warnings:\n")
	for _, m := range warnings {
		fmt.Fprintf(&b, "  # %s\n", messages.Clean(m.Text))
		// A YAML double-quoted string takes Go's escapes, so any ID reads
		// back as it was.
		fmt.Fprintf(&b, "  - id: %s\n    reason: \"\"\n", strconv.Quote(m.ID()))
	}
	return []byte(b.String())
}
