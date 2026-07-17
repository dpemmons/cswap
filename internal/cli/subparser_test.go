package cli

import (
	"bytes"
	"strings"
	"testing"
)

func runSub(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run("cswap", argv, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	return code, out.String(), errb.String()
}

// TestAliasArgValidation pins the three exit-2 argument errors (spec 08§7.4).
// These fire before switcher construction, so no home is needed.
func TestAliasArgValidation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		msg  string
	}{
		{"unset with name", []string{"alias", "2", "dev", "--unset"}, "--unset does not take a NAME argument"},
		{"unset without target", []string{"alias", "--unset"}, "NUM|EMAIL is required with --unset"},
		{"set without name", []string{"alias", "2"}, "NAME is required (or pass --unset to remove the alias)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errStr := runSub(t, tc.argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
			}
			if !strings.Contains(errStr, tc.msg) {
				t.Errorf("stderr = %q, want %q", errStr, tc.msg)
			}
		})
	}
}

// TestSwapMissingArgs: swap requires two positionals (exit 2). The message lists
// only the still-missing metavars, mirroring argparse (spec 08§7.5).
func TestSwapMissingArgs(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		msg  string
	}{
		{"one arg", []string{"swap", "1"}, "the following arguments are required: NUM|EMAIL|ALIAS"},
		{"no args", []string{"swap"}, "the following arguments are required: NUM|EMAIL|ALIAS, NUM|EMAIL|ALIAS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errStr := runSub(t, tc.argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
			}
			if !strings.Contains(errStr, tc.msg) {
				t.Errorf("stderr = %q, want %q", errStr, tc.msg)
			}
		})
	}
}

// TestMoveMissingArgs: move requires account + slot (exit 2). The message uses
// the metavars NUM|EMAIL|ALIAS / SLOT and lists only the still-missing ones,
// mirroring argparse (spec 08§7.6).
func TestMoveMissingArgs(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		msg  string
	}{
		{"one arg", []string{"move", "2"}, "the following arguments are required: SLOT"},
		{"no args", []string{"move"}, "the following arguments are required: NUM|EMAIL|ALIAS, SLOT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errStr := runSub(t, tc.argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
			}
			if !strings.Contains(errStr, tc.msg) {
				t.Errorf("stderr = %q, want %q", errStr, tc.msg)
			}
		})
	}
}

// TestRunTailSplit: `run 2 -- --bogus` forwards the tail unparsed; a bad flag
// BEFORE the "--" is an exit-2 error. Both go through runCommand's head parse.
func TestRunBadFlagBeforeDashDash(t *testing.T) {
	cleanHome(t)
	code, _, errStr := runSub(t, "run", "2", "--bogus")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(errStr, "unrecognized arguments") {
		t.Errorf("stderr = %q, want 'unrecognized arguments'", errStr)
	}
}

// TestRunExtraPositional: two positional accounts is an exit-2 error.
func TestRunExtraPositional(t *testing.T) {
	cleanHome(t)
	code, _, errStr := runSub(t, "run", "2", "3")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(errStr, "unrecognized arguments") {
		t.Errorf("stderr = %q, want 'unrecognized arguments'", errStr)
	}
}

// TestAutoBadValue: a non-numeric --threshold is an exit-2 error (before the
// switcher is built).
func TestAutoBadThreshold(t *testing.T) {
	code, _, errStr := runSub(t, "auto", "--threshold", "high")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(errStr, "invalid float value") {
		t.Errorf("stderr = %q, want 'invalid float value'", errStr)
	}
}

// TestAutoUnknownFlag: an unknown auto flag is an exit-2 error.
func TestAutoUnknownFlag(t *testing.T) {
	code, _, errStr := runSub(t, "auto", "--bogus")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(errStr, "unrecognized arguments") {
		t.Errorf("stderr = %q, want 'unrecognized arguments'", errStr)
	}
}
