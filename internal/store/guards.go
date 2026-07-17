// guards.go — the live-session guards and session-profile lifecycle helpers the
// store shares with internal/session (via the sessprofile leaf, which breaks the
// store↔session cycle).
//
// Implements spec 01§7 (_ensure_no_live_session, _session_dir,
// _live_session_pids, _invalidate_session_credentials, _delete_session_profile).
// Every destructive slot op funnels through EnsureNoLiveSession, which refuses
// while a session-mode `cswap run` process is live against that slot.
package store

import (
	"os"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

// SessionDir is the per-account session profile directory
// sessions/{num}-{slugify_email(email)}/ (spec 01§7).
func (s *Store) SessionDir(num, email string) string {
	return sessprofile.SessionDirFor(s.backupDir, num, email)
}

// LiveSessionPidsFor returns the PIDs of Claude instances running against a
// slot's session profile (spec 01§ live_session_pids_for).
func (s *Store) LiveSessionPidsFor(num, email string) []int {
	return sessprofile.LiveSessionPIDs(s.SessionDir(num, email))
}

// EnsureNoLiveSession refuses a destructive operation while a session-mode
// Claude instance is live against the slot (spec 01§7). action names the
// operation for the error message ("the operation", "--remove-account",
// "--swap-accounts", "--move-account").
func (s *Store) EnsureNoLiveSession(num, email, action string) error {
	pids := s.LiveSessionPidsFor(num, email)
	if len(pids) == 0 {
		return nil
	}
	return cerr.Session(
		"Account-%s (%s) has a live session-mode Claude instance (PID %s). Exit it first, then retry %s.",
		num, email, joinPIDs(pids), action)
}

// DeleteSessionProfile removes a slot's session profile directory and its
// Keychain entry (Keychain first — the hashed service name derives from the dir
// path; spec 01§7). Absent is a no-op. Logs on an actual removal.
func (s *Store) DeleteSessionProfile(num, email string) {
	dir := s.SessionDir(num, email)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	sessprofile.DeleteSessionProfile(s.kc, dir)
	if s.Log != nil {
		s.Log.Infof("Removed session profile for account %s at %s", num, dir)
	}
}
