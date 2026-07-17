package logging

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// TestFormatLineExact pins the on-disk contract format including the COMMA
// milliseconds separator (DESIGN A4).
func TestFormatLineExact(t *testing.T) {
	// 2026-07-17 01:45:45,258 in a fixed zone.
	loc := time.FixedZone("test", 0)
	ts := time.Date(2026, 7, 17, 1, 45, 45, 258_000_000, loc)
	got := FormatLine(Record{Time: ts, Level: LevelInfo, Message: "Switched from account 2 to 1"})
	want := "2026-07-17 01:45:45,258 - INFO - Switched from account 2 to 1"
	if got != want {
		t.Errorf("FormatLine = %q, want %q", got, want)
	}
}

// TestGoldenLogRoundTrip parses every line of the Python-produced fixture log and
// re-formats it, asserting byte identity — proving the format contract.
func TestGoldenLogRoundTrip(t *testing.T) {
	path := filepath.Join(testutil.FixturesDir(t), "claude-swap-data", "claude-swap.log")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	n := 0
	sawSwitch := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		rec, ok := ParseLine(line)
		if !ok {
			t.Fatalf("line %d did not parse: %q", n, line)
		}
		if FormatLine(rec) != line {
			t.Errorf("round-trip mismatch:\n got %q\nwant %q", FormatLine(rec), line)
		}
		if strings.HasPrefix(rec.Message, "Switched from account") {
			sawSwitch = true
			if rec.Message != "Switched from account 2 to 1" || rec.Level != LevelInfo {
				t.Errorf("switch line = %q level %v", rec.Message, rec.Level)
			}
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no lines parsed")
	}
	if !sawSwitch {
		t.Error("fixture did not contain the 'Switched from account 2 to 1' line")
	}
}

func TestParseLineRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "no dashes here", "2026-07-17 01:45:45,258 - NOTALEVEL - x", "bad-ts - INFO - x"} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("ParseLine(%q) = ok, want false", bad)
		}
	}
}

func TestLazyDirCreation(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "backuproot")
	clk := clock.NewFake(time.Date(2026, 7, 17, 1, 45, 45, 258_000_000, time.UTC))
	lg := NewWithClock(dir, false, clk)

	// Nothing written yet → directory must not exist (no-op-run invariant).
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("log dir created eagerly (err=%v)", err)
	}

	lg.Info("hello")
	logPath := filepath.Join(dir, "claude-swap.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file not created after first write: %v", err)
	}
	want := "2026-07-17 01:45:45,258 - INFO - hello\n"
	if string(data) != want {
		t.Errorf("log content = %q, want %q", data, want)
	}
}

func TestDebugGatedByLevel(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Unix(0, 0).UTC())
	lg := NewWithClock(dir, false, clk) // INFO level
	lg.Debug("should be dropped")
	lg.Info("kept")
	data, _ := os.ReadFile(filepath.Join(dir, "claude-swap.log"))
	if strings.Contains(string(data), "should be dropped") {
		t.Error("DEBUG record written at INFO level")
	}
	if !strings.Contains(string(data), "kept") {
		t.Error("INFO record missing")
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-swap.log")
	w := newRotatingWriter(path, 64, 3)            // tiny cap to force rotation
	line := []byte("0123456789ABCDEF0123456789\n") // 27 bytes
	for i := 0; i < 6; i++ {
		if err := w.write(line); err != nil {
			t.Fatal(err)
		}
	}
	// With a 64-byte cap and 27-byte lines, rotation must have produced .1.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated file %s.1: %v", path, err)
	}
	// Never more backups than backupCount.
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Errorf("backup .4 exists but backupCount is 3")
	}
}
