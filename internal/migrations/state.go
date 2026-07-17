// .migrations.json state file (spec 07§5.1): {"version": 1, "applied":
// {migration_id: iso-timestamp}}. STATE_VERSION is this file's own schema
// version — unrelated to the .cswap export FORMAT_VERSION, and never itself
// checked on read (Python's _load_applied never inspects data["version"],
// only data["applied"]).
//
// loadApplied mirrors Python's _load_applied: a missing or unparseable file,
// or a top-level/"applied" shape that isn't the expected object, is treated as
// "nothing applied" ({}) rather than an error — a corrupt state file can never
// permanently block a migration. markApplied mirrors _mark_applied: re-load
// first (so it preserves every previously-recorded migration, including any
// this binary doesn't itself know about — e.g. a future migration id already
// recorded by a newer claude-swap that ran on this machine before), then write
// atomically via internal/atomicfile (spec 07§9 unifies Python's four
// near-identical atomic-write helpers, including this one's mkstemp+os.replace
// variant, into one).
package migrations

import (
	"encoding/json"
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
)

const stateVersion = 1

// stateFile is .migrations.json's on-disk shape. Applied values are kept as
// json.RawMessage (not string) so a re-write preserves whatever a foreign
// writer put there byte-for-byte, even if it's not a JSON string — mirroring
// Python's tolerance of "any dict" for the applied map's value types.
type stateFile struct {
	Version int                        `json:"version"`
	Applied map[string]json.RawMessage `json:"applied"`
}

// loadApplied returns the {migration_id: raw-value} map, or {} if the file is
// missing, unparseable, not a JSON object, or has no usable "applied" object.
// Never returns an error — see the package doc.
func loadApplied(path string) map[string]json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	// Only the "applied" object is read; the "version" field is deliberately
	// ignored (no Version field here), so a non-integer version — e.g. a
	// corrupted 1.0 or "1" — can't fail the parse and discard an intact applied
	// map. This mirrors Python's _load_applied, which never inspects
	// data["version"] and returns data.get("applied") whenever it is a dict.
	var parsed struct {
		Applied map[string]json.RawMessage `json:"applied"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return map[string]json.RawMessage{}
	}
	if parsed.Applied == nil {
		return map[string]json.RawMessage{}
	}
	return parsed.Applied
}

// markApplied records migrationID as applied at clk.Now(), preserving every
// previously-recorded entry (it re-loads before writing, so concurrent
// migrations recorded one at a time within the same run don't clobber each
// other — spec 07§5.1).
func markApplied(path string, clk clock.Clock, migrationID string) error {
	applied := loadApplied(path)
	ts, err := json.Marshal(clk.Now().UTC().Format("2006-01-02T15:04:05Z"))
	if err != nil {
		return err
	}
	if applied == nil {
		applied = map[string]json.RawMessage{}
	}
	applied[migrationID] = ts
	return atomicfile.WriteJSON(path, stateFile{Version: stateVersion, Applied: applied}, atomicfile.Opts{})
}

// dirExists reports whether path exists, treating any stat error (including a
// permission failure on an unsearchable parent) as "missing" — spec 03§9.3's
// Path.exists() normalization, mirrored throughout the port (e.g.
// credstore.exists).
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
