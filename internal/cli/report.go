package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/credentials"
	"github.com/atlassian-labs/jsm-ops-migrator/internal/preflight"
)

var statusLabels = map[preflight.Status]string{
	preflight.OK:         "ok",
	preflight.Warning:    "warning",
	preflight.Failed:     "failed",
	preflight.Missing:    "missing",
	preflight.NotChecked: "not checked",
}

// writeChecks prints one line per check: its title, its status in words, so a
// screen reader reads it out, and the detail. A detail that several checks
// share is printed once, under the table, with the checks it is about.
func writeChecks(w io.Writer, heading string, checks []preflight.Check) error {
	titleWidth, statusWidth := 0, 0
	for _, c := range checks {
		titleWidth = max(titleWidth, len(c.Title))
		statusWidth = max(statusWidth, len(statusLabels[c.Status]))
	}
	shared := sharedDetails(checks)
	var b strings.Builder
	b.WriteString(heading + "\n")
	for _, c := range checks {
		detail := c.Detail
		if _, ok := shared[detail]; ok {
			detail = ""
		}
		line := fmt.Sprintf("  %-*s  %-*s  %s", titleWidth, c.Title, statusWidth, statusLabels[c.Status], detail)
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	for _, g := range groupByDetail(checks) {
		if _, ok := shared[g.detail]; ok {
			fmt.Fprintf(&b, "  %s: %s.\n", joinWords(g.titles), g.detail)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// sharedDetails are the details more than one check gives.
func sharedDetails(checks []preflight.Check) map[string]struct{} {
	shared := map[string]struct{}{}
	for _, g := range groupByDetail(checks) {
		if len(g.titles) > 1 {
			shared[g.detail] = struct{}{}
		}
	}
	return shared
}

type detailGroup struct {
	status preflight.Status
	detail string
	titles []string
}

// groupByDetail groups the checks with the same status and detail, in the
// order the checks ran.
func groupByDetail(checks []preflight.Check) []detailGroup {
	var groups []detailGroup
	for _, c := range checks {
		i := slices.IndexFunc(groups, func(g detailGroup) bool { return g.status == c.Status && g.detail == c.Detail })
		if i < 0 {
			groups = append(groups, detailGroup{status: c.Status, detail: c.Detail})
			i = len(groups) - 1
		}
		groups[i].titles = append(groups[i].titles, c.Title)
	}
	return groups
}

// checksSummary is the short form of a report the menu shows: what needs
// attention, one line per reason, or that everything passed.
func checksSummary(checks []preflight.Check) []string {
	var problems, warnings []string
	var missing []string
	for _, g := range groupByDetail(checks) {
		switch g.status {
		case preflight.Failed:
			problems = append(problems, fmt.Sprintf("Failed: %s (%s)", joinWords(g.titles), g.detail))
		case preflight.NotChecked:
			problems = append(problems, fmt.Sprintf("Not checked: %s (%s)", joinWords(g.titles), g.detail))
		case preflight.Missing:
			missing = append(missing, g.titles...)
		case preflight.Warning:
			warnings = append(warnings, fmt.Sprintf("Warning: %s (%s)", joinWords(g.titles), g.detail))
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "Missing: "+joinWords(missing))
	}
	if len(problems) == 0 {
		problems = []string{"All checks passed"}
	}
	return append(problems, warnings...)
}

// joinWords joins items as a sentence does: "a", "a and b", "a, b and c".
func joinWords(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeReport prints the pre-flight report, as text or JSON.
func writeReport(env *Env, heading string, report preflight.Report) error {
	if env.Output == OutputJSON {
		return writeJSON(env.Stdout, struct {
			Preflight preflight.Report `json:"preflight"`
		}{report})
	}
	return writeChecks(env.Stdout, heading, report.Checks)
}

type savedConfig struct {
	Site            string `json:"site,omitempty"`
	CloudID         string `json:"cloud_id,omitempty"`
	OrgID           string `json:"org_id,omitempty"`
	PagerDutyRegion string `json:"pagerduty_region,omitempty"`
}

// loginResult is what login reports. It names credentials, never their
// values.
type loginResult struct {
	Checks   []preflight.Check  `json:"checks"`
	Stored   []credentials.Kind `json:"stored"`
	StoredIn string             `json:"stored_in"`
	// Rejected are the credentials just given whose check failed, so they
	// were not stored.
	Rejected []credentials.Kind `json:"rejected,omitempty"`
	// Unchecked are the credentials just given whose check could not run, so
	// they were not stored either.
	Unchecked []credentials.Kind `json:"unchecked,omitempty"`
	// Config is what config.yaml holds after login, and NotSaved the site
	// and organization given whose check did not pass.
	Config   savedConfig `json:"config"`
	NotSaved savedConfig `json:"not_saved,omitzero"`

	// What this login stored, saved and removed, for the summaries.
	storedNow                        []credentials.Kind
	savedSite, savedOrg, savedRegion bool
	removed                          []string // "site", "organization"
}

func writeLoginResult(env *Env, res loginResult) error {
	if env.Output == OutputJSON {
		return writeJSON(env.Stdout, res)
	}
	var b strings.Builder
	if err := writeChecks(&b, "Login checks", res.Checks); err != nil {
		return err
	}
	b.WriteString("\n")
	switch {
	case len(res.Stored) > 0:
		fmt.Fprintf(&b, "Stored in %s: %s.\n", res.StoredIn, kindLabels(res.Stored))
	case len(res.Rejected) == 0 && len(res.Unchecked) == 0:
		fmt.Fprintf(&b, "No credentials are stored in %s.\n", res.StoredIn)
	}
	if len(res.Rejected) > 0 {
		fmt.Fprintf(&b, "Not stored, because the check failed: %s.\n", kindLabels(res.Rejected))
	}
	if len(res.Unchecked) > 0 {
		fmt.Fprintf(&b, "Not stored, because the check could not run yet: %s.\n", kindLabels(res.Unchecked))
	}
	if s := settingsText(res.Config); s != "" {
		fmt.Fprintf(&b, "Saved in %s: %s.\n", displayPath(env.Workspace.ConfigPath()), s)
	}
	if s := settingsText(res.NotSaved); s != "" {
		fmt.Fprintf(&b, "Not saved, because the check did not pass: %s.\n", s)
	}
	if s := removedText(res.removed); s != "" {
		fmt.Fprintf(&b, "%s.\n", s)
	}
	_, err := io.WriteString(env.Stdout, b.String())
	return err
}

func settingsText(c savedConfig) string {
	var settings []string
	for _, s := range []struct{ name, value string }{
		{"site", c.Site}, {"cloudId", c.CloudID}, {"organization", c.OrgID}, {"PagerDuty region", c.PagerDutyRegion},
	} {
		if s.value != "" {
			settings = append(settings, s.name+" "+s.value)
		}
	}
	return strings.Join(settings, ", ")
}

// loginSummary is the short form of the login result the menu shows.
func loginSummary(res loginResult) []string {
	lines := checksSummary(res.Checks)
	var parts []string
	if len(res.storedNow) > 0 {
		parts = append(parts, "stored the "+kindLabels(res.storedNow))
	}
	var saved []string
	for _, s := range []struct {
		name string
		ok   bool
	}{{"site", res.savedSite}, {"organization", res.savedOrg}, {"PagerDuty region", res.savedRegion}} {
		if s.ok {
			saved = append(saved, s.name)
		}
	}
	if len(saved) > 0 {
		parts = append(parts, "saved the "+joinWords(saved))
	}
	notSaved := settingsText(res.NotSaved)
	switch {
	case len(parts) > 0:
		lines = append(lines, capitalize(strings.Join(parts, "; ")))
	case notSaved == "":
		lines = append(lines, "Nothing new was stored or saved")
	default:
		lines = append(lines, "No credentials were stored")
	}
	if notSaved != "" {
		lines = append(lines, "Not saved, because the check did not pass: "+notSaved)
	}
	if s := removedText(res.removed); s != "" {
		lines = append(lines, s)
	}
	return lines
}

// removedText says which saved settings login removed, if any.
func removedText(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	pronoun := "it is"
	if len(removed) > 1 {
		pronoun = "they are"
	}
	return "Removed the " + joinWords(removed) + " saved before, so " + pronoun + " not used by mistake"
}

func kindLabels(kinds []credentials.Kind) string {
	labels := make([]string, len(kinds))
	for i, k := range kinds {
		labels[i] = k.Label()
	}
	return joinWords(labels)
}
