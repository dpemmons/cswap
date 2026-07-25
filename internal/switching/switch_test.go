package switching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// asMap asserts a Switch return value is a JSON payload map.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected payload map, got %T (%v)", v, v)
	}
	return m
}

// refField returns the integer number in a from/to ref, or -1 for null.
func refField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	ref, ok := m[key].(map[string]any)
	if !ok || ref == nil {
		return -1
	}
	switch n := ref["number"].(type) {
	case *int:
		if n == nil {
			return -1
		}
		return *n
	case int:
		return n
	}
	return -1
}

// TestSwitchRotationJSON: a plain rotation with a live login advances to the
// next slot and reports switched:true.
func TestSwitchRotationJSON(t *testing.T) {
	s := newTestStore(t, nil)
	ca := oauthCreds("acc-a", "ref-a")
	cb := oauthCreds("acc-b", "ref-b")
	recs := map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
	}
	writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, recs))
	seedBackup(t, s, "1", "a@x.com", ca, "")
	seedBackup(t, s, "2", "b@x.com", cb, "")
	seedLive(t, s, "a@x.com", "", ca) // live == account 1's backup (own-bytes)

	out, err := Switch(s, nil, true, nil, nil)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	m := asMap(t, out)
	if m["switched"] != true {
		t.Fatalf("switched = %v, want true", m["switched"])
	}
	if m["reason"] != "switched" || m["strategy"] != "rotation" {
		t.Fatalf("reason/strategy = %v/%v", m["reason"], m["strategy"])
	}
	if refField(t, m, "from") != 1 || refField(t, m, "to") != 2 {
		t.Fatalf("from/to = %d/%d, want 1/2", refField(t, m, "from"), refField(t, m, "to"))
	}
	if got := readActiveCreds(t, s); got != cb {
		t.Fatalf("active credential not switched to account 2's backup")
	}
	data, _ := s.ReadSequence()
	if data.ActiveAccountNumber == nil || *data.ActiveAccountNumber != 2 {
		t.Fatalf("activeAccountNumber = %v, want 2", data.ActiveAccountNumber)
	}
	// DESIGN A4: the exact switch-history INFO line is a hard interop contract
	// (the TUI history reader parses it).
	logBytes, err := os.ReadFile(filepath.Join(s.BackupDir(), "claude-swap.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), "Switched from account 1 to 2") {
		t.Fatalf("missing A4 switch-history line in log:\n%s", logBytes)
	}
}

