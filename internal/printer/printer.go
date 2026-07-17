// Package printer owns human console output: ANSI styling, color/TTY detection
// with first-call caching, and small display helpers.
//
// Implements spec 08§10 (printer.py) and 02§11. Color detection precedence
// (NO_COLOR present incl. empty → off; FORCE_COLOR present → on; non-TTY → off;
// Windows → try VT; TERM=dumb → off; else on) and its first-call caching are
// byte-for-byte contracts.
package printer

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ANSI escape codes (spec 08§10.1).
const (
	codeReset  = "\033[0m"
	codeBold   = "\033[1m"
	codeDim    = "\033[2m"
	codeRed    = "\033[31m"
	codeYellow = "\033[33m"
	codeAccent = "\033[38;5;173m" // warm salmon/terracotta
	codeMuted  = "\033[38;5;250m" // soft gray
)

var (
	colorMu       sync.Mutex
	colorsCached  *bool
	forcedOverlay *bool // set by ForceColor while active
)

// detectColorSupport evaluates the precedence chain against injected inputs so
// it can be tested without touching real env/TTY/platform state.
func detectColorSupport(lookup func(string) (string, bool), isTTY, isWindows bool, enableVT func() bool) bool {
	if _, ok := lookup("NO_COLOR"); ok {
		return false
	}
	if _, ok := lookup("FORCE_COLOR"); ok {
		return true
	}
	if !isTTY {
		return false
	}
	if isWindows {
		return enableVT()
	}
	if v, _ := lookup("TERM"); v == "dumb" {
		return false
	}
	return true
}

func envLookup(key string) (string, bool) { return os.LookupEnv(key) }

// ColorsEnabled reports whether color output is active, memoizing the first
// result (so later env changes do not take effect), matching colors_enabled().
func ColorsEnabled() bool {
	colorMu.Lock()
	defer colorMu.Unlock()
	if forcedOverlay != nil {
		return *forcedOverlay
	}
	if colorsCached == nil {
		v := detectColorSupport(envLookup, isTerminal(os.Stdout), isWindows(), enableWindowsVT)
		colorsCached = &v
	}
	return *colorsCached
}

// ForceColor forces colored output on for the duration of fn, restoring the
// prior state afterward. Used by the TUI when capturing CLI output into a
// non-TTY buffer.
func ForceColor(fn func()) {
	colorMu.Lock()
	prev := forcedOverlay
	on := true
	forcedOverlay = &on
	colorMu.Unlock()

	defer func() {
		colorMu.Lock()
		forcedOverlay = prev
		colorMu.Unlock()
	}()
	fn()
}

// resetColorCache clears the memoized detection result (test seam).
func resetColorCache() {
	colorMu.Lock()
	colorsCached = nil
	forcedOverlay = nil
	colorMu.Unlock()
}

func style(text string, codes ...string) string {
	if !ColorsEnabled() {
		return text
	}
	return strings.Join(codes, "") + text + codeReset
}

// Inline stylers return styled strings for composing lines.

// Accent applies the warm accent color.
func Accent(text string) string { return style(text, codeAccent) }

// Muted applies the soft gray.
func Muted(text string) string { return style(text, codeMuted) }

// Dimmed applies dim.
func Dimmed(text string) string { return style(text, codeDim) }

// Bolded applies bold (no color).
func Bolded(text string) string { return style(text, codeBold) }

// BoldAccent applies bold + accent.
func BoldAccent(text string) string { return style(text, codeBold, codeAccent) }

// Yellowed applies yellow (warning-toned string; Warning prints).
func Yellowed(text string) string { return style(text, codeYellow) }

// Error prints a red error message to stderr.
func Error(msg string) {
	fmt.Fprintln(os.Stderr, style(msg, codeRed))
}

// Warning prints a yellow warning message to stdout.
func Warning(msg string) {
	fmt.Fprintln(os.Stdout, style(msg, codeYellow))
}

// Display helpers for process detection (spec 08§10.5).

var entrypointLabels = map[string]string{
	"cli":            "CLI",
	"claude-vscode":  "VS Code",
	"claude-desktop": "Desktop",
	"sdk-cli":        "SDK",
	"sdk-ts":         "SDK",
	"sdk-py":         "SDK",
	"mcp":            "MCP",
	"local-agent":    "Agent",
	"remote":         "Remote",
}

var ideShortNames = map[string]string{
	"Visual Studio Code": "VS Code",
}

// EntrypointLabel returns a human-readable label for a Claude Code entrypoint,
// or the entrypoint itself when unmapped.
func EntrypointLabel(entrypoint string) string {
	if v, ok := entrypointLabels[entrypoint]; ok {
		return v
	}
	return entrypoint
}

// IDEShortName returns a short display name for an IDE, or the name unchanged.
func IDEShortName(ideName string) string {
	if v, ok := ideShortNames[ideName]; ok {
		return v
	}
	return ideName
}

// AbbreviatePath replaces the user's home-directory prefix with ~.
func AbbreviatePath(path string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return path
	}
	if strings.HasPrefix(path, h) {
		return "~" + path[len(h):]
	}
	return path
}

// FormatAge renders a millisecond epoch timestamp as a human-readable age
// relative to now: "just now" (<60s), "{m}m ago", "{h}h ago", "{d}d ago".
func FormatAge(startedAtMs int64) string {
	return formatAgeAt(startedAtMs, time.Now())
}

func formatAgeAt(startedAtMs int64, now time.Time) string {
	elapsed := now.Unix() - startedAtMs/1000
	switch {
	case elapsed < 60:
		return "just now"
	case elapsed < 3600:
		return fmt.Sprintf("%dm ago", elapsed/60)
	case elapsed < 86400:
		return fmt.Sprintf("%dh ago", elapsed/3600)
	default:
		return fmt.Sprintf("%dd ago", elapsed/86400)
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
