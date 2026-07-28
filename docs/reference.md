# cswap — Command Reference

## NAME

cswap — multi-account switcher for Claude Code.

## SYNOPSIS

```
cswap                                       open the interactive dashboard (TTY only)
cswap help                                  print the command list and options
cswap list [--json] [--token-status]        list managed accounts
cswap status [--json]                       show the active account
cswap switch [--json]                       rotate to the next account
cswap switch <NUM|EMAIL|ALIAS> [--json] [--force]
                                            switch to a specific account
cswap switch --strategy {best|next-available} [--model NAMES] [--json]
                                            pick the target by remaining quota
cswap add [--slot NUM] [--alias NAME]       snapshot the current login as an account
cswap add-token [TOKEN|-] [--email EMAIL] [--slot NUM]
                                            register a setup-token or API key
cswap remove <NUM|EMAIL|ALIAS>              remove an account (prompts to confirm)
cswap disable <NUM|EMAIL|ALIAS>             hold an account out of auto-rotation
cswap enable <NUM|EMAIL|ALIAS>              return a disabled account to rotation
cswap alias [<NUM|EMAIL|ALIAS> <NAME>]      set / list account aliases
cswap alias <NUM|EMAIL|ALIAS> --unset       remove an account's alias
cswap move <NUM|EMAIL|ALIAS> <SLOT>         assign an account to a slot (swaps if taken)
cswap swap <A> <B>                          exchange two accounts' slot numbers
cswap run [<NUM|EMAIL|ALIAS>] [--no-share] [--share-history|--no-share-history] [-- ARGS...]
                                            launch Claude Code as an account, this terminal only
cswap env [<NUM|EMAIL|ALIAS>] [--no-share] [--share-history] [--shell {sh|fish|pwsh}] [--unset]
                                            print eval-able env lines that pin this shell
cswap map [<NUM|EMAIL|ALIAS> [PATH]]        map a directory to an account / list mappings
cswap unmap [PATH]                          remove a directory mapping
cswap auto [--once] [--json] [--interval SECONDS] [--threshold PCT] [--cooldown SECONDS]
           [--model NAMES] [--include-api-key-accounts|--no-include-api-key-accounts]
           [--dry-run]                      auto-switch when nearing rate limits
cswap config [list] [--json]                show settings
cswap config get <KEY> [--json]             read one setting
cswap config set <KEY> <VALUE>              change one setting
cswap config unset <KEY>                    clear one setting
cswap config path                           print the settings.json path
cswap export <PATH> [--account NUM|EMAIL] [--full]
                                            export accounts to a file ("-" = stdout)
cswap import <PATH> [--force]               import accounts from a file ("-" = stdin)
cswap tui                                    interactive dashboard
cswap watch                                  interactive dashboard, live watch page
cswap menubar                                macOS menu bar app (not available in this build)
cswap upgrade                                self-upgrade to the latest release
cswap purge                                  remove all claude-swap data (prompts to confirm)
cswap --version                              print the program name and version
```

Every subcommand also accepts `--debug` (enable debug logging) and `-h` / `--help`.

Legacy flag spellings are equivalent to the verbs and remain supported:
`--list`, `--status`, `--switch`, `--switch-to <id>`, `--add-account`,
`--add-token [tok]`, `--remove-account <id>`, `--disable-account <id>`,
`--enable-account <id>`, `--export <path>`, `--import <path>`, `--tui`,
`--watch`, `--menubar`, `--upgrade`, `--purge`. `ls` is an alias for `list`,
`rm` for `remove`, `update` for `upgrade`. A verb, or the leading legacy flag,
must be the first argument; `cswap --debug run 2` is not accepted (place the
verb first).

## Account resolution: NUM | EMAIL | ALIAS

Commands that name an account accept any of three forms, resolved in this order:

- **NUM** — the account's slot number (`2`). Slot numbers are stable and need
  not be contiguous.
- **EMAIL** — the account's exact recorded email address (`bob@example.com`).
- **ALIAS** — a short display name previously set with `cswap alias`
  (`dev`). An all-numeric string is never treated as an alias; it resolves as
  a slot number.

A form that matches no account raises `AccountNotFoundError`
(`No account found with identifier: <id>`), exit status 1.

---

## cswap list

### Synopsis

```
cswap list [--json] [--token-status] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--json` | flag | off | Mutually exclusive with `--token-status`. |
| `--token-status` | flag | off | Cannot combine with `--json`. |
| `--debug` | flag | off | — |

### Description

Lists every managed account in slot order with its usage summary. For each
account `list` shows the slot number, email (and alias when set), the
organization tag (`[personal]` or the organization name), and status markers:
`(active)` for the current default login, `(disabled)` for an account held out
of auto-rotation, and `at limit: <windows>` — the comma-separated labels of the
limiting windows (for example `5h`, `7d`, or a per-model name such as
`Fable 5`) — when a relevant rate-limit window is at or over its limit.
Accounts registered from an API key show `API key (no quota)`. `--token-status` adds an OAuth token expiry line per account.
When usage cannot be fetched the row reads `usage unavailable (<reason>)` where
`<reason>` is the last fetch error (for example `http-429`, `http-401`).

### Files

Reads `sequence.json`, per-account `configs/` and `credentials/`, and
`cache/usage.json` from the backup root. Writes nothing.

### Exit status

`0` on success. `1` on a handled error. `2` on a usage error (for example
`--token-status` combined with `--json`).

### Output

Human output is a titled `Accounts:` block, one stanza per account.

With `--json`, exactly one indented JSON document is written to stdout:

```
{
  "schemaVersion": 1,
  "activeAccountNumber": <int|null>,
  "accounts": [ <account-row>, ... ]
}
```

An `<account-row>` object contains these keys.

Always present:

| Key | Type | Meaning |
|-----|------|---------|
| `number` | int | Slot number. |
| `email` | string | Recorded email. |
| `organizationName` | string | Organization name, or `""`. |
| `organizationUuid` | string | Organization UUID, or `""`. |
| `isOrganization` | bool | `true` when `organizationUuid` is non-empty. |
| `active` | bool | `true` for the current default login. |
| `usageStatus` | string | One of the seven values below. |
| `usage` | object \| null | Usage windows when `usageStatus` is `ok`; otherwise `null`. |

Present only when applicable (additive, schemaVersion 1):

| Key | Type | Present when |
|-----|------|--------------|
| `alias` | string | An alias is set. |
| `disabled` | `true` | The account is disabled. |
| `atLimit` | `true` | A relevant window is at/over its limit. |
| `limitingWindows` | array of string | Emitted with `atLimit`; the windows at/over limit, in relevant-window order. |
| `usageFetchedAt` | string (ISO-8601 UTC) | `usage` is non-null and the measurement time is known. |
| `usageAgeSeconds` | number | Emitted with `usageFetchedAt`. |

The `usageStatus` enumeration:

| Value | Meaning |
|-------|---------|
| `ok` | Usage was fetched; `usage` is populated. |
| `token_expired` | The OAuth token is expired. |
| `api_key` | The account authenticates with an API key (no OAuth quota). |
| `keychain_unavailable` | The macOS Keychain could not be read. |
| `relogin_required` | The account must be logged in again. |
| `no_credentials` | No stored credentials. |
| `unavailable` | Usage could not be determined for any other reason. |

The `usage` object (present when `usageStatus` is `ok`) holds these keys, each
present only when its data exists in the source: `fiveHour` and `sevenDay`
(each `{pct, resetsAt?, countdown?, clock?}`), `spend`
(`{used, limit, pct, currency, resetsAt?, countdown?, clock?}`), and `scoped`
(an array of per-model weekly windows `{name, pct, resetsAt?, countdown?, clock?}`).
`pct` is a number; `resetsAt` is an ISO-8601 string; `countdown` and `clock`
are human strings recomputed at serialization time.

### Errors

| Message (exit 2) | Condition |
|------------------|-----------|
| `--token-status cannot be combined with --json` | Both flags given. |
| `--json can only be used with 'list', 'status', or 'switch'` | `--json` on a command that does not emit it (caught by the parser, not `list`). |

### Example

```
$ cswap list
Accounts:
  1: alice@example.com [personal] (active)
     usage unavailable (http-429)

  2: dev (bob@example.com) [personal]
     usage unavailable (http-429)

  3: key@example.com [personal]
     API key (no quota)

  5: carol@example.com [personal] (disabled)
     usage unavailable (http-429)
```

### See also

