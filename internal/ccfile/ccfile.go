// Package ccfile is Claude Code's own file I/O: the global config
// (~/.claude.json) and the active-credentials file (~/.claude/.credentials.json).
//
// Implements spec 03§3 (external Claude Code storage), 03§5.5 (writing the
// active credential and the key-scoped ~/.claude.json RMW). All paths resolve
// through the paths package, so CLAUDE_CONFIG_DIR is honored transparently.
//
// The atomic writers here deliberately do NOT reuse internal/atomicfile: that
// helper chmods the parent directory to 0700, but Python's
// _update_global_config / _write_active_credentials_file only chmod the file
// (0600) and never touch the parent — critical for ~/.claude.json, whose parent
// is the user's $HOME. This package mirrors Python exactly: mkdir the parent
// (default perms), write a temp sibling, rename, then chmod the file (non-Windows).
package ccfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// ReadGlobalConfig reads and parses ~/.claude.json (paths.GetGlobalConfigPath),
// preserving every key. It returns (nil, nil) when the file is absent (and when
// its content is the JSON literal null, mirroring Python's isinstance(dict)
// guard), (nil, err) on a read failure or when the content is not a JSON object.
//
// Python's _read_global_config swallows those errors to None with a warning log;
// that log+swallow is the caller's job (credstore), so this primitive surfaces
// the error and lets the caller decide.
func ReadGlobalConfig() (map[string]any, error) {
	path := paths.GetGlobalConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// UpdateGlobalConfig applies mutate to the current ~/.claude.json contents and
// writes the result back atomically with 0600 perms, preserving every key that
// mutate does not touch (oauthAccount, projects, settings, ...).
//
// Mirrors Python _update_global_config: a missing or unparseable config starts
// from an empty object (the `_read_global_config() or {}` idiom), so a corrupt
// file is replaced rather than aborting.
func UpdateGlobalConfig(mutate func(map[string]any)) error {
	path := paths.GetGlobalConfigPath()
	data := readLenient(path)
	if data == nil {
		data = map[string]any{}
	}
	mutate(data)
	encoded, err := marshalIndent2(data)
	if err != nil {
		return err
	}
	return atomicWrite(path, encoded)
}

// ReadCredentialsFile reads the raw text of ~/.claude/.credentials.json. The
// returned exists flag is false (with a nil error) when the file is absent; the
// text is returned verbatim (callers apply any strip/non-blank check).
func ReadCredentialsFile() (raw string, exists bool, err error) {
	path := paths.GetCredentialsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// WriteCredentialsFile atomically writes raw verbatim to
// ~/.claude/.credentials.json (0600), creating the config-home directory if
// needed. The payload is stored raw — no JSON re-encoding — matching Claude
// Code's own file, whose changed mtime is what invalidates its cached token.
func WriteCredentialsFile(raw string) error {
	return atomicWrite(paths.GetCredentialsPath(), []byte(raw))
}

// SpliceOAuthAccount parses configText, replaces its "oauthAccount" with oauth,
// and returns the re-serialized config text (two-space indent, no trailing
// newline). Every other key of configText is preserved. Empty or non-object
// configText starts from an empty object.
//
// This is the pure form of the switch-time config splice: the caller reads the
// live ~/.claude.json, splices in the target account's stored oauthAccount, and
// writes the result back under the config lock.
func SpliceOAuthAccount(configText string, oauth map[string]any) (string, error) {
	data := map[string]any{}
	if strings.TrimSpace(configText) != "" {
		// A non-object top level leaves data as the empty object we started with
		// (json.Unmarshal of e.g. "null" is a no-op for a map target).
		if err := json.Unmarshal([]byte(configText), &data); err != nil {
			return "", err
		}
		if data == nil {
			data = map[string]any{}
		}
	}
	data["oauthAccount"] = oauth
	encoded, err := marshalIndent2(data)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ReadOAuthIdentity reads the active login identity from ~/.claude.json's
// oauthAccount. ok is false when the file is absent/unparseable, the
// oauthAccount is missing, or emailAddress is blank. A null or missing
// organizationUuid yields "" (personal account), mirroring Python's
// `oauth.get("organizationUuid", "") or ""`.
func ReadOAuthIdentity() (email, orgUUID string, ok bool) {
	m := readLenient(paths.GetGlobalConfigPath())
	if m == nil {
		return "", "", false
	}
	// A nil map (missing/typed-wrong oauthAccount) reads as zero values below.
	oauth, _ := m["oauthAccount"].(map[string]any)
	email, _ = oauth["emailAddress"].(string)
	if email == "" {
		return "", "", false
	}
	orgUUID, _ = oauth["organizationUuid"].(string)
	return email, orgUUID, true
}

// readLenient reads and parses path into a JSON object, returning nil on any
// failure (absent, unreadable, malformed, or a non-object top level). It mirrors
// Python _read_json / _read_global_config, which swallow every error to None.
func readLenient(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// marshalIndent2 renders v as two-space-indented JSON with no trailing newline,
// matching Python's json.dumps(data, indent=2). HTML escaping is disabled so
// characters like <, >, & survive as themselves rather than <-style escapes
// (Python does not HTML-escape). Non-ASCII is emitted as UTF-8 rather than
// Python's \uXXXX form; both parse back identically.
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

// atomicWrite writes data to path via a temp sibling + rename, then chmods the
// final file to 0600 (skipped on Windows). It mkdirs the parent with default
// permissions but never chmods it — mirroring Python _update_global_config /
// _write_active_credentials_file, which must not alter $HOME's mode.
func atomicWrite(path string, data []byte) error {
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
