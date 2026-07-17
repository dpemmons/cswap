package store

import (
	"encoding/json"
	"testing"
)

// TestSortedSlotKeysTotalOrder pins Finding 11 for the store: sortedSlotKeys
// must impose a total order so a mixed set of numeric and non-numeric account
// keys sorts deterministically. The old comparator was intransitive on
// "15"/"3"/"2abc" (3<15, 2abc<3, 15<2abc), a cycle sort.Slice may resolve
// nondeterministically. Expected: numerics first by value (3, 15), then
// non-numerics lexicographically (2abc). Map iteration order is randomized, so
// running it repeatedly exercises the comparator against shuffled inputs.
func TestSortedSlotKeysTotalOrder(t *testing.T) {
	want := []string{"3", "15", "2abc"}
	for iter := 0; iter < 50; iter++ {
		data := &SequenceData{Accounts: map[string]json.RawMessage{
			"15":   json.RawMessage(`{}`),
			"3":    json.RawMessage(`{}`),
			"2abc": json.RawMessage(`{}`),
		}}
		got := sortedSlotKeys(data)
		if len(got) != len(want) {
			t.Fatalf("sortedSlotKeys = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sortedSlotKeys = %v, want %v", got, want)
			}
		}
	}
}
