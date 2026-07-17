// Shared write-verify-delete relocation shape used by both registry
// migrations (spec 07§5.3 steps 1–6, 07§5.4's per-pending-account loop): pick
// a legacy source (canonical username wins; the "account-None-{email}" legacy
// artifact is used as a fallback only when the slot's email is globally
// unique), write it to the new backend, read it back to verify byte-identity
// before ever deleting the only other copy, and only then remove the legacy
// entry (entries are never deleted without a verified successful write —
// "no unsafe window").
package migrations

import (
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/slotkey"
)

// relocateConfig parameterizes relocate over the two migrations' distinct
// legacy backend (keychain.KeychainClient vs wincred.Client) and new-backend
// write path (credstore's transparent .enc-wins ops vs its Keychain-only
// ops), while sharing the account-None disambiguation and write-verify-delete
// safety logic exactly.
type relocateConfig struct {
	// label prefixes every warning this relocation logs (matches Python's
	// f"{context}: ..." messages, e.g. "windows_keyring_to_files").
	label string
	// pending is the slot-number → email map to actually process this call
	// (the macOS migration pre-filters this to slots not yet in the security
	// service; the Windows migration passes every managed account).
	pending map[string]string
	// allAccounts is every managed account (not just pending), used to
	// compute the email-uniqueness count the account-None fallback is gated
	// on — always the full set, even when pending is a strict subset (spec
	// 07§5.4's pre-check narrowing must not change who "unique email" means).
	allAccounts map[string]string

	// readLegacy reads one legacy username; ("", nil) means absent, a non-nil
	// error means a genuine backend failure (not "not found").
	readLegacy func(username string) (string, error)
	// deleteLegacy best-effort removes one legacy username; it never returns
	// an error — failures are logged internally (mirrors
	// _delete_keyring_quietly's swallow-and-warn).
	deleteLegacy func(username string)

	// writeNew writes creds to the new backend for (num, email); an error
	// aborts this account (failed++) without touching the legacy source.
	writeNew func(num, email, creds string) error
	// readNew reads back what writeNew just wrote, to verify byte-identity
	// before the legacy source is ever deleted.
	readNew func(num, email string) (string, error)
	// deleteBadNew best-effort discards a just-written new-backend entry that
	// failed verification, so it can never shadow the still-intact legacy
	// source on a retry.
	deleteBadNew func(num, email string)

	// afterSuccess runs once a (num, email) relocation has fully succeeded
	// (legacy source deleted); nil is fine (the Windows migration has no
	// equivalent step). The macOS migration uses it for the post-delete
	// item_exists check (spec 07§5.4 — keyring's PasswordDeleteError can't
	// distinguish "already gone" from "user denied the prompt").
	afterSuccess func(num, email, sourceUsername string)

	log *logging.Logger
}

// relocate runs cfg.pending through the shared relocation shape and returns
// how many accounts were fully migrated vs. left for a retry.
func relocate(cfg relocateConfig) (migrated, failed int) {
	emailCounts := map[string]int{}
	for _, email := range cfg.allAccounts {
		emailCounts[email]++
	}

	for _, num := range sortedSlotKeys(cfg.pending) {
		email := cfg.pending[num]
		canonical := "account-" + num + "-" + email
		noneUser := "account-None-" + email

		creds, err := cfg.readLegacy(canonical)
		if err != nil {
			cfg.log.Warningf("%s: read of %s failed: %v", cfg.label, canonical, err)
			failed++
			continue
		}

		sourceUsername := canonical
		if creds == "" && num != "None" && emailCounts[email] == 1 {
			// Canonical missing; fall back to account-None only when the
			// email unambiguously maps to this one slot (spec 07§5.3/§5.4).
			creds, err = cfg.readLegacy(noneUser)
			if err != nil {
				cfg.log.Warningf("%s: read of %s failed: %v", cfg.label, noneUser, err)
				failed++
				continue
			}
			if creds != "" {
				sourceUsername = noneUser
			}
		}

		if creds == "" {
			// Nothing legacy for this slot (added on the new version, or an
			// ambiguous account-None we deliberately leave untouched).
			// Benign — not a failure.
			continue
		}

		if err := cfg.writeNew(num, email, creds); err != nil {
			cfg.log.Warningf("%s: write/read-back for %s failed: %v", cfg.label, canonical, err)
			cfg.deleteBadNew(num, email)
			failed++
			continue
		}
		readback, err := cfg.readNew(num, email)
		if err != nil {
			cfg.log.Warningf("%s: write/read-back for %s failed: %v", cfg.label, canonical, err)
			cfg.deleteBadNew(num, email)
			failed++
			continue
		}
		if readback != creds {
			cfg.log.Warningf("%s: read-back mismatch for %s; discarding the bad copy and leaving the legacy entry in place", cfg.label, canonical)
			cfg.deleteBadNew(num, email)
			failed++
			continue
		}

		// Data is safely relocated → the new backend is authoritative.
		// Remove the source, and the redundant account-None entry too when a
		// distinct one was actually consulted.
		cfg.deleteLegacy(sourceUsername)
		if num != "None" && sourceUsername != noneUser {
			cfg.deleteLegacy(noneUser)
		}
		if cfg.afterSuccess != nil {
			cfg.afterSuccess(num, email, sourceUsername)
		}
		migrated++
	}
	return migrated, failed
}

// sortedSlotKeys returns m's keys in the canonical slot-key total order
// (numerics first by value, then non-numerics lexicographically). Processing
// order has no effect on any account's outcome (email uniqueness is computed up
// front over the full set); this is purely for deterministic tests and readable
// logs.
func sortedSlotKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return slotkey.Sorted(keys)
}
