//go:build !windows

// Implements spec 08§10.2/§10.4 console setup on non-Windows: VT is always
// available and UTF-8 is native, so both are no-ops.

package printer

func isWindows() bool { return false }

// enableWindowsVT is a no-op on non-Windows (returns true).
func enableWindowsVT() bool { return true }

// ForceUTF8Output is a no-op on non-Windows; Go writes UTF-8 bytes natively.
func ForceUTF8Output() {}
