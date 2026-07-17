//go:build linux

package lifecycle

import "golang.org/x/sys/unix"

// Linux (and Linux-compatible) termios read/write ioctl requests.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