// TestSwitchNextAvailableAnchorDrift: with the live login drifted off the
// recorded active slot, plain rotation self-no-ops while next-available anchors
// on the live slot and moves forward.
func TestSwitchNextAvailableAnchorDrift(t *testing.T) {
	setup := func(t *testing.T) *store.Store {
		s := newTestStore(t, nil)
		ca := oauthCreds("acc-a", "ref-a")
		cb := oauthCreds("acc-b", "ref-b")
		cc := oauthCreds("acc-c", "ref-c")
		recs := map[string]json.RawMessage{
			"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
			"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
			"3": record(map[string]any{"email": "c@x.com", "organizationUuid": ""}),
		}
		writeSeq(t, s, seqData(ptrInt(1), []int{1, 2, 3}, recs)) // recorded active = 1
		seedBackup(t, s, "1", "a@x.com", ca, "")
		seedBackup(t, s, "2", "b@x.com", cb, "")
		seedBackup(t, s, "3", "c@x.com", cc, "")
		seedLive(t, s, "b@x.com", "", cb) // live login is account 2 (drift)
		return s
	}

	// Plain rotation anchors on recorded active=1 → lands on the live slot 2 →
	// self-switch no-op.
	t.Run("plain rotation self-no-ops on the drifted slot", func(t *testing.T) {
		s := setup(t)
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if m["switched"] != false || m["reason"] != "already-active" {
			t.Fatalf("got switched=%v reason=%v, want false/already-active", m["switched"], m["reason"])
		}
		if refField(t, m, "to") != 2 {
			t.Fatalf("to = %d, want 2", refField(t, m, "to"))
		}
	})

	// next-available anchors on the live slot 2 → advances to 3.
	t.Run("next-available anchors on the live slot", func(t *testing.T) {
		s := setup(t)
		strat := "next-available"
		out, err := Switch(s, &strat, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if m["switched"] != true || m["reason"] != "switched" {
			t.Fatalf("got switched=%v reason=%v, want true/switched", m["switched"], m["reason"])
		}
		if refField(t, m, "from") != 2 || refField(t, m, "to") != 3 {
			t.Fatalf("from/to = %d/%d, want 2/3", refField(t, m, "from"), refField(t, m, "to"))
		}
	})
}

// TestSwitchNoopReasons covers the structured JSON no-ops that never mutate.
func TestSwitchNoopReasons(t *testing.T) {
	t.Run("only-one-account", func(t *testing.T) {
		s := newTestStore(t, nil)
		ca := oauthCreds("acc-a", "ref-a")
		writeSeq(t, s, seqData(ptrInt(1), []int{1}, map[string]json.RawMessage{
			"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		}))
		seedBackup(t, s, "1", "a@x.com", ca, "")
		seedLive(t, s, "a@x.com", "", ca)
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if m["switched"] != false || m["reason"] != "only-one-account" {
			t.Fatalf("got %v/%v", m["switched"], m["reason"])
		}
		assertFromEqualsTo(t, m)
	})

	t.Run("unmanaged-account", func(t *testing.T) {
		s := newTestStore(t, nil)
		ca := oauthCreds("acc-a", "ref-a")
		cb := oauthCreds("acc-b", "ref-b")
		writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, map[string]json.RawMessage{
			"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
			"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
		}))
		seedBackup(t, s, "1", "a@x.com", ca, "")
		seedBackup(t, s, "2", "b@x.com", cb, "")
		seedLive(t, s, "stranger@x.com", "", oauthCreds("s", "s")) // not managed
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if m["reason"] != "unmanaged-account" {
			t.Fatalf("reason = %v", m["reason"])
		}
		assertFromEqualsTo(t, m)
	})

	t.Run("no-valid-target when every other slot is broken", func(t *testing.T) {
		s := newTestStore(t, nil)
		ca := oauthCreds("acc-a", "ref-a")
		writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, map[string]json.RawMessage{
			"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
			"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
		}))
		seedBackup(t, s, "1", "a@x.com", ca, "")
		// slot 2 has no backups ⇒ not switchable.
		seedLive(t, s, "a@x.com", "", ca)
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if m["reason"] != "no-valid-target" {
			t.Fatalf("reason = %v", m["reason"])
		}
		assertFromEqualsTo(t, m)
	})
}

