package switching

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// setupAccounts writes switchable backups + sequence for slots (the first slot
// is recorded active). disabled marks slots parked out of rotation.
func setupAccounts(t *testing.T, s *store.Store, slots []int, disabled map[int]bool) {
	t.Helper()
	records := map[string]json.RawMessage{}
	for i, n := range slots {
		email := fmt.Sprintf("acct%d@x.com", n)
		seedBackup(t, s, strconv.Itoa(n), email, oauthCreds(fmt.Sprintf("acc-%d", n), fmt.Sprintf("ref-%d", n)), "")
		f := map[string]any{"email": email, "organizationUuid": "", "uuid": fmt.Sprintf("uuid-%d", n)}
		if disabled[n] {
			f["disabled"] = true
		}
		records[strconv.Itoa(n)] = record(f)
		_ = i
	}
	writeSeq(t, s, seqData(ptrInt(slots[0]), slots, records))
}

func TestSelectBestSwitchable(t *testing.T) {
	tests := []struct {
		name       string
		slots      []int
		disabled   map[int]bool
		usage      map[string]any
		current    string
		wantTarget string
		wantNote   string
	}{
		{
			name: "best never moves onto a worse account", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(89, 0), "2": usageDict(100, 0)},
			wantTarget: "", wantNote: "stay",
		},
		{
			name: "switch to strictly more headroom", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0), "2": usageDict(20, 0)},
			wantTarget: "2", wantNote: "",
		},
		{
			name: "ties resolve to the earliest slot", slots: []int{1, 2, 3}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0), "2": usageDict(20, 0), "3": usageDict(20, 0)},
			wantTarget: "2", wantNote: "",
		},
		{
			name: "current-vs-other tie stays put", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(20, 0), "2": usageDict(20, 0)},
			wantTarget: "", wantNote: "stay",
		},
		{
			name: "current usage unavailable", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": nil, "2": usageDict(20, 0)},
			wantTarget: "", wantNote: "current-unavailable",
		},
		{
			name: "no other account has usage", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(20, 0), "2": nil},
			wantTarget: "", wantNote: "no-comparison",
		},
		{
			name: "incomplete comparison when a candidate is unknown", slots: []int{1, 2, 3}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0), "2": usageDict(80, 0), "3": nil},
			wantTarget: "", wantNote: "incomplete-comparison",
		},
		{
			name: "all exhausted", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(100, 0), "2": usageDict(100, 0)},
			wantTarget: "", wantNote: "exhausted",
		},
		{
			name: "no other switchable account", slots: []int{1}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0)},
			wantTarget: "", wantNote: "none",
		},
		{
			name: "api-key sentinel excluded from comparison", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0), "2": "api key"},
			wantTarget: "", wantNote: "no-comparison",
		},
		{
			name: "quarantine sentinel excluded from comparison", slots: []int{1, 2}, current: "1",
			usage:      map[string]any{"1": usageDict(50, 0), "2": "re-login needed"},
			wantTarget: "", wantNote: "no-comparison",
		},
		{
			name: "disabled candidate excluded even with most headroom", slots: []int{1, 2, 3}, current: "1",
			disabled:   map[int]bool{3: true},
			usage:      map[string]any{"1": usageDict(50, 0), "2": usageDict(20, 0), "3": usageDict(0, 0)},
			wantTarget: "2", wantNote: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t, nil)
			setupAccounts(t, s, tc.slots, tc.disabled)
			target, note := selectBestSwitchable(s, tc.current, nil, tc.usage)
			if target != tc.wantTarget || note != tc.wantNote {
				t.Fatalf("got (%q, %q), want (%q, %q)", target, note, tc.wantTarget, tc.wantNote)
			}
		})
	}
}

// TestSelectBestSwitchableWithModels folds a named scoped window into the
// headroom comparison: the scoped 100% binds even when 5h/7d have room.
func TestSelectBestSwitchableWithModels(t *testing.T) {
	s := newTestStore(t, nil)
	setupAccounts(t, s, []int{1, 2}, nil)
	usage := map[string]any{
		"1": scopedUsage(10, "Fable", 100), // Fable at limit ⇒ headroom 0
		"2": scopedUsage(10, "Fable", 10),  // Fable has room ⇒ headroom 90
	}
	// Without models the scoped window is invisible: both look identical (hr 90).
	if target, note := selectBestSwitchable(s, "1", nil, usage); target != "" || note != "stay" {
		t.Fatalf("no models: got (%q,%q), want (\"\",\"stay\")", target, note)
	}
	// With models=[Fable], account 1 is exhausted, account 2 has room.
	if target, note := selectBestSwitchable(s, "1", []string{"Fable"}, usage); target != "2" || note != "" {
		t.Fatalf("models=Fable: got (%q,%q), want (\"2\",\"\")", target, note)
	}
}

func TestWarnInertModels(t *testing.T) {
	t.Run("fires when every account readable and a model is unseen", func(t *testing.T) {
		resetSeams(t)
		usage := map[string]any{
			"1": scopedUsage(0, "Fable", 0),
			"2": scopedUsage(0, "Fable", 0),
		}
		var warnings []string
		warnInertModels(usage, []string{"Fable", "Ghost"}, true, &warnings)
		want := "model(s) Ghost match no account's usage windows (typo?)"
		if len(warnings) != 1 || warnings[0] != want {
			t.Fatalf("warnings = %v, want [%q]", warnings, want)
		}
	})
	t.Run("silent when an account's usage is not a dict", func(t *testing.T) {
		resetSeams(t)
		usage := map[string]any{
			"1": scopedUsage(0, "Fable", 0),
			"2": "api key", // unreadable ⇒ could be the one carrying the window
		}
		var warnings []string
		warnInertModels(usage, []string{"Ghost"}, true, &warnings)
		if len(warnings) != 0 {
			t.Fatalf("warnings = %v, want none", warnings)
		}
	})
	t.Run("the all sentinel never warns", func(t *testing.T) {
		resetSeams(t)
		usage := map[string]any{"1": scopedUsage(0, "Fable", 0)}
		var warnings []string
		warnInertModels(usage, []string{"all"}, true, &warnings)
		if len(warnings) != 0 {
			t.Fatalf("warnings = %v, want none", warnings)
		}
	})
}
