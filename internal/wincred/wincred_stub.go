//go:build !windows

// Non-Windows stub: implements spec 07§5.3's platform gate for the Go port
// (DESIGN Amendment A9 — "Non-Windows builds compile a stub returning
// not-found"). The migrations package's Windows migration already checks
// Host.Platform() == platform.Windows before it ever calls Real, so this stub
// is never reached in a real run off Windows; it exists purely so
// internal/migrations links (and cross-platform tests can construct a Host)
// without a build tag of its own.

package wincred

// Real is the concrete Client used off Windows. Every call reports "not
// found" / succeeds as a no-op, matching a Windows Credential Manager with no
// legacy entries (there is no such store to query on this platform).
type Real struct{}

// New returns the non-Windows stub Client.
func New() Real { return Real{} }

// Get always reports not-found.
func (Real) Get(service, account string) (string, bool, error) { return "", false, nil }

// Delete is always a no-op success.
func (Real) Delete(service, account string) error { return nil }

var _ Client = Real{}
