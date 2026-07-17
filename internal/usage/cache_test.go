package usage

import (
	"os"
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// TestCacheRoundTrip covers write/read within TTL and the null-vs-miss
// distinction (04§4).
func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "update_check.json")

	if err := WriteCache(path, "0.21.0", 1000); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadCache(path, 100, 1050) // 50s < 100 ttl
	if !ok || got != "0.21.0" {
		t.Errorf("read within ttl = %v/%v, want 0.21.0/true", got, ok)
	}

	// Expired -> miss.
	if _, ok := ReadCache(path, 100, 1200); ok { // 200s > 100 ttl
		t.Error("expired entry should read as a miss")
	}

	// A genuine cached null is distinguishable from a miss.
	if err := WriteCache(path, nil, 2000); err != nil {
		t.Fatal(err)
	}
	got, ok = ReadCache(path, 100, 2050)
	if !ok {
		t.Error("cached null should read as a hit (ok=true)")
	}
	if got != nil {
		t.Errorf("cached null value = %v, want nil", got)
	}
}

func TestReadCacheMisses(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
	}{
		{"corrupt", `{not json`},
		{"not-a-dict", `"hello"`},
		{"missing-timestamp", `{"data":"x"}`},
		{"non-numeric-timestamp", `{"timestamp":"soon","data":"x"}`},
		{"missing-data", `{"timestamp":1000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok := ReadCache(path, 1e9, 1000); ok {
				t.Errorf("%s should read as a miss", tc.name)
			}
		})
	}

	if _, ok := ReadCache(filepath.Join(dir, "nope.json"), 1e9, 1000); ok {
		t.Error("missing file should read as a miss")
	}
}

// TestReadCacheParsesPythonFixture proves interop with the Python-produced
// update_check.json cache format (DESIGN A6, spec 04§4).
func TestReadCacheParsesPythonFixture(t *testing.T) {
	path := filepath.Join(testutil.FixturesDir(t), "claude-swap-data", "cache", "update_check.json")
	// Fixture timestamp is 1784277945.4183247, data "0.21.0". Read within a
	// generous TTL relative to that timestamp.
	got, ok := ReadCache(path, 86400, 1784277950)
	if !ok {
		t.Fatal("fixture update_check.json should parse as a hit")
	}
	if got != "0.21.0" {
		t.Errorf("fixture data = %v, want 0.21.0", got)
	}
}
