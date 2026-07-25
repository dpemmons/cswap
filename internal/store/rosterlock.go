// rosterlock.go — the locked entry read every write-capable operation begins
// with.
//
// sequence.json is a read-modify-write of the WHOLE file: an operation reads a
// roster, decides against it, and renames a rebuilt file over it. Two such spans
// overlapping is last-writer-wins on every record, not on the field either run
// touched — the loser's record vanishes while the credential and config backups
// it already wrote stay on disk, named by nothing. Holding the store FileLock
// across the span is what makes the roster an operation decides from the same
// BYTES it commits, rather than merely the same in-memory object.
package store

// WithRosterLocked runs fn under the store FileLock with exactly ONE classified
// roster read taken INSIDE the lock, after the org backfill. No other cswap can
// write sequence.json while fn runs, so the roster fn decides from is the roster
// fn commits — the same bytes on disk, not merely the same object — and the
// backfill fn's commit carries is the backfill that is on disk.
//
// Two constraints the type system cannot state:
//
//   - The caller must NOT already hold s.Lock. filelock.FileLock is
//     non-reentrant and its Acquire takes an in-process mutex BEFORE entering
//     the timeout loop, so nesting the same *FileLock is a permanent goroutine
//     deadlock, not a timeout. (A second *FileLock object on the same path
//     conflicts at the flock level instead: a 10s stall, then cerr.Lock.)
//   - fn must NOT prompt a human. The cross-process acquire budget is 10s
//     (filelock.DefaultTimeout) and `cswap run`'s session bootstrap waits on the
//     same file, so a question held under the lock fails other commands outright.
//     Ask before the lock and re-validate the answer's premise inside it.
//
// A corrupt roster refuses before fn is invoked; the lock is released on that
// path like any other.
func (s *Store) WithRosterLocked(fn func(*SequenceData) error) error {
	return s.Lock.With(func() error {
		data, err := s.MigratedSequenceForUpdate()
		if err != nil {
			return err
		}
		return fn(data)
	})
}
