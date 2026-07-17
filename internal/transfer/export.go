// export.go — export_accounts (spec 07§2).
//
// Reads the local backup store (transparently backend-agnostic: file .enc or
// macOS Keychain, whichever the credential store resolves) and serializes one or
// all accounts to a .cswap file or stdout. The live active account is read from
// the live vault (fresher tokens) rather than its backup; a bulk export skips
// individually-broken slots with a stderr warning (issue #41) while a single
// named account treats the same condition as a hard failure. Missing oauthAccount
// is always fatal; --full embeds the whole ~/.claude.json, default embeds only
// oauthAccount (a hard privacy boundary, spec 07§8).
package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/credstore"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/slotkey"
	"git.dpemmons.com/dpemmons/cswap/internal/version"
)

// exportEntry is one per-account object in the envelope. Field order matches
// Python's insertion order; kind/alias are omitted when empty (never null/"",
// spec 07§1.2). Credentials is an OAuth object (map) or a raw API-key string;
// Config is the slim {"oauthAccount": ...} or the full parsed ~/.claude.json.
type exportEntry struct {
	Number           int    `json:"number"`
	Email            string `json:"email"`
	UUID             string `json:"uuid"`
	OrganizationUUID string `json:"organizationUuid"`
	OrganizationName string `json:"organizationName"`
	Added            string `json:"added"`
	Credentials      any    `json:"credentials"`
	Config           any    `json:"config"`
	Kind             string `json:"kind,omitempty"`
	Alias            string `json:"alias,omitempty"`
}

// exportEnvelope is the whole .cswap document (spec 07§1.1). activeAccountNumber
// is a *int so it serializes as null (never omitted) when no in-payload slot is
// the recorded active one.
type exportEnvelope struct {
	Version             int           `json:"version"`
	ExportedAt          string        `json:"exportedAt"`
	ExportedFrom        string        `json:"exportedFrom"`
	SwapVersion         string        `json:"swapVersion"`
	Encrypted           bool          `json:"encrypted"`
	ActiveAccountNumber *int          `json:"activeAccountNumber"`
	Accounts            []exportEntry `json:"accounts"`
}

// Export serializes accounts to destination ("-" for stdout). account limits the
// export to one NUM|EMAIL when non-empty; full embeds the entire ~/.claude.json
// per account. Mirrors export_accounts (spec 07§2).
func Export(acc Accounts, destination, account string, full bool) error {
	data, err := acc.MigratedSequence()
	if err != nil {
		return err
	}
	if data == nil || len(data.Accounts) == 0 {
		return cerr.Transfer("no accounts to export — run cswap --add-account first")
	}

	explicit := account != ""
	var targetNums []string
	if explicit {
		resolved, err := acc.ResolveSlot(account)
		if err != nil {
			return err
		}
		if resolved == "" || !slotOccupied(data, resolved) {
			return cerr.Transfer("account not found: %s", account)
		}
		targetNums = []string{resolved}
	} else {
		targetNums = sortedSlotKeys(data)
	}

	curEmail, curOrg, curOK := acc.CurrentAccount()

	var payload []exportEntry
	for _, num := range targetNums {
		record := decodeRecord(data.Accounts[num])
		email := strOrEmpty(record["email"])
		orgUUID := strOrEmpty(record["organizationUuid"])

		isActive := curOK && curEmail == email && curOrg == orgUUID

		var credsText, configText string
		if isActive {
			credsText, err = acc.ReadActiveCredentials()
			if err != nil || credsText == "" {
				return cerr.CredentialRead("failed to read live credentials for active account %s", email)
			}
			var found bool
			configText, found, err = acc.ReadActiveConfig()
			if err != nil {
				return err
			}
			if !found {
				return cerr.Config("Claude config file not found")
			}
		} else {
			credsText, err = acc.ReadAccountCredentials(num, email)
			if err != nil {
				return err
			}
			configText, err = acc.ReadAccountConfig(num, email)
			if err != nil {
				return err
			}
			if credsText == "" || configText == "" {
				if explicit {
					if credsText == "" {
						return cerr.CredentialRead("no backup credentials found for account %s (%s)", num, email)
					}
					return cerr.Config("no backup config found for account %s (%s)", num, email)
				}
				eprint("Skipping Account-" + num + " (" + email + "): no stored " +
					"credentials/config — re-add with: cswap --add-account --slot " + num)
				continue
			}
		}

		configObj, err := parsePayload(configText, "config for "+email)
		if err != nil {
			return err
		}
		var configOut any = configObj
		if !full {
			slim, err := slimConfig(configObj, "config for "+email)
			if err != nil {
				return err
			}
			configOut = slim
		}

		isAPIKey := credstore.LooksLikeAPIKey(credsText)
		var credsOut any
		if isAPIKey {
			credsOut = strings.TrimSpace(credsText)
		} else {
			obj, err := parsePayload(credsText, "credentials for "+email)
			if err != nil {
				return err
			}
			credsOut = obj
		}

		n, _ := strconv.Atoi(num)
		entry := exportEntry{
			Number:           n,
			Email:            email,
			UUID:             strOrEmpty(record["uuid"]),
			OrganizationUUID: orgUUID,
			OrganizationName: strOrEmpty(record["organizationName"]),
			Added:            strOrEmpty(record["added"]),
			Credentials:      credsOut,
			Config:           configOut,
		}
		if isAPIKey {
			entry.Kind = "api_key"
		}
		if a := strOrEmpty(record["alias"]); a != "" {
			entry.Alias = a
		}
		payload = append(payload, entry)
	}

	if len(payload) == 0 {
		return cerr.Transfer("no exportable accounts — all managed slots are missing stored " +
			"credentials/config. Re-add with: cswap --add-account --slot <number>")
	}

	// activeAccountNumber only carries a slot that is actually present in the
	// payload, else import would reference a missing account (spec 07§2.7).
	var activeInPayload *int
	if data.ActiveAccountNumber != nil {
		for _, e := range payload {
			if e.Number == *data.ActiveAccountNumber {
				v := *data.ActiveAccountNumber
				activeInPayload = &v
				break
			}
		}
	}

	envelope := exportEnvelope{
		Version:             FormatVersion,
		ExportedAt:          acc.Timestamp(),
		ExportedFrom:        platformTag(acc.Platform()),
		SwapVersion:         version.Display(),
		Encrypted:           false,
		ActiveAccountNumber: activeInPayload,
		Accounts:            payload,
	}

	serialized, err := marshalIndent2NoHTML(envelope)
	if err != nil {
		return err
	}

	if destination == "-" {
		// Pure JSON to stdout for pipe consumers; no summary line at all.
		if _, err := Stdout.Write(append(serialized, '\n')); err != nil {
			return err
		}
		return nil
	}

	outPath := expandUser(destination)
	if err := atomicWriteFile(outPath, string(serialized)+"\n"); err != nil {
		return err
	}
	eprint("Exported " + strconv.Itoa(len(payload)) + " account(s) to " + outPath)
	return nil
}

