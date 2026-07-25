// moveswap.go — MoveAccount (relocate/swap) and SwapAccounts, plus the staging,
// session-dir exchange, and rollback machinery.
//
// Implements spec 01§10.3–10.5 (move_account / _relocate_locked /
// _swap_accounts_locked). The whole resolve-validate-mutate span runs under one
// FileLock acquisition (non-reentrant, so resolution and dispatch share it —
// a slot resolved outside the lock could be renumbered by a concurrent move),
// and reads the roster exactly once inside it: both entry points read (refusing
// an unparseable sequence.json rather than treating it as no accounts) and hand
// that roster to the locked bodies, which validate and commit the same object.
// The sequence.json write is THE commit point: a failure before it rolls both
// slots back (via durable 0600 O_EXCL staging copies when the backup keys
// overlap on a same-email swap), and after it only best-effort cleanup remains.
// Required destination clears are fail-closed (DeleteBackupStrict aborts the
// commit); post-commit cleanup, .prev drops, and session moves are best-effort.
package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// MoveAccount assigns account (NUM|EMAIL|ALIAS) to slot number target (spec
// 01§10.3, the general form of swap). Returns (sourceNum, targetNum, swapped).
func MoveAccount(s *store.Store, account, target string) (srcNum, tgtNum string, swapped bool, err error) {
	if !sequenceFileExists(s) {
		return "", "", false, cerr.Config("No accounts are managed yet")
	}

	target = trimSpace(target)
	tnum, ok := parseSlot(target)
	if !ok || tnum < 1 {
		return "", "", false, cerr.Validation(
			"Target slot must be a positive slot number, got: '%s' (use `swap` to trade two accounts by identifier)", target)
	}
	target = strconv.Itoa(tnum) // normalize "01" -> "1"

	// One roster for the whole locked span (store.WithRosterLocked: the backfill,
	// then one classified read, all inside the lock), read before resolving and
	// handed down to relocateLocked/swapAccountsLocked. The file cannot change
	// under the lock, so re-reading would only add ways for the branches to
	// disagree with the validation below. Reading before resolving also runs the
	// org backfill — which WRITES a roster — ahead of the read this span commits,
	// so the commit carries the backfill rather than reverting it. A corrupt
	// roster refuses before any of that, from this read or from ResolveAccount's
	// own classified one; neither reports it as a missing account.
	err = s.WithRosterLocked(func(data *store.SequenceData) error {
		numSrc, _, _, e := s.ResolveAccount(account)
		if e != nil {
			return e
		}

		maxSlot := 0
		for n := range data.Accounts {
			if v, okk := parseSlot(n); okk && v > maxSlot {
				maxSlot = v
			}
		}
		cap := 99
		if maxSlot > cap {
			cap = maxSlot
		}
		if tnum > cap {
			return cerr.Validation(
				"Target slot %s is out of range (1-%d): new accounts are numbered from the highest slot, so a large target would inflate future account numbers",
				target, cap)
		}

		srcNum, tgtNum = numSrc, target
		if numSrc == target {
			swapped = false
			return nil
		}
		if _, occupied := data.Accounts[target]; occupied {
			if _, _, e := swapAccountsLocked(s, data, numSrc, target); e != nil {
				return e
			}
			swapped = true
			return nil
		}
		swapped = false
		return relocateLocked(s, data, numSrc, target)
	})
	if err != nil {
		return "", "", false, err
	}
	return srcNum, tgtNum, swapped, nil
}

// SwapAccounts exchanges two accounts' slot numbers (spec 01§10.5). The whole
// resolve-validate-mutate span runs under one FileLock.
func SwapAccounts(s *store.Store, first, second string) (numA, numB string, err error) {
	if !sequenceFileExists(s) {
		return "", "", cerr.Config("No accounts are managed yet")
	}
	// The backfill and the classified entry read of the locked span, in the order
	// MoveAccount documents.
	err = s.WithRosterLocked(func(data *store.SequenceData) error {
		a, b, e := swapAccountsLocked(s, data, first, second)
		numA, numB = a, b
		return e
	})
	if err != nil {
		return "", "", err
	}
	return numA, numB, nil
}