`cswap status`, `cswap switch`, [JSON OUTPUT CONTRACT](#json-output-contract).

---

## cswap status

### Synopsis

```
cswap status [--json] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--json` | flag | off | — |
| `--debug` | flag | off | — |

### Description

Reports the current default login (the account Claude Code will use) and the
number of managed accounts. When the live account is not one cswap manages, the
active record is reported as unmanaged.

### Files

Reads `sequence.json`, the live `~/.claude.json` (or a custom `CLAUDE_CONFIG_DIR`
equivalent; a cswap session pin is ignored — see ENVIRONMENT), and
`cache/usage.json`. Writes nothing.

### Exit status

`0` on success; `1` on a handled error.

### Output

Human output is a `Status:` line plus a managed-account count and a usage
summary. With `--json`:

```
{
  "schemaVersion": 1,
  "totalManagedAccounts": <int>,
  "active": <active-object>
}
```

The `<active-object>` always contains: `number` (int, or `null` for an
unmanaged live account), `email`, `organizationName`, `organizationUuid`,
`isOrganization`, `managed` (bool), `usageStatus` (same enum as `list`), and
`usage` (object or `null`). The same additive keys `list` emits per row —
`alias`, `atLimit`, `limitingWindows`, `usageFetchedAt`, `usageAgeSeconds` —
appear here under the same conditions.

### Errors

Handled errors (exit 1) surface as `Error: <message>` on stderr, or as the JSON
error envelope on stdout under `--json`.

### Example

```
$ cswap status
Status: Account-1 (alice@example.com [personal])
  Total managed accounts: 4
  usage unavailable (http-429)
```

```
$ cswap status --json
{
  "active": {
    "email": "alice@example.com",
    "isOrganization": false,
    "managed": true,
    "number": 1,
    "organizationName": "",
    "organizationUuid": "",
    "usage": null,
    "usageStatus": "unavailable"
  },
  "schemaVersion": 1,
  "totalManagedAccounts": 4
}
```

To extract the active account's email in a script:

```
$ cswap status --json | jq -r '.active.email'
alice@example.com
```

### See also

`cswap list`, `cswap switch`.

---

## cswap switch

### Synopsis

```
cswap switch [--json] [--debug]
cswap switch <NUM|EMAIL|ALIAS> [--json] [--force] [--debug]
cswap switch --strategy {best|next-available} [--model NAMES] [--json] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--json` | flag | off | — |
| `--force` | flag | off | Only with `switch <NUM\|EMAIL\|ALIAS>`; activates without backing up the current login first. |
| `--strategy` | choice `best` \| `next-available` | unset | Only with bare `switch` (no target). |
| `--model` | string (comma-separated names) | unset | Only with `switch --strategy`. |
| `--debug` | flag | off | — |

### Description

Bare `cswap switch` rotates to the next account in the sequence. `cswap switch
<NUM|EMAIL|ALIAS>` activates a specific account. `--strategy best` picks the
account with the most remaining 5h/7d quota; `--strategy next-available`
rotates, skipping rate-limited accounts. `--model` adds the named models'
per-model weekly limits to the quota computation for the usage-aware
strategies. A switch never requires restarting Claude Code to be correct; the
post-switch note is informational (see NOTES). The switch cooperates with
Claude Code's own credential lock, so it never interleaves with a token
refresh.

### Files

Reads and writes `sequence.json` (the active pointer), the live Claude Code
config/credentials, and per-account `configs/`/`credentials/`. Acquires the
backup-root and Claude Code credential locks.

### Exit status

`0` on success (including a no-op "already active"); `1` on a handled error;
`2` on a usage error (for example `--strategy` without bare `switch`).

### Output

Human output is `Switched to Account-<n> (<email>)` (or `Already on ...`), then
a refreshed account list and the post-switch note. With `--json`:

```
{
  "schemaVersion": 1,
  "switched": <bool>,
  "from": <ref|null>,
  "to": <ref>,
  "strategy": "<string>",
  "reason": "<string>",
  "message": "<string>",
  "warnings": [ <string>, ... ]
}
```

A `<ref>` is `{"number": <int|null>, "email": "<string>"}`. `switched` is
`true` when `from` and `to` differ. `strategy` is `direct` for a named target,
`rotation` for a bare rotate, or `best` / `next-available`. `reason` is one of
`switched`, `already-active`, `activated` (with `--force`), `unmanaged-account`,
`only-one-account`, `candidates-exhausted`, `no-valid-target`,
`usage-unavailable`, or `already-best`. A bare `switch --strategy` with
`--model` additionally emits `models` (array of string) and `modelSource`
(`cli` or `autoswitch.model`).

### Errors

| Message | Type / exit |
|---------|-------------|
| `No account found with identifier: <id>` | `AccountNotFoundError`, exit 1 |
| `argument --strategy: invalid choice: '<v>' (choose from 'best', 'next-available')` | usage, exit 2 |
| `--strategy can only be used with bare 'switch'` | usage, exit 2 |
| `--model can only be used with 'switch --strategy best' or 'switch --strategy next-available'` | usage, exit 2 |
| `--force can only be used with 'import' or 'switch <num\|email>'` | usage, exit 2 |

### Example

```
$ cswap switch 2 --json
{
  "from": {
    "email": "alice@example.com",
    "number": 1
  },
  "message": "Switched to Account-2 (bob@example.com)",
  "reason": "switched",
  "schemaVersion": 1,
  "strategy": "direct",
  "switched": true,
  "to": {
    "email": "bob@example.com",
    "number": 2
  },
  "warnings": []
}
```

### See also

`cswap auto`, `cswap list`, `cswap status`, [SETTINGS](#settings).

---

## cswap add

### Synopsis

```
cswap add [--slot NUM] [--alias NAME] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--slot` | int | next free slot | Only with `add` or `add-token`. |
| `--alias` | string | none | Only with `add`. Letters/digits/`.`/`-`/`_`; not all-numeric. |
| `--debug` | flag | off | — |

### Description

Snapshots the account currently logged into Claude Code as a new managed
account, copying its credentials and `oauthAccount` config into the backup
root. `--slot` places it in a specific slot (swapping if occupied); `--alias`
sets a short display name at the same time.

When the current login's identity `(email, organizationUuid)` already belongs to
a managed account and no `--slot` is given, `add` refreshes that account's
stored credentials and config in place rather than allocating a second slot, and
reports `Updated credentials for Account <n> (<email> [<tag>]).`. This is the
supported way to repair an account whose refresh token has died (`usageStatus:
relogin_required`): log in with the account in Claude Code, then re-run
`cswap add`. The refresh also clears any `cswap auto` quarantine on that slot.

### Files

Reads the live `~/.claude.json` and credentials store. Writes `sequence.json`,
`configs/`, and `credentials/` (or the macOS Keychain).

### Exit status

`0` on success; `1` on a handled error (for example no current login, or an
invalid alias).

### Output

`Added Account <n>: <email> [<tag>]` on a new account, or `Updated credentials
for Account <n> (<email> [<tag>]).` when refreshing an already-managed identity
in place.

### Errors

An invalid alias raises `ConfigError` (exit 1). A missing or unreadable Claude
config raises `ConfigError` (`Claude config file not found`,
`Permission denied reading Claude config`, or a wrapped read error).

### Example (illustrative — requires a current Claude Code login to snapshot)

```
$ cswap add --slot 3 --alias work
Added Account 3: me@example.com [personal]
```

### See also

`cswap add-token`, `cswap alias`, `cswap remove`.

---

## cswap add-token

### Synopsis

```
cswap add-token [TOKEN|-] [--email EMAIL] [--slot NUM] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `TOKEN` | positional string | read interactively if omitted | A lone `-` reads the token from stdin. |
| `--email` | string | derived if possible | Only with `add-token`. |
| `--slot` | int | next free slot | Only with `add` or `add-token`. |
| `--debug` | flag | off | — |

### Description

Registers an account from a setup token (`sk-ant-oat01-...`) or an API key
(`sk-ant-api03-...`) without an interactive Claude Code login. API-key accounts
are marked `kind: api_key` and carry no OAuth quota.

### Files

Writes `sequence.json`, `configs/`, and `credentials/` (or the macOS Keychain).

### Exit status

`0` on success; `1` on a handled error.

### Output

`Added Account <n>: <email> [<tag>] (from token)`.

### Errors

Handled errors surface as `Error: <message>` (exit 1).

### Example (illustrative token)

```
$ cswap add-token sk-ant-oat01-EXAMPLE --email me@example.com
Added Account 6: me@example.com [personal] (from token)
```

### See also

`cswap add`, `cswap remove`.

---

## cswap remove

### Synopsis

```
cswap remove <NUM|EMAIL|ALIAS> [--debug]
```

Legacy: `cswap --remove-account <id>`. Alias: `cswap rm <id>`.

### Options

Only `--debug`. The account identifier is required.

### Description

Permanently removes a managed account: its `sequence.json` entry, stored
config, and stored credentials. Prompts for confirmation on an interactive
terminal (`[y/N]`); any answer other than `y` cancels.

### Files

Removes the account's `configs/` and `credentials/` entries and its
`sequence.json` record.

### Exit status

`0` on success or a cancelled prompt; `1` on a handled error.

### Output

`Removed Account-<n> (<email>).` on success; `Cancelled` when declined.

### Errors

`No account found with identifier: <id>` (`AccountNotFoundError`, exit 1).

### Example

```
$ cswap remove 5
Are you sure you want to permanently remove Account-5 (carol@example.com)? [y/N] Cancelled
```

### See also

`cswap disable`, `cswap add`.

---

## cswap disable

### Synopsis

```
cswap disable <NUM|EMAIL|ALIAS> [--debug]
```

Legacy: `cswap --disable-account <id>`.

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `NUM\|EMAIL\|ALIAS` | positional | — | Required. The account to disable. |
| `--debug` | flag | off | — |

### Description

Holds an account out of auto-rotation. `cswap auto` and the usage-aware `switch`
strategies skip a disabled account; a direct `switch <id>` still activates it.
The account and its credentials are retained.

### Files

Writes the `disabled` flag on the account's `sequence.json` record.

### Exit status

`0` on success; `1` on a handled error.

### Output

`Disabled Account-<n> (<email>).` When the disabled account is also the active
login, a second line follows: `  It is the active account — it stays live until
you switch away; it just won't be an automatic switch target.`

### Errors

`No account found with identifier: <id>` (`AccountNotFoundError`, exit 1).

### Example

```
$ cswap disable 1
Disabled Account-1 (alice@example.com).
  It is the active account — it stays live until you switch away; it just won't be an automatic switch target.
```

### See also

`cswap enable`, `cswap auto`.

---

## cswap enable

### Synopsis

```
cswap enable <NUM|EMAIL|ALIAS> [--debug]
```

Legacy: `cswap --enable-account <id>`.

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `NUM\|EMAIL\|ALIAS` | positional | — | Required. The account to enable. |
| `--debug` | flag | off | — |

### Description

Returns a disabled account to auto-rotation, clearing its `disabled` flag.

### Files

Clears the `disabled` flag on the account's `sequence.json` record.

### Exit status

`0` on success; `1` on a handled error.

### Output

For an account that was disabled:

```
Enabled Account-<n> (<email>).
  It is back in the rotation.
```

For an account that is already enabled:
`Account-<n> (<email>) is already enabled.`

### Errors

`No account found with identifier: <id>` (`AccountNotFoundError`, exit 1).

### Example

```
$ cswap enable 5
Enabled Account-5 (carol@example.com).
  It is back in the rotation.
```

### See also

`cswap disable`, `cswap auto`.

---

## cswap alias

### Synopsis

```
cswap alias                                 list all aliases
cswap alias <NUM|EMAIL|ALIAS> <NAME>        set an alias
cswap alias <NUM|EMAIL|ALIAS> --unset       remove an alias
cswap alias [...] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--unset` | flag | off | Requires an account; takes no `NAME`. |
| `--debug` | flag | off | — |

### Description

With no arguments, lists every account that has an alias. With an account and a
name, sets the alias. With an account and `--unset`, removes it. A valid alias
is letters, digits, `.`, `-`, or `_`, and is not purely numeric (which would be
ambiguous with a slot number).

### Files

Writes the `alias` field on the account's `sequence.json` record.

### Exit status

`0` on success; `1` on a handled error (for example an invalid alias name);
`2` on a usage error.

### Output

- List: `Aliases:` followed by `  <n>: <alias> (<email>)` lines, or
  `No aliases set`.
- Set: `Set alias '<alias>' for Account <n>`.
- Unset: `Removed alias for Account <n>`.

### Errors

| Message | Exit |
|---------|------|
| `--unset does not take a NAME argument` | 2 |
| `NUM\|EMAIL is required with --unset` | 2 |
| `NAME is required (or pass --unset to remove the alias)` | 2 |
| `unrecognized arguments: <tok>` | 2 |
| Invalid alias name (`ConfigError`) | 1 |
| Corrupt or unreadable `sequence.json` (`ConfigError`, names the file and the repair/restart options) | 1 |

A corrupt roster refuses rather than reporting `No aliases set` (list) or
silently writing over it (set/unset): the roster's records may still be
hand-repairable, and every account's stored credential and config backup is
intact even though the roster naming it is not.

### Example

```
$ cswap alias
Aliases:
  2: dev (bob@example.com)
```

### See also

`cswap add`, [Account resolution](#account-resolution-num--email--alias).

---

## cswap move

### Synopsis

```
cswap move <NUM|EMAIL|ALIAS> <SLOT> [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `NUM\|EMAIL\|ALIAS` | positional | — | Required. The account to move. |
| `SLOT` | positional int | — | Required. The destination slot; a positive integer. |
| `--debug` | flag | off | — |

### Description

Assigns an account to a specific slot number. If the target slot is occupied by
another account, the two accounts exchange slots. If the account is already in
the target slot, nothing changes.

### Files

Rewrites the affected `sequence.json` records and renames the per-account
`configs/`, `credentials/`, and `sessions/` entries to match the new slot
numbers.

### Exit status

`0` on success; `1` on a handled error; `2` on a usage error (missing
argument).

### Output

- Moved onto a free slot: `Moved <email> to slot <n>`.
- Swapped with an occupant: `Swapped Account <a> and Account <b>:` then a
  numbered two-line list.
- Already there: `Already in slot <n>: <email>`.

### Errors

| Message | Exit |
|---------|------|
| `the following arguments are required: NUM\|EMAIL\|ALIAS, SLOT` (or `SLOT`) | 2 |
| `unrecognized arguments: <tok>` | 2 |
| `No account found with identifier: <id>` | 1 |
| `Target slot must be a positive slot number, got: '<v>' (use \`swap\` to trade two accounts by identifier)` | 1 |

### Example

```
$ cswap move 1 3
Swapped Account 1 and Account 3:
  1: key@example.com
  3: alice@example.com
```

### See also

`cswap swap`.

---

## cswap swap

### Synopsis

```
cswap swap <A> <B> [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `A` | positional `NUM\|EMAIL\|ALIAS` | — | Required. The first account. |
| `B` | positional `NUM\|EMAIL\|ALIAS` | — | Required. The second account. |
| `--debug` | flag | off | — |

### Description

Exchanges the slot numbers of two accounts. Both accounts must exist.

### Files

Rewrites the two `sequence.json` records and renames the corresponding
`configs/`, `credentials/`, and `sessions/` entries.

### Exit status

`0` on success; `1` on a handled error; `2` on a usage error.

### Output

`Swapped Account <a> and Account <b>:` followed by two `  <n>: <email>` lines
in numeric order.

### Errors

| Message | Exit |
|---------|------|
| `the following arguments are required: NUM\|EMAIL\|ALIAS` | 2 |
| `unrecognized arguments: <tok>` | 2 |
| `No account found with identifier: <id>` | 1 |

### Example

```
$ cswap swap 1 2
Swapped Account 1 and Account 2:
  1: bob@example.com
  2: alice@example.com
```

### See also

`cswap move`.

---

## cswap run

### Synopsis

```
cswap run [<NUM|EMAIL|ALIAS>] [--no-share] [--share-history|--no-share-history] [--debug] [-- ARGS...]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--no-share` | flag | off (session shares the default profile's MCP/config) | — |
| `--share-history` | flag | off | Mutually last-wins with `--no-share-history`. |
| `--no-share-history` | flag | on (history not shared) | — |
| `--debug` | flag | off | — |
| `-- ARGS...` | passthrough | — | Everything after the first `--` is forwarded to `claude` verbatim. |

### Description

Launches Claude Code as a stored account in the current terminal only, without
changing the machine-wide default login. It prepares the account's persistent
session profile (bootstrap, token validate, MCP mirror, credential scrub) and
then execs `claude` with `CLAUDE_CONFIG_DIR` pointed at that profile. With no
account argument, the account is resolved from the current directory's mapping
(see `cswap map`); if the directory has no usable mapping, `claude` launches as
the default login. This is an experimental feature.

### Files

Reads `sequence.json`, `mappings.json`, per-account `configs/`/`credentials/`.
Creates and maintains the account's `sessions/<n>-<email>` profile directory.

### Exit status

The exit status of the launched `claude` process. `1` on a pre-exec failure;
`2` on a usage error. On POSIX the process is replaced by `claude`, so cswap
itself does not return on success.

### Output

Preparation notices (`Launching Account-<n> (<email>) [session mode]`, warnings)
are printed before the handoff. A directory with no mapping prints
`No account mapped for <dir> — launching the default account.`; a mapping to a
removed account prints a warning and launches the default.

### Errors

| Message | Exit |
|---------|------|
| `unrecognized arguments: <tok>` | 2 |
| A `SessionError` / bootstrap failure (`Error: <message>`) | 1 |
| Corrupt or unreadable `sequence.json` (`ConfigError`, names the file and the repair/restart options) | 1 |

An API-key account cannot be run in session mode and is rejected.

Account resolution — an explicit account, or the current directory's
mapping — refuses on a corrupt roster; with no account argument this means
`run` refuses rather than falling back to the default login.

### Example (illustrative — requires a real Claude Code install)

```
$ cswap run 2 -- --resume
Launching Account-2 (bob@example.com) [session mode]
# ... claude starts as account 2 with --resume ...
```

### See also

`cswap env`, `cswap map`.

---

## cswap env

### Synopsis

```
cswap env [<NUM|EMAIL|ALIAS>] [--no-share] [--share-history] [--shell {sh|fish|pwsh}] [--unset] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--shell` | choice `sh` \| `fish` \| `pwsh` | `sh` | Selects the output syntax. |
| `--no-share` | flag | off | — |
| `--share-history` | flag | off | — |
| `--unset` | flag | off | Takes no account argument; skips bootstrap. |
| `--debug` | flag | off | — |

### Description

Prints shell-evalable lines that pin the current shell to a stored account's
session profile without launching `claude`. It prepares the same profile
`cswap run` does (bootstrap / validate / mirror / `--no-share` /
`--share-history` all apply), then prints a `CLAUDE_CONFIG_DIR` export instead
of exec'ing. Intended for `eval "$(cswap env 2)"`. `--unset` prints only the
`CLAUDE_CONFIG_DIR` unset line for the chosen shell, dropping the pin. With no
account, the account is resolved from the current directory's mapping; unlike
`run`, `env` has no default-login fallback and errors when nothing is mapped.

When the chosen account is already the active default login and
`CLAUDE_CONFIG_DIR` is not already set, `cswap env` does nothing: it prepares no
profile, creates no second credential copy, and exports nothing. It writes one
note to stderr and exits 0, so `eval "$(cswap env <n>)"` is a safe no-op. When
`CLAUDE_CONFIG_DIR` is already set, the preset-override behavior applies instead:
the profile is prepared and exported for this shell, overriding the preset,
identical to `cswap run`.

Output discipline: stdout carries only the eval-able lines. Before the export,
one unset line is emitted for each currently-set authentication-override
variable (see ENVIRONMENT) so it cannot shadow the pinned account. All notices
and warnings go to stderr.

### Files

Same as `cswap run`: reads the registry and mappings, prepares the account's
`sessions/<n>-<email>` profile.

### Exit status

`0` on success; `1` on a handled error (including "nothing to prepare"); `2` on
a usage error.

### Output

The eval stream, per shell:

| Shell | Export | Unset |
|-------|--------|-------|
| `sh` | `export CLAUDE_CONFIG_DIR='<dir>'` | `unset CLAUDE_CONFIG_DIR` |
| `fish` | `set -gx CLAUDE_CONFIG_DIR '<dir>'` | `set -e CLAUDE_CONFIG_DIR` |
| `pwsh` | `$env:CLAUDE_CONFIG_DIR = '<dir>'` | `Remove-Item Env:CLAUDE_CONFIG_DIR -ErrorAction SilentlyContinue` |

When the chosen account is already the active default login and no
`CLAUDE_CONFIG_DIR` preset is set, stdout is empty; only the stderr note is
written (see Description).

### Errors

| Message | Exit |
|---------|------|
| `argument --shell: invalid choice: '<v>' (choose from sh, fish, pwsh)` | 2 |
| `--unset does not take a NUM\|EMAIL\|ALIAS argument` | 2 |
| `argument --shell: expected one argument` | 2 |
| `unrecognized arguments: <tok>` | 2 |
| `Nothing to prepare an environment for (...). Pass an account ..., map this directory ..., or clear a pinned profile with cswap env --unset.` | 1 |
| Corrupt or unreadable `sequence.json` (`ConfigError`, names the file and the repair/restart options) | 1 |

Account resolution (an explicit `NUM\|EMAIL\|ALIAS`, or the current
directory's mapping) refuses on a corrupt roster rather than reporting that
no such account exists.

### Example

```
$ cswap env 2
Prepared Account-2 (bob@example.com) [session mode]      # (stderr)
export CLAUDE_CONFIG_DIR='~/.local/share/claude-swap/sessions/2-bob_example.com'
$ cswap env 1                                            # already the active default login
Account-1 (alice@example.com) is the active default login — an unpinned shell already uses it; nothing exported.      # (stderr; exit 0, nothing exported)
$ cswap env --unset
unset CLAUDE_CONFIG_DIR
$ cswap env --unset --shell fish
set -e CLAUDE_CONFIG_DIR
```

`cswap env` has no counterpart in claude-swap (Python); it is defined by this
implementation.

### See also

`cswap run`, `cswap map`, ENVIRONMENT.

---

## cswap map

### Synopsis

```
cswap map                                   list directory mappings
cswap map <NUM|EMAIL|ALIAS> [PATH]          map a directory to an account
cswap map [...] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `NUM\|EMAIL\|ALIAS` | positional | — | Optional. Omit (with no `PATH`) to list mappings; supply it to map a directory. |
| `PATH` | positional string | the current directory | The directory to map. A non-existent directory is mapped with a warning. |
| `--debug` | flag | off | — |

### Description

With no arguments, lists directory→account mappings. With an account and an
optional path (defaulting to the current directory), records that a bare
`cswap run` / `cswap env` in that directory resolves to the given account. A
path that is not an existing directory is mapped anyway with a warning.

### Files

Reads and writes `mappings.json` in the backup root.

### Exit status

`0` on success; `1` on a handled error; `2` on a usage error.

### Output

- List: `Directory mappings:` and one `  <path> → <n>: <email> [<tag>]` line
  per mapping (or `... (account removed)` for a dangling mapping); or the empty
  notice `No directory mappings yet.` plus a hint.
- Set: `Mapped <path> → Account-<n> (<email>)`, with `(was <prev-email>)`
  appended when the path was previously mapped to a different account.

### Errors

| Message | Exit |
|---------|------|
| `unrecognized arguments: <tok>` | 2 |
| `No account found with identifier: <id>` | 1 |
| Corrupt or unreadable `sequence.json`, given an account argument (`ConfigError`, names the file and the repair/restart options) | 1 |

A non-directory path prints
`Warning: <path> is not an existing directory (mapping it anyway)` on stdout
and proceeds.

The corrupt-roster refusal applies only when an account is given: account
resolution refuses rather than reporting "no such account." The bare,
no-argument listing does not detect a corrupt roster at all and exits 0 —
every mapping prints as `... (account removed)`, whether or not the account
it names still exists, because the listing cannot tell corruption from
absence.

### Example

```
$ cswap map
Directory mappings:
  ~/work/client-app → 2: bob@example.com [personal]
```

### See also

`cswap unmap`, `cswap run`, `cswap env`.

---

## cswap unmap

### Synopsis

```
cswap unmap [PATH] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `PATH` | positional string | the current directory | The directory whose mapping is removed. |
| `--debug` | flag | off | — |

### Description

Removes the mapping for a directory (defaulting to the current directory).

### Files

Reads and writes `mappings.json`.

### Exit status

`0` on success (including "no mapping present"); `2` on a usage error.

### Output

`Unmapped <path>` when a mapping was removed, else `No mapping for <path>`.

### Errors

`unrecognized arguments: <tok>` (exit 2).

### Example

```
$ cswap unmap ~/work/client-app
Unmapped ~/work/client-app
```

### See also

`cswap map`.

---

## cswap auto

### Synopsis

```
cswap auto [--once] [--json] [--interval SECONDS] [--threshold PCT] [--cooldown SECONDS]
           [--model NAMES] [--include-api-key-accounts|--no-include-api-key-accounts]
           [--dry-run] [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--once` | flag | off (loop) | Runs a single tick; the exit status is the tick outcome. |
| `--json` | flag | off | Emit JSONL events (compact) instead of human lines. |
| `--interval` | float seconds | `autoswitch.intervalSeconds` (60) | Clamped to 15–3600. |
| `--threshold` | float percent | `autoswitch.threshold` (90) | Clamped to 50–99.9. |
| `--cooldown` | float seconds | `autoswitch.cooldownSeconds` (300) | Clamped to 0–86400. |
| `--model` | string (comma names) | `autoswitch.model` | Adds per-model weekly limits. |
| `--include-api-key-accounts` | flag | `autoswitch.includeApiKeyAccounts` (false) | Allow rotating onto API-key accounts. |
| `--no-include-api-key-accounts` | flag | — | The negated form. |
| `--dry-run` | flag | off | Evaluate and report but never switch. |
| `--debug` | flag | off | — |

Command-line values override the corresponding `settings.json` values for this
run only, then are re-clamped to the ranges above.

### Description

Watches the active account's usage and proactively switches before it reaches
the threshold. In loop mode it polls every interval; in `--once` mode it
evaluates a single tick and exits with the outcome as the status code — the
form intended for cron. Switching proactively (while the old account is still
valid) is what keeps the change safe under the macOS Keychain propagation
latency.

Which accounts qualify as targets is governed by the threshold, hysteresis,
cooldown, and quarantine rules; the order in which qualifying targets are
tried is governed by `autoswitch.strategy` — most headroom first (`best`,
the default) or earliest weekly renewal first (`soonest-reset`). See
[SETTINGS](#settings) for the ordering rules.

### Files

Reads `sequence.json`, `settings.json`, `cache/usage.json`. Reads and writes
`autoswitch_state.json` (cooldown / quarantine state) under
`.autoswitch_state.lock`. A real switch writes the same files `cswap switch`
does.

### Exit status

Loop mode returns `0` on a clean stop. `--once` returns the tick outcome:

| Code | Outcome |
|------|---------|
| `0` | Switched (or, in `--dry-run`, would have switched). |
| `1` | Error (network trouble, lock contention, transient freshen failure). |
| `2` | No action needed (below threshold, cooldown, idle). |
| `3` | Blocked: a switch was wanted but no viable target exists / all exhausted. |

A construction or configuration error before the first tick exits `1`.

### Output

Human mode prints one timestamped line per event, colored by kind. `--json`
prints one compact JSON object per event on its own line (JSONL). Every event
object carries `schemaVersion` (1), `event` (the kind), `ts` (RFC3339 UTC with
a `Z` suffix), plus per-kind fields:

| `event` | Fields |
|---------|--------|
| `poll` | `active` (`{number,email}` or null), `headroomPct` (`{num: float|null}`), `threshold` (float), `fetchErrors` (`{num: msg}`, optional), `windowsPct` (`{num: {label: float}}`, optional) |
| `switch` | `trigger` (`proactive`\|`at-limit`\|`failover`), `from` (ref), `to` (ref), `warnings` (array), `dryRun` (bool) |
| `no-switch` | `reason` (string), `detail` (string) |
| `account-quarantined` | `number`, `email`, `reason` |
| `account-unquarantined` | `number`, `email`, `reason` |
| `all-exhausted` | `earliestResetAt` (string or null) |
| `sleep` | `seconds` (float), `until` (string) |
| `error` | `message` (string), `transient` (bool) |
| `config-warning` | `message` (string) |

A handled error in `--json` mode is emitted as the compact error envelope
(see JSON OUTPUT CONTRACT) on stdout.

### Errors

| Message | Exit |
|---------|------|
| `argument --interval: invalid float value: '<v>'` (and `--threshold`, `--cooldown`) | 2 |
| `argument --interval: expected one argument` (and the others) | 2 |
| `unrecognized arguments: <tok>` | 2 |
| A `ClaudeSwitchError` before the loop | 1 |

### Example

```
$ cswap auto --once --json
{"active":{"email":"alice@example.com","number":1},"event":"poll","fetchErrors":{"1":"http-429","2":"http-429","5":"http-401"},"headroomPct":{"1":null,"2":null,"3":null,"5":null},"schemaVersion":1,"threshold":80.0,"ts":"2026-07-17T20:52:55Z"}
{"detail":"1/3 before failover","event":"no-switch","reason":"active-usage-unknown","schemaVersion":1,"ts":"2026-07-17T20:52:55Z"}
$ echo $?
2
```

A cron entry that checks every five minutes and appends output to a log:

```
*/5 * * * * cswap auto --once --json >> cswap-auto.log 2>&1
```

cron runs with a minimal `PATH`; the entry finds `cswap` only when its install
directory is on cron's `PATH`. Either set a `PATH=` line at the top of the
crontab that includes that directory, or place `cswap` in a directory cron
already searches.

### See also

`cswap switch`, [SETTINGS](#settings), [SIGNALS](#signals).

---

## cswap config

### Synopsis

```
cswap config [list] [--json] [--debug]
cswap config get <KEY> [--json] [--debug]
cswap config set <KEY> <VALUE> [--debug]
cswap config unset <KEY> [--debug]
cswap config path [--debug]
```

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `--json` | flag | off | Only with `list` or `get`. |
| `--debug` | flag | off | — |

`KEY` is a dotted settings key (see SETTINGS). The default action with no
positional is `list`.

### Description

Reads and edits `settings.json` in the backup root. `list` shows every key with
its effective value and whether it is explicitly set; `get` reads one key; `set`
validates and stores one key (writing only that key, never freezing the other
defaults into the file); `unset` removes one key; `path` prints the settings
file path. Values are validated strictly on `set`: an out-of-range or mistyped
value is rejected rather than silently clamped.

### Files

Reads and writes `settings.json`.

### Exit status

`0` on success; `1` on a handled error (`ConfigError` — unknown key, out of
range, wrong type, corrupt file); `2` on a usage error.

### Output

- `list` (human): aligned `key  value` rows; unset keys are tagged `(default)`.
- `list --json`:
  ```
  {"schemaVersion":1,"path":"<settings.json>","settings":[{"key":"<k>","value":<v>,"isSet":<bool>},...]}
  ```
- `get` (human): the bare value; `get --json`:
  `{"schemaVersion":1,"key":"<k>","value":<v>,"isSet":<bool>}`.
- `set`: `<key> = <value>`.
- `unset`: `<key> unset (default: <default>)`, or the stderr notice
  `<key> is not set; nothing to do` (exit 0) when it was not set.
- `path`: the absolute settings file path.

### Errors

| Message | Exit |
|---------|------|
| `argument {list,get,set,unset,path}: invalid choice: '<a>' (...)` | 2 |
| `the following arguments are required: KEY` (or `KEY, VALUE`, `VALUE`) | 2 |
| `unrecognized arguments: <tok>` | 2 |
| `--json can only be used with list or get` | 2 |
| `unknown setting '<key>'\nValid keys: ...` | 1 |
| `<key> must be between <lo> and <hi>` | 1 |
| `<key> expects an integer, got '<v>'` / `expects a number, got '<v>'` | 1 |
| `<key> expects true or false (or 1/0, yes/no), got '<v>'` | 1 |
| `<key> must be one of: <choices>` | 1 |
| `<key> expects a non-empty value; use 'cswap config unset <key>' to clear it` | 1 |

### Example

```
$ cswap config set autoswitch.threshold 80
autoswitch.threshold = 80
$ cswap config get autoswitch.threshold
80
$ cswap config set autoswitch.threshold 40
Error: autoswitch.threshold must be between 50 and 99.9
$ echo $?
1
```

### See also

[SETTINGS](#settings), `cswap auto`.

---

## cswap export

### Synopsis

```
cswap export <PATH> [--account NUM|EMAIL] [--full] [--debug]
```

Legacy: `cswap --export <path>`.

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `PATH` | positional string | — | `-` writes to stdout. |
| `--account` | string `NUM\|EMAIL` | all accounts | Only with `export`. Limits the export to one account. |
| `--full` | flag | off | Only with `export`. Embeds the entire `~/.claude.json` per account instead of just `oauthAccount`. |
| `--debug` | flag | off | — |

### Description

Serializes managed accounts (with their credentials) to a `.cswap` JSON file
for transfer to another machine. By default each account carries only its
`oauthAccount` config; `--full` embeds the whole per-account config. The active
account is read from the live vault for the freshest tokens. In a bulk export, a
single broken account is skipped with a stderr warning; a named single-account
export treats the same condition as a hard failure. A missing `oauthAccount` is
always fatal.

### Files

Reads `sequence.json`, `configs/`, `credentials/` (or Keychain). Writes the
`.cswap` file (or stdout).

### Exit status

`0` on success; `1` on a handled error.

### Output

`Exported <n> account(s) to <path>`. The file is a JSON envelope:

```
{
  "version": 1,
  "exportedAt": "<ISO-8601 UTC>",
  "exportedFrom": "<platform>",
  "swapVersion": "<version>",
  "encrypted": false,
  "activeAccountNumber": <int|null>,
  "accounts": [ { "number", "email", "uuid", "organizationUuid",
                  "organizationName", "added", "credentials", "config",
                  "kind"?, "alias"? }, ... ]
}
```

`encrypted` is always `false`. `credentials` is an OAuth object or a raw
API-key string; `config` is `{"oauthAccount": ...}` (default) or the full
config (`--full`). `kind` and `alias` are omitted when empty.

### Errors

Handled errors surface as `Error: <message>` (`TransferError` or a credential
error), exit 1. A corrupt or unreadable `sequence.json` surfaces a
`ConfigError` naming the file and the repair/restart options instead of the
`TransferError` an absent or empty roster reports (`no accounts to
export — run cswap --add-account first`).

### Example

```
$ cswap export backup.cswap
Exported 4 account(s) to backup.cswap
```

### See also

`cswap import`, [COMPATIBILITY](#compatibility).

---

## cswap import

### Synopsis

```
cswap import <PATH> [--force] [--debug]
```

Legacy: `cswap --import <path>`.

### Options

| Option | Type | Default | Constraints |
|--------|------|---------|-------------|
| `PATH` | positional string | — | `-` reads from stdin. |
| `--force` | flag | off | Only with `import` (or `switch <id>`). Overwrites an existing matching local account in place. |
| `--debug` | flag | off | — |

### Description

Reads a `.cswap` envelope and writes its accounts into the local store. It
validates every account first (path-traversal defence, credential shape,
duplicate-identity and alias-collision checks) with zero writes, so a malformed
account late in the file never half-imports an earlier one. Each account is then
matched on its `(email, organizationUuid)` identity: an existing match is
skipped, or overwritten with `--force`; a new identity gets a freshly allocated
slot. Only a destination with no prior preference inherits the file's
`activeAccountNumber`. Encrypted exports are rejected.

### Files

Reads the `.cswap` file (or stdin). Writes `sequence.json`, `configs/`,
`credentials/` under one lock spanning the write pass.

### Exit status

`0` on success (including an all-skipped import); `1` on a handled error.

### Output

Per-account lines (`Imported ...`, `Overwrote ...`, `Skipped <email> (already
exists, use --force)`) then a summary
`Done: <i> imported, <o> overwritten, <s> skipped`.

### Errors

| Message | Type |
|---------|------|
| `export file is not valid JSON: <detail>` | `TransferError` |
| `export file must be a JSON object` | `TransferError` |
| `unsupported export version: <v> (expected 1)` | `TransferError` |
| `encrypted exports are not supported in this version — decrypt before piping (e.g. gpg -d backup.gpg \| cswap --import -)` | `TransferError` |
| `export file has no accounts to import` | `TransferError` |

All are exit 1.

### Example

```
$ cswap import backup-acct2.cswap
Skipped bob@example.com (already exists, use --force)
Done: 0 imported, 0 overwritten, 1 skipped
```

### See also

`cswap export`, [COMPATIBILITY](#compatibility).

---

## cswap tui

### Synopsis

```
cswap tui [--debug]
```

Legacy: `cswap --tui`. A bare `cswap` in an interactive terminal opens the TUI.

### Description

Opens the full-screen interactive dashboard (accounts, usage, live switching,
an auto-switch screen). Requires an interactive terminal on both stdin and
stdout; a bare `cswap` invoked from a pipe or non-interactive context prints the
`no command given` usage error instead of opening the dashboard.

A keybinding bar at the bottom of each screen lists the keys available there:
on the dashboard, `s` switch accounts, `w` watch, `q` quit; on the switch
screen, `enter` switch, `b` best pick, `esc` back; on the watch screen, `s`
switch (`enter` confirm while a target is selected), `esc` back; on the
auto-switch screen, `l` go live / dry-run, `t` threshold (with `←`/`→` to adjust
and `enter` to finish while adjusting), `esc` back.

Disabled accounts appear in the switch and watch lists and remain valid
explicit targets there, since disabling holds an account out of automatic
rotation rather than out of the roster. The auto-switch screen's ranked
"Next best" candidates panel excludes disabled accounts entirely — an
account held out of automatic rotation never appears in a ranking of
automatic-switch targets. A quarantined account (one the auto engine has
sidelined for a dead or identity-mismatched stored credential; see
`cswap auto` below) is not excluded the same way: the panel keeps its row
but labels it "quarantined (<reason>)" in place of a usage figure, rather
than ranking it as a target, since quarantine is transient and clears
itself once the account's stored credential is replaced. Recovery is
logging in with the account and running `cswap add`.

A candidate row with a readable usage figure lists every rate-limit window
the account reports, not only the one it is ranked by: `5h`, `7d`, and one
entry per per-model weekly window the account carries, each rendered as
`<label> <pct>% (resets <duration>)` and joined on the one line, in that
fixed order (`5h`, then `7d`, then the account's own per-model windows).
The countdown is recomputed from the window's reset timestamp at render
time, never served from the countdown string the measurement was fetched
with: a stored string is correct only at fetch time and drifts as the
measurement ages. A window whose reset has already elapsed reads `(resets
now)`; a window whose reset timestamp is absent or unparseable carries no
parenthetical at all. Which windows count
toward the row's rank — and toward the auto engine's own pick — is a
narrower set: `5h` and `7d` always, plus a per-model window named by
`autoswitch.model` (see SETTINGS below). Every other per-model window still
renders — so a window can be watched before it is configured to matter —
but never moves the row's rank. The highest percentage among the counted
windows is the row's binding window: the same figure the row is ranked by
and the one the engine acts on. Its label and percentage render bold, in
the usual severity color; the row's other counted windows render in the
ordinary readable style; its uncounted windows render muted, so the three
kinds read apart at a glance. A countdown takes its cell's kind but is
never bold, including inside the binding cell: the percentage is the
figure the row is ranked by, and the countdown supports it. The panel header states the counted axis once — `Next best ·
counting 5h, 7d`, or, with `autoswitch.model` set, `Next best · counting
5h, 7d, Fable` (`Next best · counting 5h, 7d, all models` for the `all`
sentinel) — so an uncounted window's muting reads as a stated rule rather
than an unexplained dimming.

No line in the panel ever wraps, regardless of terminal width — the header,
a readable usage row, a quarantined/sentinel/usage-unknown row, and the
empty-state "no other switchable accounts" line alike. A readable usage row
that does not fit narrows one reduction at a time, in this order: the
countdowns of its uncounted windows, then those windows entirely, then the
countdowns of its counted windows other than the binding one, then those
windows entirely, then the binding window's own countdown, and last the
email, which truncates with an ellipsis. A countdown always goes before the
window carrying it, and a whole class goes before the next more informative
one; within a class the rightmost cell gives ground first. The binding
window's label and percentage — the row's ranking key — are on no rung and
survive every reduction. A quarantined, sentinel, or usage-unknown row carries no window
cells to drop, so it narrows differently: the email truncates first, down
to a bare ellipsis, and only once the email is gone does the label itself
lose its tail to an ellipsis; the slot number always survives, and the
label's own wording is truncated, never reworded. The header and the
empty-state line carry neither windows nor a label and, when the terminal
is narrower than the text itself, clip as a whole line with a trailing
ellipsis. Any line that still does not fit after its own narrowing
truncates as a whole line rather than wrap.

### Files

Reads and writes the same files the underlying commands do while the dashboard
is open.

### Exit status

`0` on a clean quit; `1` when a terminal is unavailable.

### Output

Full-screen terminal UI (no JSON). `--json` does not apply.

### Errors

| Message | Condition | Exit |
|---------|-----------|------|
| (terminal error, no dashboard opened) | stdin or stdout is not an interactive terminal (`cswap tui` from a pipe or redirect). | 1 |
| `no command given — try 'cswap help'` | A bare `cswap` (which would open the TUI) in a non-interactive context. | 2 |

### Example (illustrative — requires an interactive terminal)

```
$ cswap
# ... full-screen dashboard ...
```

### See also

`cswap watch`, `cswap list`.

---

## cswap watch

### Synopsis

```
cswap watch [--debug]
```

Legacy: `cswap --watch`.

### Description

Opens the same interactive dashboard as `cswap tui`, starting on the live watch
page.

### Files

As for `cswap tui`.

### Exit status

As for `cswap tui`: `0` on a clean quit; `1` when a terminal is unavailable.

### Output

As for `cswap tui`: a full-screen terminal UI (no JSON).

### Errors

As for `cswap tui`: a terminal error (exit 1) when stdin or stdout is not an
interactive terminal.

### Example (illustrative — requires an interactive terminal)

```
$ cswap watch
# ... dashboard on the watch page ...
```

### See also

`cswap tui`.

---

## cswap menubar

### Synopsis

```
cswap menubar
```

Legacy: `cswap --menubar`.

### Description

The macOS menu bar app. It is not available in this build. On macOS the command
reports that menu bar mode is not available; on every other platform it reports
that the menu bar is macOS-only. Either way it exits 1.

### Files

None. `cswap menubar` touches no files.

### Exit status

Always `1`.

### Output

- macOS: `Menu bar mode is not available in this build.` (stderr)
- Other platforms: `The menu bar is only available on macOS.` (stderr)

### Errors

| Message | Condition | Exit |
|---------|-----------|------|
| `Menu bar mode is not available in this build.` | Invoked on macOS. | 1 |
| `The menu bar is only available on macOS.` | Invoked on any non-macOS platform. | 1 |

### Example

```
$ cswap menubar
The menu bar is only available on macOS.
$ echo $?
1
```

### See also

`cswap tui`.

---

## cswap upgrade

### Synopsis

```
cswap upgrade
```

Legacy: `cswap --upgrade`. Alias: `cswap update`.

### Description

Self-upgrades to the latest release. It runs before the switcher is
constructed, so upgrading never touches config or credentials. When the running
binary lives in a Go-managed bin directory (`$GOBIN`, `$GOPATH/bin`, or
`$HOME/go/bin`), it re-runs `go install
git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest`. Otherwise it prints manual
upgrade guidance. On Windows the running executable is locked, so the command
always prints the upgrade command rather than running it.

### Files

None of cswap's data files. On a `go install` upgrade, the Go toolchain
replaces the binary.

### Exit status

The exit status of the `go install` subprocess on a `go install` layout; `1`
when only guidance was printed (unknown layout, Windows, `go` missing from
PATH).

### Output

On an unknown layout:

```
Could not detect a `go install` layout (looked for $GOBIN, $GOPATH/bin, $HOME/go/bin).
  binary: <path>
To upgrade manually, run:
  go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest
Or download a release from:
  https://git.dpemmons.com/dpemmons/cswap/releases
```

On Windows: `To upgrade claude-swap on Windows, run:` followed by the
`go install` command.

### Errors

`Detected a go install layout but \`go\` is not on PATH. Run the upgrade
manually from a shell where it is available.` (exit 1).

### Example

```
$ cswap upgrade
Could not detect a `go install` layout (looked for $GOBIN, $GOPATH/bin, $HOME/go/bin).
  binary: ~/go/bin/cswap
To upgrade manually, run:
  go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest
Or download a release from:
  https://git.dpemmons.com/dpemmons/cswap/releases
$ echo $?
1
```

### See also

The passive update notice under NOTES.

---

## cswap purge

### Synopsis

```
cswap purge [--debug]
```

Legacy: `cswap --purge`.

### Description

Removes all claude-swap data — the backup directory and every stored account
credential file. It does not affect the current Claude Code login. Prompts for
confirmation (`[y/N]`) on an interactive terminal; any answer other than `y`
cancels. The passive update notice is suppressed for this command.

### Files

Removes the backup root and its contents (and the macOS Keychain entries under
service `claude-swap`).

### Exit status

`0` on success or a cancelled prompt; `1` on a handled error.

### Output

A warning block naming the backup directory, then the prompt. `Cancelled` when
declined.

### Errors

A filesystem or Keychain removal failure surfaces as `Error: <message>`
(`ClaudeSwitchError`), exit 1.

### Example

```
$ cswap purge
This will remove ALL claude-swap data from your system:
  - Backup directory: ~/.local/share/claude-swap
  - All stored account credential files

Note: This does NOT affect your current Claude Code login.

Are you sure you want to purge all data? [y/N] Cancelled
```

### See also

`cswap remove`.

---

## cswap help

### Synopsis

```
cswap help
cswap -h | --help
```

### Description

Prints the usage line, the command list, the visible options, and the flag
examples. `-h` / `--help` after any subcommand prints that subcommand's usage
line instead.

### Files

None. `cswap help` touches no files.

### Exit status

`0`.

### Output

The full help text (usage line, `Commands:` block, `options:` block, examples,
and the legacy-spelling note).

### Errors

None. `cswap help` cannot fail.

### Example

```
$ cswap help
usage: cswap <command> [args] [options]

Multi-Account Switcher for Claude Code

Commands:
  cswap help                       show this help
  cswap list                       list managed accounts
  ...
Aliases: ls=list  rm=remove  update=upgrade
  ...
```

### See also

`cswap --version`.

---

# Global reference

## ENVIRONMENT

cswap reads the following environment variables.

| Variable | Effect |
|----------|--------|
| `CLAUDE_CONFIG_DIR` | Overrides the Claude Code config home: the global config is `<dir>/.claude.json` and credentials are `<dir>/.credentials.json`. `cswap env` and `cswap run` treat any set value as a preset and override it with the session profile they prepare. For every other command the effect depends on the value: a cswap session pin — a value inside `<backup_root>/sessions/`, as set by `cswap env` — is ignored, and the command operates on the default login and prints `This shell is pinned via cswap env; operating on the default login.` to stderr; a custom value outside `<backup_root>/sessions/` is honored. The default-profile mirror used by session sharing deliberately ignores this and reads the real `~/.claude` profile. |
| `XDG_DATA_HOME` | On Linux/WSL, selects the backup-root parent: `<XDG_DATA_HOME>/claude-swap`. Ignored when unset, empty, or non-absolute; a leading `~` is expanded. |
| `HOME` | The user's home directory; the base for `~/.claude.json`, `~/.claude/`, `~/.local/share`, `~/.claude-swap-backup`, and `~/go/bin`. |
| `WSL_DISTRO_NAME` | When non-empty, the platform is detected as WSL (which uses the Linux/WSL backup-root layout). |
| `CONTAINER`, `container` | When either is non-empty, the root-user guard is bypassed (running as root is permitted in a container). |
| `NO_COLOR` | When present (even empty), disables ANSI color. Highest color precedence. |
| `FORCE_COLOR` | When present, forces color on (unless `NO_COLOR` is also present). |
| `TERM` | `TERM=dumb` disables color on a TTY. |
| `GOBIN`, `GOPATH` | Consulted by `cswap upgrade` and the passive update notice to detect a `go install` layout. |

Color precedence: `NO_COLOR` present → off; else `FORCE_COLOR` present → on;
else non-TTY → off; else `TERM=dumb` → off; else on. The result is computed once
per process and cached.

**Authentication-override variables (scrubbed).** The following make `claude`
bypass account OAuth. `cswap run` and `cswap env` scrub them from the prepared
session environment (and `cswap env` emits an unset line for each one currently
set, so the calling shell drops it too):

- `ANTHROPIC_API_KEY`
- `ANTHROPIC_AUTH_TOKEN`
- `CLAUDE_CODE_OAUTH_TOKEN`
- `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`
- `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`

They are not scrubbed on the same-account fast path or when `cswap run` falls
through to launching the default login.

## FILES

The **backup root** is platform-dependent:

| Platform | Backup root | Credential store |
|----------|-------------|------------------|
| Linux / WSL | `${XDG_DATA_HOME:-~/.local/share}/claude-swap/` | file-based, under the backup root |
| macOS | `~/.claude-swap-backup/` | login Keychain, service `claude-swap` |
| Windows | `~/.claude-swap-backup/` | file-based, under the backup root |

A legacy `~/.claude-swap-backup/` on Linux/WSL is migrated to the XDG location
on first use.

Inside the backup root:

| Path | Contents |
|------|----------|
| `sequence.json` | The account registry: `activeAccountNumber`, `lastUpdated`, the `sequence` array of slot numbers, and an `accounts` map keyed by slot (`email`, `uuid`, `organizationUuid`, `organizationName`, `added`, and optional `alias`, `kind: "api_key"`, `disabled: true`). |
| `settings.json` | Settings (see SETTINGS). |
| `mappings.json` | Directory→account mappings (`schemaVersion`, `mappings` keyed by absolute path). |
| `autoswitch_state.json` | `cswap auto` cooldown / quarantine state, guarded by `.autoswitch_state.lock`. |
| `configs/` | Per-account config snapshots, `.claude-config-<n>-<email>.json`. |
| `credentials/` | Per-account credential files, `.creds-<n>-<email>.enc` (file backend). macOS stores these in the Keychain instead. |
| `sessions/` | Per-account session-mode profiles, `<n>-<email>` (the `@` in the email replaced by `_`), created by `cswap run` / `cswap env`. |
| `cache/usage.json` | Cached usage measurements. |
| `cache/update_check.json` | Last passive update-check result (`{"timestamp": <epoch-seconds>, "data": <latest-version>}`). |
| `claude-swap.log` | Rotating log file, 1 MB per file, 3 backups (`claude-swap.log.1`, `.2`, `.3`). |

Claude Code's own files that cswap reads and writes:

| Path | Role |
|------|------|
| `~/.claude.json` (or `<CLAUDE_CONFIG_DIR>/.claude.json`, or the legacy `<config_home>/.config.json` when present) | The global config; the active `oauthAccount` lives here. |
| `~/.claude/.credentials.json` (file backend) | The active OAuth credentials. |

## SETTINGS

`settings.json` holds one section, `autoswitch`, plus a `schemaVersion` (1).
Every key, with its type, range, default, and meaning:

| Key | Type | Range | Default | Meaning |
|-----|------|-------|---------|---------|
| `autoswitch.threshold` | float (percent) | 50–99.9 | 90 | Switch when the binding 5h/7d window reaches this percent. |
| `autoswitch.intervalSeconds` | float (seconds) | 15–3600 | 60 | Poll interval for the `cswap auto` loop. |
| `autoswitch.cooldownSeconds` | float (seconds) | 0–86400 | 300 | Minimum seconds between proactive switches. |
| `autoswitch.hysteresisPct` | float (percent) | 0–50 | 10 | A switch target must beat the active account by at least this many percent. |
| `autoswitch.strategy` | choice | `best`, `soonest-reset` | `best` | How auto-switch orders qualifying targets: `best` (most headroom) or `soonest-reset` (earliest weekly renewal). See below. |
| `autoswitch.includeApiKeyAccounts` | bool | — | false | Allow rotating onto managed API-key accounts (billed per token). |
| `autoswitch.unhealthyTicks` | int | 1–100 | 3 | Consecutive failed polls before an account is treated as unhealthy. |
| `autoswitch.model` | string | — | (none) | Also switch on these models' weekly limits (for example `Fable`, `Fable,Opus`, or `all`). |

Reads are forgiving: a missing file, a bad type, or an out-of-range value
degrades to the (clamped) default without error. Writes via `cswap config set`
are strict: an out-of-range or mistyped value is rejected with a `ConfigError`.
A whole-number float is stored and shown without a fractional part
(`80.0` → `80`).

**`autoswitch.strategy` ordering.** The strategy governs only the order in
which already-qualifying candidates are offered to `cswap auto`; it changes
none of the qualification gates. Known and positive headroom, the cooldown,
quarantine exclusion, and the API-key last resort apply identically under
both values and for every trigger (`proactive`, `at-limit`, `failover`). The
threshold-landing and hysteresis checks apply only under the `proactive`
trigger — an `at-limit` or `failover` tick must leave the active account
regardless, so refusing every imperfect target would strand it — and a
qualifying candidate reached by either of those two triggers may therefore
sit at or above the threshold.

- `best` orders candidates by headroom, most remaining first; accounts tied
  on headroom keep sequence order. This is the setting's default.
- `soonest-reset` orders candidates by *renewal time*, in two tiers. The
  first tier holds every candidate below the threshold — headroom such that
  `100` minus headroom is under `autoswitch.threshold` — and ranks by the
  latest parseable `resets_at` among the account's weekly-scope windows: the
  7-day window plus every per-model scoped window matched by
  `autoswitch.model`. The 5h window is never part of the renewal, since it
  is not weekly. A window whose `resets_at` is absent or unparseable is
  skipped; an account with no parseable weekly `resets_at` at all has an
  *unknown* renewal. Within this tier, a known renewal sorts before an
  unknown one; among known renewals the earliest sorts first; ties — an
  equal renewal, or two unknown renewals — fall back to headroom descending,
  then to sequence order. The second tier holds every candidate at or above
  the threshold and ranks by headroom descending, as a last resort. This
  tier is reachable only under the `at-limit` and `failover` triggers: under
  `proactive`, the threshold-landing gate above already excludes such a
  candidate from qualifying at all, so the second tier is always empty and
  `soonest-reset` orders proactive candidates exactly as the first tier
  describes. Every first-tier candidate sorts before every second-tier
  candidate — a candidate at or above the threshold is never preferred over
  one below it merely for an earlier renewal.

```
$ cswap config set autoswitch.strategy soonest-reset
autoswitch.strategy = soonest-reset
```

**`autoswitch.model` name matching.** A model name is matched against the
scoped weekly window's display name, compared case-insensitively and exactly.
The value stored and matched is the bare display name (for example `Fable`), not
a decorated form; a name that matches no window is silently inert (no window is
counted and no error is raised). The `--strategy` command-line flag and the
usage-aware `switch`/`auto` strategies accept `best` and `next-available` — a
separate vocabulary from the persisted `autoswitch.strategy` setting, which
accepts `best` and `soonest-reset`.

This matching decision is visible, not just enforced: `cswap tui`'s
auto-switch screen (above) lists every per-model weekly window an account
reports in its "Next best" candidates panel whether or not `autoswitch.model`
names it, but only a window this setting matches counts toward that row's
binding percentage and rank. An unmatched window renders — muted — for
visibility alone, so a user can watch a model's weekly window climb before
deciding to fold it into the switching decision.

## EXIT STATUS

| Code | Meaning |
|------|---------|
| `0` | Success. For `switch`, includes a no-op "already active". |
| `1` | A handled domain error (a `ClaudeSwitchError`: config, switch, session, credential, lock, transfer, or migration). Also printed for guidance-only `upgrade`. |
| `2` | A usage error: an unknown flag, a bad value, a missing required argument, or an illegal flag combination (the argument-parser surface). |
| `3` | `cswap auto --once` only: blocked — a switch was wanted but no viable target exists / all accounts exhausted. |
| `130` | Interrupted by SIGINT (Ctrl-C). |

For `cswap auto --once` the status is the tick outcome: `0` switched, `1` error,
`2` no action, `3` blocked. `cswap run` on success is replaced by the launched
`claude` and exits with its status. `cswap menubar` always exits `1` in this
build.

## JSON OUTPUT CONTRACT

`--json` is accepted by `list`, `status`, `switch` (and `switch <id>`),
`config list`, and `config get`; `cswap auto --json` emits JSONL. Every JSON
payload carries `"schemaVersion": 1`.

- **One document per command.** `list`, `status`, `switch`, and `config`
  write exactly one indented (2-space) JSON document to stdout.
- **JSONL for `auto`.** `cswap auto --json` writes one compact JSON object per
  event, one per line.
- **Additive rule.** Under schemaVersion 1 the schema only grows: optional keys
  (`alias`, `disabled`, `atLimit`, `limitingWindows`, `usageFetchedAt`,
  `usageAgeSeconds`, and the window sub-keys `resetsAt`, `countdown`, `clock`)
  are present only when they apply and omitted otherwise; no existing key
  changes type or meaning. Consumers must ignore unknown keys and, for `auto`,
  unknown `event` kinds.
- **Error envelope.** A handled error under `--json` is emitted as:
  ```
  {
    "schemaVersion": 1,
    "error": { "type": "<ErrorClass>", "message": "<text>" }
  }
  ```
  On the main commands the envelope is indented on stdout; for `cswap auto` it
  is compact on stdout. `<ErrorClass>` is one of `ConfigError`, `SwitchError`,
  `SessionError`, `ValidationError`, `AccountNotFoundError`, `CredentialError`,
  `CredentialReadError`, `CredentialWriteError`, `LockError`,
  `ClaudeCodeLockTimeout`, `TransferError`, `MigrationError`, or
  `MigrationIncomplete`.
- **`auto` event kinds** and their fields are enumerated under `cswap auto`
  above (`poll`, `switch`, `no-switch`, `account-quarantined`,
  `account-unquarantined`, `all-exhausted`, `sleep`, `error`,
  `config-warning`). Every event also carries `event` and `ts` (RFC3339 UTC,
  `Z` suffix).

## SIGNALS

| Signal | Effect |
|--------|--------|
| `SIGINT` (Ctrl-C) | Prints a dimmed cancellation note (to stderr under `--json`, otherwise stdout) and exits `130`. During `cswap auto` the note reads `Auto-switch stopped`; elsewhere `Operation cancelled`. |
| `SIGTERM` | During the `cswap auto` loop, stops the loop cleanly and exits `0` (the systemd-stop path). Other commands take the default action. |

## COMPATIBILITY

- The on-disk formats are shared with claude-swap (Python): `sequence.json`,
  `settings.json`, `mappings.json`, the per-account `configs/` and
  `credentials/` files, `cache/usage.json`, and the log format are read and
  written identically. A backup directory created by either implementation
  works in place with the other.
- The `.cswap` export file format is identical; either implementation reads the
  other's export files byte-for-byte. Exports are always written with
  `"encrypted": false`, and an export marked `"encrypted": true` is rejected on
  import by both.
- The command grammar, `--json` schemas, exit codes, and the Claude Code
  credential-lock protocol are preserved. The legacy `--flag` spellings work
  alongside the memorable verbs.
- This implementation defines these behaviors differently from claude-swap
  (Python):
  - It ships as a single static binary; there is no Python runtime.
  - The macOS menu bar app is not included (`cswap menubar` exits 1).
  - `cswap upgrade` and the passive update notice use this repository's releases
    and `go install`, not PyPI/uv/pipx. Pre-release builds do receive update
    notices.
  - `cswap env` is provided; it has no claude-swap (Python) counterpart.
  - `cswap list` / `cswap status` (human and `--json`) and the TUI surface an
    at-limit marker that folds in the per-model weekly windows configured by
    `autoswitch.model`. The additive `--json` fields `atLimit` and
    `limitingWindows` are present only when an account is at limit.

## NOTES

- **macOS Keychain propagation (~30 s).** On macOS, a running Claude Code
  process reads credentials through a Keychain cache and may take up to about 30
  seconds to observe a just-switched account. `cswap switch` on macOS prints
  `Restart Claude Code to apply immediately — otherwise the session can take up
  to ~30 seconds to pick up the new account.` Restarting Claude Code applies the
  change at once. `cswap auto` switches proactively (while the old account is
  still valid) so this latency is harmless.
- **A switch never requires a restart to be correct.** On the file-credential
  platforms (Linux, WSL, Windows) the new account is active on the next message;
  `cswap switch` prints `New account is active on your next message — no restart
  needed.` The restart advice on macOS is only about applying the change sooner
  than the Keychain cache would.
- **Same account in two places can drift.** An account held as both the default
  login and a session-mode profile can make one copy's token go stale if the
  server rotates it, because each profile refreshes independently. `cswap run`
  and `cswap env` do not create a second copy for an account that is already the
  active default login — `run` launches the default login directly and `env`
  exports nothing — so this arises only when the two roles are established
  separately, for example pinning an account with `cswap env` and later making it
  the default login with `cswap switch`. If a session later fails to
  authenticate, exit it and re-run `cswap run <n>`. `cswap list` flags two slots
  that report identical usage and reset times as possibly the same account.
- **At-limit is independent of `usageStatus`.** `atLimit` reflects a relevant
  rate-limit window (including a per-model weekly window from `autoswitch.model`)
  being at or over its limit. An account can be `usageStatus: "ok"` and still
  `atLimit: true` when a scoped weekly window is exhausted even though the
  account-wide 7d window has room; conversely `atLimit` is absent whenever no
  window is at its limit. Treat the two fields as orthogonal.
- **macOS Keychain unavailable.** When the login Keychain is locked or busy,
  cswap does not misreport the active account as having no credentials. A read of
  the active account's OAuth credential is retried briefly and, on continued
  failure, surfaces as `usageStatus: "keychain_unavailable"` (human line
  `keychain unavailable — locked or in use; try again`) rather than
  `no_credentials`. Backup-store credential reads and writes fall back to
  file-based storage for the rest of the process run, so a flaky Keychain never
  blocks a switch.
- **Auto-switch quarantine.** During a real (non-`--dry-run`) `cswap auto` tick,
  a slot whose token cannot be freshened is quarantined: excluded from the
  auto-switch candidate pool until recovered. A quarantine is raised for
  `invalid_grant` (the stored refresh-token lineage is dead) or
  `identity-conflict` (the stored credential now authenticates as a different
  account or organization); the `account-quarantined` event carries the slot
  `number`, `email`, and this `reason`, and its human line is `Account-<n>
  (<email>) quarantined: <reason>. Log in with it and run 'cswap --add-account
  --slot <n>' to recover.` The state persists in `autoswitch_state.json`.
  Quarantine affects only `cswap auto`; a direct `cswap switch <n>` still
  activates a quarantined account. A quarantine is released automatically at the
  next real tick once the slot's recorded email changes (`account-replaced`) or
  its stored credential fingerprint changes (`credentials-replaced`) — which is
  exactly what re-running `cswap add --slot <n>` (or `cswap add-token ... --slot
  <n>`) after logging the account back in produces; an `account-unquarantined`
  event is then emitted.
- **Passive update notice.** After most commands, a muted one-line update notice
  may be printed to stderr when a newer release is known. It is suppressed in
  `--json` mode and after `purge` and `upgrade`, and it never affects the exit
  status.

## See also

Project overview: `README.md`. Architecture and design decisions:
`docs/DESIGN.md`.
