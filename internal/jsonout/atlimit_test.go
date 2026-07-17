// Tests for the additive at-limit fields (DESIGN A15): AtLimitFields omits both
// keys when false and carries them when true, and AccountRow merges them without
// disturbing usageStatus or any existing key.
package jsonout

import "testing"

func TestAtLimitFields_OmittedWhenFalse(t *testing.T) {
	if got := AtLimitFields(false, nil); len(got) != 0 {
		t.Errorf("AtLimitFields(false) = %v, want empty (both keys omitted)", got)
	}
	if got := AtLimitFields(false, []string{"7d"}); len(got) != 0 {
		t.Errorf("AtLimitFields(false, [7d]) = %v, want empty", got)
	}
}

func TestAtLimitFields_PresentWhenTrue(t *testing.T) {
	got := AtLimitFields(true, []string{"7d", "Fable 5"})
	if got["atLimit"] != true {
		t.Errorf("atLimit = %v, want true", got["atLimit"])
	}
	win, ok := got["limitingWindows"].([]string)
	if !ok || len(win) != 2 || win[0] != "7d" || win[1] != "Fable 5" {
		t.Errorf("limitingWindows = %v, want [7d Fable 5]", got["limitingWindows"])
	}
}

func TestAccountRow_AtLimitMergedAdditively(t *testing.T) {
	usageEntry := map[string]any{"seven_day": map[string]any{"pct": 100.0}}

	// At-limit row: usageStatus stays "ok", both additive keys present.
	at := AccountRow(1, "a@example.com", "", "", false, usageEntry,
		RowOpts{AtLimit: true, LimitingWindows: []string{"7d"}})
	if at["usageStatus"] != "ok" {
		t.Errorf("usageStatus = %v, want ok (unchanged)", at["usageStatus"])
	}
	if at["atLimit"] != true {
		t.Errorf("atLimit = %v, want true", at["atLimit"])
	}

	// Under-limit row omits both keys entirely.
	under := AccountRow(1, "a@example.com", "", "", false, usageEntry, RowOpts{})
	if _, ok := under["atLimit"]; ok {
		t.Errorf("atLimit must be omitted when false, got %v", under["atLimit"])
	}
	if _, ok := under["limitingWindows"]; ok {
		t.Errorf("limitingWindows must be omitted when false, got %v", under["limitingWindows"])
	}
}
