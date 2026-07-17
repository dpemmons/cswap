// classify.go — the issue-#117 credential-ownership oracle
// (_classify_outgoing_credential, spec 02§9) and the unclaimed-credential stash.
//
// classifyOutgoing decides what the switch-time backup may do with the live
// credential when it no longer byte-matches the outgoing slot: back it up
// (own family/rotation), leave it (own-bytes / foreign-synced), or preserve it
// in a write-only safety copy and never write it into a slot (foreign / alien).
// The oracle is strictly advisory — "unresolved" falls back to the exact pre-fix
// backup, so endpoint state never decides whether a switch completes.
package switching

import (
	"os"
	"sort"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// classifyOutgoing returns (kind, foreignSlot) per spec 02§9. kind ∈
// {own-bytes, own-family, own-rotated, foreign, foreign-synced, alien,
// unresolved}. foreignSlot is set only for foreign / foreign-synced.
func classifyOutgoing(s *store.Store, currentAccount, currentEmail, originalCreds string, prov *Provenance, data *store.SequenceData) (string, string) {
	backup, _ := s.ReadAccountCredentials(currentAccount, currentEmail)

	// 1. Byte-identical to the slot's stored backup.
	if backup != "" && backup == originalCreds {
		return "own-bytes", ""
	}
	// 2. Same refresh-token lineage (access token rotated).
	if backup != "" && fingerprintEqual(backup, originalCreds) {
		return "own-family", ""
	}
	// 3. Mismatch and identity could not be established — or the bytes moved
	//    since the pre-lock read. Fail open (exact pre-fix backup).
	if prov == nil || prov.Resolved == nil {
		return "unresolved", ""
	}
	if prov.Live == nil || *prov.Live != originalCreds {
		return "unresolved", ""
	}

	rEmail := prov.Resolved.Email
	rOrg := prov.Resolved.OrgUUID
	rUUID := strings.TrimSpace(prov.Resolved.UUID)

	// 4. Outgoing-slot uuid match first (robust to partial responses / changed
	//    email). Org must agree only when both sides record one.
	own, _ := accountRec(data, currentAccount)
	ownUUID := strings.TrimSpace(recStr(own, "uuid"))
	ownOrg := recStr(own, "organizationUuid")
	if rUUID != "" && ownUUID != "" && rUUID == ownUUID &&
		(rOrg == "" || ownOrg == "" || rOrg == ownOrg) {
		return "own-rotated", ""
	}

	// 5. Find slot by (resolved.email, resolved.org).
	slot := ""
	if rEmail != "" {
		slot = s.FindAccountSlot(data, rEmail, rOrg)
	}
	if slot != "" && rUUID != "" {
		// Both sides carry a uuid → it must agree: an email+org match with a
		// conflicting uuid is a different account wearing a recycled email.
		storedRec, _ := accountRec(data, slot)
		storedUUID := strings.TrimSpace(recStr(storedRec, "uuid"))
		if storedUUID != "" && storedUUID != rUUID {
			slot = ""
		}
	}
	if slot == "" && rUUID != "" {
		// Fall back to the account uuid (org-scoped): the slot's stored email
		// may be stale or an add-token placeholder.
		for _, num := range sortedAccountKeys(data) {
			rec, _ := accountRec(data, num)
			if u := recStr(rec, "uuid"); u != "" && u == rUUID &&
				recStr(rec, "organizationUuid") == rOrg {
				slot = num
				break
			}
		}
	}
	if slot == currentAccount {
		return "own-rotated", ""
	}
	if slot == "" {
		// 6. A positive "alien" needs a structurally complete identity (email +
		//    organization) matching nothing. A partial one is indistinguishable
		//    from schema drift and must fail open.
		if rEmail != "" && orgPresent(prov.Resolved) {
			return "alien", ""
		}
		return "unresolved", ""
	}
	// 7. A cross-slot attribution must be uuid-positive.
	storedRec, _ := accountRec(data, slot)
	storedUUID := strings.TrimSpace(recStr(storedRec, "uuid"))
	if rUUID == "" || storedUUID != rUUID {
		return "alien", ""
	}
	// 8. If the foreign slot already holds this lineage → foreign-synced.
	foreignEmail := recStr(storedRec, "email")
	foreignBackup, _ := s.ReadAccountCredentials(slot, foreignEmail)
	if foreignBackup != "" && (foreignBackup == originalCreds || fingerprintEqual(foreignBackup, originalCreds)) {
		return "foreign-synced", slot
	}
	return "foreign", slot
}

// orgPresent reports whether the resolved identity carries an organization,
// standing in for Python's `resolved.get("organizationUuid") is not None`. The
// projection collapses Python's None and "" into "", so a personal login (org
// None → "") reads as absent — which matches Python's None case falling through
// to "unresolved". (A profile literally returning an empty-string org — never
// observed — would be "alien" in Python but "unresolved" here; a documented,
// unreachable edge.)
func orgPresent(id *oauth.Identity) bool {
	return id != nil && id.OrgUUID != ""
}

// sortedAccountKeys returns the sequence-ordered slot keys for a deterministic
// uuid fallback scan (Python relied on dict insertion order).
func sortedAccountKeys(data *store.SequenceData) []string {
	if data == nil {
		return nil
	}
	out := make([]string, 0, len(data.Sequence))
	seen := map[string]bool{}
	for _, n := range data.Sequence {
		k := itoa(n)
		if _, ok := data.Accounts[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	// Include any accounts not present in sequence (defensive), lexically last.
	// Map iteration order is nondeterministic, so sort the tail before appending
	// to keep the fallback scan reproducible across runs.
	tail := make([]string, 0, len(data.Accounts))
	for k := range data.Accounts {
		if !seen[k] {
			tail = append(tail, k)
		}
	}
	sort.Strings(tail)
	out = append(out, tail...)
	return out
}

// stashLiveCredential preserves an unowned live credential before it is
// overwritten, via credstore's write-only unclaimed stash (spec 02§9). It raises
// on write failure — a successful stash is the license to overwrite the live
// store. Logs a WARNING. resolved may be nil.
func stashLiveCredential(s *store.Store, originalCreds, reason, currentAccount string, resolved *oauth.Identity) (string, error) {
	var credsMtime any
	if fi, err := os.Stat(paths.GetCredentialsPath()); err == nil {
		credsMtime = fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
	}
	var liveOAuthAccount any
	if cfg := readConfigJSON(s); cfg != nil {
		liveOAuthAccount = cfg["oauthAccount"]
	}
	var resolvedIdentity any
	if resolved != nil {
		resolvedIdentity = map[string]any{
			"uuid":             resolved.UUID,
			"email":            resolved.Email,
			"organizationUuid": resolved.OrgUUID,
		}
	}
	ctx := map[string]any{
		"reason":           reason,
		"configSlot":       currentAccount,
		"fingerprint":      fingerprintValue(originalCreds),
		"liveOauthAccount": liveOAuthAccount,
		"resolvedIdentity": resolvedIdentity,
		"credentialsMtime": credsMtime,
	}
	id, err := s.Creds.WriteUnclaimed(originalCreds, ctx)
	if err != nil {
		return "", err
	}
	if s.Log != nil {
		mt := "unknown"
		if m, ok := credsMtime.(string); ok {
			mt = m
		}
		s.Log.Warningf(
			"Live credential does not belong to Account-%s (%s): stashed as %s "+
				"(credentials mtime %s). Something outside cswap rewrote the live "+
				"login after the last switch.", currentAccount, reason, id, mt)
	}
	return id, nil
}

// fingerprintValue returns the credential fingerprint as a plain any (nil for
// empty input) for the stash context.
func fingerprintValue(creds string) any {
	if fp := oauth.CredentialFingerprint(creds); fp != nil {
		return *fp
	}
	return nil
}