// parsePayload parses a JSON string that must decode to an object (transfer.py::
// _parse_payload). Corruption of a stored credential/config surfaces as a
// TransferError, distinct from user input error.
func parsePayload(text, label string) (map[string]any, error) {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil, cerr.Transfer("%s is not valid JSON: %s", label, err.Error())
	}
	m, ok := asObject(v)
	if !ok {
		return nil, cerr.Transfer("%s must be a JSON object", label)
	}
	return m, nil
}

// slimConfig reduces a parsed ~/.claude.json to just {"oauthAccount": ...},
// stripping every other top-level key (userID, anonymousId, projects, cached
// feature flags, absolute paths, …) so a cross-machine transfer stays small and
// leaks no source-machine identity (spec 07§2.4). Missing oauthAccount is fatal.
func slimConfig(config map[string]any, label string) (map[string]any, error) {
	oauth, ok := config["oauthAccount"].(map[string]any)
	if !ok {
		return nil, cerr.Transfer("%s is missing oauthAccount — cannot export", label)
	}
	return map[string]any{"oauthAccount": oauth}, nil
}

// platformTag maps the switcher's Platform to the _PLATFORM_TAG string, falling
// back to "unknown" for any unmapped value (spec 07§1.1). Platform.String()
// already yields exactly these tags.
func platformTag(p platform.Platform) string { return p.String() }

// sortedSlotKeys returns the account-map keys in the canonical slot-key total
// order (numerics first by value, then non-numerics lexicographically), matching
// Python sorted(keys, key=int) for the bulk-export case (spec 07§2.2).
func sortedSlotKeys(data *SequenceData) []string {
	keys := make([]string, 0, len(data.Accounts))
	for k := range data.Accounts {
		keys = append(keys, k)
	}
	return slotkey.Sorted(keys)
}

// atomicWriteFile writes content to path atomically with 0600 perms and NO
// parent-directory chmod — mirroring transfer.py::_atomic_write_file, not
// internal/atomicfile (which chmods the parent 0700; wrong for a user-chosen
// export destination). A temp file in the target's own directory guarantees the
// rename stays same-filesystem. The pid-suffixed temp name of the Python original
// is replaced by os.CreateTemp (not observable in the final file).
func atomicWriteFile(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cswap-*.tmp")
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
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !platform.IsWindows() {
		if err := os.Chmod(tmpName, 0o600); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	if !platform.IsWindows() {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// expandUser expands a leading ~ (Python Path.expanduser). ~user forms and a
// missing $HOME leave the path unchanged, matching Python's best-effort behaviour.
func expandUser(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
