//go:build windows

// Implements spec 06§4.3 Windows backend: OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION).
// Not exercised by CI on this host, but implemented for parity.

package procdetect

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// processQueryLimitedInformation mirrors ctypes' PROCESS_QUERY_LIMITED_INFORMATION.
const processQueryLimitedInformation = 0x1000

func isPIDAliveNative(pid int) bool {
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}

// ignoreStatError reports whether an os.Stat error means "the path can't name a
// directory" (treat as absent) rather than "the lookup itself failed". It mirrors
// pathlib's _ignore_error on Windows: the POSIX errno set (ENOENT/ENOTDIR/EBADF/
// ELOOP) plus the ignored winerrors (NOT_READY / INVALID_NAME / CANT_RESOLVE_
// FILENAME). Anything else — notably access-denied — is surfaced so the probe
// fails closed. An error carrying no recognizable code is treated as surfaced.
func ignoreStatError(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP:
			return true
		}
	}
	var werr windows.Errno
	if errors.As(err, &werr) {
		switch werr {
		case windows.ERROR_NOT_READY, windows.ERROR_INVALID_NAME, windows.ERROR_CANT_RESOLVE_FILENAME:
			return true
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return true
		}
	}
	return false
}