// TestSwitchFreshMachineFallbackIsRotationEligible covers the fresh-machine path
// (no live login), where BOTH decisions — is the preferred slot usable, and which
// slot replaces it — are store.RotationEligible's, the sole owner of "may
// automatic selection pick this slot" (DESIGN A18). DESIGN A19's inline exception
// belongs to the rotation loop in Switch alone, which decides on its own tests;
// this path only WORDS its notice from the two halves, and the fallback scan does
// not even do that. What is pinned here is the observable behaviour that must not
// shift: which slot is chosen, and that the one warning names the skipped
// preferred slot with the reason it was skipped for.
func TestSwitchFreshMachineFallbackIsRotationEligible(t *testing.T) {
	// setup seeds a no-live-login store whose preferred slot 1 is disabled, plus
	// slots 2..4 configured per the case. Every seeded slot gets backups unless
	// noBackup says otherwise, so "not eligible" comes only from the flag under test.
	setup := func(t *testing.T, disabled map[string]bool, noBackup map[string]bool) *store.Store {
		s := newTestStore(t, nil)
		recs := map[string]json.RawMessage{}
		for _, num := range []string{"1", "2", "3", "4"} {
			email := "acc" + num + "@x.com"
			fields := map[string]any{"email": email, "organizationUuid": ""}
			if disabled[num] {
				fields["disabled"] = true
			}
			recs[num] = record(fields)
			if !noBackup[num] {
				seedBackup(t, s, num, email, oauthCreds("acc-"+num, "ref-"+num), "")
			}
		}
		writeSeq(t, s, seqData(ptrInt(1), []int{1, 2, 3, 4}, recs))
		return s // no ~/.claude.json ⇒ fresh machine
	}

	warningsOf := func(t *testing.T, m map[string]any) []string {
		t.Helper()
		w, _ := m["warnings"].([]string)
		return w
	}

	t.Run("skips a disabled candidate silently and lands on the first eligible slot", func(t *testing.T) {
		s := setup(t, map[string]bool{"1": true, "2": true}, nil)
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if refField(t, m, "to") != 3 {
			t.Fatalf("landed on %d, want 3 (2 is disabled)", refField(t, m, "to"))
		}
		w := warningsOf(t, m)
		if len(w) != 1 || w[0] != "Skipped Account-1 (disabled)" {
			t.Fatalf("warnings = %v, want exactly the skipped-preferred-slot line", w)
		}
	})

	// The preferred slot is skipped by one rule and reported by two: the decision
	// is the owner's, the wording still separates "you disabled this" from "this
	// slot has no backups", which are different things for the user to do next.
	t.Run("the preferred slot's skip reason survives the shared rule", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			disabled    map[string]bool
			noBackup    map[string]bool
			wantWarning string
		}{
			{
				name:        "disabled",
				disabled:    map[string]bool{"1": true},
				wantWarning: "Skipped Account-1 (disabled)",
			},
			{
				name:        "no backups",
				noBackup:    map[string]bool{"1": true},
				wantWarning: "Skipped Account-1 (no stored credentials/config)",
			},
			{
				name:        "disabled wins when both are true",
				disabled:    map[string]bool{"1": true},
				noBackup:    map[string]bool{"1": true},
				wantWarning: "Skipped Account-1 (disabled)",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := setup(t, tc.disabled, tc.noBackup)
				out, err := Switch(s, nil, true, nil, nil)
				if err != nil {
					t.Fatalf("Switch: %v", err)
				}
				m := asMap(t, out)
				if refField(t, m, "to") != 2 {
					t.Fatalf("landed on %d, want 2", refField(t, m, "to"))
				}
				w := warningsOf(t, m)
				if len(w) != 1 || w[0] != tc.wantWarning {
					t.Fatalf("warnings = %v, want exactly [%q]", w, tc.wantWarning)
				}
			})
		}
	})

	t.Run("skips an unswitchable candidate silently", func(t *testing.T) {
		s := setup(t, map[string]bool{"1": true}, map[string]bool{"2": true})
		out, err := Switch(s, nil, true, nil, nil)
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		m := asMap(t, out)
		if refField(t, m, "to") != 3 {
			t.Fatalf("landed on %d, want 3 (2 has no backups)", refField(t, m, "to"))
		}
		if w := warningsOf(t, m); len(w) != 1 {
			t.Fatalf("warnings = %v, want only the skipped-preferred-slot line", w)
		}
	})

	t.Run("every other slot disabled leaves no rotation target", func(t *testing.T) {
		s := setup(t, map[string]bool{"1": true, "2": true, "3": true, "4": true}, nil)
		_, err := Switch(s, nil, true, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "No accounts remain in rotation") {
			t.Fatalf("err = %v, want the re-enable guidance", err)
		}
	})
}

// TestSwitchNoAccounts: no sequence.json ⇒ ConfigError.
func TestSwitchNoAccounts(t *testing.T) {
	s := newTestStore(t, nil)
	_, err := Switch(s, nil, true, nil, nil)
	if err == nil || err.Error() != "No accounts are managed yet" {
		t.Fatalf("err = %v, want ConfigError(No accounts are managed yet)", err)
	}
}

// assertFromEqualsTo checks the every-switched:false-payload-has-from==to
// invariant (spec 02§17).
func assertFromEqualsTo(t *testing.T, m map[string]any) {
	t.Helper()
	if m["switched"] != false {
		return
	}
	from, _ := m["from"].(map[string]any)
	to, _ := m["to"].(map[string]any)
	if !refEqual(from, to) {
		t.Fatalf("switched:false payload has from != to: from=%v to=%v", from, to)
	}
}
