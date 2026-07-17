// Runner: the process seam — PATH resolution (shutil.which), the auth-status
// probe (subprocess.run capture_output+timeout), and the terminal handoff
// (execvpe on POSIX / subprocess+exit on Windows), all mockable for tests.
//
// Implements spec 06§1.7 (_is_session_valid probe invocation) and 06§1.8
// (_exec). LookPath and Probe are platform-independent; the exec image swap is
// in exec_posix.go / exec_windows.go.
package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cclock"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
)

// probeWaitDelay bounds how long Probe blocks after the deadline fires (or the
// child exits) waiting on captured pipes that a grandchild may still hold open.
// Without it cmd.Run reads the stdout/stderr pipes until EOF and can outlive the
// timeout indefinitely when an orphaned subprocess keeps a descriptor.
const probeWaitDelay = 2 * time.Second

// Runner abstracts process discovery and execution.
type Runner interface {
	// LookPath resolves a binary on PATH (shutil.which). Returns a non-nil
	// error when the binary is not found. On Windows this must consult PATHEXT
	// so a `claude.cmd` shim resolves.
	LookPath(name string) (string, error)
	// Probe runs argv with env, capturing stdout, up to timeout. It returns
	// (stdout, exitCode, err); err is non-nil only on a spawn failure or
	// timeout (Python OSError / TimeoutExpired), never for a non-zero exit.
	Probe(argv, env []string, timeout time.Duration) (stdout string, rc int, err error)
	// Exec hands the terminal to the child. POSIX replaces the process image
	// (returns only on exec failure); Windows spawns, waits, and exits with the
	// child's code (Ctrl+C → 130).
	Exec(bin string, argv, env []string) error
}

// osRunner is the production Runner.
type osRunner struct{}

// LookPath resolves name on PATH.
func (osRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Probe runs argv (capturing stdout only) with a hard timeout, mirroring
// subprocess.run(..., capture_output=True, text=True, timeout=...).
func (osRunner) Probe(argv, env []string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.WaitDelay = probeWaitDelay // don't let a leaked pipe outlive the deadline
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return classifyProbe(out.String(), err, ctx.Err())
}

// classifyProbe maps a cmd.Run() result to the (stdout, rc, err) probe contract,
// mirroring Python's subprocess.run: a TimeoutExpired fires only when the
// process is actually killed by the deadline, never when the process completed
// and produced a result. Kept as a pure seam so the classification is
// unit-testable (see classify-at-deadline coverage in runner_test.go).
func classifyProbe(out string, runErr error, ctxErr error) (string, int, error) {
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The child exited with a successful status but a grandchild held a
		// captured pipe past the grace window, so WaitDelay force-closed the
		// pipe. stdout was flushed before the child exited, making the capture
		// complete and valid — treat it as success with the captured output
		// rather than a timeout that would delete freshly built profiles.
		return out, 0, nil
	}
	if runErr == nil {
		// Success is honored regardless of ctx state: a probe that completed at
		// the deadline produced a full result that must not be discarded.
		return out, 0, nil
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		// A process the deadline killed dies via signal (never Exited): surface
		// TimeoutExpired parity → validation fails. A process that exited on its
		// own — any rc — is honored even if the deadline then fired.
		if ctxErr == context.DeadlineExceeded && !ee.Exited() {
			return "", 0, context.DeadlineExceeded
		}
		return out, ee.ExitCode(), nil // non-zero rc is not an error
	}
	if ctxErr == context.DeadlineExceeded {
		return "", 0, context.DeadlineExceeded // killed before start → timeout parity
	}
	return "", 0, runErr // OSError parity (could not spawn)
}

// clockLockConfig acquires Claude Code's <config>.lock via cclock and returns a
// release closure. A ClaudeCodeLockTimeout (or mkdir OSError) propagates so the
// MCP mirror can fail open.
func clockLockConfig(lockDir string, clk clock.Clock) (func(), error) {
	h, err := cclock.Acquire(lockDir, 0, clk) // 0 → DefaultTimeoutS (9s)
	if err != nil {
		return nil, err
	}
	return h.Release, nil
}
