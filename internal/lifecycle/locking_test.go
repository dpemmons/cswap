// Locking contracts for the write-capable lifecycle operations: the roster
// read-decide-write span runs under the store FileLock, nothing inside that span
// re-acquires it, and a span that cannot take it changes nothing.
//
// The concurrency tests build TWO *store.Store over ONE $HOME. That is the
// cross-process shape reproduced in-process: flock is per open file
// description, so two distinct *filelock.FileLock on the same path exclude each
// other exactly as two `cswap` processes do — unlike two goroutines sharing one
// *FileLock, which queue on its in-process mutex and would prove nothing about
// the file.
package lifecycle

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/filelock"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// secondStore builds another *store.Store over the same $HOME as s — a second
// process's view of one backup directory, with its own FileLock object.
func secondStore(t *testing.T) *store.Store {
	t.Helper()
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	s, err := store.New(store.Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("store.New (second view): %v", err)
	}
	return s
}

// syncOutput points the human-output seam at a mutex-guarded buffer for the
// test's duration: two lifecycle operations running at once both write to it.
func syncOutput(t *testing.T) {
	t.Helper()
	prev := Output
	Output = &lockedWriter{}
	t.Cleanup(func() { Output = prev })
}

type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// runBoth runs two operations concurrently and fails the test if either one does
// not return within the bound — the shape a re-acquisition of the store lock
// from inside the locked span takes (filelock.Acquire takes its in-process mutex
// BEFORE entering the timeout loop, so nesting one *FileLock never times out).
func runBoth(t *testing.T, bound time.Duration, a, b func() error) (errA, errB error) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errA = a() }()
	go func() { defer wg.Done(); errB = b() }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return errA, errB
	case <-time.After(bound):
		t.Fatalf("two concurrent operations did not both finish within %s", bound)
		return nil, nil
	}
}

// TestConcurrentAddTokensBothLand is the regression: two adds against one store,
// each holding a roster of its own, both reporting success. Unlocked, the second
// commit renames a file built from the pre-read roster over the first's record,
// and the first's credential and config backups stay on disk named by nothing.
// Under the lock the loser reads the winner's roster and appends to it.
func TestConcurrentAddTokensBothLand(t *testing.T) {
	s1 := newStore(t)
	s2 := secondStore(t)
	syncOutput(t)

	errA, errB := runBoth(t, 30*time.Second,
		func() error {
			return AddAccountFromToken(s1, "sk-ant-oat01-AAA", sp("one@token.local"), nil, true)
		},
		func() error {
			return AddAccountFromToken(s2, "sk-ant-oat01-BBB", sp("two@token.local"), nil, true)
		})
	if errA != nil || errB != nil {
		t.Fatalf("concurrent add-tokens: %v / %v", errA, errB)
	}

	assertRosterHolds(t, s1, map[string]string{
		"one@token.local": setupTokenCredentials("sk-ant-oat01-AAA"),
		"two@token.local": setupTokenCredentials("sk-ant-oat01-BBB"),
	})
}

// TestConcurrentAddAndAddTokenBothLand: the same race across the two entry
// points, which pick their slot through different code paths (the live login's
// next-free vs the token path's) but commit the same file.
func TestConcurrentAddAndAddTokenBothLand(t *testing.T) {
	s1 := newStore(t)
	seed(t, s1, ip(1), acct{num: "1", email: "a@example.com", uuid: "uuid-a", creds: "c1", config: "g1"})
	seedLiveLogin(t, s1, "live@example.com", "", "", "uuid-l", oauthBlob)
	s2 := secondStore(t)
	syncOutput(t)

	errA, errB := runBoth(t, 30*time.Second,
		func() error { return AddAccount(s1, nil, true, nil) },
		func() error {
			return AddAccountFromToken(s2, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, true)
		})
	if errA != nil || errB != nil {
		t.Fatalf("concurrent add / add-token: %v / %v", errA, errB)
	}

	data := readSeq(t, s1)
	if len(data.Accounts) != 3 {
		t.Fatalf("want 3 accounts, got %d: %v", len(data.Accounts), data.Accounts)
	}
	assertRosterHolds(t, s1, map[string]string{
		"a@example.com":    "c1",
		"live@example.com": oauthBlob,
		"tok@token.local":  setupTokenCredentials("sk-ant-oat01-TOK"),
	})
}

