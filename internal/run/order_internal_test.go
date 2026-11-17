package run

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

func item(key string, deps ...string) review.Item {
	return review.Item{Key: key, Action: review.Create, DependsOn: deps}
}

func keys(items []review.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}

func TestOrderPutsDependenciesFirst(t *testing.T) {
	cases := map[string]struct {
		in   []review.Item
		want []string
	}{
		"already in order": {
			[]review.Item{item("users/U1"), item("teams/T1", "users/U1")},
			[]string{"users/U1", "teams/T1"},
		},
		"a dependency given later": {
			[]review.Item{item("teams/T1", "users/U1"), item("users/U1")},
			[]string{"users/U1", "teams/T1"},
		},
		// Of the items that are ready, the one given first goes first: T2
		// needs nothing and comes before U1 in the input, so it is taken
		// before U1, and T1 as soon as U1 is done.
		"ties go to the order given": {
			[]review.Item{item("teams/T1", "users/U1"), item("teams/T2"), item("users/U1"), item("users/U2")},
			[]string{"teams/T2", "users/U1", "teams/T1", "users/U2"},
		},
		"a chain and a diamond": {
			[]review.Item{
				item("teams/T1-members", "teams/T1-ops", "users/U1"),
				item("teams/T1-ops", "teams/T1"),
				item("teams/T1", "users/U1"),
				item("users/U1"),
			},
			[]string{"users/U1", "teams/T1", "teams/T1-ops", "teams/T1-members"},
		},
		"nothing": {nil, []string{}},
	}
	for name, c := range cases {
		got, err := order(c.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(keys(got), c.want) {
			t.Errorf("%s: %v, want %v", name, keys(got), c.want)
		}
	}
}

// The same items always give the same order, so the same reads always give
// the same plan.json and the same hash.
func TestOrderIsTheSameEveryTime(t *testing.T) {
	var items []review.Item
	for i := range 200 {
		var deps []string
		for _, d := range []int{i / 2, i / 3, i - 7} {
			if d >= 0 && d < i && !slices.Contains(deps, fmt.Sprintf("s/%03d", d)) {
				deps = append(deps, fmt.Sprintf("s/%03d", d))
			}
		}
		items = append(items, item(fmt.Sprintf("s/%03d", i), deps...))
	}
	slices.Reverse(items)
	first, err := order(items)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := order(slices.Clone(items))
		if err != nil || !slices.Equal(keys(again), keys(first)) {
			t.Fatalf("the order changed: %v", err)
		}
	}
	at := map[string]int{}
	for i, it := range first {
		at[it.Key] = i
	}
	for _, it := range first {
		for _, d := range it.DependsOn {
			if at[d] > at[it.Key] {
				t.Fatalf("%s comes before %s, which it depends on", it.Key, d)
			}
		}
	}
}

func TestOrderRefusesWhatCannotBeOrdered(t *testing.T) {
	cases := map[string]struct {
		in   []review.Item
		want string
	}{
		"a loop": {
			[]review.Item{item("a/1", "b/1"), item("b/1", "c/1"), item("c/1", "a/1"), item("d/1")},
			`"a/1" depends on "b/1" depends on "c/1" depends on "a/1"`,
		},
		"an item that depends on itself": {[]review.Item{item("a/1", "a/1")}, `"a/1" depends on "a/1"`},
		"a loop behind a ready item": {
			[]review.Item{item("a/1"), item("b/1", "a/1", "c/1"), item("c/1", "b/1")},
			`"b/1" depends on "c/1" depends on "b/1"`,
		},
		"a key no step planned": {[]review.Item{item("a/1", "users/U9")}, `"a/1" depends on "users/U9", which no step planned`},
		"a key planned twice":   {[]review.Item{item("a/1"), item("a/1")}, `"a/1" is planned twice`},
	}
	for name, c := range cases {
		_, err := order(c.in)
		if !errors.Is(err, ErrOrder) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
