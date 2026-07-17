// Integration-style tests for spec 03§6 (claude_locks.py) and 03§8's lock list:
// mkdir-mutex acquire/release, held-fresh timeout, back-dated stale takeover,
// the 3s toucher keeping mtime fresh, release tolerating a stolen lock, mutual
// exclusion under real goroutines, and CLAUDE_CONFIG_DIR-honoring lock paths.
package cclock_test

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/cclock"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

var sys = clock.System{}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestConstants(t *testing.T) {
	if cclock.StalenessS != 10*time.Second {
		t.Errorf("StalenessS = %v, want 10s", cclock.StalenessS)
	}
	if cclock.TouchIntervalS != 3*time.Second {
		t.Errorf("TouchIntervalS = %v, want 3s", cclock.TouchIntervalS)
	}
	if cclock.DefaultTimeoutS != 9*time.Second {
		t.Errorf("DefaultTimeoutS = %v, want 9s", cclock.DefaultTimeoutS)
	}
}

func TestAcquireCreatesAndReleaseRemoves(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	h, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !exists(lockDir) {
		t.Fatal("lock dir not created after Acquire")
	}
	h.Release()
	if exists(lockDir) {
		t.Fatal("lock dir not removed after Release")
	}
}

func TestReacquireAfterRelease(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	h1, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	h1.Release()
	h2, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	h2.Release()
}

func TestCreatesMissingParentDirs(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "a", "b", "c", "x.lock")
	h, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("Acquire with missing parents: %v", err)
	}
	defer h.Release()
	if !exists(lockDir) {
		t.Fatal("lock dir not created under missing parents")
	}
}

// TestHeldFreshLockTimesOut: a fresh lock held by "someone else" (a bare mkdir
// with no toucher, so mtime stays recent) must not be stolen; Acquire times out
// with ClaudeCodeLockTimeout in well under 5s and leaves the holder's dir intact.
func TestHeldFreshLockTimesOut(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(lockDir, 0o777); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockDir)

	start := time.Now()
	h, err := cclock.Acquire(lockDir, 500*time.Millisecond, sys)
	elapsed := time.Since(start)

	if err == nil {
		h.Release()
		t.Fatal("Acquire succeeded on a fresh held lock; want timeout")
	}
	if got := cerr.TypeName(err); got != "ClaudeCodeLockTimeout" {
		t.Errorf("error type = %q, want ClaudeCodeLockTimeout", got)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("timeout took %v, want < 5s", elapsed)
	}
	if !exists(lockDir) {
		t.Error("holder's lock dir was removed despite being fresh")
	}
}

// TestBackDatedLockTakenOver: a lock whose mtime is 30s old is stale and gets
// removed and retaken; the new lock's mtime is fresh (< 5s).
func TestBackDatedLockTakenOver(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(lockDir, 0o777); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(lockDir, past, past); err != nil {
		t.Fatal(err)
	}

	h, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("Acquire should take over a stale lock: %v", err)
	}
	defer h.Release()

	fi, err := os.Stat(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age >= 5*time.Second {
		t.Errorf("new lock mtime age = %v, want < 5s (fresh after takeover)", age)
	}
}

// TestReleaseToleratesStolenLock: the lock dir is removed out from under a live
// holder; Release must not panic or error, and must leave nothing behind.
func TestReleaseToleratesStolenLock(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	h, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Simulate another holder taking it over as stale.
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	h.Release() // must be a no-op, tolerated
	if exists(lockDir) {
		t.Error("lock dir present after Release of a stolen lock")
	}
	// A second Release is safe (idempotent).
	h.Release()
}

// TestToucherKeepsMtimeFresh: hold the lock for 4s after back-dating its mtime;
// the 3s toucher must have bumped it back to ~now.
func TestToucherKeepsMtimeFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 4s toucher timing test in -short mode")
	}
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	h, err := cclock.Acquire(lockDir, cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()

	// Back-date the mtime; the toucher (fires at 3s) must refresh it before 4s.
	past := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(lockDir, past, past); err != nil {
		t.Fatal(err)
	}

	time.Sleep(4 * time.Second)

	fi, err := os.Stat(lockDir)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age >= 5*time.Second {
		t.Errorf("mtime age after 4s hold = %v; toucher did not keep it fresh", age)
	}
}

