package cli

import (
	"bytes"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// runCLI drives the front controller with buffered streams and no TTY, unless
// overridden. It returns (exit code, stdout, stderr).
func runCLI(t *testing.T, argv []string, stdinTTY, stdoutTTY bool) (int, string, string) {
	t.Helper()
	testutil.Setenv(t, "NO_COLOR", "1")
	var out, errb bytes.Buffer
	code := run("cswap", argv, ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, stdinTTY, stdoutTTY)
	return code, out.String(), errb.String()
}

// TestNoCommandNonTTY: bare invocation in a non-TTY exits 2 with the clean
// message and never leaks legacy flag names (spec 08§4.1/§14).
func TestNoCommandNonTTY(t *testing.T) {
	code, _, errStr := runCLI(t, []string{}, false, false)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errStr, "no command given") {
		t.Errorf("stderr = %q, want it to contain %q", errStr, "no command given")
	}
	if !strings.Contains(errStr, "cswap help") {
		t.Errorf("stderr = %q, want it to mention 'cswap help'", errStr)
	}
	for _, leaked := range []string{"--add-account", "one of the arguments"} {
		if strings.Contains(errStr, leaked) {
			t.Errorf("stderr must not leak %q; got %q", leaked, errStr)
		}
	}
}

// TestCrossFlagValidation pins every exit-2 message (spec 08§4, in order).
func TestCrossFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		msg  string
	}{
		{"token-status without list", []string{"--status", "--token-status"}, "--token-status can only be used with 'list'"},
		{"json without command surface", []string{"--purge", "--json"}, "--json can only be used with 'list', 'status', or 'switch'"},
		{"json with token-status", []string{"--list", "--token-status", "--json"}, "--token-status cannot be combined with --json"},
		{"strategy without switch", []string{"--list", "--strategy", "best"}, "--strategy can only be used with bare 'switch'"},
		{"model without strategy", []string{"--switch", "--model", "Fable"}, "--model can only be used with 'switch --strategy best' or 'switch --strategy next-available'"},
		{"slot without add", []string{"--list", "--slot", "3"}, "--slot can only be used with 'add' or 'add-token'"},
		{"email without add-token", []string{"--add-account", "--email", "me@x.com"}, "--email can only be used with 'add-token'"},
		{"account without export", []string{"--list", "--account", "2"}, "--account can only be used with 'export'"},
		{"alias without add", []string{"--list", "--alias", "dev"}, "--alias can only be used with 'add'"},
		{"force without import or switch-to", []string{"--list", "--force"}, "--force can only be used with 'import' or 'switch <num|email>'"},
		{"full without export", []string{"--list", "--full"}, "--full can only be used with 'export'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errStr := runCLI(t, tc.argv, false, false)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
			}
			if !strings.Contains(errStr, tc.msg) {
				t.Errorf("stderr = %q, want it to contain %q", errStr, tc.msg)
			}
		})
	}
}

// TestMutuallyExclusiveGroup: two legacy flags → argparse "not allowed" (08§3.2).
func TestMutuallyExclusiveGroup(t *testing.T) {
	code, _, errStr := runCLI(t, []string{"--export", "/p", "--import", "/q"}, false, false)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errStr, "not allowed with argument") {
		t.Errorf("stderr = %q, want 'not allowed with argument'", errStr)
	}
}

// TestStrategyInvalidChoice: an out-of-set --strategy value is an exit-2 choice
// error (spec 08§14).
func TestStrategyInvalidChoice(t *testing.T) {
	code, _, errStr := runCLI(t, []string{"--switch", "--strategy", "bogus"}, false, false)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errStr, "invalid choice") {
		t.Errorf("stderr = %q, want 'invalid choice'", errStr)
	}
}

// TestSlotInvalidInt: a non-integer --slot is an exit-2 error.
func TestSlotInvalidInt(t *testing.T) {
	code, _, errStr := runCLI(t, []string{"--add-account", "--slot", "abc"}, false, false)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errStr, "invalid int value") {
		t.Errorf("stderr = %q, want 'invalid int value'", errStr)
	}
}

// TestNegativeNumberValues: argparse's _negative_number_matcher lets tokens like
// "-1" / "-5" be consumed as flag values (no cswap option looks like a negative
// number), so they flow to domain logic instead of being rejected as options
// (spec 08§15). A "-1.5" still fails the int parse — but as "invalid int value",
// not "expected one argument", proving it was consumed as the value.
func TestNegativeNumberValues(t *testing.T) {
	// --slot -1.5 is consumed then rejected by strconv.Atoi.
	code, _, errStr := runCLI(t, []string{"--add-account", "--slot", "-1.5"}, false, false)
	if code != 2 {
		t.Fatalf("--slot -1.5: exit = %d, want 2 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(errStr, "invalid int value: '-1.5'") {
		t.Errorf("--slot -1.5: stderr = %q, want \"invalid int value: '-1.5'\"", errStr)
	}
	if strings.Contains(errStr, "expected one argument") {
		t.Errorf("--slot -1.5: stderr = %q, must not reject the value as a missing argument", errStr)
	}

	// --slot -1 and --switch-to -5 are consumed as values and must not raise the
	// "expected one argument" usage error from the parse layer.
	for _, argv := range [][]string{
		{"--add-account", "--slot", "-1"},
		{"--switch-to", "-5"},
	} {
		_, _, errStr := runCLI(t, argv, false, false)
		if strings.Contains(errStr, "expected one argument") {
			t.Errorf("%v: stderr = %q, must consume the negative number as a value", argv, errStr)
		}
	}
}

// TestVersionFlag prints "<prog> <version>" and exits 0 (DESIGN A5, spec 08§14).
func TestVersionFlag(t *testing.T) {
	code, out, _ := runCLI(t, []string{"--version"}, false, false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "cswap ") {
		t.Errorf("version output = %q, want it to start with 'cswap '", out)
	}
	if !strings.Contains(out, "0.0.0-dev") {
		t.Errorf("version output = %q, want the default build version", out)
	}
}
