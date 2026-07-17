// Package filelock is cswap's own cross-process advisory lock.
//
// Implements spec 03§7 (locking.py). POSIX uses flock(LOCK_EX|LOCK_NB) with a
// 0.1s poll and a monotonic timeout (default 10s); Windows uses LockFileEx on a
// single byte. The lock is NON-REENTRANT — callers never nest it. A failed With
// acquisition raises cerr.Lock (Python LockError).
package filelock

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// pollInterval matches Python's time.sleep(0.1).
const pollInterval = 100 * time.Millisecond

// DefaultTimeout matches Python FileLock(timeout=10.0).
const DefaultTimeout = 10 * time.Second

// FileLock is a cross-process advisory file lock.
//
// A single *FileLock may be shared across goroutines within one process (e.g.
// store.Lock, reused by parallel usage-fetch persist callbacks). The `hold`
// mutex is held for the entire span a goroutine owns the lock (from a
// successful Acquire until Release), so a second in-process caller blocks on it
// rather than opening a second file descriptor and clobbering the file/locked
// fields — which would leak the first holder's flock and spuriously time the
// second caller out. This mirrors Python's fresh-FileLock-per-with-site
// behavior, where concurrent holders serialize at the OS flock level and each
// releases its own descriptor. `mu` guards the file/locked fields.
type FileLock struct {
	path    string
	timeout time.Duration
	hold    sync.Mutex
	mu      sync.Mutex
	file    *os.File
	locked  bool
}

// New returns a FileLock for path. A non-positive timeout uses DefaultTimeout.
func New(path string, timeout time.Duration) *FileLock {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &FileLock{path: path, timeout: timeout}
}

// Acquire tries to take the lock, waiting up to timeout (its own timeout when
// <= 0). It returns false on timeout rather than an error, matching Python.
func (l *FileLock) Acquire(timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = l.timeout
	}
	// Serialize in-process holders: hold stays locked until Release, so a
	// concurrent caller waits here instead of clobbering the file descriptor.
	// It is unlocked here on any failure and by Release on success.
	l.hold.Lock()

	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		l.hold.Unlock()
		return false, err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		l.hold.Unlock()
		return false, err
	}

	start := time.Now()
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			l.hold.Unlock()
			return false, err
		}
		if ok {
			l.mu.Lock()
			l.file = f
			l.locked = true
			l.mu.Unlock()
			return true, nil
		}
		if time.Since(start) > timeout {
			f.Close()
			l.hold.Unlock()
			return false, nil
		}
		time.Sleep(pollInterval)
	}
}

// Release drops the lock. It is safe to call more than once.
func (l *FileLock) Release() error {
	l.mu.Lock()
	f := l.file
	locked := l.locked
	l.file = nil
	l.locked = false
	l.mu.Unlock()
	if f == nil || !locked {
		// Nothing held: either never acquired or already released. Do not
		// touch hold (a failed Acquire / prior Release already unlocked it).
		return nil
	}
	unlock(f)
	err := f.Close()
	l.hold.Unlock()
	return err
}

// With acquires the lock (default timeout), runs fn, then releases. If the lock
// cannot be acquired it returns cerr.Lock without running fn.
func (l *FileLock) With(fn func() error) error {
	ok, err := l.Acquire(l.timeout)
	if err != nil {
		return err
	}
	if !ok {
		return cerr.Lock("Failed to acquire lock - another instance may be running")
	}
	defer l.Release()
	return fn()
}
