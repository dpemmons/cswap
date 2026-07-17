//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package lifecycle

import "golang.org/x/sys/unix"

// BSD/Darwin termios read/write ioctl requests.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
