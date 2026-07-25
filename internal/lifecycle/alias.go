// alias.go — SetAlias / UnsetAlias / ListAliases.
//
// Implements spec 01§8.3 (the alias command): set (or rename) with a
// case-insensitive conflict check, idempotent unset, and the slot-number-ordered
// listing of truthy aliases.
package lifecycle

import (
	"sort"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
)

// AliasRow is one row of ListAliases: (slot, alias, email).
type AliasRow struct {
	Num   string
	Alias string
	Email string
}

// SetAlias sets or renames the alias for the account matching identifier (spec
// 01§8.3). identifier may itself be an existing alias (rename). Returns the
// resolved slot and the normalized alias.
func SetAlias(s *store.Store, identifier, alias string) (num, normalized string, err error) {
	normalized, nerr := normalizeAlias(alias)
	if nerr != nil {
		return "", "", cerr.Validation("%s", nerr.Error())
	}
	// Answered before the lock is taken: acquiring it creates the backup root, so
	// a machine with no store would be left holding a directory containing only a
	// .lock — enough to mask a legacy backup the migration would otherwise adopt.
	if !sequenceFileExists(s) {
		return "", "", cerr.Config("No accounts are managed yet")
	}

	// The whole read-decide-write span runs under the store lock, with the one
	// classified read taken inside it (store.WithRosterLocked): the roster this
	// call commits is then the bytes on disk, so a concurrent cswap cannot land a
	// record between the read and the write for this commit to rename away.
	//
	// Inside the span, the entry read comes before store.ResolveAccount because
	// resolving fires the lazy org backfill, which WRITES a backfilled roster: the
	// entry read runs that backfill ahead of itself, so this call's commit carries
	// it rather than reverting it for every slot the alias never touched. A corrupt
	// roster refuses whichever read reaches it first — ResolveAccount's own read is
	// classified too — with the message that names the file and the repair, not
	// with "no such account" about a record still sitting on disk.
	//
	// The resolve stays INSIDE the span for the same reason the write is: it names
	// the slot the write lands on, and a slot resolved before the lock can be
	// renumbered by a concurrent move or swap before the write commits, putting the
	// alias on an account the user never named.
	err = s.WithRosterLocked(func(data *store.SequenceData) error {
		resolved, _, _, e := s.ResolveAccount(identifier)
		if e != nil {
			return e
		}
		num = resolved
		rec, ok := recordAt(data, num)
		if !ok {
			return cerr.AccountNotFound("Account-%s does not exist", num)
		}
		if conflict := s.AliasInUse(data, normalized, num); conflict != "" {
			return cerr.Config("Alias '%s' is already used by account %s", normalized, conflict)
		}
		rec.set("alias", normalized)
		if e := putRecord(data, num, rec); e != nil {
			return e
		}
		data.LastUpdated = timestamp(s)
		return s.WriteSequence(data)
	})
	if err != nil {
		return "", "", err
	}
	return num, normalized, nil
}

// UnsetAlias clears the alias for the account matching identifier (spec 01§8.3).
// Idempotent: an already-unset alias succeeds silently without writing.
func UnsetAlias(s *store.Store, identifier string) (num string, err error) {
	if !sequenceFileExists(s) {
		return "", cerr.Config("No accounts are managed yet")
	}
	// One locked span, one classified read inside it — see SetAlias.
	err = s.WithRosterLocked(func(data *store.SequenceData) error {
		resolved, _, _, e := s.ResolveAccount(identifier)
		if e != nil {
			return e
		}
		num = resolved
		rec, ok := recordAt(data, num)
		if !ok {
			return cerr.AccountNotFound("Account-%s does not exist", num)
		}
		if !rec.has("alias") {
			return nil // idempotent: nothing to clear, nothing to write
		}
		rec.del("alias")
		if e := putRecord(data, num, rec); e != nil {
			return e
		}
		data.LastUpdated = timestamp(s)
		return s.WriteSequence(data)
	})
	if err != nil {
		return "", err
	}
	return num, nil
}

// ListAliases returns every set alias as (num, alias, email), slot-number order
// (spec 01§8.3 list_aliases).
func ListAliases(s *store.Store) ([]AliasRow, error) {
	data, err := s.SequenceMigrated()
	if err != nil {
		return nil, err
	}
	var rows []AliasRow
	if data != nil {
		for num := range data.Accounts {
			rec := decodeRecord(data.Accounts[num])
			if a := rec.str("alias"); a != "" {
				rows = append(rows, AliasRow{Num: num, Alias: a, Email: rec.str("email")})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		ni, _ := parseSlot(rows[i].Num)
		nj, _ := parseSlot(rows[j].Num)
		return ni < nj
	})
	return rows, nil
}
