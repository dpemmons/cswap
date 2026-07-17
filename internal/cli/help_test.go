package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestHelpFlag pins the --help structure (spec 08§14): the header, bare
// subcommands, the visible flags, the "keep working" note, and the absence of
// legacy flag spellings / argparse's raw "one of the arguments" from the
// options section (the substring before "Flags combine with subcommands:").
func TestHelpFlag(t *testing.T) {
	var out bytes.Buffer
	if code := renderMainHelp("cswap", &out); code != 0 {
		t.Fatalf("renderMainHelp code = %d, want 0", code)
	}
	help := out.String()

	mustContain := []string{
		"Multi-Account Switcher for Claude Code",
		"cswap switch <num|email>",
		"cswap list",
		"cswap status",
		"cswap add",
		"cswap add-token [TOKEN|-]",
		"cswap export <path>",
		"cswap import <path>",
		"cswap upgrade",
		"cswap alias <num|email>",
		"cswap auto",
		"cswap config",
		"--slot",
		"--email",
		"cswap run 2 -- --resume", // epilog example
		"keep working",
	}
	for _, s := range mustContain {
		if !strings.Contains(help, s) {
			t.Errorf("--help missing %q", s)
		}
	}

	// The legacy --flag spellings must be absent from the options section
	// (everything before the epilog marker), and argparse's raw error text too.
	marker := "Flags combine with subcommands:"
	idx := strings.Index(help, marker)
	if idx < 0 {
		t.Fatalf("--help missing epilog marker %q", marker)
	}
	optionsSection := help[:idx]
	for _, legacy := range []string{"--add-account", "--switch-to", "one of the arguments"} {
		if strings.Contains(optionsSection, legacy) {
			t.Errorf("options section must not contain %q", legacy)
		}
	}
}

// TestHelpProgSubstitution: a non-default program name is substituted throughout
// (%(prog)s parity), and "claude-swap" is never mangled by the replace.
func TestHelpProgSubstitution(t *testing.T) {
	var out bytes.Buffer
	renderMainHelp("claude-swap", &out)
	help := out.String()
	if !strings.Contains(help, "claude-swap list") {
		t.Errorf("--help did not substitute prog into command lines: %q", firstLines(help, 6))
	}
	if !strings.Contains(help, "remove all claude-swap data") {
		t.Errorf("--help mangled the literal 'claude-swap' data line")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
