// warnings.go — the offline duplicate-credential and lockstep-usage warnings
// surfaced in the JSON --list payload and the human list (spec 02§10.1 / 02§11).
//
// Implements spec 02§13 (_duplicate_account_warnings, _lockstep_usage_warnings).
// Duplicates are provable offline (identical fingerprint, or identical non-empty
// uuid+org across two slots — issue #117's end state); lockstep is a heuristic
// for two live generations of the same account (identical 5h AND 7d pct + reset
// timestamps) that carry different fingerprints and untouched identities.
package reporting

import (
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// duplicateAccountWarnings returns warnings for slots that provably authenticate
// as the same account (spec 02§13). Two offline signals: an identical credential
// fingerprint across two slots, or the same non-empty uuid+org recorded for two
// slots (empty uuids — add-token placeholders — never match each other).
func duplicateAccountWarnings(s *store.Store, infos []AccountInfo) []string {
	data, _ := s.ReadSequence()
	byFP := map[string]string{}
	byIdentity := map[string]string{}
	var out []string
	for _, info := range infos {
		snum := strconv.Itoa(info.Number)
		if info.Creds != "" {
			if fp := oauth.CredentialFingerprint(info.Creds); fp != nil {
				if other, ok := byFP[*fp]; ok {
					out = append(out, "Account-"+other+" and Account-"+snum+
						" hold the same credential ("+info.Email+") — one slot's backup was "+
						"overwritten. Log in with the missing account and re-add it: cswap add --slot N")
				} else {
					byFP[*fp] = snum
				}
			}
		}
		rec, _ := recordFor(data, snum)
		uuid := strings.TrimSpace(recStr(rec, "uuid"))
		if uuid != "" {
			key := uuid + "\x00" + info.OrgUUID
			other, ok := byIdentity[key]
			if ok && other != snum {
				out = append(out, "Account-"+other+" and Account-"+snum+
					" both authenticate as "+info.Email+" — remove or re-login one of them.")
			} else if !ok {
				byIdentity[key] = snum
			}
		}
	}
	return out
}

// lockstepUsageWarnings returns warnings for slots whose usage moves in perfect
// lockstep — identical 5h AND 7d pct with identical reset timestamps (spec
// 02§13). Only rows where both windows carry a non-null resets_at and pct are
// compared; API-key slots carry sentinel usage and never reach the comparison.
func lockstepUsageWarnings(infos []AccountInfo, entries map[string]usage.UsageEntry) []string {
	seen := map[string]string{}
	var out []string
	for _, info := range infos {
		snum := strconv.Itoa(info.Number)
		entry, ok := entries[snum]
		if !ok {
			continue
		}
		dv := entry.DecisionValue()
		u, ok := dv.(map[string]any)
		if !ok {
			continue
		}
		h5, ok5 := u["five_hour"].(map[string]any)
		d7, ok7 := u["seven_day"].(map[string]any)
		if !ok5 || !ok7 {
			continue
		}
		p5, p5ok := h5["pct"]
		r5, r5ok := h5["resets_at"].(string)
		p7, p7ok := d7["pct"]
		r7, r7ok := d7["resets_at"].(string)
		// Both windows must carry a non-null pct and resets_at; two idle 0% rows
		// with nothing scheduled are indistinguishable and never flagged.
		if !p5ok || p5 == nil || !p7ok || p7 == nil || !r5ok || r5 == "" || !r7ok || r7 == "" {
			continue
		}
		key := lockstepKey(p5) + "|" + r5 + "|" + lockstepKey(p7) + "|" + r7
		if other, exists := seen[key]; exists {
			out = append(out, "Account-"+other+" and Account-"+snum+
				" report identical usage and reset times — they may be the same account "+
				"(issue #117). If it persists, log in with the missing account and re-add it: cswap add --slot N")
		} else {
			seen[key] = snum
		}
	}
	return out
}

// lockstepKey renders a pct value for the lockstep comparison key. Two windows
// match only when their numeric pct is equal; formatting to a canonical decimal
// makes int 62 and float 62.0 compare equal (as Python's == does).
func lockstepKey(pct any) string {
	if f, ok := asFloat(pct); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return ""
}
