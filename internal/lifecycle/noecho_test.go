package lifecycle

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// withStdin swaps the package-shared stdinReader for the test's duration.
func withStdin(t *testing.T, data string) {
	t.Helper()
	prev := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(data))
	t.Cleanup(func() { stdinReader = prev })
}

// fakeTerm is a scripted terminalControl seam.
type fakeTerm struct {
	tty        bool
	disableErr error
	restored   bool
}

func (f *fakeTerm) isTerminal() bool { return f.tty }
func (f *fakeTerm) disableEcho() (func() error, error) {
	if f.disableErr != nil {
		return nil, f.disableErr
	}
	return func() error { f.restored = true; return nil }, nil
}

func withTerminal(t *testing.T, tc terminalControl) {
	t.Helper()
	prev := activeTerminal
	activeTerminal = tc
	t.Cleanup(func() { activeTerminal = prev })
}

// TestPromptStripsNewlines pins Python universal-newline input() parity through
// StdPrompter.Prompt: CRLF loses both the LF and the single preceding CR, LF
// alone loses just the LF, and interior spaces are preserved (no TrimSpace).
func TestPromptStripsNewlines(t *testing.T) {
	captureOut(t)
	cases := []struct{ in, want string }{
		{"y\r\n", "y"},
		{"y \r\n", "y "},
		{"y\n", "y"},
	}
	for _, tc := range cases {
		withStdin(t, tc.in)
		got, ok := StdPrompter{}.Prompt("? ")
		if !ok || got != tc.want {
			t.Errorf("Prompt(%q) = %q,%v want %q,true", tc.in, got, ok, tc.want)
		}
	}
}

// TestStdinLineStripsNewlines pins the same CRLF handling for the add-token "-"
// path (StdinLine).
func TestStdinLineStripsNewlines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sk-ant-oat01-X\r\n", "sk-ant-oat01-X"},
		{"tok \r\n", "tok "},
		{"tok\n", "tok"},
	}
	for _, tc := range cases {
		withStdin(t, tc.in)
		got, ok := StdPrompter{}.StdinLine()
		if !ok || got != tc.want {
			t.Errorf("StdinLine(%q) = %q,%v want %q,true", tc.in, got, ok, tc.want)
		}
	}
}

// TestSecretNonTTYFallback: a non-terminal stdin reads plainly with no
// suppressed-echo newline appended, exactly like getpass's fallback.
func TestSecretNonTTYFallback(t *testing.T) {
	out := captureOut(t)
	withTerminal(t, &fakeTerm{tty: false})
	withStdin(t, "sk-ant-oat01-TOKEN\n")
	got, ok := StdPrompter{}.Secret("Token: ")
	if !ok || got != "sk-ant-oat01-TOKEN" {
		t.Fatalf("Secret = %q,%v want %q,true", got, ok, "sk-ant-oat01-TOKEN")
	}
	if out.String() != "Token: " {
		t.Errorf("non-tty fallback output = %q, want no trailing newline", out.String())
	}
}

// TestSecretTTYSuppressesEcho: a terminal stdin disables echo, restores it, and
// prints the trailing newline the suppressed Enter-echo swallowed (getpass).
func TestSecretTTYSuppressesEcho(t *testing.T) {
	out := captureOut(t)
	ft := &fakeTerm{tty: true}
	withTerminal(t, ft)
	withStdin(t, "secret\r\n")
	got, ok := StdPrompter{}.Secret("Token: ")
	if !ok || got != "secret" {
		t.Fatalf("Secret = %q,%v want %q,true", got, ok, "secret")
	}
	if !ft.restored {
		t.Error("echo not restored")
	}
	if out.String() != "Token: \n" {
		t.Errorf("tty output = %q, want %q", out.String(), "Token: \n")
	}
}

// TestSecretTTYRestoresOnEOF: an interrupt/EOF read (ok=false) still restores
// echo and emits the trailing newline (getpass finally clause).
func TestSecretTTYRestoresOnEOF(t *testing.T) {
	out := captureOut(t)
	ft := &fakeTerm{tty: true}
	withTerminal(t, ft)
	withStdin(t, "")
	got, ok := StdPrompter{}.Secret("Token: ")
	if ok || got != "" {
		t.Fatalf("Secret on EOF = %q,%v want \"\",false", got, ok)
	}
	if !ft.restored {
		t.Error("echo not restored on EOF path")
	}
	if out.String() != "Token: \n" {
		t.Errorf("tty EOF output = %q, want %q", out.String(), "Token: \n")
	}
}

// TestSecretEchoUnavailableFallback: when disableEcho fails, Secret reads with
// echo (no restore, no suppressed-echo newline), like getpass's fallback.
func TestSecretEchoUnavailableFallback(t *testing.T) {
	out := captureOut(t)
	ft := &fakeTerm{tty: true, disableErr: errors.New("ioctl failed")}
	withTerminal(t, ft)
	withStdin(t, "secret\n")
	got, ok := StdPrompter{}.Secret("Token: ")
	if !ok || got != "secret" {
		t.Fatalf("Secret = %q,%v want %q,true", got, ok, "secret")
	}
	if ft.restored {
		t.Error("restore should not run when disableEcho failed")
	}
	if out.String() != "Token: " {
		t.Errorf("echo-unavailable output = %q, want no trailing newline", out.String())
	}
}
