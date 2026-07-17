//go:build !windows

// POSIX terminal handoff: syscall.Exec replaces the cswap process image
// entirely (same as execvpe) — the FileLock is already released, so an exec'd
// claude never inherits a held flock.
//
// Implements spec 06§1.8 (_exec POSIX branch).
package session

import "syscall"

// Exec replaces the current process image with claude. It returns only if the
// exec syscall itself fails; on success control never comes back.
func (osRunner) Exec(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}
