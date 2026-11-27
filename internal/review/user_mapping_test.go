package review

import (
	"bytes"
	"encoding/csv"
	"slices"
	"strings"
	"testing"
)

func TestImportReviewedMappings(t *testing.T) {
	p := Plan{Version: 3, Items: []Item{
		{PagerDuty: &Ref{ID: "P1"}, Summary: "Exact, active match", UserMapping: &UserMapping{AccountID: "account", Decision: "map_existing"}},
		{PagerDuty: &Ref{ID: "P2"}, UserMapping: &UserMapping{Decision: "review_ambiguous"}},
	}}
	const hash = "sha256:example"
	export, err := proposedMappings(p, hash)
	if err != nil {
		t.Fatal(err)
	}
	parse := func(t *testing.T) [][]string {
		t.Helper()
		rows, err := csv.NewReader(bytes.NewReader(export)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	check := func(t *testing.T, rows [][]string) ([]MappingDecision, error) {
		t.Helper()
		var b bytes.Buffer
		if err := csv.NewWriter(&b).WriteAll(rows); err != nil {
			t.Fatal(err)
		}
		return ImportMappings(&b, p, hash)
	}
	if _, err := ImportMappings(bytes.NewReader(export), p, hash); err == nil {
		t.Fatal("unedited export approved")
	}
	reviewed := func(t *testing.T) [][]string {
		rows := parse(t)
		rows[2][7], rows[3][7] = "  AcCePt ", "defer"
		return rows
	}
	rows := reviewed(t)
	rows[2], rows[3] = rows[3], rows[2]
	rows[2][7] = "skip"
	got, err := check(t, rows)
	want := []MappingDecision{{"P1", "accept", "account"}, {"P2", "skip", ""}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, sep := range []rune{',', ';'} {
		var b bytes.Buffer
		b.WriteString("\uFEFF")
		w := csv.NewWriter(&b)
		w.Comma, w.UseCRLF = sep, true
		if err := w.WriteAll(append(rows, make([]string, len(mappingColumns)))); err != nil {
			t.Fatal(err)
		}
		got, err := ImportMappings(&b, p, hash)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("spreadsheet import (%c): %v, %v", sep, got, err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func([][]string) [][]string
	}{
		{"missing metadata", func(r [][]string) [][]string { return r[1:] }},
		{"duplicate metadata", func(r [][]string) [][]string { return append(r, r[0]) }},
		{"unknown metadata", func(r [][]string) [][]string { r[0][0] = "other"; return r }},
		{"missing hash", func(r [][]string) [][]string { r[0][1] = ""; return r }},
		{"wrong hash", func(r [][]string) [][]string { r[0][1] = "other"; return r }},
		{"extra metadata", func(r [][]string) [][]string { r[0][2] = "other"; return r }},
		{"changed header", func(r [][]string) [][]string { r[1][0] = "other"; return r }},
		{"missing row", func(r [][]string) [][]string { return r[:3] }},
		{"duplicate row", func(r [][]string) [][]string { return append(r, r[2]) }},
		{"unknown ID", func(r [][]string) [][]string { r[2][0] = "other"; return r }},
		{"changed account", func(r [][]string) [][]string { r[2][3] = "other"; return r }},
		{"missing decision", func(r [][]string) [][]string { r[2] = r[2][:7]; return r }},
		{"blank decision", func(r [][]string) [][]string { r[3][7] = ""; return r }},
		{"one placeholder left", func(r [][]string) [][]string { r[3][7] = parse(t)[3][7]; return r }},
		{"invalid decision", func(r [][]string) [][]string { r[2][7] = "approve"; return r }},
		{"accepted ambiguity", func(r [][]string) [][]string { r[3][7] = "accept"; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := check(t, tc.edit(reviewed(t))); err == nil || got != nil {
				t.Fatalf("accepted invalid review: %+v", got)
			}
		})
	}
	if _, err := ImportMappings(strings.NewReader(""), p, hash); err == nil {
		t.Fatal("accepted empty file")
	}
}
