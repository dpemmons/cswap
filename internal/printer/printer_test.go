package printer

import (
	"testing"
	"time"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestDetectColorSupportPrecedence(t *testing.T) {
	vtOK := func() bool { return true }
	vtFail := func() bool { return false }
	tests := []struct {
		name      string
		env       map[string]string
		isTTY     bool
		isWindows bool
		vt        func() bool
		want      bool
	}{
		{"NO_COLOR present empty wins", map[string]string{"NO_COLOR": "", "FORCE_COLOR": "1"}, true, false, vtOK, false},
		{"FORCE_COLOR present", map[string]string{"FORCE_COLOR": ""}, false, false, vtOK, true},
		{"not a tty", map[string]string{}, false, false, vtOK, false},
		{"windows vt ok", map[string]string{}, true, true, vtOK, true},
		{"windows vt fail", map[string]string{}, true, true, vtFail, false},
		{"term dumb", map[string]string{"TERM": "dumb"}, true, false, vtOK, false},
		{"default tty", map[string]string{"TERM": "xterm"}, true, false, vtOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectColorSupport(lookupFrom(tt.env), tt.isTTY, tt.isWindows, tt.vt)
			if got != tt.want {
				t.Errorf("detectColorSupport = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestColorsEnabledCaches(t *testing.T) {
	resetColorCache()
	first := ColorsEnabled()
	// Even if env changed, the cached result must stand.
	t.Setenv("FORCE_COLOR", "1")
	if ColorsEnabled() != first {
		t.Errorf("ColorsEnabled changed after caching")
	}
	resetColorCache()
}

func TestStylersGatedByColor(t *testing.T) {
	resetColorCache()
	// Tests run with stdout not a TTY → colors off → plain passthrough.
	if got := Accent("hi"); got != "hi" {
		t.Errorf("Accent with color off = %q, want plain", got)
	}
	// ForceColor turns codes on.
	ForceColor(func() {
		if got := Accent("hi"); got != codeAccent+"hi"+codeReset {
			t.Errorf("Accent under ForceColor = %q", got)
		}
		if got := BoldAccent("x"); got != codeBold+codeAccent+"x"+codeReset {
			t.Errorf("BoldAccent = %q", got)
		}
	})
	// Restored afterward.
	if got := Yellowed("y"); got != "y" {
		t.Errorf("after ForceColor, Yellowed = %q, want plain", got)
	}
	resetColorCache()
}

func TestDisplayHelpers(t *testing.T) {
	if EntrypointLabel("claude-vscode") != "VS Code" {
		t.Error("entrypoint label")
	}
	if EntrypointLabel("unknown-x") != "unknown-x" {
		t.Error("entrypoint passthrough")
	}
	if IDEShortName("Visual Studio Code") != "VS Code" {
		t.Error("ide short name")
	}
	if IDEShortName("Vim") != "Vim" {
		t.Error("ide passthrough")
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	ms := func(secondsAgo int64) int64 { return (now.Unix() - secondsAgo) * 1000 }
	cases := []struct {
		secondsAgo int64
		want       string
	}{
		{10, "just now"},
		{59, "just now"},
		{60, "1m ago"},
		{3599, "59m ago"},
		{3600, "1h ago"},
		{86399, "23h ago"},
		{86400, "1d ago"},
		{200000, "2d ago"},
	}
	for _, c := range cases {
		if got := formatAgeAt(ms(c.secondsAgo), now); got != c.want {
			t.Errorf("formatAge(%ds ago) = %q, want %q", c.secondsAgo, got, c.want)
		}
	}
}