// TestConcurrentRemoveAndAddTokenBothLand: a removal and an add overlapping. The
// removal must not resurrect the added record, and the add must not resurrect
// the removed one — both of which a pre-lock roster produces, in whichever
// direction the two commits land.
func TestConcurrentRemoveAndAddTokenBothLand(t *testing.T) {
	s1 := newStore(t)
	seed(t, s1, ip(1),
		acct{num: "1", email: "gone@example.com", uuid: "uuid-g", creds: "c1", config: "g1"},
		acct{num: "2", email: "keep@example.com", uuid: "uuid-k", creds: "c2", config: "g2"},
	)
	s2 := secondStore(t)
	syncOutput(t)

	errA, errB := runBoth(t, 30*time.Second,
		func() error { return RemoveAccount(s1, "1", true) },
		func() error {
			return AddAccountFromToken(s2, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, true)
		})
	if errA != nil || errB != nil {
		t.Fatalf("concurrent remove / add-token: %v / %v", errA, errB)
	}

	data := readSeq(t, s1)
	if _, ok := data.Accounts["1"]; ok {
		t.Error("the removed record came back")
	}
	assertRosterHolds(t, s1, map[string]string{
		"keep@example.com": "c2",
		"tok@token.local":  setupTokenCredentials("sk-ant-oat01-TOK"),
	})
}

// assertRosterHolds asserts the roster names exactly the given identities and
// that each one's credential backup is still reachable under the slot its record
// names — the pair a lost update breaks: the record vanishes while the file it
// pointed at stays on disk.
func assertRosterHolds(t *testing.T, s *store.Store, want map[string]string) {
	t.Helper()
	data := readSeq(t, s)
	got := map[string]string{}
	for num := range data.Accounts {
		email := rec(t, data, num).str("email")
		creds, _ := s.ReadAccountCredentials(num, email)
		got[email] = creds
	}
	for email, creds := range want {
		stored, present := got[email]
		if !present {
			t.Errorf("%s is missing from the roster: %v", email, got)
			continue
		}
		if stored != creds {
			t.Errorf("%s credential backup = %q, want %q (record kept, backup orphaned)", email, stored, creds)
		}
	}
	if len(got) != len(want) {
		t.Errorf("roster holds %d accounts, want %d: %v", len(got), len(want), got)
	}
}

// probingPrompter answers "y" and, as it is asked, checks that the store lock is
// free. A DISTINCT FileLock on the same path is the other process's handle: if
// it can be taken, no cswap is holding one across this question.
type probingPrompter struct {
	lockPath string
	asked    int
	heldAt   int
}

func (p *probingPrompter) probe() {
	p.asked++
	rival := filelock.New(p.lockPath, 200*time.Millisecond)
	ok, err := rival.Acquire(200 * time.Millisecond)
	if err == nil && ok {
		rival.Release()
		return
	}
	p.heldAt++
}

func (p *probingPrompter) Prompt(string) (string, bool) { p.probe(); return "y", true }
func (p *probingPrompter) Secret(string) (string, bool) { p.probe(); return "sk-ant-oat01-TOK", true }
func (p *probingPrompter) StdinLine() (string, bool)    { p.probe(); return "sk-ant-oat01-TOK", true }

// TestNoPromptIsAskedUnderTheStoreLock is the constraint that shapes every
// operation above: the store lock has a 10-second cross-process budget and
// `cswap run`'s session bootstrap queues on the same file, so a question held
// under it fails other commands outright — and a user who steps away turns a
// lock into a hang. Each interactive path is exercised and the lock probed at
// the moment the question is asked.
func TestNoPromptIsAskedUnderTheStoreLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T) *store.Store
		run  func(s *store.Store) error
	}{
		{"add overwrite confirmation", func(t *testing.T) *store.Store {
			s := newStore(t)
			seed(t, s, ip(1), acct{num: "1", email: "old@example.com", uuid: "uuid-o", creds: "c1", config: "g1"})
			seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
			return s
		}, func(s *store.Store) error { return AddAccount(s, ip(1), false, nil) }},

		{"add-token overwrite confirmation", func(t *testing.T) *store.Store {
			s := newStore(t)
			seed(t, s, ip(1), acct{num: "1", email: "old@example.com", uuid: "uuid-o", creds: "c1", config: "g1"})
			return s
		}, func(s *store.Store) error {
			return AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), sp("1"), false)
		}},

		{"add-token secret entry", func(t *testing.T) *store.Store { return newStore(t) },
			func(s *store.Store) error { return AddAccountFromToken(s, "", nil, nil, false) }},

		{"remove confirmation", func(t *testing.T) *store.Store {
			s := newStore(t)
			seed(t, s, ip(1),
				acct{num: "1", email: "one@example.com", uuid: "uuid-1", creds: "c1", config: "g1"},
				acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
			)
			return s
		}, func(s *store.Store) error { return RemoveAccount(s, "2", false) }},

		{"remove ambiguous-email choice", func(t *testing.T) *store.Store {
			s := newStore(t)
			seed(t, s, ip(1),
				acct{num: "1", email: "dup@example.com", org: "orgA", uuid: "uuid-1", creds: "c1", config: "g1"},
				acct{num: "2", email: "dup@example.com", org: "orgB", uuid: "uuid-2", creds: "c2", config: "g2"},
			)
			return s
		}, func(s *store.Store) error { return RemoveAccount(s, "dup@example.com", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.seed(t)
			p := &probingPrompter{lockPath: s.LockFile}
			withPrompter(t, p)

			_ = tc.run(s)
			if p.asked == 0 {
				t.Fatal("precondition: no question was asked, so nothing was probed")
			}
			if p.heldAt != 0 {
				t.Errorf("%d of %d question(s) were asked with the store lock held", p.heldAt, p.asked)
			}
		})
	}
}

