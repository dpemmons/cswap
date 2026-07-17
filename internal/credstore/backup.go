// Per-account backup credentials: base64 .enc files (every platform) and the
// macOS Keychain (service "claude-swap"). Reads are .enc-wins on every platform;
// a successful Keychain write reconciles the .enc away (correctness-critical).
// One .prev generation is retained per slot, routed by the same rule as the
// backup itself.
//
// Implements spec 03§5.7–5.8 and 01§3.2–3.5 (incl. the 01§14 fail-closed vs
// best-effort split: DeleteBackupStrict propagates, everything else logs).

package credstore

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

func (s *FileKeychainStore) backupEncPath(num, email string) string {
	return filepath.Join(s.credentialsDir, ".creds-"+num+"-"+email+".enc")
}

func (s *FileKeychainStore) prevBackupPath(num, email string) string {
	return filepath.Join(s.credentialsDir, ".creds-"+num+"-"+email+".enc.prev")
}

func (s *FileKeychainStore) backupUsername(num, email string) string {
	return "account-" + num + "-" + email
}

func (s *FileKeychainStore) prevBackupUsername(num, email string) string {
	return s.backupUsername(num, email) + ".prev"
}

// kcReadBackup reads a per-account backup from the Keychain only (via learn),
// "" when absent; it raises on a Keychain failure so the caller decides.
func (s *FileKeychainStore) kcReadBackup(num, email string) (string, error) {
	v, _, err := s.kcGet(securityService, s.backupUsername(num, email))
	if err != nil {
		return "", err
	}
	return v, nil
}

func (s *FileKeychainStore) kcWriteBackup(num, email, creds string) error {
	return s.kcSet(securityService, s.backupUsername(num, email), creds)
}

func (s *FileKeychainStore) kcDeleteBackup(num, email string) error {
	return s.kcDelete(securityService, s.backupUsername(num, email))
}

func (s *FileKeychainStore) kcDeleteBackupPrev(num, email string) error {
	return s.kcDelete(securityService, s.prevBackupUsername(num, email))
}

// deleteBackupKeychainQuiet is a best-effort backup Keychain delete (logs, never
// raises) that still updates the usability cache via learn.
func (s *FileKeychainStore) deleteBackupKeychainQuiet(num, email string) {
	if err := s.kcDeleteBackup(num, email); err != nil {
		s.log.Warningf("Failed to delete credentials from Keychain: %v", err)
	}
}

// KCReadBackup is the exported Keychain-only backup read (migrations, WP12).
func (s *FileKeychainStore) KCReadBackup(num, email string) (string, error) {
	return s.kcReadBackup(num, email)
}

// KCWriteBackup is the exported Keychain-only backup write (migrations, WP12).
func (s *FileKeychainStore) KCWriteBackup(num, email, creds string) error {
	return s.kcWriteBackup(num, email, creds)
}

// atomicB64Write base64-encodes credentials and atomically writes them to target
// under credentialsDir (0600), mirroring _atomic_b64_write. It mkdirs the parent
// with default perms but never chmods it (matching Python; unlike atomicfile,
// which chmods the parent to 0700).
func (s *FileKeychainStore) atomicB64Write(target, credentials string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
	return atomicRawWrite(s.credentialsDir, target, []byte(encoded))
}

func (s *FileKeychainStore) writeBackupEnc(num, email, creds string) error {
	return s.atomicB64Write(s.backupEncPath(num, email), creds)
}

