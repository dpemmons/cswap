// Tests for the portable pieces of internal/wincred (spec 07§5.3, DESIGN A9):
// the in-memory Fake used by internal/migrations' Windows-migration tests, and
// the always-not-found stub Real that every non-Windows build actually links
// and runs. The real Windows Credential Manager backend (wincred_windows.go)
// cannot be exercised here — no Windows host — and is instead checked for
// validity via `GOOS=windows go build ./internal/wincred/...` (see the task
// notes); this file only covers what actually runs on this platform.
package wincred

import "testing"

func TestFakeGetSetDeleteRoundTrip(t *testing.T) {
	f := NewFake()

	if _, found, err := f.Get("claude-code", "account-1-a@x.com"); err != nil || found {
		t.Fatalf("Get on empty fake = (_, %v, %v), want not found", found, err)
	}

	f.Set("claude-code", "account-1-a@x.com", "secret-1")
	v, found, err := f.Get("claude-code", "account-1-a@x.com")
	if err != nil || !found || v != "secret-1" {
		t.Fatalf("Get after Set = (%q, %v, %v), want (secret-1, true, nil)", v, found, err)
	}

	// Distinct (service, account) pairs never collide.
	f.Set("claude-code", "account-2-b@x.com", "secret-2")
	if v, _, _ := f.Get("claude-code", "account-1-a@x.com"); v != "secret-1" {
		t.Fatalf("unrelated Set perturbed an existing entry: got %q", v)
	}

	if err := f.Delete("claude-code", "account-1-a@x.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := f.Get("claude-code", "account-1-a@x.com"); found {
		t.Fatal("entry survived Delete")
	}

	// Delete of an absent key is a no-op success (rc-44 parity), not an error.
	if err := f.Delete("claude-code", "account-1-a@x.com"); err != nil {
		t.Fatalf("Delete of absent entry: %v", err)
	}
}

func TestNonWindowsStubAlwaysReportsNotFound(t *testing.T) {
	r := New()

	v, found, err := r.Get("claude-code", "account-1-a@x.com")
	if v != "" || found || err != nil {
		t.Fatalf("stub Get = (%q, %v, %v), want (\"\", false, nil)", v, found, err)
	}
	if err := r.Delete("claude-code", "account-1-a@x.com"); err != nil {
		t.Fatalf("stub Delete = %v, want nil (no-op success)", err)
	}
}
