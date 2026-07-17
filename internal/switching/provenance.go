// provenance.go — the self-switch provenance oracle helpers (spec 02§7).
//
// These establish, BEFORE any lock is taken (network allowed here, forbidden
// under the FileLock), whether the live credential provably belongs to a slot's
// stored lineage, and if it has diverged, whose token it is. The oracle is
// strictly advisory: every failure is swallowed so it can never fail a switch.
package switching

import (
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// Provenance mirrors Python's {"live": str|None, "resolved": dict|None}. Live is
// the pre-lock live-credential bytes (nil when unread/unreadable); Resolved is
// the profile-resolved owner identity (nil when unresolved). Trustworthy only
// while the live bytes have not moved — the under-lock classifier re-checks byte
// equality before using Resolved.
type Provenance struct {
	Live     *string
	Resolved *oauth.Identity
}

// readActive reads Claude Code's active credential the way Python's
// _read_credentials does: a hard read failure (credstore's non-nil error,
// Python's None outcome) returns ok=false; otherwise the value ("" on a Keychain
// timeout or genuine absence).
func readActive(s *store.Store) (value string, ok bool) {
	v, _, err := s.Creds.ReadActive()
	if err != nil {
		return "", false
	}
	return v, true
}

// liveMatchesSlotBackup reports whether the live credential is provably the
// slot's stored lineage — byte or refresh-token-fingerprint equality against the
// backup (spec 02§7). Unreadable/empty live → true (keep the no-op: forcing a
// switch on missing evidence would fail later anyway); missing backup → false.
func liveMatchesSlotBackup(s *store.Store, slot, email string) bool {
	live, ok := readActive(s)
	if !ok {
		return true
	}
	if live == "" {
		return true
	}
	backup, _ := s.ReadAccountCredentials(slot, email)
	if backup == "" {
		return false
	}
	return live == backup || fingerprintEqual(live, backup)
}

// selfSwitchAction decides how to treat a switch targeting the already-active
// slot (spec 02§7 _self_switch_action):
//
//   - "noop"          — live matches the slot's backup; nothing to do.
//   - "reconcile"     — live diverged and its owner resolved: run the full switch
//     so classifyOutgoing can classify/preserve.
//   - "noop-diverged" — live diverged but could not be classified (offline /
//     endpoint failure / no profile access): the exact pre-fix silent no-op.
func selfSwitchAction(s *store.Store, slot, email string) (string, *Provenance) {
	if liveMatchesSlotBackup(s, slot, email) {
		return "noop", nil
	}
	prov := prefetchLiveIdentity(s)
	if prov.Resolved == nil {
		if s.Log != nil {
			s.Log.Infof(
				"Live credential diverges from Account-%s's stored backup and "+
					"ownership could not be verified; self-switch left everything "+
					"untouched (pre-fix no-op).", slot)
		}
		return "noop-diverged", nil
	}
	return "reconcile", prov
}

// prefetchLiveIdentity resolves the live credential's owner BEFORE the locks are
// taken (spec 02§7 _prefetch_live_identity). Returns {live, resolved}; resolved
// is filled only when the live bytes diverge from the slot backup AND the
// profile endpoint answers. All failures are swallowed (advisory oracle). May
// hit the network — never call it with any lock held.
func prefetchLiveIdentity(s *store.Store) *Provenance {
	result := &Provenance{}
	live, ok := readActive(s)
	if !ok {
		return result
	}
	lv := live
	result.Live = &lv
	if live == "" {
		return result
	}
	email, orgUUID, identOK := s.GetCurrentAccount()
	if !identOK {
		return result
	}
	data, _ := s.ReadSequence()
	slot := s.FindAccountSlot(data, email, orgUUID)
	if slot == "" {
		return result
	}
	backup, _ := s.ReadAccountCredentials(slot, email)
	if backup == live || fingerprintEqual(backup, live) {
		return result // provenance already established locally
	}
	accessToken := oauth.ExtractAccessToken(live)
	if accessToken == "" {
		return result // raw API key / garbled JSON — nothing to resolve
	}
	if s.OAuth == nil {
		return result
	}
	if id := s.OAuth.Profile(bgCtx(), accessToken); id != nil {
		result.Resolved = id
	}
	return result
}

// fingerprintEqual reports whether two credentials share a fingerprint,
// mirroring Python's credential_fingerprint(a) == credential_fingerprint(b):
// two empty inputs both fingerprint to nil and compare equal (None == None).
// Every call site here guards against empty inputs first, so this only ever
// compares real bytes.
func fingerprintEqual(a, b string) bool {
	fa := oauth.CredentialFingerprint(a)
	fb := oauth.CredentialFingerprint(b)
	if fa == nil || fb == nil {
		return fa == nil && fb == nil
	}
	return *fa == *fb
}
