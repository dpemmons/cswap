// credproxy.go — the credential/config backup proxies plus the
// _post_backup_write session-invalidation chokepoint.
//
// Implements spec 01§3 / 03§5.2–5.5 (backup credential + config I/O delegated to
// credstore, with the switcher wrapper running _post_backup_write exactly once
// after a successful write), spec 01§7 (_delete_account_files, _prune_mappings),
// and the persist/backfill helpers taken under the account FileLock (spec
// 01§10.1, non-reentrant). The store performs the pure write and raises on
// failure BEFORE returning, so _post_backup_write only ever runs after success.
package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/mappings"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/sessprofile"
)

// configBackupPath is configs/.claude-config-{num}-{email}.json (email raw,
// unslugified; spec 01§1.2).
func (s *Store) configBackupPath(num, email string) string {
	return filepath.Join(s.ConfigsDir, ".claude-config-"+num+"-"+email+".json")
}

// ReadAccountCredentials returns a slot's backup credential (.enc-wins), "" when
// missing (spec 03§5.3, via credstore).
func (s *Store) ReadAccountCredentials(num, email string) (string, error) {
	return s.Creds.ReadBackup(num, email)
}

// ReadAccountConfig returns a slot's backup config text, or "" when absent (spec
// 01§ _read_account_config). A non-NotExist read failure propagates.
func (s *Store) ReadAccountConfig(num, email string) (string, error) {
	data, err := os.ReadFile(s.configBackupPath(num, email))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// WriteAccountConfig writes a slot's backup config, chmod 0600 on non-Windows
// (spec 01§ _write_account_config). The config directory is created if needed.
func (s *Store) WriteAccountConfig(num, email, config string) error {
	path := s.configBackupPath(num, email)
	if err := os.MkdirAll(s.ConfigsDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return err
	}
	if !platform.IsWindows() {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// DeleteConfigBackup unconditionally unlinks a slot's config backup, treating a
// missing file as success (spec 01§10.5 _delete_config_backup). It is never
// exists()-guarded — that would fail open on an inaccessible dir in the
// required-clear paths — so permission/I/O errors propagate.
func (s *Store) DeleteConfigBackup(num, email string) error {
	err := os.Remove(s.configBackupPath(num, email))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// WriteAccountCredentials writes a slot's backup credential and then invalidates
// the slot's session profile exactly once (spec 01§3.2 / the switcher wrapper).
// The store raises on write failure before _post_backup_write runs.
func (s *Store) WriteAccountCredentials(num, email, creds string) error {
	if err := s.Creds.WriteBackup(num, email, creds); err != nil {
		return err
	}
	s.postBackupWrite(num, email)
	return nil
}

// postBackupWrite is _post_backup_write (spec 01§7): a LIVE session keeps its own
// credential copy but is stale-marked so setup_session re-bootstraps it once it
// exits; a non-live profile has its credential material dropped immediately so
// the next `cswap run` re-bootstraps from the fresh backup (history preserved).
func (s *Store) postBackupWrite(num, email string) {
	dir := s.SessionDir(num, email)
	if len(sessprofile.LiveSessionPIDs(dir)) > 0 {
		sessprofile.MarkStale(dir)
		return
	}
	existed, _ := sessprofile.InvalidateSessionCredentials(s.kc, dir)
	if existed && s.Log != nil {
		s.Log.Infof("Invalidated session credentials for account %s", num)
	}
}

// DeleteAccountFiles is the single chokepoint for every path that removes or
// displaces a slot (spec 01§7 _delete_account_files): refuse while a live
// session-mode instance holds the slot, then delete the backup credential, the
// config file, and the session profile (Keychain entry before the dir).
func (s *Store) DeleteAccountFiles(num, email string) error {
	if err := s.EnsureNoLiveSession(num, email, "the operation"); err != nil {
		return err
	}
	_ = s.Creds.DeleteBackup(num, email)
	if err := os.Remove(s.configBackupPath(num, email)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.DeleteSessionProfile(num, email)
	return nil
}

// PruneMappings drops directory mappings for an identity that no longer has a
// slot, returning the number pruned (spec 01§7 _prune_mappings). Slot migration
// and swap keep the (email, org) identity, so they never call this.
func (s *Store) PruneMappings(email, orgUUID string) (int, error) {
	return mappings.New(s.backupDir).PruneAccount(email, orgUUID)
}

// PersistBackupCredentials writes a rotated credential to an inactive slot's
// backup store under the account FileLock (spec 01§ persist_backup_credentials).
// The caller must NOT already hold the FileLock (it is non-reentrant).
func (s *Store) PersistBackupCredentials(num, email, creds string) error {
	return s.Lock.With(func() error {
		return s.WriteAccountCredentials(num, email, creds)
	})
}

// BackfillAccountUUID records a resolved account uuid on a slot that lacks one,
// under the FileLock (spec 01§ backfill_account_uuid). It only ever fills an
// EMPTY uuid — an existing uuid is identity and is never rewritten. An empty
// uuid argument is a no-op. The caller must NOT already hold the FileLock.
func (s *Store) BackfillAccountUUID(num, uuid string) error {
	if uuid == "" {
		return nil
	}
	return s.Lock.With(func() error {
		data, err := s.ReadSequence()
		if err != nil || data == nil {
			return err
		}
		rec, ok := recordFor(data, num)
		if !ok {
			return nil
		}
		if strings.TrimSpace(strField(rec, "uuid")) != "" {
			return nil
		}
		rec["uuid"] = uuid
		nb, err := encodeRecord(rec)
		if err != nil {
			return err
		}
		data.Accounts[num] = nb
		data.LastUpdated = s.timestamp()
		return s.WriteSequence(data)
	})
}
