package testutil

import (
	"os"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/paths"
)

func TestBuildFixtureHomeMaterializes(t *testing.T) {
	fh := BuildFixtureHome(t)

	// Dotfiles renamed correctly.
	for _, p := range []string{fh.GlobalConfig, fh.CredentialsFile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected fixture file %s: %v", p, err)
		}
	}
	// Backup root carries the Python data.
	if _, err := os.Stat(fh.BackupRoot + "/sequence.json"); err != nil {
		t.Errorf("sequence.json not materialized: %v", err)
	}

	// Path resolution lands on the materialized tree (HOME set, XDG/CCD unset).
	if got := paths.GetGlobalConfigPath(); got != fh.GlobalConfig {
		t.Errorf("GetGlobalConfigPath = %q, want %q", got, fh.GlobalConfig)
	}
	if got := paths.GetBackupRoot(); got != fh.BackupRoot {
		t.Errorf("GetBackupRoot = %q, want %q", got, fh.BackupRoot)
	}
	if got := paths.GetCredentialsPath(); got != fh.CredentialsFile {
		t.Errorf("GetCredentialsPath = %q, want %q", got, fh.CredentialsFile)
	}
}

func TestEnvSaveRestore(t *testing.T) {
	Setenv(t, "CSWAP_TEST_VAR", "value1")
	if os.Getenv("CSWAP_TEST_VAR") != "value1" {
		t.Fatal("Setenv did not set")
	}
	Unsetenv(t, "CSWAP_TEST_VAR")
	if _, ok := os.LookupEnv("CSWAP_TEST_VAR"); ok {
		t.Error("Unsetenv did not clear the var")
	}
}

func TestFixedClock(t *testing.T) {
	c := FixedClock(t, "2026-07-17T08:46:09Z")
	if c.Now().Unix() != 1784277969 {
		t.Errorf("FixedClock Now = %d", c.Now().Unix())
	}
}
