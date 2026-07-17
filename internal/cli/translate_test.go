package cli

import (
	"reflect"
	"testing"
)

// TestTranslateSubcommand pins the _translate_subcommand table (spec 08§2/§14).
func TestTranslateSubcommand(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"legacy flag unchanged", []string{"--list"}, []string{"--list"}},
		{"empty unchanged", []string{}, []string{}},
		{"bare switch rotates", []string{"switch"}, []string{"--switch"}},
		{"switch with strategy", []string{"switch", "--strategy", "best"}, []string{"--switch", "--strategy", "best"}},
		{"switch to number", []string{"switch", "2"}, []string{"--switch-to", "2"}},
		{"switch to email with json", []string{"switch", "u@x.com", "--json"}, []string{"--switch-to", "u@x.com", "--json"}},
		{"ls alias", []string{"ls"}, []string{"--list"}},
		{"rm with target", []string{"rm", "2"}, []string{"--remove-account", "2"}},
		{"update to upgrade", []string{"update"}, []string{"--upgrade"}},
		{"export passthrough", []string{"export", "b.cswap", "--full"}, []string{"--export", "b.cswap", "--full"}},
		{"bogus unchanged", []string{"bogus"}, []string{"bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateSubcommand(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("translateSubcommand(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestProgName pins _prog_name (spec 08§1, 10-audit Gap 2).
func TestProgName(t *testing.T) {
	cases := map[string]string{
		"cswap":                "cswap",
		"/usr/bin/claude-swap": "claude-swap",
		"cswap.exe":            "cswap",
		"claude-swap.EXE":      "claude-swap",
		"__main__":             "cswap",
		"python":               "cswap",
		"python3":              "cswap",
		"py":                   "cswap",
		"":                     "cswap",
		"/opt/tools/cswap":     "cswap",
	}
	for in, want := range cases {
		if got := progName(in); got != want {
			t.Errorf("progName(%q) = %q, want %q", in, got, want)
		}
	}
}
