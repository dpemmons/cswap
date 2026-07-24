// Tests for the exported read-only quarantine reader (ReadQuarantine) and the
// StatePath helper the TUI reuses to shadow the engine's candidate exclusion
// (DESIGN A18). The reader is tolerant like readState and shares its one
// read helper, so these cases also pin that the shared refactor stays lenient.

package autoswitch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadQuarantine(t *testing.T) {
	cases := []struct {
		name    string
		write   bool   // false → no file on disk (missing-file case)
		content string // file body when write is true
		want    map[string]string
	}{
		{name: "missing file", write: false, want: map[string]string{}},
		{name: "invalid JSON", write: true, content: "{not json", want: map[string]string{}},
		{name: "non-object top level", write: true, content: `[1,2,3]`, want: map[string]string{}},
		{name: "no quarantine key", write: true,
			content: `{"schemaVersion":1,"lastSwitchAt":123}`, want: map[string]string{}},
		{name: "quarantine not an object", write: true,
			content: `{"quarantine":"nope"}`, want: map[string]string{}},
		{
			name:  "entries with and without reason",
			write: true,
			content: `{"schemaVersion":1,"quarantine":{` +
				`"2":{"email":"b@x","reason":"invalid_grant","at":"t"},` +
				`"3":{"email":"c@x"},` +
				`"4":{"email":"d@x","reason":""}}}`,
			want: map[string]string{"2": "invalid_grant", "3": "", "4": ""},
		},
		{
			name:    "non-object entry still counts as quarantined",
			write:   true,
			content: `{"quarantine":{"5":true,"6":{"reason":"identity-conflict"}}}`,
			want:    map[string]string{"5": "", "6": "identity-conflict"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), StateFilename)
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := ReadQuarantine(path); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReadQuarantine = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestStatePathJoin pins that the exported path helper joins under the backup
// dir the same way the engine defaults e.statePath, so the reader and the engine
// never target different files.
func TestStatePathJoin(t *testing.T) {
	dir := t.TempDir()
	if got, want := StatePath(dir), filepath.Join(dir, StateFilename); got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
}

// TestReadQuarantineMatchesQuarantinedSet pins that the exported reader reports
// exactly the slot set the engine excludes (quarantinedSet keys), for the same
// state map — the read seam and the exclusion seam agree.
func TestReadQuarantineMatchesQuarantinedSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFilename)
	body := `{"quarantine":{"2":{"reason":"invalid_grant"},"7":{"email":"g@x"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadQuarantine(path)
	want := quarantinedSet(readStateFile(path))
	if len(got) != len(want) {
		t.Fatalf("reader slots %v, exclusion set %v", got, want)
	}
	for num := range want {
		if _, ok := got[num]; !ok {
			t.Fatalf("reader missing quarantined slot %q (exclusion set has it)", num)
		}
	}
}
