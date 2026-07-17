package switching

import (
	"encoding/json"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// TestClassifyOutgoing exercises the issue-#117 ownership oracle (spec 02§9).
func TestClassifyOutgoing(t *testing.T) {
	const curEmail = "a@x.com"

	// data with two managed slots: 1 (the outgoing slot) and 2 (a sibling).
	mkData := func(slot2UUID string) *store.SequenceData {
		recs := map[string]json.RawMessage{
			"1": record(map[string]any{"email": curEmail, "organizationUuid": "", "uuid": "uuid-1"}),
			"2": record(map[string]any{"email": "b@x.com", "organizationUuid": "", "uuid": slot2UUID}),
		}
		return seqData(ptrInt(1), []int{1, 2}, recs)
	}

	id := func(uuid, email, org string) *oauth.Identity {
		return &oauth.Identity{UUID: uuid, Email: email, OrgUUID: org}
	}

	tests := []struct {
		name        string
		setup       func(t *testing.T, s *store.Store) (orig string, prov *Provenance, data *store.SequenceData)
		wantKind    string
		wantForeign string
	}{
		{
			name: "own-bytes: live equals the slot backup",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				c := oauthCreds("acc", "ref1")
				seedBackup(t, s, "1", curEmail, c, "")
				return c, nil, mkData("uuid-2")
			},
			wantKind: "own-bytes",
		},
		{
			name: "own-family: same refresh-token lineage, rotated access token",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("old", "ref1"), "")
				return oauthCreds("new", "ref1"), nil, mkData("uuid-2")
			},
			wantKind: "own-family",
		},
		{
			name: "unresolved: diverged and no provenance",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				return oauthCreds("b", "refB"), nil, mkData("uuid-2")
			},
			wantKind: "unresolved",
		},
		{
			name: "unresolved: bytes moved since prefetch",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				orig := oauthCreds("b", "refB")
				moved := oauthCreds("c", "refC")
				return orig, &Provenance{Live: &moved, Resolved: id("uuid-1", curEmail, "")}, mkData("uuid-2")
			},
			wantKind: "unresolved",
		},
		{
			name: "own-rotated: profile resolves the live token to this slot",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				orig := oauthCreds("b", "refB")
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-1", curEmail, "")}, mkData("uuid-2")
			},
			wantKind: "own-rotated",
		},
		{
			name: "foreign: resolves to another slot with a different lineage",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				seedBackup(t, s, "2", "b@x.com", oauthCreds("x", "ref2"), "")
				orig := oauthCreds("live", "refLive")
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-2", "b@x.com", "")}, mkData("uuid-2")
			},
			wantKind: "foreign", wantForeign: "2",
		},
		{
			name: "foreign-synced: the other slot already holds this lineage",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				orig := oauthCreds("live", "refLive")
				seedBackup(t, s, "2", "b@x.com", orig, "")
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-2", "b@x.com", "")}, mkData("uuid-2")
			},
			wantKind: "foreign-synced", wantForeign: "2",
		},
		{
			name: "alien: structurally complete identity matching no slot",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				orig := oauthCreds("live", "refLive")
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-ghost", "ghost@x.com", "org-ghost")}, mkData("uuid-2")
			},
			wantKind: "alien",
		},
		{
			name: "alien: cross-slot match without a uuid on the stored slot",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				seedBackup(t, s, "2", "b@x.com", oauthCreds("x", "ref2"), "")
				orig := oauthCreds("live", "refLive")
				// slot 2 has no stored uuid ⇒ email+org match is not uuid-positive.
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-x", "b@x.com", "")}, mkData("")
			},
			wantKind: "alien",
		},
		{
			name: "unresolved: personal (org-less) identity matching no slot fails open",
			setup: func(t *testing.T, s *store.Store) (string, *Provenance, *store.SequenceData) {
				seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
				orig := oauthCreds("live", "refLive")
				return orig, &Provenance{Live: &orig, Resolved: id("uuid-ghost", "ghost@x.com", "")}, mkData("uuid-2")
			},
			wantKind: "unresolved",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t, nil)
			orig, prov, data := tc.setup(t, s)
			kind, foreign := classifyOutgoing(s, "1", curEmail, orig, prov, data)
			if kind != tc.wantKind || foreign != tc.wantForeign {
				t.Fatalf("got (%q, %q), want (%q, %q)", kind, foreign, tc.wantKind, tc.wantForeign)
			}
		})
	}
}

// TestClassifyOutgoingOffSequenceDeterministic pins the FINDING 12 fix: when the
// uuid fallback scan must disambiguate two OFF-sequence slots carrying an
// identical uuid+org, sortedAccountKeys must present them in a stable order so
// the classification (and the attributed foreign slot) is deterministic — not
// dependent on Go's randomized map iteration. Slots "8" and "9" live only in
// data.Accounts (not in the sequence), so both land in the sorted tail; the scan
// must always attribute to the lexically-first one ("8").
func TestClassifyOutgoingOffSequenceDeterministic(t *testing.T) {
	const curEmail = "a@x.com"
	live := oauthCreds("live-access", "ref-live")

	buildData := func() *store.SequenceData {
		recs := map[string]json.RawMessage{
			// Outgoing slot 1 is the only slot in the sequence.
			"1": record(map[string]any{"email": curEmail, "organizationUuid": "", "uuid": "uuid-1"}),
			// Two off-sequence slots with an IDENTICAL uuid+org: only the sorted
			// tail order decides which one the fallback scan picks.
			"8": record(map[string]any{"email": "eight@x.com", "organizationUuid": "org-dup", "uuid": "uuid-dup"}),
			"9": record(map[string]any{"email": "nine@x.com", "organizationUuid": "org-dup", "uuid": "uuid-dup"}),
		}
		return seqData(ptrInt(1), []int{1}, recs)
	}

	for i := 0; i < 50; i++ {
		s := newTestStore(t, nil)
		// Slot 1's backup diverges from the live bytes so classification consults
		// the resolved identity rather than short-circuiting on own-bytes/family.
		seedBackup(t, s, "1", curEmail, oauthCreds("a", "refA"), "")
		data := buildData()
		// Resolved identity: uuid+org match slots 8 and 9; the email matches no
		// slot, so the email-based lookup returns "" and the uuid fallback scan
		// (which walks sortedAccountKeys) decides the attribution.
		prov := &Provenance{
			Live:     &live,
			Resolved: &oauth.Identity{UUID: "uuid-dup", Email: "dup@x.com", OrgUUID: "org-dup"},
		}
		kind, foreign := classifyOutgoing(s, "1", curEmail, live, prov, data)
		if kind != "foreign" || foreign != "8" {
			t.Fatalf("iteration %d: got (%q, %q), want (\"foreign\", \"8\")", i, kind, foreign)
		}
	}
}
