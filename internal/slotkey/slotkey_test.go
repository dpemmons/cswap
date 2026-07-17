package slotkey

import "testing"

// TestSortedTotalOrder pins the canonical order: numerics first by value
// (3, 15), then non-numerics lexicographically (2abc). Every permutation must
// converge to the same result — the old numeric-or-lexicographic comparator was
// intransitive on this set (3<15, 2abc<3, 15<2abc), a cycle sort.Slice may
// resolve nondeterministically.
func TestSortedTotalOrder(t *testing.T) {
	want := []string{"3", "15", "2abc"}
	for _, in := range [][]string{
		{"15", "3", "2abc"},
		{"2abc", "15", "3"},
		{"3", "2abc", "15"},
		{"2abc", "3", "15"},
		{"15", "2abc", "3"},
		{"3", "15", "2abc"},
	} {
		got := Sorted(in)
		if len(got) != len(want) {
			t.Fatalf("Sorted(%v) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Sorted(%v) = %v, want %v", in, got, want)
			}
		}
	}
}

// TestIntransitivityCycle exercises the exact three-element cycle that a naive
// comparator produced, asserting Less is a consistent strict weak order: for the
// three keys, exactly one direction holds per pair and the ordering is
// transitive (num < num by value, all numerics before non-numerics).
func TestIntransitivityCycle(t *testing.T) {
	// Under a total order: "3" < "15" (value), "3" < "2abc" and "15" < "2abc"
	// (numerics before non-numerics). The old code had "2abc" < "3" and
	// "15" < "2abc", forming a cycle; assert those are gone.
	if !Less("3", "15") {
		t.Errorf(`Less("3","15") = false, want true`)
	}
	if Less("15", "3") {
		t.Errorf(`Less("15","3") = true, want false`)
	}
	if !Less("3", "2abc") {
		t.Errorf(`Less("3","2abc") = false, want true`)
	}
	if !Less("15", "2abc") {
		t.Errorf(`Less("15","2abc") = false, want true`)
	}
	// No pair may be less-than in both directions (asymmetry).
	for _, p := range [][2]string{{"3", "15"}, {"3", "2abc"}, {"15", "2abc"}} {
		if Less(p[0], p[1]) && Less(p[1], p[0]) {
			t.Errorf("Less not asymmetric for %v", p)
		}
	}
}

// TestSortedDoesNotMutate confirms the input slice is left untouched.
func TestSortedDoesNotMutate(t *testing.T) {
	in := []string{"15", "3", "2abc"}
	orig := append([]string(nil), in...)
	_ = Sorted(in)
	for i := range orig {
		if in[i] != orig[i] {
			t.Fatalf("Sorted mutated input: %v, want %v", in, orig)
		}
	}
}

// TestLessEqualNumericsTieBreak pins the lexicographic tie-break for numerics
// that compare equal by value (e.g. "07" and "7").
func TestLessEqualNumericsTieBreak(t *testing.T) {
	if !Less("07", "7") {
		t.Errorf(`Less("07","7") = false, want true (equal value, lexical tie-break)`)
	}
	if Less("7", "07") {
		t.Errorf(`Less("7","07") = true, want false`)
	}
}
