//go:build windows

// Implements spec 06§4.3 Windows backend: OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION).
// Not exercised by CI on this host, but implemented for parity.

package procdetect

import "golang.org/x/sys/windows"

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
