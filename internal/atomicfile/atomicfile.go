// Package atomicfile is the single unified atomic-write helper.
//
// Implements spec 01§2.3 (_write_json), 03§9.3 and 07§9 (unifying _write_json,
// atomic_write_json, _atomic_b64_write, _atomic_write_file, _mark_applied).
// Sequence: temp-in-same-dir → chmod → rename, unlinking the temp on any error.
// Parent mkdir and the 0700/0600 chmods are skipped on Windows (per DESIGN A6
// gating via platform.IsWindows). WriteJSONValidated adds the sequence.json
// re-parse guard.
package atomicfile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// Opts controls the file and directory permission bits. A zero value uses the
// defaults 0600 (file) and 0700 (dir). Both are ignored on Windows.
type Opts struct {
	FileMode os.FileMode
	DirMode  os.FileMode
}

func (o Opts) fileMode() os.FileMode {
	if o.FileMode == 0 {
		return 0o600
	}
	return o.FileMode
}

func (o Opts) dirMode() os.FileMode {
	if o.DirMode == 0 {
		return 0o700
	}
	return o.DirMode
}

// Write atomically writes data to path: it creates a temp file in the target's
// directory, chmods it (non-Windows), then renames it over path. The temp file
// is removed if any step fails.
func Write(path string, data []byte, o Opts) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, o.dirMode()); err != nil {
		return err
	}
	if !platform.IsWindows() {
		// Best-effort: match Python chmod of the parent dir to 0700.
		_ = os.Chmod(dir, o.dirMode())
	}

	tmp, err := os.CreateTemp(dir, ".atomic-*.tmp")
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
	if !platform.IsWindows() {
		if err := os.Chmod(tmpName, o.fileMode()); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// marshalIndent2 renders v the way Python's json.dumps(v, indent=2) does: a
// two-space indent, a space after each colon, no trailing newline. HTML escaping
// is disabled so <, >, & survive as themselves — Python's json.dumps does not
// HTML-escape, and mapping keys are filesystem paths that can legally contain
// those characters, so json.MarshalIndent's default SetEscapeHTML(true) would
// diverge byte-for-byte from a Python-produced file. Matches the other on-disk
// writers in this port (store.marshalIndent2, switching, transfer, ccfile).
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

// WriteJSON marshals v with two-space indentation (matching json.dumps
// indent=2, no trailing newline) and writes it atomically.
func WriteJSON(path string, v any, o Opts) error {
	data, err := marshalIndent2(v)
	if err != nil {
		return err
	}
	return Write(path, data, o)
}

// WriteJSONValidated marshals v, writes the temp file, re-parses it, and only
// then renames it over path. On a parse failure it removes the temp and returns
// cerr.Config("Generated invalid JSON"). Used for sequence.json.
func WriteJSONValidated(path string, v any, o Opts) error {
	data, err := marshalIndent2(v)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, o.dirMode()); err != nil {
		return err
	}
	if !platform.IsWindows() {
		_ = os.Chmod(dir, o.dirMode())
	}

	tmp, err := os.CreateTemp(dir, ".atomic-*.tmp")
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

	// Re-parse the bytes we just wrote before publishing.
	raw, err := os.ReadFile(tmpName)
	if err != nil {
		return err
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return cerr.Config("Generated invalid JSON").Wrap(err)
	}

	if !platform.IsWindows() {
		if err := os.Chmod(tmpName, o.fileMode()); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
