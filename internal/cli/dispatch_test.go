package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// cleanHome points $HOME at a fresh temp dir with a fixed non-root uid and
// colors disabled, so switcher construction is deterministic and offline.
func cleanHome(t *testing.T) {
	t.Helper()
	testutil.Setenv(t, "HOME", t.TempDir())
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	testutil.Setenv(t, "NO_COLOR", "1")
	prev := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = prev })
}

// TestUpgradeDoesNotConstructSwitcher: --upgrade runs self-upgrade before the
// switcher is built (spec 08§5, test_upgrade_dispatches_without_constructing).
func TestUpgradeDoesNotConstructSwitcher(t *testing.T) {
	testutil.Setenv(t, "NO_COLOR", "1")
	constructed := false
	prevNew := newSwitcher
	newSwitcher = func(store.Options) (*core.Switcher, error) {
		constructed = true
		return nil, nil
	}
	t.Cleanup(func() { newSwitcher = prevNew })
	prevExe := exePath
	exePath = func() string { return "" } // force "unknown" install shape (no go install)
	t.Cleanup(func() { exePath = prevExe })

	for _, argv := range [][]string{{"upgrade"}, {"--upgrade"}} {
		constructed = false
		var out, errb bytes.Buffer
		run("cswap", argv, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
		if constructed {
			t.Errorf("%v constructed the switcher; --upgrade must not", argv)
		}
	}
}

// TestListJSONEndToEnd: `list --json` over a fresh home returns exactly one JSON
// document (empty account set) on stdout, nothing on stderr (DESIGN §3.2).
func TestListJSONEndToEnd(t *testing.T) {
	cleanHome(t)
	var out, errb bytes.Buffer
	code := run("cswap", []string{"list", "--json"}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not a single JSON document: %q (err=%v)", out.String(), err)
	}
	if _, ok := payload["schemaVersion"]; !ok {
		t.Errorf("payload missing schemaVersion: %v", payload)
	}
	if _, ok := payload["accounts"]; !ok {
		t.Errorf("payload missing accounts: %v", payload)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr must be empty in JSON mode, got %q", errb.String())
	}
}

// TestStatusJSONEndToEnd: `status --json` with no active account yields
// {"schemaVersion":1,"active":null} (spec 08§9.4).
func TestStatusJSONEndToEnd(t *testing.T) {
	cleanHome(t)
	var out, errb bytes.Buffer
	code := run("cswap", []string{"status", "--json"}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("stdout not JSON: %q", out.String())
	}
	if v, ok := payload["active"]; !ok || v != nil {
		t.Errorf("status payload active = %v (ok=%v), want nil", v, ok)
	}
}

// TestBareTTYGateOpensTUI: an empty argv on a both-ends TTY routes to --tui;
// with RunTUI unwired that prints the build notice and exits 1 (task mandate).
func TestBareTTYGateOpensTUI(t *testing.T) {
	cleanHome(t)
	prev := RunTUI
	RunTUI = nil
	t.Cleanup(func() { RunTUI = prev })
	var out, errb bytes.Buffer
	code := run("cswap", []string{}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, true, true)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (unwired TUI)", code)
	}
	if !strings.Contains(errb.String(), "TUI not wired in this build") {
		t.Errorf("stderr = %q, want the TUI-not-wired notice", errb.String())
	}
}

// TestBareTTYGateWiredTUI: with RunTUI wired, the bare TTY gate hands it the
// switcher and the dashboard start ("").
func TestBareTTYGateWiredTUI(t *testing.T) {
	cleanHome(t)
	var gotStart string
	var gotFacade any
	prev := RunTUI
	RunTUI = func(f any, start string) int {
		gotFacade = f
		gotStart = start
		return 7
	}
	t.Cleanup(func() { RunTUI = prev })
	var out, errb bytes.Buffer
	code := run("cswap", []string{}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, true, true)
	if code != 7 {
		t.Fatalf("exit = %d, want 7 (RunTUI return, stderr=%q)", code, errb.String())
	}
	if gotStart != "" {
		t.Errorf("start = %q, want \"\" (dashboard)", gotStart)
	}
	if _, ok := gotFacade.(*core.Switcher); !ok {
		t.Errorf("RunTUI facade = %T, want *core.Switcher", gotFacade)
	}
}

// TestWatchRoutesToWatchScreen: `watch` opens the TUI on the "watch" screen.
func TestWatchRoutesToWatchScreen(t *testing.T) {
	cleanHome(t)
	var gotStart string
	prev := RunTUI
	RunTUI = func(_ any, start string) int { gotStart = start; return 0 }
	t.Cleanup(func() { RunTUI = prev })
	var out, errb bytes.Buffer
	run("cswap", []string{"watch"}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	if gotStart != "watch" {
		t.Errorf("start = %q, want \"watch\"", gotStart)
	}
}

// TestMenubarNotAvailable: --menubar exits 1 with a message on every platform
// (DESIGN Deviation 5).
func TestMenubarNotAvailable(t *testing.T) {
	cleanHome(t)
	var out, errb bytes.Buffer
	code := run("cswap", []string{"menubar"}, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "menu bar") && !strings.Contains(errb.String(), "Menu bar") {
		t.Errorf("stderr = %q, want a menu-bar message", errb.String())
	}
}
