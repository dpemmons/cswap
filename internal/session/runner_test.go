package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestProbeGrandchildHoldingPipe pins the WaitDelay + ErrWaitDelay fix: a probe
// whose child exits 0 but leaves a background grandchild holding the captured
// stdout pipe must return after the grace window (probeWaitDelay), not after the
// grandchild's own long sleep. ErrWaitDelay is raised only for a successful exit,
// and the child's stdout was flushed before it exited, so the probe is a SUCCESS
// with the captured output — not a timeout that would delete a valid profile.
//
// This assertion is the inverse of the pre-fix behavior, which mapped
// ErrWaitDelay to DeadlineExceeded and discarded the captured output.
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
	// Successful exit with flushed output: nil error, captured stdout, rc 0.
	if err != nil {
		t.Fatalf("err = %v; want nil (successful exit, stdout already flushed)", err)
	}
	if stdout != "ready\n" || rc != 0 {
		t.Fatalf("stdout=%q rc=%d; want %q/0 (captured output honored)", stdout, rc, "ready\n")
	}
}

// TestClassifyProbeSuccessAtDeadline pins the FINDING 10 fix via the pure
// classify seam: a probe whose process exited cleanly (runErr == nil) exactly as
// the deadline fired must be honored as a success with its full captured output,
// never reclassified as a timeout because ctx has since expired.
func TestClassifyProbeSuccessAtDeadline(t *testing.T) {
	stdout, rc, err := classifyProbe("payload", nil, context.DeadlineExceeded)
	if err != nil || rc != 0 || stdout != "payload" {
		t.Fatalf("classifyProbe(success-at-deadline) = (%q, %d, %v); want (\"payload\", 0, nil)", stdout, rc, err)
	}
}

// TestClassifyProbeErrWaitDelay pins the FINDING 3 fix: ErrWaitDelay (raised only
// on a successful exit whose pipes stayed open) is a success carrying the
// captured output, not a timeout.
func TestClassifyProbeErrWaitDelay(t *testing.T) {
	stdout, rc, err := classifyProbe("ready\n", exec.ErrWaitDelay, nil)
	if err != nil || rc != 0 || stdout != "ready\n" {
		t.Fatalf("classifyProbe(ErrWaitDelay) = (%q, %d, %v); want (%q, 0, nil)", stdout, rc, err, "ready\n")
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