// TestConcurrentAcquirersExcludeEachOther runs real goroutines contending for
// one lock dir and asserts mutual exclusion: no two hold the critical section at
// once, and every increment lands.
func TestConcurrentAcquirersExcludeEachOther(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "x.lock")
	const workers = 4

	var (
		wg       sync.WaitGroup
		inside   int32
		overlap  int32
		counter  int
		counterM sync.Mutex
		acqErr   atomic.Value
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Generous timeout so no worker times out under a loaded host; the
			// exclusion (not the timeout) is what is under test.
			h, err := cclock.Acquire(lockDir, 15*time.Second, sys)
			if err != nil {
				acqErr.Store(err)
				return
			}
			if atomic.AddInt32(&inside, 1) != 1 {
				atomic.StoreInt32(&overlap, 1)
			}
			counterM.Lock()
			counter++
			counterM.Unlock()
			time.Sleep(5 * time.Millisecond) // widen the exclusion window
			atomic.AddInt32(&inside, -1)
			h.Release()
		}()
	}
	wg.Wait()

	if v := acqErr.Load(); v != nil {
		t.Fatalf("a worker failed to acquire: %v", v)
	}
	if overlap != 0 {
		t.Error("two goroutines held the lock simultaneously")
	}
	if counter != workers {
		t.Errorf("counter = %d, want %d", counter, workers)
	}
	if exists(lockDir) {
		t.Error("lock dir left behind after all releases")
	}
}

func TestLockDirPaths_Default(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")

	if got, want := cclock.CredentialsLockDir(), filepath.Join(home, ".claude.lock"); got != want {
		t.Errorf("CredentialsLockDir = %q, want %q", got, want)
	}
	if got, want := cclock.ConfigLockDir(), filepath.Join(home, ".claude.json.lock"); got != want {
		t.Errorf("ConfigLockDir = %q, want %q", got, want)
	}
}

func TestLockDirPaths_ClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	ccd := filepath.Join(t.TempDir(), "custom-claude")
	if err := os.MkdirAll(ccd, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Setenv(t, "HOME", home)
	testutil.Setenv(t, "CLAUDE_CONFIG_DIR", ccd)

	// credentials_lock_dir: <config_home>.lock at the CCD's parent.
	if got, want := cclock.CredentialsLockDir(), ccd+".lock"; got != want {
		t.Errorf("CredentialsLockDir = %q, want %q", got, want)
	}
	// config_lock_dir: <CCD>/.claude.json.lock.
	if got, want := cclock.ConfigLockDir(), filepath.Join(ccd, ".claude.json.lock"); got != want {
		t.Errorf("ConfigLockDir = %q, want %q", got, want)
	}
}

// TestNamedLockDirsNest confirms the credentials lock is the outer dir and the
// config lock the inner one, so callers can hold them in the documented order.
func TestNamedLockDirsNest(t *testing.T) {
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")

	credH, err := cclock.Acquire(cclock.CredentialsLockDir(), cclock.DefaultTimeoutS, sys)
	if err != nil {
		t.Fatalf("credentials lock: %v", err)
	}
	cfgH, err := cclock.Acquire(cclock.ConfigLockDir(), cclock.DefaultTimeoutS, sys)
	if err != nil {
		credH.Release()
		t.Fatalf("config lock (nested): %v", err)
	}
	if !exists(cclock.CredentialsLockDir()) || !exists(cclock.ConfigLockDir()) {
		t.Error("both lock dirs should exist while held")
	}
	cfgH.Release()
	credH.Release()
	if exists(cclock.CredentialsLockDir()) || exists(cclock.ConfigLockDir()) {
		t.Error("lock dirs should be gone after release")
	}
}
