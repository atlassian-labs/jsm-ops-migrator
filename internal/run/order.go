package run

import (
	"container/heap"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/atlassian-labs/jsm-ops-migrator/internal/review"
)

// ErrOrder means the steps planned items that cannot be put in an order:
// a key planned twice, a dependency on an item no step planned, or items
// that depend on each other in a loop. It is a fault in a step, so plan
// stops with it.
var ErrOrder = errors.New("the planned items cannot be ordered")

// order sorts items so that each comes after every item it depends on
// (Kahn's algorithm). Among the items that are ready, the one given first
// goes first: items come in step order, then in the order the step returned
// them, so the same items always give the same plan.
//
// A dependency on an object an earlier run migrated is a dependency on the
// map item that links it, so a key no step planned is a fault, not
// something to skip.
func order(items []review.Item) ([]review.Item, error) {
	index := make(map[string]int, len(items))
	var errs []error
	for i, it := range items {
		if _, dup := index[it.Key]; dup {
			errs = append(errs, fmt.Errorf("item %s is planned twice", review.Quote(it.Key)))
			continue
		}
		index[it.Key] = i
	}
	waiting := make([]int, len(items))
	dependents := make([][]int, len(items))
	for i, it := range items {
		for _, d := range it.DependsOn {
			j, ok := index[d]
			if !ok {
				errs = append(errs, fmt.Errorf("item %s depends on %s, which no step planned", review.Quote(it.Key), review.Quote(d)))
				continue
			}
			waiting[i]++
			dependents[j] = append(dependents[j], i)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrOrder, errors.Join(errs...))
	}

	ready := &minHeap{}
	for i := range items {
		if waiting[i] == 0 {
			heap.Push(ready, i)
		}
	}
	out := make([]review.Item, 0, len(items))
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		out = append(out, items[i])
		for _, d := range dependents[i] {
			if waiting[d]--; waiting[d] == 0 {
				heap.Push(ready, d)
			}
		}
	}
	if len(out) < len(items) {
		return nil, fmt.Errorf("%w: items depend on each other in a loop: %s", ErrOrder, loop(items, index, waiting))
	}
	return out, nil
}

// loop names one loop among the items still waiting. Each of them waits for
// at least one other waiting item, so following those from the first one
// must come back to an item already passed.
func loop(items []review.Item, index map[string]int, waiting []int) string {
	start := slices.IndexFunc(waiting, func(n int) bool { return n > 0 })
	var path []int
	at := map[int]int{}
	for i := start; ; {
		if pos, seen := at[i]; seen {
			path = append(path[pos:], i)
			break
		}
		at[i] = len(path)
		path = append(path, i)
		for _, d := range items[i].DependsOn {
			if j := index[d]; waiting[j] > 0 {
				i = j
				break
			}
		}
	}
	keys := make([]string, len(path))
	for n, i := range path {
		keys[n] = review.Quote(items[i].Key)
	}
	return strings.Join(keys, " depends on ")
}

type minHeap []int

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *minHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
