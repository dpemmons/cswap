//go:build windows

// Implements spec 03§7 Windows backend: LockFileEx / UnlockFileEx on one byte.
// Not exercised by CI on this host, but implemented for parity.

package filelock

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock attempts a non-blocking exclusive byte-range lock on the first byte.
func tryLock(f *os.File) (bool, error) {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		h,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		return false, nil
	}
	return false, err
}

func unlock(f *os.File) {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
}
