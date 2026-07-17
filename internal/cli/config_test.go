package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runConfig drives `cswap config <args>` over a clean home with buffered I/O.
func runConfig(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run("cswap", append([]string{"config"}, args...),
		ioStreams{in: strings.NewReader(""), out: &out, err: &errb}, false, false)
	return code, out.String(), errb.String()
}

// TestConfigListShowsAllDefaults: no args lists all 8 keys, each marked
// "(default)" on a fresh home (spec 08§7.8/§14).
func TestConfigListShowsAllDefaults(t *testing.T) {
	cleanHome(t)
	code, out, errStr := runConfig(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errStr)
	}
	if n := strings.Count(out, "(default)"); n != 8 {
		t.Errorf("(default) count = %d, want 8\n%s", n, out)
	}
	if !strings.Contains(out, "autoswitch.threshold") {
		t.Errorf("list missing autoswitch.threshold:\n%s", out)
	}
}

// TestConfigListJSON: --json list has 8 settings with key/value/isSet.
func TestConfigListJSON(t *testing.T) {
	cleanHome(t)
	code, out, errStr := runConfig(t, "--json", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errStr)
	}
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Settings      []struct {
			Key   string `json:"key"`
			IsSet bool   `json:"isSet"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("stdout not JSON: %q", out)
	}
	if payload.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", payload.SchemaVersion)
	}
	if len(payload.Settings) != 8 {
		t.Errorf("settings len = %d, want 8", len(payload.Settings))
	}
	for _, s := range payload.Settings {
		if s.IsSet {
			t.Errorf("fresh home: key %q reported isSet=true", s.Key)
		}
	}
}

// TestConfigSetThenGet: set persists one key, get reads it back, and the row is
// no longer marked default (spec 08§7.8/§14).
func TestConfigSetThenGet(t *testing.T) {
	cleanHome(t)
	code, out, errStr := runConfig(t, "set", "autoswitch.threshold", "80")
	if code != 0 {
		t.Fatalf("set exit = %d, want 0 (stderr=%q)", code, errStr)
	}
	if !strings.Contains(out, "autoswitch.threshold = 80") {
		t.Errorf("set output = %q, want 'autoswitch.threshold = 80'", out)
	}

	code, out, _ = runConfig(t, "get", "autoswitch.threshold")
	if code != 0 {
		t.Fatalf("get exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "80" {
		t.Errorf("get output = %q, want '80'", out)
	}

	// The set key is no longer marked default; the other 7 still are.
	code, out, _ = runConfig(t)
	if n := strings.Count(out, "(default)"); n != 7 {
		t.Errorf("(default) count after set = %d, want 7\n%s", n, out)
	}
}

// TestConfigGetJSONBothOrders: `get X --json` and `--json get X` behave alike.
func TestConfigGetJSONBothOrders(t *testing.T) {
	cleanHome(t)
	for _, args := range [][]string{
		{"get", "autoswitch.threshold", "--json"},
		{"--json", "get", "autoswitch.threshold"},
	} {
		code, out, errStr := runConfig(t, args...)
		if code != 0 {
			t.Fatalf("%v exit = %d (stderr=%q)", args, code, errStr)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("%v stdout not JSON: %q", args, out)
		}
		if payload["key"] != "autoswitch.threshold" {
			t.Errorf("%v key = %v", args, payload["key"])
		}
	}
}

// TestConfigJSONWithMutatingActionExit2: --json with set/unset is exit 2.
func TestConfigJSONWithMutatingActionExit2(t *testing.T) {
	cleanHome(t)
	code, _, errStr := runConfig(t, "--json", "set", "autoswitch.threshold", "80")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errStr, "--json can only be used with list or get") {
		t.Errorf("stderr = %q", errStr)
	}
}

// TestConfigSetOutOfRange: exit 1 with the range message (spec 08§14).
func TestConfigSetOutOfRange(t *testing.T) {
	cleanHome(t)
	code, _, errStr := runConfig(t, "set", "autoswitch.threshold", "200")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errStr, "between 50 and 99.9") {
		t.Errorf("stderr = %q, want 'between 50 and 99.9'", errStr)
	}
}

// TestConfigGetUnknownKey: exit 1 with the unknown-setting message.
func TestConfigGetUnknownKey(t *testing.T) {
	cleanHome(t)
	code, _, errStr := runConfig(t, "get", "autoswitch.bogus")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errStr, "unknown setting") {
		t.Errorf("stderr = %q, want 'unknown setting'", errStr)
	}
}

// TestConfigGetUnknownKeyJSON: JSON error envelope on `--json get bogus`.
func TestConfigGetUnknownKeyJSON(t *testing.T) {
	cleanHome(t)
	code, out, _ := runConfig(t, "--json", "get", "autoswitch.bogus")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Error         struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("stdout not JSON: %q", out)
	}
	if payload.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", payload.SchemaVersion)
	}
	if !strings.Contains(payload.Error.Message, "unknown setting") {
		t.Errorf("error message = %q, want 'unknown setting'", payload.Error.Message)
	}
}

// TestConfigUnsetNotSet: unset of an absent key is exit 0 with a stderr notice.
func TestConfigUnsetNotSet(t *testing.T) {
	cleanHome(t)
	code, out, errStr := runConfig(t, "unset", "autoswitch.threshold")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errStr, "not set; nothing to do") {
		t.Errorf("stderr = %q, want the 'nothing to do' notice", errStr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

// TestConfigPath: prints the settings.json path.
func TestConfigPath(t *testing.T) {
	cleanHome(t)
	code, out, _ := runConfig(t, "path")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "settings.json") {
		t.Errorf("path output = %q, want it to end in settings.json", out)
	}
}

// TestConfigUnknownAction: a bad action is an exit-2 usage error.
func TestConfigUnknownAction(t *testing.T) {
	cleanHome(t)
	code, _, _ := runConfig(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// TestConfigSetMissingValue: `set KEY` with no VALUE is exit 2.
func TestConfigSetMissingValue(t *testing.T) {
	cleanHome(t)
	code, _, _ := runConfig(t, "set", "autoswitch.threshold")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
