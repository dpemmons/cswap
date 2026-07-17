package filelock

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

func TestAcquireReleaseReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cswap.lock")
	l := New(path, DefaultTimeout)
	ok, err := l.Acquire(time.Second)
	if err != nil || !ok {
		t.Fatalf("Acquire = (%v, %v), want (true, nil)", ok, err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release = %v", err)
	}
	// Reacquirable after release.
	ok, err = l.Acquire(time.Second)
	if err != nil || !ok {
		t.Fatalf("re-Acquire = (%v, %v)", ok, err)
	}
	// Double release is safe.
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
}

func TestContentionTimesOut(t *testing.T) {
	// Two independent open file descriptions on the same path contend under
	// flock, mirroring two processes.
	path := filepath.Join(t.TempDir(), "cswap.lock")
	holder := New(path, DefaultTimeout)
	ok, err := holder.Acquire(time.Second)
	if err != nil || !ok {
		t.Fatalf("holder Acquire failed: %v", err)
	}
	defer holder.Release()

	other := New(path, DefaultTimeout)
	start := time.Now()
	ok, err = other.Acquire(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("contended Acquire error: %v", err)
	}
	if ok {
		t.Fatal("contended Acquire succeeded while lock held")
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("contended Acquire returned too fast (%v); did it really wait?", elapsed)
	}

	// Once the holder releases, the lock is acquirable.
	holder.Release()
	ok, err = other.Acquire(time.Second)
	if err != nil || !ok {
		t.Fatalf("Acquire after release = (%v, %v)", ok, err)
	}
	other.Release()
}

func TestWithReturnsLockErrorOnContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cswap.lock")
	holder := New(path, DefaultTimeout)
	if ok, err := holder.Acquire(time.Second); err != nil || !ok {
		t.Fatalf("holder Acquire failed: %v", err)
	}
	defer holder.Release()

	other := New(path, 300*time.Millisecond)
	ran := false
	err := other.With(func() error {
		ran = true
		return nil
	})
	if ran {
		t.Error("fn ran despite failed acquisition")
	}
	if err == nil || cerr.TypeName(err) != "LockError" {
		t.Errorf("With error = %v (type %q), want LockError", err, cerr.TypeName(err))
	}
	if err.Error() != "Failed to acquire lock - another instance may be running" {
		t.Errorf("LockError message = %q", err.Error())
	}
}

// TestSharedInstanceConcurrentWith mirrors store.Lock being reused by the
// parallel usage-fetch persist callbacks (reporting.runUsageFetches): one shared
// *FileLock, many goroutines calling With at once. Every With must run its fn
// under mutual exclusion and succeed — none may spuriously time out, and the
// lock must stay usable afterward (no leaked flock). The pre-fix code clobbered
// the shared file field, leaking the first holder's flock and timing the others
// out with a LockError.
func TestSharedInstanceConcurrentWith(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cswap.lock")
	l := New(path, 3*time.Second)

	const n = 8
	var running int32    // must never exceed 1 (mutual exclusion)
	var maxRunning int32 // observed peak concurrency
	var ran int32        // successful fn runs
	var errs int32       // With errors
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := l.With(func() error {
				cur := atomic.AddInt32(&running, 1)
				for {
					m := atomic.LoadInt32(&maxRunning)
					if cur <= m || atomic.CompareAndSwapInt32(&maxRunning, m, cur) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&running, -1)
				atomic.AddInt32(&ran, 1)
				return nil
			})
			if err != nil {
				atomic.AddInt32(&errs, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&errs); got != 0 {
		t.Errorf("With errors = %d, want 0 (shared instance must not spuriously fail)", got)
	}
	if got := atomic.LoadInt32(&ran); got != n {
		t.Errorf("fn ran %d times, want %d", got, n)
	}
	if got := atomic.LoadInt32(&maxRunning); got != 1 {
		t.Errorf("peak concurrent fn = %d, want 1 (mutual exclusion violated)", got)
	}
	// The lock must not be leaked: a fresh, immediate acquire on the same path
	// (separate open file description, mirroring another process) succeeds.
	other := New(path, DefaultTimeout)
	if ok, err := other.Acquire(500 * time.Millisecond); err != nil || !ok {
		t.Fatalf("post-concurrency Acquire = (%v, %v), want (true, nil) — flock leaked?", ok, err)
	}
	other.Release()
}

func TestWithRunsAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cswap.lock")
	l := New(path, DefaultTimeout)
	ran := false
	if err := l.With(func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("fn did not run")
	}
	// Lock released, so a fresh acquire succeeds immediately.
	other := New(path, DefaultTimeout)
	if ok, _ := other.Acquire(200 * time.Millisecond); !ok {
		t.Error("lock not released by With")
	}
	other.Release()
}