// relocateLocked moves numSrc into the empty slot target (spec 01§10.4). Caller
// holds the FileLock and passes the roster it read under it (never nil); this
// mutates that roster and commits it.
func relocateLocked(s *store.Store, data *store.SequenceData, numSrc, target string) error {
	if _, ok := recordAt(data, numSrc); !ok {
		return cerr.AccountNotFound("Account-%s does not exist", numSrc)
	}
	if _, occupied := data.Accounts[target]; occupied {
		return cerr.Validation("Slot %s is already occupied — retry the move", target)
	}
	email := decodeRecord(data.Accounts[numSrc]).str("email")

	if err := s.EnsureNoLiveSession(numSrc, email, "--move-account"); err != nil {
		return err
	}

	creds, _ := s.ReadAccountCredentials(numSrc, email)
	config, _ := s.ReadAccountConfig(numSrc, email)

	srcDir := s.SessionDir(numSrc, email)
	dstDir := s.SessionDir(target, email)

	intSrc, _ := parseSlot(numSrc)
	intTarget, _ := parseSlot(target)

	run := func() error {
		// Best-effort session-profile move.
		if dirExists(srcDir) && !dirExists(dstDir) {
			if e := os.Rename(srcDir, dstDir); e != nil {
				logWarningf(s, "Session profile move skipped during move: %v", e)
			}
		}
		// Write-or-clear the target key to num_src's exact state.
		if err := writeOrClearCreds(s, target, email, creds); err != nil {
			return err
		}
		if err := writeOrClearConfig(s, target, email, config); err != nil {
			return err
		}
		// Mutate + commit.
		data.Accounts[target] = data.Accounts[numSrc]
		delete(data.Accounts, numSrc)
		data.Sequence = renumberSequence(data.Sequence, intSrc, intTarget)
		sort.Ints(data.Sequence)
		if data.ActiveAccountNumber != nil && *data.ActiveAccountNumber == intSrc {
			setActive(data, intTarget)
		}
		data.LastUpdated = timestamp(s)
		return s.WriteSequence(data) // commit point
	}

	if err := run(); err != nil {
		// Pre-commit failure: drop strays under the target key, put the profile
		// back, best-effort (records still point at num_src).
		_ = s.Creds.DeleteBackup(target, email)
		if e := s.DeleteConfigBackup(target, email); e != nil {
			logErrorf(s, "Cleanup after failed move incomplete: %v", e)
		}
		if dirExists(dstDir) && !dirExists(srcDir) {
			if e := os.Rename(dstDir, srcDir); e != nil {
				logErrorf(s, "Cleanup after failed move incomplete: %v", e)
			}
		}
		return err
	}

	// Post-commit best-effort cleanup.
	if e := s.DeleteAccountFiles(numSrc, email); e != nil {
		logErrorf(s, "Stale backup left under old key %s (%s): %v", numSrc, email, e)
	}
	if creds != "" {
		_ = s.Creds.DeletePrev(target, email)
	}
	logInfof(s, "Moved slot: %s (%s) -> %s", numSrc, email, target)
	return nil
}

// swapAccountsLocked is the body of SwapAccounts; caller holds the FileLock, has
// run the org backfill, and passes the roster it read under the lock (never
// nil), which this mutates and commits (spec 01§10.5).
func swapAccountsLocked(s *store.Store, data *store.SequenceData, first, second string) (string, string, error) {
	numA, _, _, err := s.ResolveAccount(first)
	if err != nil {
		return "", "", swapResolveErr(err, first)
	}
	numB, _, _, err := s.ResolveAccount(second)
	if err != nil {
		return "", "", swapResolveErr(err, second)
	}
	if numA == numB {
		return "", "", cerr.Validation("Cannot swap an account with itself")
	}

	recA, okA := data.Accounts[numA]
	recB, okB := data.Accounts[numB]
	if !okA {
		return "", "", cerr.AccountNotFound("Account-%s does not exist", numA)
	}
	if !okB {
		return "", "", cerr.AccountNotFound("Account-%s does not exist", numB)
	}
	emailA := decodeRecord(recA).str("email")
	emailB := decodeRecord(recB).str("email")

	if err := s.EnsureNoLiveSession(numA, emailA, "--swap-accounts"); err != nil {
		return "", "", err
	}
	if err := s.EnsureNoLiveSession(numB, emailB, "--swap-accounts"); err != nil {
		return "", "", err
	}

	credsA, _ := s.ReadAccountCredentials(numA, emailA)
	credsB, _ := s.ReadAccountCredentials(numB, emailB)
	configA, _ := s.ReadAccountConfig(numA, emailA)
	configB, _ := s.ReadAccountConfig(numB, emailB)

	intA, _ := parseSlot(numA)
	intB, _ := parseSlot(numB)

	staging := map[string]string{}
	run := func() error {
		if emailA == emailB {
			st, e := stageOverlapMaterial(s, []stageItem{
				{num: numA, creds: credsA, config: configA},
				{num: numB, creds: credsB, config: configB},
			})
			if e != nil {
				return e
			}
			staging = st
		}
		swapSessionDirs(s, numA, emailA, numB, emailB)

		if err := writeOrClearCreds(s, numB, emailA, credsA); err != nil {
			return err
		}
		if err := writeOrClearConfig(s, numB, emailA, configA); err != nil {
			return err
		}
		if err := writeOrClearCreds(s, numA, emailB, credsB); err != nil {
			return err
		}
		if err := writeOrClearConfig(s, numA, emailB, configB); err != nil {
			return err
		}

		data.Accounts[numA] = recB
		data.Accounts[numB] = recA
		data.Sequence = swapSequence(data.Sequence, intA, intB)
		sort.Ints(data.Sequence)
		if data.ActiveAccountNumber != nil {
			switch *data.ActiveAccountNumber {
			case intA:
				setActive(data, intB)
			case intB:
				setActive(data, intA)
			}
		}
		data.LastUpdated = timestamp(s)
		return s.WriteSequence(data) // commit point
	}

	if err := run(); err != nil {
		rollbackSwap(s, numA, emailA, credsA, configA, numB, emailB, credsB, configB, staging)
		return "", "", err
	}

	// Post-commit best-effort cleanup.
	if emailA != emailB {
		for _, ne := range [][2]string{{numA, emailA}, {numB, emailB}} {
			if e := s.DeleteAccountFiles(ne[0], ne[1]); e != nil {
				logErrorf(s, "Stale backup left under old key %s (%s): %v", ne[0], ne[1], e)
			}
		}
	}
	if credsA != "" {
		_ = s.Creds.DeletePrev(numB, emailA)
	}
	if credsB != "" {
		_ = s.Creds.DeletePrev(numA, emailB)
	}
	discardStaging(s, staging)
	logInfof(s, "Swapped slots: %s (%s) <-> %s (%s)", numA, emailA, numB, emailB)
	return numA, numB, nil
}

