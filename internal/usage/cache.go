// Trivial TTL JSON cache helper.
//
// Implements spec 04§4 (cache.py). On-disk format:
// {"timestamp": <epoch seconds float>, "data": <any JSON>}. The (value, ok)
// return distinguishes a genuine cached null from a miss (Python's MISSING
// sentinel). Per Amendment A8 the writer is DELIBERATELY non-atomic and
// non-chmod'd (a plain write, NOT routed through atomicfile) — this is the one
// low-value cache in the codebase.
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ReadCache returns the cached data if the file exists, parses, carries a
// numeric timestamp, and is within ttl of now (epoch seconds). ok is false on
// any miss (missing/corrupt/expired/malformed), matching Python's MISSING
// return, so a genuine cached null is distinguishable from a miss.
func ReadCache(path string, ttl, now float64) (data any, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, false
	}
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, false
	}
	ts := numPtr(m["timestamp"])
	if ts == nil {
		return nil, false
	}
	if now-*ts < ttl {
		d, present := m["data"]
		if !present {
			return nil, false
		}
		return d, true
	}
	return nil, false
}

// WriteCache writes {"timestamp": now, "data": data} to path (04§4). DELIBERATE
// (Amendment A8): a plain, non-atomic, non-chmod'd write — do NOT route this
// through atomicfile. The parent directory is created if absent.
func WriteCache(path string, data any, now float64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{"timestamp": now, "data": data})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
