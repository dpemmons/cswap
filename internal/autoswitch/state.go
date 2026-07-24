// State file read/mutate, cooldown gate, and the quarantine lifecycle.
//
// Implements spec 05§5 (autoswitch_state.json + .autoswitch_state.lock,
// read-modify-write under the lock, schemaVersion=1, non-dict → {}), 05§13
// (cooldown), and 05§14 (quarantine trigger/clear/persistence). Every mutation
// goes through mutateState under the state lock; mutateState is never called
// while another lock is held (DESIGN §2.18).

package autoswitch

import (
	"encoding/json"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
)

// StatePath is the persisted cooldown/quarantine state file path under a backup
// dir (<backupDir>/autoswitch_state.json). NewEngine defaults e.statePath the
// same way; the TUI's read-only quarantine reader reuses this so the engine and
// the panel that shadows it can never join the path differently.
func StatePath(backupDir string) string {
	return filepath.Join(backupDir, StateFilename)
}

// readStateFile reads and JSON-parses the state file at statePath, swallowing
// any read/parse error and a non-object top level → {} (05§5). Lock-free, as
// the engine's own read at tick start is. Both the engine's readState and the
// exported ReadQuarantine funnel through this one tolerant read.
func readStateFile(statePath string) map[string]any {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// readState reads the state file, swallowing any read/parse error and a
// non-object top level → {} (05§5).
func (e *Engine) readState() map[string]any {
	return readStateFile(e.statePath)
}

// ReadQuarantine returns the quarantined slots recorded in the state file at
// statePath: slot number → reason ("" when the entry carries no readable
// reason string). Tolerant like readState (missing file / parse error /
// non-object top level → empty map) and lock-free, matching the engine's own
// readState at tick start. Every key of the "quarantine" object is reported,
// mirroring quarantinedSet, so the reader and the engine's exclusion set agree.
//
// This is the read seam the TUI's Auto "next best" panel uses to label the
// quarantined slots the engine excludes from its candidate set but that would
// otherwise rank as viable targets (DESIGN A18).
func ReadQuarantine(statePath string) map[string]string {
	out := map[string]string{}
	q, ok := readStateFile(statePath)["quarantine"].(map[string]any)
	if !ok {
		return out
	}
	for num, raw := range q {
		reason := ""
		if entry, ok := raw.(map[string]any); ok {
			reason, _ = entry["reason"].(string)
		}
		out[num] = reason
	}
	return out
}

// writeState atomically writes the state file (indent 2, 0600/0700), matching
// atomic_write_json (05§5).
func (e *Engine) writeState(state map[string]any) error {
	return atomicfile.WriteJSON(e.statePath, state, atomicfile.Opts{})
}

// mutateState read-modify-writes the state file under its lock and returns the
// new state (05§5). schemaVersion is always stamped before the mutator runs.
func (e *Engine) mutateState(mutator func(state map[string]any)) (map[string]any, error) {
	lock := filelock.New(e.lockPath, 0)
	var result map[string]any
	err := lock.With(func() error {
		state := e.readState()
		state["schemaVersion"] = StateSchemaVersion
		mutator(state)
		if err := e.writeState(state); err != nil {
			return err
		}
		result = state
		return nil
	})
	return result, err
}

// inCooldown reports whether the last real switch is within cooldown_seconds
// (05§13). A missing / non-numeric lastSwitchAt is not a cooldown.
func (e *Engine) inCooldown(state map[string]any) bool {
	last, ok := numOfAny(state["lastSwitchAt"])
	if !ok {
		return false
	}
	return (e.nowSeconds() - last) < e.currentSettings().CooldownSeconds
}

// quarantinedSet builds the set of quarantined slot numbers from a state map
// (05§6 step 2).
func quarantinedSet(state map[string]any) map[string]bool {
	out := map[string]bool{}
	q, ok := state["quarantine"].(map[string]any)
	if !ok {
		return out
	}
	for num := range q {
		out[num] = true
	}
	return out
}

// quarantine records a slot's dead/mismatched credential and emits the event
// (05§14). The fingerprint is the credential's refresh-token hash, or nil.
func (e *Engine) quarantine(number, email, reason string) error {
	creds := e.sw.ReadAccountCredentials(number, email)
	var fingerprint *string
	if creds != "" {
		fingerprint = oauth.CredentialFingerprint(creds)
	}
	at := e.nowISO()
	_, err := e.mutateState(func(state map[string]any) {
		q, ok := state["quarantine"].(map[string]any)
		if !ok {
			q = map[string]any{}
			state["quarantine"] = q
		}
		entry := map[string]any{
			"email":  email,
			"reason": reason,
			"at":     at,
		}
		if fingerprint != nil {
			entry["refreshTokenFingerprint"] = *fingerprint
		} else {
			entry["refreshTokenFingerprint"] = nil
		}
		q[number] = entry
	})
	if err != nil {
		return err
	}
	e.emit(QuarantineEvent{Ts: e.nowISO(), Number: number, Email: email, Reason: reason})
	return nil
}

type releaseRec struct {
	number string
	email  string
	reason string
}

// releaseRecoveredQuarantines drops quarantine entries whose credential was
// replaced (email changed / slot re-added → "account-replaced", or a changed
// refresh-token fingerprint → "credentials-replaced") and emits an
// UnquarantineEvent per release (05§14). Real ticks only.
func (e *Engine) releaseRecoveredQuarantines(state map[string]any) (map[string]any, error) {
	q, ok := state["quarantine"].(map[string]any)
	if !ok || len(q) == 0 {
		return state, nil
	}
	var toRelease []releaseRec
	for _, number := range sortNumeric(mapKeys(q)) {
		entry, _ := q[number].(map[string]any)
		wantEmail, _ := entry["email"].(string)
		emailNow := e.sw.AccountEmail(number)
		if emailNow == "" || emailNow != wantEmail {
			toRelease = append(toRelease, releaseRec{number, wantEmail, "account-replaced"})
			continue
		}
		creds := e.sw.ReadAccountCredentials(number, emailNow)
		var fingerprint *string
		if creds != "" {
			fingerprint = oauth.CredentialFingerprint(creds)
		}
		if !fpEqual(fingerprint, entry["refreshTokenFingerprint"]) {
			toRelease = append(toRelease, releaseRec{number, emailNow, "credentials-replaced"})
		}
	}
	if len(toRelease) == 0 {
		return state, nil
	}
	newState, err := e.mutateState(func(s map[string]any) {
		sq, ok := s["quarantine"].(map[string]any)
		if !ok {
			return
		}
		for _, r := range toRelease {
			delete(sq, r.number)
		}
	})
	if err != nil {
		return state, err
	}
	for _, r := range toRelease {
		e.emit(UnquarantineEvent{Ts: e.nowISO(), Number: r.number, Email: r.email, Reason: r.reason})
	}
	return newState, nil
}

// numOfAny extracts a float64 from a JSON-decoded numeric value (float64 or
// json.Number), reporting ok=false for anything else (mirrors Python's
// isinstance(x, (int, float)) guards, minus the bool edge case).
func numOfAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// fpEqual compares a computed fingerprint (nil = none) against a stored JSON
// value (a string or null), matching Python's `fingerprint != entry.get(...)`.
func fpEqual(fp *string, raw any) bool {
	if fp == nil {
		return raw == nil
	}
	s, ok := raw.(string)
	return ok && s == *fp
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