// swapResolveErr rewrites resolve's AccountNotFound "No account found with
// identifier: X" message to name the swap argument (Python re-raises with the
// same text keyed on first/second). Non-AccountNotFound errors pass through.
func swapResolveErr(err error, identifier string) error {
	var ce *cerr.Error
	if e, ok := err.(*cerr.Error); ok {
		ce = e
	}
	if ce != nil && ce.Kind == cerr.KindAccountNotFound {
		return cerr.AccountNotFound("No account found with identifier: %s", identifier)
	}
	return err
}

// stageItem is one slot's backup material to stage.
type stageItem struct {
	num, creds, config string
}

// stageOverlapMaterial parks durable 0600 O_EXCL copies of overlapping backup
// material before a same-email swap overwrites it (spec 01§10.5
// _stage_overlap_material). A leftover staging file is a loud refusal, never an
// overwrite. Returns kind-num → path for later discard.
func stageOverlapMaterial(s *store.Store, items []stageItem) (map[string]string, error) {
	staged := map[string]string{}
	for _, item := range items {
		for _, kc := range []struct{ kind, content string }{
			{"creds", item.creds}, {"config", item.config},
		} {
			if kc.content == "" {
				continue
			}
			path := stagingPath(s, kc.kind, item.num)
			if _, err := os.Stat(path); err == nil {
				discardStaging(s, staged)
				return nil, cerr.Config(
					"Found leftover staging from an interrupted swap: %s. It holds that slot's pre-swap credentials and may be the only surviving copy. Verify both accounts still work (`cswap list`), then delete the file and retry.",
					path)
			}
			fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				discardStaging(s, staged)
				return nil, cerr.Config("Could not stage swap material, nothing was changed: %v", err)
			}
			_, werr := fd.WriteString(kc.content)
			cerr2 := fd.Close()
			if werr != nil || cerr2 != nil {
				discardStaging(s, staged)
				if werr == nil {
					werr = cerr2
				}
				return nil, cerr.Config("Could not stage swap material, nothing was changed: %v", werr)
			}
			staged[kc.kind+"-"+item.num] = path
		}
	}
	return staged, nil
}

// discardStaging removes staged copies, warning loudly about any survivor (it
// holds plaintext credentials and also blocks the next same-email swap).
func discardStaging(s *store.Store, staging map[string]string) {
	for _, path := range staging {
		if err := os.Remove(path); err != nil {
			logErrorf(s, "Could not remove swap staging copy: %v", err)
			emitWarning(fmt.Sprintf("Could not remove swap staging file %s — it holds pre-swap credentials; please delete it manually.", path))
		}
	}
}

// swapSessionDirs exchanges two slots' session-profile directories, best-effort
// (spec 01§10.5 _swap_session_dirs), staging the first through a ".swapping"
// rename so the two same-email paths can cross.
func swapSessionDirs(s *store.Store, numA, emailA, numB, emailB string) {
	dirA := s.SessionDir(numA, emailA)
	dirB := s.SessionDir(numB, emailB)
	newA := s.SessionDir(numB, emailA) // A's new home
	newB := s.SessionDir(numA, emailB) // B's new home

	staging := ""
	var opErr error
	if dirExists(dirA) {
		staging = dirA + ".swapping"
		if e := os.Rename(dirA, staging); e != nil {
			opErr, staging = e, ""
		}
	}
	if opErr == nil && dirExists(dirB) && !dirExists(newB) {
		if e := os.Rename(dirB, newB); e != nil {
			opErr = e
		}
	}
	if opErr == nil && staging != "" && !dirExists(newA) {
		if e := os.Rename(staging, newA); e != nil {
			opErr = e
		} else {
			staging = ""
		}
	}
	if opErr != nil {
		logWarningf(s, "Session profile move skipped during swap: %v", opErr)
	}
	if staging != "" && !dirExists(dirA) {
		_ = os.Rename(staging, dirA) // never strand a profile under the staging name
	}
}

