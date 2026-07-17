//go:build windows

// Windows terminal handoff: os.exec* detaches from the console confusingly, so
// cswap stays resident as a thin wrapper subprocess and exits with claude's own
// return code. Ctrl+C while waiting mirrors to exit code 130.
//
// Implements spec 06§1.8 (_exec Windows branch). Not compiled/vetted on the
// Linux build host; provided for parity (mirrors WP0's Windows-tagged files).
package session

import (
	"errors"
	"os"
	"os/exec"
)

// Exec spawns claude, waits, and exits with its return code. It never returns
// on success.
func (osRunner) Exec(bin string, argv, env []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		// Ctrl+C (interrupt) or a spawn failure: mirror claude's exit as 130,
		// matching the Python KeyboardInterrupt path.
		os.Exit(130)
	}
	os.Exit(0)
	return nil // unreachable
}
