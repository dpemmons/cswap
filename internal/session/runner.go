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
	"io"
	"os/exec"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cclock"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
)

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
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", 0, ctx.Err() // TimeoutExpired parity → validation fails
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode(), nil // non-zero rc is not an error
		}
		return "", 0, err // OSError parity (could not spawn)
	}
	return out.String(), 0, nil
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
