// The write-only unclaimed-credential stash: forensic safety copies of live
// credential bytes a switch positively attributed to someone other than the
// outgoing slot. Always 0600 base64 files on every platform (never the Keychain,
// so a flaky Keychain can't start blocking switches). The entry file is written
// before the manifest — an entry without metadata is recoverable, a manifest row
// without bytes is not — and any failure is raised (a successful stash is the
// license to overwrite the live store).
//
// Implements spec 03§5.9 and 01§3.6.

package credstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
)

// stashDoc is the manifest document. The struct field order fixes the JSON key
// order (schemaVersion before entries) that a map cannot guarantee.
type stashDoc struct {
	SchemaVersion int            `json:"schemaVersion"`
	Entries       map[string]any `json:"entries"`
}

func (s *FileKeychainStore) stashManifestPath() string {
	return filepath.Join(s.credentialsDir, ".unclaimed-manifest.json")
}

func (s *FileKeychainStore) stashEntryPath(entryID string) string {
	return filepath.Join(s.credentialsDir, ".unclaimed-"+entryID+".enc")
}

// WriteUnclaimed stashes credentials of unknown provenance and returns the entry
// id (spec 03§5.9). The entry file is written before the manifest; any failure
// is raised.
func (s *FileKeychainStore) WriteUnclaimed(credentials string, context map[string]any) (string, error) {
	ts := s.clk.Now().UTC().Format("20060102T150405")
	sum := sha256.Sum256([]byte(credentials))
	digest := hex.EncodeToString(sum[:])[:12]
	nonceBytes := make([]byte, 3)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	entryID := ts + "-" + digest + "-" + hex.EncodeToString(nonceBytes)

	// Entry file first: recoverable even without manifest metadata.
	if err := s.atomicB64Write(s.stashEntryPath(entryID), credentials); err != nil {
		return "", err
	}

	entries := s.readStashManifest()
	row := map[string]any{"createdAt": s.clk.Now().UTC().Format("2006-01-02T15:04:05Z")}
	for k, v := range context {
		row[k] = v
	}
	entries[entryID] = row
	if err := s.writeStashManifest(entries); err != nil {
		return "", err
	}
	return entryID, nil
}

// readStashManifest returns the manifest's inner entries map (or an empty map),
// swallowing an absent/unreadable/corrupt manifest with a warning (spec 03§5.9).
func (s *FileKeychainStore) readStashManifest() map[string]any {
	path := s.stashManifestPath()
	if !exists(path) {
		return map[string]any{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		s.log.Warningf("Failed to read unclaimed manifest: %v", err)
		return map[string]any{}
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		s.log.Warningf("Failed to read unclaimed manifest: %v", err)
		return map[string]any{}
	}
	if entries, ok := data["entries"].(map[string]any); ok {
		return entries
	}
	return map[string]any{}
}

// writeStashManifest writes the manifest atomically, preserving a corrupt
// existing manifest aside (its rows are classification evidence) rather than
// clobbering it (spec 03§5.9).
func (s *FileKeychainStore) writeStashManifest(entries map[string]any) error {
	path := s.stashManifestPath()
	if exists(path) {
		corrupt := false
		if raw, err := os.ReadFile(path); err != nil {
			corrupt = true
		} else {
			var probe any
			if json.Unmarshal(raw, &probe) != nil {
				corrupt = true
			}
		}
		if corrupt {
			aside := fmt.Sprintf("%s.corrupt-%d", path, s.clk.Now().Unix())
			if err := os.Rename(path, aside); err != nil {
				s.log.Warningf("Could not preserve corrupt unclaimed manifest: %v", err)
			} else {
				s.log.Warningf("Unreadable unclaimed manifest preserved as %s", filepath.Base(aside))
			}
		}
	}
	return atomicfile.WriteJSON(path, stashDoc{SchemaVersion: 1, Entries: entries}, atomicfile.Opts{})
}

// ListUnclaimed returns manifest rows by id, merged with any orphaned entry
// files (glob .unclaimed-*.enc) defaulted to {"createdAt": nil} (spec 03§5.9).
func (s *FileKeychainStore) ListUnclaimed() (map[string]map[string]any, error) {
	entries := s.readStashManifest()
	result := make(map[string]map[string]any, len(entries))
	for id, v := range entries {
		if m, ok := v.(map[string]any); ok {
			result[id] = m
		} else {
			result[id] = map[string]any{}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(s.credentialsDir, ".unclaimed-*.enc"))
	for _, p := range matches {
		name := filepath.Base(p)
		id := strings.TrimSuffix(strings.TrimPrefix(name, ".unclaimed-"), ".enc")
		if _, ok := result[id]; !ok {
			result[id] = map[string]any{"createdAt": nil}
		}
	}
	return result, nil
}
