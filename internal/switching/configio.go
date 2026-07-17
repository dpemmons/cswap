// configio.go — the live ~/.claude.json read/write helpers used under the
// config lock, mirroring switcher.py's _read_json / _write_json and the
// oauthAccount splice.
//
// The atomic writer deliberately does NOT reuse internal/atomicfile (which
// chmods the parent 0700): ~/.claude.json's parent is $HOME. It mirrors Python
// _write_json exactly — mkdir parent with default perms, temp sibling, rename,
// then chmod the file 0600 (non-Windows) — so the rename is the last fallible op.
package switching

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// claudeConfigPath is _get_claude_config_path() = get_global_config_path().
func claudeConfigPath() string { return paths.GetGlobalConfigPath() }

// readConfigText reads the live config's raw text and whether it exists (for the
// rollback snapshot). A present-but-unreadable file surfaces the error.
func readConfigText() (text string, exists bool, err error) {
	data, err := os.ReadFile(claudeConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// readConfigJSON reads and parses the live config, returning nil on absence or a
// parse failure (mirroring _read_json, which logs "Invalid JSON in {path}" and
// swallows to None).
func readConfigJSON(s *store.Store) map[string]any {
	data, err := os.ReadFile(claudeConfigPath())
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		if s.Log != nil {
			s.Log.Warningf("Invalid JSON in %s", claudeConfigPath())
		}
		return nil
	}
	return m
}

// writeConfigJSON renders data as two-space-indented JSON, rejects a
// non-round-tripping result with ConfigError("Generated invalid JSON"), then
// writes it atomically (mirrors _write_json).
func writeConfigJSON(data map[string]any) error {
	encoded, err := marshalIndent2(data)
	if err != nil {
		return err
	}
	var probe any
	if err := json.Unmarshal(encoded, &probe); err != nil {
		return cerr.Config("Generated invalid JSON").Wrap(err)
	}
	return atomicConfigWrite(claudeConfigPath(), encoded)
}

// writeConfigText restores raw config text atomically (rollback path; Python
// does a direct write_text + chmod 0600, atomic here is a safe superset).
func writeConfigText(text string) error {
	return atomicConfigWrite(claudeConfigPath(), []byte(text))
}

// marshalIndent2 renders v the way Python's json.dumps(v, indent=2) does — a
// two-space indent, no trailing newline, no HTML escaping of <>&.
func marshalIndent2(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

// atomicConfigWrite writes to path via a temp sibling + rename, then chmods the
// file 0600 (non-Windows). It mkdirs the parent with default perms but never
// chmods it — $HOME must keep its mode.
func atomicConfigWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	if !platform.IsWindows() {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}