// lifecycleOps is every write-capable lifecycle operation, each with a store
// seeded so the call succeeds. They are the operations whose whole
// read-decide-write span must run under the store lock.
func lifecycleOps() []struct {
	name string
	seed func(t *testing.T) *store.Store
	run  func(s *store.Store) error
} {
	seeded := func(t *testing.T) *store.Store {
		s := newStore(t)
		seed(t, s, ip(1),
			acct{num: "1", email: "one@example.com", uuid: "uuid-1", alias: "one", creds: "c1", config: "g1"},
			acct{num: "2", email: "two@example.com", uuid: "uuid-2", creds: "c2", config: "g2"},
			acct{num: "3", email: "three@example.com", uuid: "uuid-3", creds: "c3", config: "g3"},
		)
		return s
	}
	withLogin := func(t *testing.T) *store.Store {
		s := seeded(t)
		seedLiveLogin(t, s, "new@example.com", "", "", "uuid-n", oauthBlob)
		return s
	}
	return []struct {
		name string
		seed func(t *testing.T) *store.Store
		run  func(s *store.Store) error
	}{
		{"AddAccount", withLogin, func(s *store.Store) error { return AddAccount(s, nil, true, nil) }},
		{"AddAccountDisplacing", withLogin, func(s *store.Store) error { return AddAccount(s, ip(2), true, nil) }},
		{"AddAccountFromToken", seeded, func(s *store.Store) error {
			return AddAccountFromToken(s, "sk-ant-oat01-TOK", sp("tok@token.local"), nil, true)
		}},
		{"RemoveAccount", seeded, func(s *store.Store) error { return RemoveAccount(s, "3", true) }},
		{"SetAlias", seeded, func(s *store.Store) error {
			_, _, err := SetAlias(s, "2", "work")
			return err
		}},
		{"UnsetAlias", seeded, func(s *store.Store) error {
			_, err := UnsetAlias(s, "1")
			return err
		}},
		{"SetAccountDisabled", seeded, func(s *store.Store) error { return SetAccountDisabled(s, "2", true) }},
		{"MoveAccount", seeded, func(s *store.Store) error {
			_, _, _, err := MoveAccount(s, "3", "7")
			return err
		}},
		{"SwapAccounts", seeded, func(s *store.Store) error {
			_, _, err := SwapAccounts(s, "1", "2")
			return err
		}},
	}
}

// TestLockedSpansNeverReacquireTheStoreLock is the deadlock guard. Every locked
// span is run end to end — including the credential and config writes, the
// dead-token clear (which takes a DIFFERENT lock file) and the session-profile
// work — with the store lock's timeout cut to 250ms. A callee that re-acquires
// the same *FileLock hangs forever and trips the bound; one that opens a second
// FileLock on the same path burns the timeout and returns a LockError. Neither
// is distinguishable from success without this test, because both would be
// invisible at the default 10s timeout in a suite nothing else contends with.
func TestLockedSpansNeverReacquireTheStoreLock(t *testing.T) {
	for _, op := range lifecycleOps() {
		t.Run(op.name, func(t *testing.T) {
			s := op.seed(t)
			s.Lock = filelock.New(s.LockFile, 250*time.Millisecond)

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- op.run(s) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s under a 250ms lock timeout: %v", op.name, err)
				}
				if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
					t.Errorf("%s took %s — long enough to have waited out a lock it already held", op.name, elapsed)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never returned: the store lock was re-acquired inside its own span", op.name)
			}
		})
	}
}

// TestLockedSpansFailClosedWhenTheLockIsHeld: a span that cannot take the lock
// does nothing at all. The holder is a DISTINCT FileLock on the same path (the
// other-process shape), so the contention is at the flock level and resolves by
// timeout rather than by queueing.
func TestLockedSpansFailClosedWhenTheLockIsHeld(t *testing.T) {
	for _, op := range lifecycleOps() {
		t.Run(op.name, func(t *testing.T) {
			s := op.seed(t)
			s.Lock = filelock.New(s.LockFile, 250*time.Millisecond)

			holder := filelock.New(s.LockFile, 5*time.Second)
			ok, err := holder.Acquire(5 * time.Second)
			if err != nil || !ok {
				t.Fatalf("precondition: could not hold the lock (%v, %v)", ok, err)
			}
			defer holder.Release()

			before := snapshotStore(t, s)
			err = op.run(s)
			if errKind(err) != "LockError" {
				t.Fatalf("%s with the lock held: want LockError, got %v (%q)", op.name, err, errKind(err))
			}
			assertStoreUnchanged(t, s, before, op.name+" with the lock held")
		})
	}
}
