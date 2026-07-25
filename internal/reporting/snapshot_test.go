// Tests for the one-pass AccountsSnapshot's rotation-eligibility derivation
// (spec 02§13 / DESIGN A18): Switchable and Disabled keep their distinct display
// meanings while RotationEligible carries store.RotationEligible's rule as one
// field for the UIs that rank automatic switch targets.
package reporting

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// TestSnapshot_RotationEligible walks the truth table: slot 1 switchable +
// enabled, slot 2 switchable + disabled, slot 3 non-switchable (no backups) +
// enabled, slot 4 non-switchable + disabled.
func TestSnapshot_RotationEligible(t *testing.T) {
	clk := testutil.FixedClock(t, fixedNow)
	s := newStore(t, clk, nil) // no client: nothing is fetched this pass

	writeSequenceRaw(t, s, `{
  "activeAccountNumber": null,
  "lastUpdated": "2026-07-17T08:00:00Z",
  "sequence": [1, 2, 3, 4],
  "accounts": {
    "1": {"email": "on@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x"},
    "2": {"email": "off@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x", "disabled": true},
    "3": {"email": "bare@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x"},
    "4": {"email": "bareoff@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x", "disabled": true}
  }
}`)
	writeBackup(t, s, "1", "on@example.com", oauthCreds("at1", "rt1", 0), `{"oauthAccount":{}}`)
	writeBackup(t, s, "2", "off@example.com", oauthCreds("at2", "rt2", 0), `{"oauthAccount":{}}`)

	byNum := map[string]AccountSnapshot{}
	for _, a := range Snapshot(s, nil).Accounts {
		byNum[a.Number] = a
	}
	for _, tc := range []struct {
		num                                   string
		switchable, disabled, wantRotEligible bool
	}{
		{"1", true, false, true},
		{"2", true, true, false},
		{"3", false, false, false},
		{"4", false, true, false},
	} {
		a, ok := byNum[tc.num]
		if !ok {
			t.Fatalf("slot %s missing from snapshot", tc.num)
		}
		if a.Switchable != tc.switchable || a.Disabled != tc.disabled {
			t.Errorf("slot %s Switchable=%v Disabled=%v, want %v/%v",
				tc.num, a.Switchable, a.Disabled, tc.switchable, tc.disabled)
		}
		if a.RotationEligible != tc.wantRotEligible {
			t.Errorf("slot %s RotationEligible=%v, want %v", tc.num, a.RotationEligible, tc.wantRotEligible)
		}
	}

	// The snapshot's eligible set is exactly the store's rotation set.
	var eligible []string
	for _, a := range Snapshot(s, nil).Accounts {
		if a.RotationEligible {
			eligible = append(eligible, a.Number)
		}
	}
	if want := s.SwitchableAccountNumbers(); !equalStrings(eligible, want) {
		t.Errorf("snapshot eligible=%v, want SwitchableAccountNumbers=%v", eligible, want)
	}
}

// TestSnapshot_RotationEligibleFailsClosedWithoutARoster is the half of the
// parity the truth table cannot reach. The snapshot holds rows from
// BuildAccountsInfo's read and reads the roster again to answer the disabled
// half, so it can reach the loop with rows and no roster; `disabled` then
// answers false for every slot on no evidence at all, and a rule written
// `switchable && !disabled` reports a slot the user disabled as rotation-
// eligible, for the TUI panel that exists to show only what the engine may pick.
// store.RotationEligible fails closed on exactly that input, and this copy of
// the rule must too.
func TestSnapshot_RotationEligibleFailsClosedWithoutARoster(t *testing.T) {
	t.Run("the rule itself", func(t *testing.T) {
		for _, tc := range []struct {
			name                 string
			nilData              bool
			switchable, disabled bool
			want                 bool
		}{
			{name: "switchable and enabled", switchable: true, want: true},
			{name: "switchable but disabled", switchable: true, disabled: true},
			{name: "not switchable", disabled: false},
			{name: "no roster, switchable, flag unknown", nilData: true, switchable: true},
			{name: "no roster, not switchable", nilData: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				data := &store.SequenceData{}
				if tc.nilData {
					data = nil
				}
				if got := rotationEligible(data, tc.switchable, tc.disabled); got != tc.want {
					t.Errorf("rotationEligible(nil=%v, switchable=%v, disabled=%v) = %v, want %v",
						tc.nilData, tc.switchable, tc.disabled, got, tc.want)
				}
			})
		}
	})

	// End to end, the whole pass degrades rather than ranking anything: the usage
	// fetch sits between the two roster reads, so removing the file from the fake
	// client's UsageFn loses the roster mid-pass deterministically, with no sleeps
	// and no goroutines of the test's own. The rows survive (they were already in
	// hand) and nothing is offered to automatic selection. This does not by itself
	// discriminate the fail-closed rule — store.AccountIsSwitchable re-reads the
	// roster too, so the switchable half goes blind in the same instant, and only
	// a roster that came BACK between those two reads separates them. The rule is
	// pinned above; what is pinned here is that a pass over a vanished roster
	// neither panics nor ranks.
	t.Run("the roster goes away mid-pass", func(t *testing.T) {
		prev := staggerSleep
		staggerSleep = func(time.Duration) {}
		t.Cleanup(func() { staggerSleep = prev })

		var s *store.Store
		var once sync.Once
		removesTheRoster := &oauth.FakeClient{
			UsageFn: func(context.Context, string) (map[string]any, error) {
				once.Do(func() {
					if err := os.Remove(s.SequenceFile); err != nil {
						t.Errorf("removing the roster mid-pass: %v", err)
					}
				})
				return nil, nil
			},
		}

		s = newStore(t, testutil.FixedClock(t, fixedNow), removesTheRoster)
		writeSequenceRaw(t, s, `{
  "activeAccountNumber": null,
  "lastUpdated": "2026-07-17T08:00:00Z",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "on@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x"},
    "2": {"email": "off@example.com", "uuid": "", "organizationUuid": "", "organizationName": "", "added": "x", "disabled": true}
  }
}`)
		writeBackup(t, s, "1", "on@example.com", oauthCreds("at1", "rt1", 0), `{"oauthAccount":{}}`)
		writeBackup(t, s, "2", "off@example.com", oauthCreds("at2", "rt2", 0), `{"oauthAccount":{}}`)

		snap := Snapshot(s, nil)
		if len(snap.Accounts) != 2 {
			t.Fatalf("want the rows BuildAccountsInfo already had, got %d", len(snap.Accounts))
		}
		for _, a := range snap.Accounts {
			if a.RotationEligible {
				t.Errorf("slot %s reported rotation-eligible from a roster that is gone", a.Number)
			}
		}
	})
}
