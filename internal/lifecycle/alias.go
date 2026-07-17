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

	num, _, _, err = s.ResolveAccount(identifier)
	if err != nil {
		return "", "", err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return "", "", err
	}
	rec, ok := recordAt(data, num)
	if !ok {
		return "", "", cerr.AccountNotFound("Account-%s does not exist", num)
	}
	if conflict := s.AliasInUse(data, normalized, num); conflict != "" {
		return "", "", cerr.Config("Alias '%s' is already used by account %s", normalized, conflict)
	}
	rec.set("alias", normalized)
	if err := putRecord(data, num, rec); err != nil {
		return "", "", err
	}
	data.LastUpdated = timestamp(s)
	if err := s.WriteSequence(data); err != nil {
		return "", "", err
	}
	return num, normalized, nil
}

// UnsetAlias clears the alias for the account matching identifier (spec 01§8.3).
// Idempotent: an already-unset alias succeeds silently without writing.
func UnsetAlias(s *store.Store, identifier string) (num string, err error) {
	num, _, _, err = s.ResolveAccount(identifier)
	if err != nil {
		return "", err
	}
	data, err := s.ReadSequence()
	if err != nil {
		return "", err
	}
	rec, ok := recordAt(data, num)
	if !ok {
		return "", cerr.AccountNotFound("Account-%s does not exist", num)
	}
	if rec.has("alias") {
		rec.del("alias")
		if err := putRecord(data, num, rec); err != nil {
			return "", err
		}
		data.LastUpdated = timestamp(s)
		if err := s.WriteSequence(data); err != nil {
			return "", err
		}
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