// rollbackSwap best-effort restores both slots to their old keys after a failed
// swap mutation (spec 01§10.5 _rollback_swap). Same-email overlap: an
// originally-empty key is strict-cleared back to empty; on any restore failure
// the staged copies are kept and reported.
func rollbackSwap(s *store.Store, numA, emailA, credsA, configA, numB, emailB, credsB, configB string, staging map[string]string) {
	logErrorf(s, "Swap %s <-> %s failed mid-write; restoring both slots", numA, numB)
	failures := 0
	swapSessionDirs(s, numB, emailA, numA, emailB) // reverse the exchange
	overlap := emailA == emailB

	type restore struct {
		kind, num, email, original string
	}
	for _, r := range []restore{
		{"creds", numA, emailA, credsA},
		{"config", numA, emailA, configA},
		{"creds", numB, emailB, credsB},
		{"config", numB, emailB, configB},
	} {
		var e error
		if r.original != "" {
			if r.kind == "creds" {
				e = s.WriteAccountCredentials(r.num, r.email, r.original)
			} else {
				e = s.WriteAccountConfig(r.num, r.email, r.original)
			}
		} else if overlap {
			if r.kind == "creds" {
				e = s.Creds.DeleteBackupStrict(r.num, r.email)
			} else {
				e = s.DeleteConfigBackup(r.num, r.email)
			}
		}
		if e != nil {
			failures++
			logErrorf(s, "Rollback %s restore failed for slot %s: %v", r.kind, r.num, e)
		}
	}

	if emailA != emailB {
		for _, ne := range [][2]string{{numB, emailA}, {numA, emailB}} {
			if e := s.Creds.DeleteBackup(ne[0], ne[1]); e != nil {
				failures++
				logErrorf(s, "Rollback cleanup failed for slot %s: %v", ne[0], e)
			}
			if e := s.DeleteConfigBackup(ne[0], ne[1]); e != nil {
				failures++
				logErrorf(s, "Rollback cleanup failed for slot %s: %v", ne[0], e)
			}
		}
	}

	if failures == 0 {
		for _, ne := range []struct{ num, email, original string }{
			{numA, emailA, credsA}, {numB, emailB, credsB},
		} {
			if ne.original != "" {
				_ = s.Creds.DeletePrev(ne.num, ne.email)
			}
		}
	}

	if len(staging) > 0 {
		if failures > 0 {
			kept := stagingPaths(staging)
			logErrorf(s, "Rollback incomplete — staged pre-swap copies kept for manual recovery: %s", kept)
			emitWarning("Swap rollback was incomplete; your pre-swap credentials are preserved in: " + kept)
		} else {
			discardStaging(s, staging)
		}
	}
}

// writeOrClearCreds writes material that exists or fail-closed-clears what
// doesn't (spec 01§10.4/10.5 write-or-clear). The strict clear aborts the commit.
func writeOrClearCreds(s *store.Store, num, email, creds string) error {
	if creds != "" {
		return s.WriteAccountCredentials(num, email, creds)
	}
	return s.Creds.DeleteBackupStrict(num, email)
}

func writeOrClearConfig(s *store.Store, num, email, config string) error {
	if config != "" {
		return s.WriteAccountConfig(num, email, config)
	}
	return s.DeleteConfigBackup(num, email)
}

// stagingPath is credentials/.swap-staging-{kind}-{num}.json (spec 01§1.2).
func stagingPath(s *store.Store, kind, num string) string {
	return filepath.Join(s.CredentialsDir, ".swap-staging-"+kind+"-"+num+".json")
}

func renumberSequence(seq []int, from, to int) []int {
	out := make([]int, len(seq))
	for i, n := range seq {
		if n == from {
			out[i] = to
		} else {
			out[i] = n
		}
	}
	return out
}

func swapSequence(seq []int, a, b int) []int {
	out := make([]int, len(seq))
	for i, n := range seq {
		switch n {
		case a:
			out[i] = b
		case b:
			out[i] = a
		default:
			out[i] = n
		}
	}
	return out
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func stagingPaths(staging map[string]string) string {
	paths := make([]string, 0, len(staging))
	for _, p := range staging {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
