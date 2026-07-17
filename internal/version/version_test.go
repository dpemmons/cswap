package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	// Default (un-injected) build marker per DESIGN A5.
	if Version != "v0.0.0-dev" {
		t.Errorf("default Version = %q, want v0.0.0-dev", Version)
	}
}

func TestDisplayStripsLeadingV(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":        "0.1.0",
		"v0.2.0-beta.1": "0.2.0-beta.1",
		"v0.0.0-dev":    "0.0.0-dev",
		"0.1.0":         "0.1.0", // no leading v → unchanged
	}
	saved := Version
	defer func() { Version = saved }()
	for in, want := range cases {
		Version = in
		if got := Display(); got != want {
			t.Errorf("Display() with Version=%q = %q, want %q", in, got, want)
		}
	}
}