// decodeEnc reads and strictly base64-decodes an .enc file, returning the
// decoded credential. found is false when the file is absent, unreadable,
// corrupt (validate=True rejects non-alphabet junk), or decodes empty — all of
// which fall through to the Keychain on macOS. logMsg is used for a read/decode
// warning to match Python's shared message.
func (s *FileKeychainStore) decodeEnc(path, logMsg string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		s.log.Warningf("%s: %v", logMsg, err)
		return "", false
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if derr != nil {
		s.log.Warningf("%s: %v", logMsg, derr)
		return "", false
	}
	if len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

// ReadBackup returns a slot's backup credential, "" when missing. macOS is
// .enc-wins: only an absent, unreadable, corrupt, or empty .enc falls through to
// the Keychain (spec 03§5.7). It never surfaces an error.
func (s *FileKeychainStore) ReadBackup(num, email string) (string, error) {
	encPath := s.backupEncPath(num, email)
	if s.encPresent(encPath) {
		if decoded, ok := s.decodeEnc(encPath, "Failed to read credentials file"); ok {
			return decoded, nil
		}
	}
	if s.macOS() {
		v, err := s.kcReadBackup(num, email)
		if err != nil {
			s.log.Warningf("Failed to read credentials from Keychain: %v", err)
			return "", nil
		}
		return v, nil
	}
	return "", nil
}

// WriteBackup persists a slot backup (spec 03§5.7). It retains the prior
// generation as .prev, then writes the Keychain (macOS, usable) reconciling the
// .enc away, else the .enc file (best-effort dropping the stale Keychain copy).
// A file-write failure is raised before returning so the switcher wrapper runs
// its post-write hook exactly once.
func (s *FileKeychainStore) WriteBackup(num, email, creds string) error {
	s.retainPreviousBackup(num, email, creds)
	if s.useKeychain() {
		err := s.kcWriteBackup(num, email, creds)
		if err == nil {
			return s.reconcileEncAfterKeychainWrite(num, email, creds)
		}
		if !keychain.IsUnusable(err) {
			return err // a programming error propagates
		}
		s.log.Warningf("Keychain backup write failed, falling back to file: %v", err)
		// fall through to file mode
	}
	if err := s.writeBackupEnc(num, email, creds); err != nil {
		s.log.Warningf("Failed to write credentials file: %v", err)
		return err
	}
	if s.macOS() {
		s.deleteBackupKeychainQuiet(num, email)
	}
	return nil
}

// reconcileEncAfterKeychainWrite removes a leftover .enc after a Keychain backup
// write; if the delete fails it rewrites the .enc with the fresh creds; if that
// also fails it propagates (never serve stale — reads are .enc-wins).
func (s *FileKeychainStore) reconcileEncAfterKeychainWrite(num, email, creds string) error {
	encPath := s.backupEncPath(num, email)
	if !exists(encPath) {
		return nil
	}
	if err := os.Remove(encPath); err == nil {
		return nil
	} else {
		s.log.Warningf("Could not delete .enc after Keychain backup write (%v); rewriting it with the fresh credentials to keep both consistent", err)
	}
	return s.writeBackupEnc(num, email, creds)
}

// DeleteBackup is the best-effort backup sweep (spec 03§5.7). It removes the
// .enc (and the legacy account-None alias), the macOS Keychain item, and the
// .prev generation for each; every failure is logged, none is raised.
func (s *FileKeychainStore) DeleteBackup(num, email string) error {
	nums := []string{num}
	if num != "None" {
		nums = append(nums, "None")
	}
	for _, n := range nums {
		encPath := s.backupEncPath(n, email)
		if exists(encPath) {
			if err := os.Remove(encPath); err != nil {
				s.log.Warningf("Failed to delete credentials file: %v", err)
			}
		}
		if s.macOS() {
			s.deleteBackupKeychainQuiet(n, email)
		}
		s.DeletePrev(n, email)
	}
	return nil
}

// DeleteBackupStrict is the fail-closed transactional clear (spec 03§5.7,
// 01§3.3): after the best-effort sweep it unconditionally removes the served
// .enc (missing_ok) and — even in file mode — the macOS Keychain item, with any
// permission/I/O or Keychain error propagating as a CredentialError that aborts
// the commit; a final read-back that still serves material raises the same.
func (s *FileKeychainStore) DeleteBackupStrict(num, email string) error {
	// Best-effort sweep first (legacy alias, .prev, quiet Keychain).
	_ = s.DeleteBackup(num, email)
	// Then assure the served key is gone, propagating failures.
	if err := removeMissingOK(s.backupEncPath(num, email)); err != nil {
		return cerr.Credential("Could not clear stored credentials for slot %s (%s) — aborting before commit: %v", num, email, err)
	}
	if s.macOS() {
		if err := s.kcDeleteBackup(num, email); err != nil {
			return cerr.Credential("Could not clear stored credentials for slot %s (%s) — aborting before commit: %v", num, email, err)
		}
	}
	// Final belt: any backend view the deletes above missed.
	if v, _ := s.ReadBackup(num, email); v != "" {
		return cerr.Credential("Could not clear stored credentials for slot %s (%s) — aborting before commit", num, email)
	}
	return nil
}

// -- previous-generation retention (.prev) -------------------------------------

// retainPreviousBackup copies the slot's current backup to .prev before it is
// replaced (spec 03§5.8): Keychain item when in use, else .enc.prev file, so a
// Keychain-backed Mac never grows a plaintext .prev. Best-effort. A same-value
// (or empty) rewrite retains nothing.
func (s *FileKeychainStore) retainPreviousBackup(num, email, newCreds string) {
	current, _ := s.ReadBackup(num, email)
	if current == "" || current == newCreds {
		return
	}
	var err error
	if s.useKeychain() {
		err = s.kcSet(securityService, s.prevBackupUsername(num, email), current)
	} else {
		err = s.atomicB64Write(s.prevBackupPath(num, email), current)
	}
	if err != nil {
		s.log.Warningf("Failed to retain previous credential generation for account %s: %v", num, err)
	}
}

// ReadPrev returns the retained previous generation, "" when absent/corrupt
// (.enc.prev-wins, spec 03§5.8).
func (s *FileKeychainStore) ReadPrev(num, email string) (string, error) {
	prevPath := s.prevBackupPath(num, email)
	if exists(prevPath) {
		if decoded, ok := s.decodeEnc(prevPath, "Failed to read .prev file"); ok {
			return decoded, nil
		}
	}
	if s.macOS() {
		v, _, err := s.kcGet(securityService, s.prevBackupUsername(num, email))
		if err != nil {
			s.log.Warningf("Failed to read .prev from Keychain: %v", err)
		} else {
			return v, nil
		}
	}
	return "", nil
}

// DeletePrev drops a slot's retained .prev generation on both backends
// (best-effort, spec 03§5.8). Also called standalone after a renumber so
// recovery can't resurrect a displaced generation onto a key's new owner.
func (s *FileKeychainStore) DeletePrev(num, email string) error {
	prevPath := s.prevBackupPath(num, email)
	if exists(prevPath) {
		if err := os.Remove(prevPath); err != nil {
			s.log.Warningf("Failed to delete .prev file: %v", err)
		}
	}
	if s.macOS() {
		if err := s.kcDeleteBackupPrev(num, email); err != nil {
			s.log.Warningf("Failed to delete .prev from Keychain: %v", err)
		}
	}
	return nil
}

// encPresent reports whether the .enc exists, normalizing a stat error on an
// unsearchable directory to "missing" (Python 3.12's Path.exists() quirk,
// spec 03§9.3): a not-exist is silent, any other stat error is logged.
func (s *FileKeychainStore) encPresent(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		s.log.Warningf("Failed to read credentials file: %v", err)
	}
	return false
}

// exists reports whether path exists (any stat error counts as absent).
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeMissingOK removes path, treating a not-exist as success but propagating
// any other error (Python's Path.unlink(missing_ok=True) — the fail-closed
// contract depends on permission/I/O errors aborting).
func removeMissingOK(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// atomicRawWrite writes data to target (under dir) via a temp sibling + rename,
// then chmods the file to 0600 (skipped on Windows). It mkdirs dir with default
// perms and never chmods it — mirroring Python's _atomic_b64_write /
// _write_active_credentials_file.
func atomicRawWrite(dir, target string, data []byte) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	committed = true
	if !platform.IsWindows() {
		if err := os.Chmod(target, 0o600); err != nil {
			return err
		}
	}
	return nil
}
