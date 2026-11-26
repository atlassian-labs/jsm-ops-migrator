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
	rows := parse(t)
	rows[1], rows[2] = rows[2], rows[1]
	rows[1][9] = "skip"
	got, err := check(t, rows)
	want := []MappingDecision{{"P1", "accept", "account"}, {"P2", "skip", ""}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name string
		edit func([][]string) [][]string
	}{
		{"missing row", func(r [][]string) [][]string { return r[:2] }},
		{"duplicate row", func(r [][]string) [][]string { return append(r, r[1]) }},
		{"unknown ID", func(r [][]string) [][]string { r[1][0] = "other"; return r }},
		{"changed account", func(r [][]string) [][]string { r[1][3] = "other"; return r }},
		{"wrong version", func(r [][]string) [][]string { r[1][7] = "2"; return r }},
		{"wrong hash", func(r [][]string) [][]string { r[1][8] = "other"; return r }},
		{"missing decision", func(r [][]string) [][]string { r[1] = r[1][:9]; return r }},
		{"invalid decision", func(r [][]string) [][]string { r[1][9] = "approve"; return r }},
		{"accepted ambiguity", func(r [][]string) [][]string { r[2][9] = "accept"; return r }},
		{"old header", func(r [][]string) [][]string { r[0] = r[0][:7]; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := check(t, tc.edit(parse(t))); err == nil || got != nil {
				t.Fatalf("accepted invalid review: %+v", got)
			}
		})
	}
	if _, err := ImportMappings(strings.NewReader(""), p, hash); err == nil {
		t.Fatal("accepted empty file")
	}
}
