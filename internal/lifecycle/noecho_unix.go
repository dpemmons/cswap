//go:build !windows

package lifecycle

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdTerminal is the production terminalControl over os.Stdin using termios
// ioctls (getpass parity, spec 01§6.1). The ioctl request numbers that read and
// write a termios differ across unix kernels; ioctlReadTermios /
// ioctlWriteTermios are chosen per-OS in the companion build-tagged files.
type stdTerminal struct{}

func (stdTerminal) isTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlReadTermios)
	return err == nil
}

func (stdTerminal) disableEcho() (func() error, error) {
	fd := int(os.Stdin.Fd())
	orig, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	raw := *orig
	raw.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}
	return func() error {
		return unix.IoctlSetTermios(fd, ioctlWriteTermios, orig)
	}, nil
}
