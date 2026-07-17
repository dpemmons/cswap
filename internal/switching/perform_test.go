package switching

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
)

// TestConfigReadErrorNotFoundOnlyForENOENT: a non-ENOENT read error on
// ~/.claude.json (here: a directory at the path) must NOT be misreported as
// "Claude config file not found" — it surfaces with the real cause. Only a
// genuine absence maps to "not found" (FINDING 10).
func TestConfigReadErrorNotFoundOnlyForENOENT(t *testing.T) {
	// A real EISDIR-class error: reading a directory as a file, matching how
	// readConfigText would fail if ~/.claude.json were a directory.
	dir := t.TempDir()
	_, dirErr := os.ReadFile(dir)
	if dirErr == nil {
		t.Fatal("reading a directory should have errored")
	}
	err := configReadError(dirErr)
	if err == nil {
		t.Fatal("configReadError(dir read error) = nil")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("directory read error misreported as not-found: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Failed to read Claude config") {
		t.Fatalf("error should carry the real cause, got %q", err.Error())
	}
	// The underlying cause is preserved on the chain.
	if !errorsIsCause(err, dirErr) {
		t.Fatalf("underlying cause not wrapped: %v", err)
	}

	// A genuine absence still maps to the "not found" message.
	missing := os.ErrNotExist
	nf := configReadError(missing)
	if !strings.Contains(nf.Error(), "Claude config file not found") {
		t.Fatalf("ENOENT should map to not-found, got %q", nf.Error())
	}
	if cerr.TypeName(nf) != "ConfigError" {
		t.Fatalf("want ConfigError kind, got %q", cerr.TypeName(nf))
	}
}

// errorsIsCause reports whether target is somewhere in err's Unwrap chain.
func errorsIsCause(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// TestSwitchEmptyCurrentCredsGuard: an empty active-credential read (a Keychain
// timeout returns "") must NOT overwrite the departing slot's backup — the
// switch fails with a CredentialReadError (spec 02§8).
func TestSwitchEmptyCurrentCredsGuard(t *testing.T) {
	s := newTestStore(t, nil)
	ca := oauthCreds("acc-a", "ref-a")
	cb := oauthCreds("acc-b", "ref-b")
	writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": record(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
	}))
	seedBackup(t, s, "1", "a@x.com", ca, "")
	seedBackup(t, s, "2", "b@x.com", cb, "")
	seedLive(t, s, "a@x.com", "", "") // empty live credential

	_, err := SwitchTo(s, "2", true, false)
	if err == nil || !strings.Contains(err.Error(), "Current account credential is empty") {
		t.Fatalf("err = %v, want CredentialReadError(empty)", err)
	}
	// The outgoing slot's backup is intact.
	if got, _ := s.ReadAccountCredentials("1", "a@x.com"); got != ca {
		t.Fatalf("outgoing backup was clobbered by an empty-read switch")
	}
}

// TestSwitchForeignCredentialPreserved: when the live credential resolves
// (via the profile endpoint) to a DIFFERENT managed slot, the switch must not
// write it into the outgoing slot — it is stashed, a warning rides back, and the
// outgoing backup stays intact (issue #117).
func TestSwitchForeignCredentialPreserved(t *testing.T) {
	fake := &oauth.FakeClient{
		ProfileFn: func(_ context.Context, accessToken string) *oauth.Identity {
			if accessToken == "live-access" {
				return &oauth.Identity{UUID: "uuid-2", Email: "b@x.com", OrgUUID: ""}
			}
			return nil
		},
	}
	s := newTestStore(t, fake)

	backup1 := oauthCreds("a1", "ref1")
	backup2 := oauthCreds("b2", "ref2")
	liveCreds := oauthCreds("live-access", "ref-live") // belongs to account 2 by identity
	recs := map[string]json.RawMessage{
		"1": record(map[string]any{"email": "a@x.com", "organizationUuid": "", "uuid": "uuid-1"}),
		"2": record(map[string]any{"email": "b@x.com", "organizationUuid": "", "uuid": "uuid-2"}),
	}
	writeSeq(t, s, seqData(ptrInt(1), []int{1, 2}, recs))
	seedBackup(t, s, "1", "a@x.com", backup1, "")
	seedBackup(t, s, "2", "b@x.com", backup2, "")
	// Live login says account 1 (email a@x.com) but the live bytes are foreign.
	seedLive(t, s, "a@x.com", "", liveCreds)

	out, err := Switch(s, nil, true, nil, nil)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	m := asMap(t, out)
	warnings, _ := m["warnings"].([]string)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "Credential ownership mismatch detected") && strings.Contains(w, "not written") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a foreign-credential warning, got %v", warnings)
	}
	// The outgoing slot 1 backup is untouched (the foreign live bytes were NOT
	// written into it).
	if got, _ := s.ReadAccountCredentials("1", "a@x.com"); got != backup1 {
		t.Fatalf("outgoing backup was poisoned with the foreign credential")
	}
	// A safety stash was written.
	stashed, _ := s.Creds.ListUnclaimed()
	if len(stashed) == 0 {
		t.Fatalf("expected the foreign credential to be stashed")
	}
	// The switch still completed onto account 2.
	if got := readActiveCreds(t, s); got != backup2 {
		t.Fatalf("switch did not activate account 2")
	}
}
