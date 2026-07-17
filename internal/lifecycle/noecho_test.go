package lifecycle

import (
	"bufio"
	"errors"
	"io"
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

// TestSecretEchoUnavailableFallback: when disableEcho fails, Secret prints
// getpass.fallback_getpass's exact warning to the prompt stream, then reads with
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
	// Prompt, then the getpass fallback warning (verbatim, to the prompt
	// stream), then no suppressed-echo newline on the read.
	if want := "Token: Warning: Password input may be echoed.\n"; out.String() != want {
		t.Errorf("echo-unavailable output = %q, want %q", out.String(), want)
	}
}

// TestRunCleanupsRunsRegistered: RegisterCleanup + RunCleanups runs each
// registered fn once and clears the registry; a second RunCleanups is a no-op.
func TestRunCleanupsRunsRegistered(t *testing.T) {
	resetCleanups(t)
	var n1, n2 int
	RegisterCleanup(func() { n1++ })
	RegisterCleanup(func() { n2++ })
	RunCleanups()
	if n1 != 1 || n2 != 1 {
		t.Fatalf("after RunCleanups n1,n2 = %d,%d want 1,1", n1, n2)
	}
	RunCleanups() // registry cleared: no re-run
	if n1 != 1 || n2 != 1 {
		t.Errorf("second RunCleanups re-ran cleanups: n1,n2 = %d,%d", n1, n2)
	}
}

// TestUnregisterRemovesCleanup: an Unregistered cleanup never runs.
func TestUnregisterRemovesCleanup(t *testing.T) {
	resetCleanups(t)
	var n int
	id := RegisterCleanup(func() { n++ })
	Unregister(id)
	RunCleanups()
	if n != 0 {
		t.Errorf("unregistered cleanup ran: n = %d", n)
	}
}

// hookReader fires fn on its first Read, then reports EOF — used to simulate the
// SIGINT handler firing while Secret is blocked on the terminal read.
type hookReader struct {
	fn   func()
	done bool
}

func (h *hookReader) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		h.fn()
	}
	return 0, io.EOF
}

// TestSecretRestoreRunsOnSignalPath: a Secret prompt with echo suppressed
// registers its restore so the SIGINT handler (simulated here by RunCleanups
// firing mid-read) restores echo before the process would exit — the deferred
// restore never gets to run on that path. Finding 7.
func TestSecretRestoreRunsOnSignalPath(t *testing.T) {
	captureOut(t)
	resetCleanups(t)
	ft := &fakeTerm{tty: true}
	var restoredInHandler bool
	prev := stdinReader
	stdinReader = bufio.NewReader(&hookReader{fn: func() {
		RunCleanups()                   // simulate the SIGINT handler
		restoredInHandler = ft.restored // restore ran from the handler path
	}})
	t.Cleanup(func() { stdinReader = prev })
	withTerminal(t, ft)

	got, ok := StdPrompter{}.Secret("Token: ")
	if ok || got != "" {
		t.Fatalf("Secret = %q,%v want \"\",false", got, ok)
	}
	if !restoredInHandler {
		t.Error("echo was not restored by the simulated SIGINT handler path")
	}
	// The handler cleared the registry; the deferred Unregister is a harmless
	// no-op and leaves nothing behind.
	assertNoCleanups(t)
}

// TestSecretUnregistersAfterNormalReturn: a normal Secret completion unregisters
// its restore so nothing is left for a later SIGINT to run twice.
func TestSecretUnregistersAfterNormalReturn(t *testing.T) {
	captureOut(t)
	resetCleanups(t)
	ft := &fakeTerm{tty: true}
	withTerminal(t, ft)
	withStdin(t, "secret\r\n")
	if _, ok := (StdPrompter{}).Secret("Token: "); !ok {
		t.Fatal("Secret returned ok=false")
	}
	assertNoCleanups(t)
}

// resetCleanups clears the cleanup registry for the test's duration.
func resetCleanups(t *testing.T) {
	t.Helper()
	cleanupMu.Lock()
	prev := cleanups
	cleanups = map[uint64]func(){}
	cleanupMu.Unlock()
	t.Cleanup(func() {
		cleanupMu.Lock()
		cleanups = prev
		cleanupMu.Unlock()
	})
}

// assertNoCleanups fails if any cleanup remains registered.
func assertNoCleanups(t *testing.T) {
	t.Helper()
	cleanupMu.Lock()
	n := len(cleanups)
	cleanupMu.Unlock()
	if n != 0 {
		t.Errorf("cleanup registry not empty: %d entries", n)
	}
}
