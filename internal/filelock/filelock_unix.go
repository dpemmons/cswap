//go:build !windows

// Implements spec 03§7 POSIX backend: flock(LOCK_EX|LOCK_NB) / flock(LOCK_UN).

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock attempts a non-blocking exclusive flock. It returns (false, nil) when
// the lock is held by another process, (true, nil) on success.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
		return false, nil
	}
	return false, err
}

func unlock(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
