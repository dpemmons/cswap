package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestProbeGrandchildHoldingPipe pins the WaitDelay fix: a probe whose child
// exits immediately but leaves a background grandchild holding the captured
// stdout pipe must return after the grace window (probeWaitDelay), not after
// the grandchild's own long sleep, and the abandoned probe classifies as a
// timeout (TimeoutExpired parity → validation fails), never a stdout capture.
func TestProbeGrandchildHoldingPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell probe fixture")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.sh")
	// The `&`-detached sleep inherits fd 1 (the pipe), so the read side never
	// sees EOF until the sleep exits ~30s later — long past the parent's exit.
	body := "#!/bin/sh\nsleep 30 &\necho ready\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	const timeout = 10 * time.Second // deadline never fires: parent exits at once
	start := time.Now()
	stdout, rc, err := osRunner{}.Probe([]string{"/bin/sh", script}, os.Environ(), timeout)
	elapsed := time.Since(start)

	// Must return via the grace window, well before the 30s grandchild sleep.
	if elapsed > probeWaitDelay+3*time.Second {
		t.Fatalf("Probe blocked %v on a held pipe; want return near the %v grace window", elapsed, probeWaitDelay)
	}
	// TimeoutExpired parity: non-nil error, empty stdout, rc 0.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded (TimeoutExpired parity)", err)
	}
	if stdout != "" || rc != 0 {
		t.Fatalf("stdout=%q rc=%d; want empty/0 on an abandoned probe", stdout, rc)
	}
}

// TestProbeCleanExitCapturesStdout guards the happy path: with WaitDelay set, a
// child that writes and exits cleanly (no leaked pipe) still returns its stdout
// and a nil error.
func TestProbeCleanExitCapturesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell probe fixture")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "clean.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf hello\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, rc, err := osRunner{}.Probe([]string{"/bin/sh", script}, os.Environ(), 10*time.Second)
	if err != nil || rc != 0 || stdout != "hello" {
		t.Fatalf("Probe = (%q, %d, %v); want (\"hello\", 0, nil)", stdout, rc, err)
	}
}
