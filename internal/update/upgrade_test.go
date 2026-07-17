// Upgrader.SelfUpgrade: install-shape dispatch, Windows print-only, and the
// go-install subprocess path (success/failure/not-on-PATH), all via fake
// CommandRunners and fake paths (no real subprocess or filesystem shape is
// touched).
//
// Implements spec 08§13.4 test coverage (run_self_upgrade), redesigned per
// Amendment A6.
package update

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

// fakeRunner returns a CommandRunner that records the invocation and returns
// the given exit code / error.
func fakeRunner(gotName *string, gotArgs *[]string, exitCode int, err error) CommandRunner {
	return func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) (int, error) {
		*gotName = name
		*gotArgs = append([]string(nil), args...)
		return exitCode, err
	}
}

func goInstallUpgrader(t *testing.T, run CommandRunner) (u Upgrader, exePath string, stdout, stderr *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	exePath = filepath.Join(home, "go", "bin", "cswap")
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	u = Upgrader{
		Getenv:  func(string) string { return "" }, // no GOBIN/GOPATH -> falls to $HOME/go/bin
		HomeDir: home,
		Run:     run,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	return u, exePath, stdout, stderr
}

func TestSelfUpgrade_UnknownShapePrintsGuidance(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := fakeRunner(&gotName, &gotArgs, 0, nil)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	u := Upgrader{
		Getenv:  func(string) string { return "" },
		HomeDir: t.TempDir(),
		Run:     run,
		Stdout:  stdout,
		Stderr:  stderr,
	}
	code := u.SelfUpgrade("/usr/bin/cswap", platform.Linux)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if gotName != "" {
		t.Error("subprocess should not run when the install shape is unknown")
	}
	if !strings.Contains(stderr.String(), "Could not detect a `go install` layout") {
		t.Errorf("stderr = %q, missing guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), ModulePath) {
		t.Errorf("stderr = %q, missing module path", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", stdout.String())
	}
}

func TestSelfUpgrade_WindowsPrintsOnlyNeverRuns(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := fakeRunner(&gotName, &gotArgs, 0, nil)

	u, exePath, stdout, stderr := goInstallUpgrader(t, run)
	code := u.SelfUpgrade(exePath, platform.Windows)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if gotName != "" {
		t.Error("SelfUpgrade must never run the subprocess on Windows (locked .exe)")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on the Windows print-only path, got %q", stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "To upgrade claude-swap on Windows, run:") {
		t.Errorf("stdout = %q, missing Windows guidance header", got)
	}
	wantCmd := "go install " + ModulePath + "@latest"
	if !strings.Contains(got, wantCmd) {
		t.Errorf("stdout = %q, missing command %q", got, wantCmd)
	}
}

func TestSelfUpgrade_GoInstallShapeRunsAndPropagatesExitCode(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
	}{
		{"success", 0},
		{"nonzero propagates", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotName string
			var gotArgs []string
			run := fakeRunner(&gotName, &gotArgs, tc.exitCode, nil)

			u, exePath, _, stderr := goInstallUpgrader(t, run)
			code := u.SelfUpgrade(exePath, platform.Linux)

			if code != tc.exitCode {
				t.Errorf("exit code = %d, want %d", code, tc.exitCode)
			}
			if gotName != "go" {
				t.Errorf("subprocess name = %q, want go", gotName)
			}
			wantArgs := []string{"install", ModulePath + "@latest"}
			if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] {
				t.Errorf("subprocess args = %v, want %v", gotArgs, wantArgs)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr should be empty on a clean run, got %q", stderr.String())
			}
		})
	}
}

func TestSelfUpgrade_GoNotOnPath(t *testing.T) {
	notFound := &exec.Error{Name: "go", Err: exec.ErrNotFound}
	run := func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) (int, error) {
		return 0, notFound
	}

	u, exePath, stdout, stderr := goInstallUpgrader(t, run)
	code := u.SelfUpgrade(exePath, platform.Linux)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not on PATH") {
		t.Errorf("stderr = %q, want a not-on-PATH message", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", stdout.String())
	}
}

func TestSelfUpgrade_SubprocessStartFailure(t *testing.T) {
	failErr := errors.New("boom")
	run := func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) (int, error) {
		return 0, failErr
	}

	u, exePath, _, stderr := goInstallUpgrader(t, run)
	code := u.SelfUpgrade(exePath, platform.Linux)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want the underlying error surfaced", stderr.String())
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&exec.Error{Name: "go", Err: exec.ErrNotFound}) {
		t.Error("IsNotFound should recognize a wrapped exec.ErrNotFound")
	}
	if IsNotFound(errors.New("some other error")) {
		t.Error("IsNotFound should not match an unrelated error")
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) should be false")
	}
}

// TestRunCommand_RealExec exercises the real CommandRunner end-to-end against
// a harmless binary, proving exit-code propagation and stdout/stderr wiring
// without depending on `go` being on PATH in the test environment.
func TestRunCommand_RealExec(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := RunCommand(context.Background(), "sh", []string{"-c", "echo out; echo err >&2; exit 7"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if strings.TrimSpace(stdout.String()) != "out" {
		t.Errorf("stdout = %q, want \"out\"", stdout.String())
	}
	if strings.TrimSpace(stderr.String()) != "err" {
		t.Errorf("stderr = %q, want \"err\"", stderr.String())
	}
}

func TestRunCommand_NotFound(t *testing.T) {
	_, err := RunCommand(context.Background(), "cswap-definitely-not-a-real-binary-xyz", nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound(err) to be true, err = %v", err)
	}
}
