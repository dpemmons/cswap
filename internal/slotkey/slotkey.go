// Package slotkey defines the canonical total order over slot-key strings used
// wherever cswap iterates an account map deterministically (Python relied on
// dict insertion order). It is a leaf package with no internal dependencies.
//
// The order is: numeric keys first, compared by integer value, with equal
// values tie-broken lexicographically for stability; then non-numeric keys
// lexicographically. A total order is required — a mixed set like
// "15"/"3"/"2abc" produces an intransitive comparator (3<15, 2abc<3, 15<2abc)
// under a naive numeric-or-lexicographic branch, which sort.Slice may resolve
// nondeterministically.
package slotkey

import (
	"sort"
	"strconv"
)

// Less reports whether slot-key a sorts before b under the canonical total
// order (numerics first by value, equal numerics tie-broken lexicographically,
// then non-numerics lexicographically).
func Less(a, b string) bool {
	na, aerr := strconv.Atoi(a)
	nb, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		if na != nb {
			return na < nb
		}
		return a < b
	case aerr == nil: // a numeric, b not: numerics first
		return true
	case berr == nil: // b numeric, a not: numerics first
		return false
	default:
		return a < b
	}
}

// Sorted returns a new slice containing keys ordered by Less. The input is not
// modified.
func Sorted(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool { return Less(out[i], out[j]) })
	return out
}
