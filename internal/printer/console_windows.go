//go:build windows

// Implements spec 08§10.2/§10.4 and 08§15 Windows console setup: enable VT
// processing on the stdout handle and set the output code page to UTF-8. Not
// exercised by CI on this host.

package printer

import "golang.org/x/sys/windows"

func isWindows() bool { return true }

// enableWindowsVT enables ENABLE_VIRTUAL_TERMINAL_PROCESSING on STD_OUTPUT_HANDLE.
func enableWindowsVT() bool {
	h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}

// cpUTF8 is the Windows UTF-8 code-page identifier (65001).
const cpUTF8 = 65001

// ForceUTF8Output sets the console output code page to UTF-8 so box-drawing
// glyphs render on a legacy cp1252 console.
func ForceUTF8Output() {
	_ = windows.SetConsoleOutputCP(cpUTF8)
}
