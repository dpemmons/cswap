// `cswap upgrade` self-upgrade dispatch.
//
// Implements spec 08§13.4 (run_self_upgrade), redesigned per DESIGN.md §6
// Deviation #2 and Amendment A6: there is no PyPI/uv/pipx for a Go binary, so
// upgrading means re-running `go install <ModulePath>@latest` when the
// running binary lives in a Go-managed bin dir, and printing manual guidance
// otherwise. On Windows the running .exe is locked (same rationale as
// Python's win32 branch, spec 08§13.4), so SelfUpgrade there always prints
// the command instead of running it.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
)

// CommandRunner executes a subprocess and reports its exit code. A non-nil
// err means the process could not be started at all (e.g. the binary is
// missing from PATH — check with IsNotFound); a completed process's nonzero
// exit is reported via exitCode with err == nil, mirroring Python's
// subprocess.run(check=False).
type CommandRunner func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) (exitCode int, err error)

// RunCommand is the real CommandRunner: os/exec with no timeout, matching
// Python's un-timed subprocess.run for the upgrade command.
func RunCommand(ctx context.Context, name string, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// IsNotFound reports whether err (as returned by a CommandRunner) indicates
// the target binary is missing from PATH.
func IsNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// Upgrader implements `cswap upgrade`. The zero value uses the real OS
// environment, subprocess execution, and stdio; tests override every seam.
type Upgrader struct {
	// Getenv looks up GOBIN/GOPATH for install-shape detection; nil -> os.Getenv.
	Getenv func(string) string
	// HomeDir is $HOME for install-shape detection; "" -> os.UserHomeDir().
	HomeDir string
	// Run executes the `go install` subprocess; nil -> RunCommand.
	Run CommandRunner
	// Stdout/Stderr receive guidance text and the subprocess's own output;
	// nil -> os.Stdout / os.Stderr.
	Stdout, Stderr io.Writer
}

func (u Upgrader) getenv() func(string) string {
	if u.Getenv != nil {
		return u.Getenv
	}
	return os.Getenv
}

func (u Upgrader) homeDir() string {
	if u.HomeDir != "" {
		return u.HomeDir
	}
	h, _ := os.UserHomeDir()
	return h
}

func (u Upgrader) run() CommandRunner {
	if u.Run != nil {
		return u.Run
	}
	return RunCommand
}

func (u Upgrader) stdout() io.Writer {
	if u.Stdout != nil {
		return u.Stdout
	}
	return os.Stdout
}

func (u Upgrader) stderr() io.Writer {
	if u.Stderr != nil {
		return u.Stderr
	}
	return os.Stderr
}

// SelfUpgrade runs the appropriate upgrade action for the running binary's
// install shape and returns the process exit code (never an error — every
// failure path prints guidance and returns 1, matching
// run_self_upgrade's contract of "return an int, don't raise").
//
// exePath is the running binary's path (symlink-resolved os.Executable());
// plat gates the Windows print-only branch.
func (u Upgrader) SelfUpgrade(exePath string, plat platform.Platform) int {
	shape := DetectInstallShape(exePath, u.getenv(), u.homeDir())
	cmdArgs := []string{"install", ModulePath + "@latest"}
	fullCmd := "go " + strings.Join(cmdArgs, " ")

	if shape != ShapeGoInstall {
		binary := exePath
		if binary == "" {
			binary = "(unknown)"
		}
		fmt.Fprintf(u.stderr(),
			"Could not detect a `go install` layout (looked for $GOBIN, $GOPATH/bin, $HOME/go/bin).\n"+
				"  binary: %s\n"+
				"To upgrade manually, run:\n"+
				"  %s\n"+
				"Or download a release from:\n"+
				"  %s\n",
			binary, fullCmd, ReleasesURL)
		return 1
	}

	// Windows: the running cswap.exe is locked, so `go install` cannot replace
	// it in place even though the module itself would update fine (spec
	// 08§13.4's win32 rationale, carried over). Print the command instead of
	// running it; cswap exits right after this, releasing the lock.
	if plat == platform.Windows {
		fmt.Fprintf(u.stdout(), "To upgrade claude-swap on Windows, run:\n  %s\n", printer.Accent(fullCmd))
		return 1
	}

	code, err := u.run()(context.Background(), "go", cmdArgs, u.stdout(), u.stderr())
	if err != nil {
		if IsNotFound(err) {
			fmt.Fprintln(u.stderr(),
				"Detected a go install layout but `go` is not on PATH. "+
					"Run the upgrade manually from a shell where it is available.")
			return 1
		}
		fmt.Fprintln(u.stderr(), err.Error())
		return 1
	}
	return code
}
