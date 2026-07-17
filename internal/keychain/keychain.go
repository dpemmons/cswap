// Package keychain wraps the macOS /usr/bin/security CLI for generic passwords.
//
// Implements spec 03§4 (macos_keychain.py). The exact argv, absolute binary
// path, hex -X encoding, single trailing-\n strip, rc 44 handling and 5s timeout
// are byte-compatibility contracts with Claude-Code-seeded items. A real Security
// client (with an exec seam for tests) and an in-memory Fake are provided.
package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// Constants mirroring macos_keychain.py.
const (
	// SecurityStdinLineLimit is security -i's fgets buffer minus 64 bytes of
	// headroom (4096-64). Commands longer than this fall back to argv.
	SecurityStdinLineLimit = 4096 - 64
	// notFoundRC is errSecItemNotFound from find/delete-generic-password.
	notFoundRC = 44
	// timeout bounds every security spawn.
	timeout = 5 * time.Second
	// securityBin is the pinned absolute path to Apple's system binary.
	securityBin = "/usr/bin/security"
)

// KeychainClient is the seam every credential store uses.
type KeychainClient interface {
	// Get returns (value, found, err). rc 0 → (value, true, nil); rc 44 →
	// ("", false, nil); any other failure → ("", false, err).
	Get(service, account string) (string, bool, error)
	// Set creates or updates the item.
	Set(service, account, password string) error
	// Delete removes the item; rc 0 and rc 44 both succeed.
	Delete(service, account string) error
	// Exists reports whether the item exists without decrypting it; never errors.
	Exists(service, account string) bool
}

// KeychainError is a security invocation failure other than "not found". It is
// classified as "Keychain unusable" by IsUnusable.
type KeychainError struct {
	Msg string
}

func (e *KeychainError) Error() string { return e.Msg }

func keychainErrorf(format string, a ...any) *KeychainError {
	return &KeychainError{Msg: fmt.Sprintf(format, a...)}
}

// IsUnusable reports whether err should route the caller to file storage rather
// than being treated as a programming bug (KEYCHAIN_ERRORS parity): a
// KeychainError (including a converted timeout), a context deadline, or a
// missing security binary.
func IsUnusable(err error) bool {
	if err == nil {
		return false
	}
	var ke *KeychainError
	if errors.As(err, &ke) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return true
	}
	return false
}

// AccountName mirrors Claude Code's getUsername: $USER, then the OS username,
// then the stable fallback "claude-code-user". Matching this keys the same
// Keychain item Claude Code uses on headless hosts where $USER is unset.
func AccountName() string {
	if u := getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "claude-code-user"
}

// quote wraps a value in double quotes for a security -i stdin command line,
// backslash-escaping embedded backslashes and double quotes.
func quote(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// execResult is one security invocation's captured output.
type execResult struct {
	stdout string
	stderr string
	rc     int
}

// execFunc runs argv (argv[0] is the binary) with optional stdin, returning the
// result. A non-nil error means the process could not be run or timed out.
type execFunc func(ctx context.Context, argv []string, stdin string) (execResult, error)

// Security is the real KeychainClient. Zero value is usable; Exec is a test seam.
type Security struct {
	// Path overrides the security binary path (default /usr/bin/security).
	Path string
	// Timeout overrides the per-spawn timeout (default 5s).
	Timeout time.Duration
	// Exec overrides the subprocess runner (default: real exec).
	Exec execFunc
}

func (s Security) bin() string {
	if s.Path != "" {
		return s.Path
	}
	return securityBin
}

func (s Security) budget() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return timeout
}

// call runs a security invocation, converting a start failure or timeout into a
// KeychainError.
func (s Security) call(argv []string, stdin string) (execResult, error) {
	runner := s.Exec
	if runner == nil {
		runner = realExec
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.budget())
	defer cancel()
	res, err := runner(ctx, argv, stdin)
	if ctx.Err() == context.DeadlineExceeded {
		return execResult{}, keychainErrorf("security %s timed out after %s", commandName(argv), s.budget())
	}
	if err != nil {
		return execResult{}, err
	}
	return res, nil
}

func commandName(argv []string) string {
	if len(argv) >= 2 {
		return argv[1]
	}
	return "security"
}

// Get reads a password via find-generic-password -a … -w -s …. It strips exactly
// one trailing newline (TrimSuffix, not TrimSpace).
func (s Security) Get(service, account string) (string, bool, error) {
	res, err := s.call([]string{s.bin(), "find-generic-password", "-a", account, "-w", "-s", service}, "")
	if err != nil {
		return "", false, err
	}
	switch res.rc {
	case 0:
		return strings.TrimSuffix(res.stdout, "\n"), true, nil
	case notFoundRC:
		return "", false, nil
	default:
		return "", false, keychainErrorf(
			"security find-generic-password failed (rc=%d): %s", res.rc, strings.TrimSpace(res.stderr))
	}
}

// Exists reports whether an item exists via an attribute-only lookup (no -w, so
// nothing is decrypted). Never raises: rc 44, error exits, timeouts and a
// missing binary all return false.
func (s Security) Exists(service, account string) bool {
	res, err := s.call([]string{s.bin(), "find-generic-password", "-a", account, "-s", service}, "")
	if err != nil {
		return false
	}
	return res.rc == 0
}

// Set creates or updates an item (-U). The secret is hex-encoded (-X) and rides
// on stdin under the line-buffer limit; larger payloads fall back to argv.
func (s Security) Set(service, account, password string) error {
	hexValue := toHex(password)
	command := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n",
		quote(account), quote(service), hexValue)

	var res execResult
	var err error
	if len(command) <= SecurityStdinLineLimit {
		res, err = s.call([]string{s.bin(), "-i"}, command)
	} else {
		res, err = s.call([]string{
			s.bin(), "add-generic-password", "-U",
			"-a", account, "-s", service, "-X", hexValue,
		}, "")
	}
	if err != nil {
		return err
	}
	if res.rc != 0 {
		return keychainErrorf(
			"security add-generic-password failed (rc=%d): %s", res.rc, strings.TrimSpace(res.stderr))
	}
	return nil
}

// Delete removes an item; rc 0 and rc 44 (already absent) both succeed.
func (s Security) Delete(service, account string) error {
	res, err := s.call([]string{s.bin(), "delete-generic-password", "-a", account, "-s", service}, "")
	if err != nil {
		return err
	}
	if res.rc == 0 || res.rc == notFoundRC {
		return nil
	}
	return keychainErrorf(
		"security delete-generic-password failed (rc=%d): %s", res.rc, strings.TrimSpace(res.stderr))
}

// realExec runs the security binary for real.
func realExec(ctx context.Context, argv []string, stdin string) (execResult, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return execResult{stdout: out.String(), stderr: errb.String(), rc: ee.ExitCode()}, nil
		}
		// Could not start (missing binary, etc.) or context cancelled.
		return execResult{}, err
	}
	return execResult{stdout: out.String(), stderr: errb.String(), rc: 0}, nil
}

var _ KeychainClient = Security{}
