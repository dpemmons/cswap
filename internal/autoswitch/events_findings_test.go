package autoswitch

import (
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
)

// TestFormatRecoveryISORoundsMicroseconds pins Finding 15: formatRecoveryISO
// must round the sub-second remainder to the nearest microsecond, matching
// Python's datetime.fromtimestamp(ts, utc).isoformat(). The old code truncated
// epoch*1e9 to nanoseconds and then to microseconds, under-reporting by ~1µs.
//
// Ground truth (python3):
//
//	>>> from datetime import datetime, timezone
//	>>> datetime.fromtimestamp(1700000000.8474338, timezone.utc).isoformat().replace('+00:00','Z')
//	'2023-11-14T22:13:20.847434Z'
//
// ns-truncation would yield ...847433Z (off by one µs); µs-rounding yields ...847434Z.
func TestFormatRecoveryISORoundsMicroseconds(t *testing.T) {
	if got, want := formatRecoveryISO(1700000000.8474338), "2023-11-14T22:13:20.847434Z"; got != want {
		t.Errorf("formatRecoveryISO = %q, want %q", got, want)
	}
	// Whole-second epoch keeps the seconds-only form.
	if got, want := formatRecoveryISO(1700000000.0), "2023-11-14T22:13:20Z"; got != want {
		t.Errorf("formatRecoveryISO(whole) = %q, want %q", got, want)
	}
}

// TestSwitchHumanRealRefShape pins Finding 5: a real (non-dry-run) switch's
// from/to refs come from jsonout.AccountRef, whose "number" is a *int. Human()
// must render the slot number, not the pointer address. A nil *int (unmanaged
// live account) renders "None" to match Python's f"Account-{None}".
func TestSwitchHumanRealRefShape(t *testing.T) {
	one, three := 1, 3
	sw := SwitchEvent{
		Ts:      "2026-07-17T12:00:00Z",
		Trigger: "proactive",
		FromRef: jsonout.AccountRef(&one, "a@x"),
		ToRef:   jsonout.AccountRef(&three, "c@x"),
	}
	if got, want := sw.Human(), "Switched Account-1 -> Account-3 (c@x) (proactive)"; got != want {
		t.Errorf("Human() = %q, want %q", got, want)
	}

	// nil *int number -> "None" (Python f-string of None), never a pointer addr.
	nilRef := SwitchEvent{
		Ts:      "2026-07-17T12:00:00Z",
		Trigger: "failover",
		FromRef: jsonout.AccountRef(nil, "live@x"),
		ToRef:   jsonout.AccountRef(&one, "a@x"),
	}
	if got, want := nilRef.Human(), "Switched Account-None -> Account-1 (a@x) (failover)"; got != want {
		t.Errorf("Human() with nil number = %q, want %q", got, want)
	}
}

// TestSortNumericTotalOrder pins Finding 11: sortNumeric must impose a total
// order so a mixed set of numeric and non-numeric slot keys sorts
// deterministically. The old comparator was intransitive on "15"/"3"/"2abc"
// (3<15, 2abc<3, 15<2abc), a cycle sort.Slice may resolve nondeterministically.
// Expected order: numerics first by value (3, 15), then non-numerics
// lexicographically (2abc).
func TestSortNumericTotalOrder(t *testing.T) {
	want := []string{"3", "15", "2abc"}
	for _, in := range [][]string{
		{"15", "3", "2abc"},
		{"2abc", "15", "3"},
		{"3", "2abc", "15"},
		{"2abc", "3", "15"},
		{"15", "2abc", "3"},
		{"3", "15", "2abc"},
	} {
		got := sortNumeric(in)
		if len(got) != len(want) {
			t.Fatalf("sortNumeric(%v) = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("sortNumeric(%v) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
