# cswap

cswap manages multiple Claude Code logins from one machine. It stores each
account's credentials and configuration under a backup directory, activates one
account at a time as Claude Code's live login, switches between accounts without
a manual logout, switches automatically before an account reaches its rate
limit, reports each account's usage in a list and a full-screen dashboard, and
runs additional accounts in parallel terminals. It works with the Claude Code
CLI and the VS Code extension, and ships as a single static Go binary.

This document is the entry-point guide: concepts, installation, and the tasks a
user performs in order. The complete per-command contract — every argument,
default, exit status, JSON schema, and error condition — is in
[`docs/reference.md`](docs/reference.md). The architecture is in
[`docs/DESIGN.md`](docs/DESIGN.md).

## Concepts

**Account.** An account is a saved Claude Code login: an OAuth credential blob
(or an API key) together with a snapshot of the login's configuration. cswap
holds accounts in its backup store and never alters the underlying Claude
subscription; it only moves credentials into and out of Claude Code's own
credential store.

**Slot.** Each account occupies a numbered slot. Slots start at 1 and may be
sparse — an account can sit at slot 5 while slot 4 is empty. Every command that
names an account accepts the slot number, the account email, or the account
alias interchangeably.

**Active login.** Exactly one account is active at a time. The active account is
the login Claude Code uses: its credentials are the ones written into Claude
Code's credential store and its identity fields into `~/.claude.json`. Switching
replaces the active login with another account's stored credentials.

**Backup store.** The backup store is a directory holding every managed
account's saved credentials and configuration, the account roster
(`sequence.json`), settings, directory mappings, the usage cache, session
profiles, and the switch log. Its location is per-platform (see
[Data locations](#data-locations)). Switching copies credentials between the
backup store and the active login; it removes nothing.

**Usage windows.** Claude enforces rate limits over rolling time windows: a
5-hour window and a 7-day window, and — for accounts that use them — per-model
weekly windows. cswap fetches each account's usage and reports the remaining
headroom as a percentage. An account is *at limit* when a relevant window has
reached or exceeded its limit. Auto-switch and the `best` switch strategy use
this headroom to choose a target; setting `autoswitch.strategy` to
`soonest-reset` makes auto-switch order targets by earliest weekly renewal
instead.

**Session profile.** A session profile is a private `CLAUDE_CONFIG_DIR` under
the backup store's `sessions/` directory. It lets one account run in a single
terminal without changing the machine-wide active login. `cswap run` and
`cswap env` prepare and use session profiles; this is how two accounts run in
parallel.

**Relationship to claude-swap (Python).** cswap is a Go port of claude-swap
(Python, MIT, by Onur Cetinkol). The two implementations share their on-disk
formats and `--json` schemas: a backup store, an export file, or a switch log
written by either implementation is read by the other. The command grammar,
exit codes, and lock protocol are identical. The macOS menu bar application is
not part of cswap. The update mechanism, the `cswap env` command, and the
at-limit markers are Go-only; where the Go binary and the Python reference
diverge, the divergences are enumerated in [`docs/DESIGN.md`](docs/DESIGN.md)
§6 and its Amendments.

## Installation

**Prerequisites.**

- Go 1.25.5 or newer to build or `go install`. cswap uses no cgo.
- The Claude Code CLI, for the accounts to run against.
- macOS only: the `security` command, used to read and write the login Keychain.

**With `go install`:**

```bash
go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest
```

The binary is named `cswap` and is placed in the Go install directory (`$GOBIN`,
or `$GOPATH/bin`, or `~/go/bin`). Add that directory to `PATH`.

**From a checkout:**

```bash
git clone https://git.dpemmons.com/dpemmons/cswap
cd cswap
make build      # builds ./cswap with the version embedded
make install    # go install with the version embedded
```

`make help` lists every target.

## Tasks

The tasks below follow the order in which a user meets them. Each transcript is
complete. Prompt lines begin with `$`; all other lines are program output.

### Add the first account

Log into Claude Code as usual, then snapshot the current login into slot 1:

```
$ cswap add
Added Account 1: alice@example.com [personal]
```

`cswap add` reads the active Claude Code login, copies its credentials and
configuration into the backup store, and makes it the active managed account. If
no account is yet managed, it lands in slot 1.

### Add a second account

There are two ways to register another account.

To capture an interactive login, log out of Claude Code, log in as the second
account, and run `cswap add` again. The new login is snapshotted into the next
free slot and becomes active.

To register an account from a setup token or API key without disturbing the
current login, use `cswap add-token`. The token is read from the argument, from
`-` (standard input), or interactively:

```
$ cswap add-token sk-ant-oat01-... --email bob@example.com --slot 2
Added Account 2: bob@example.com [personal] (from token)
```

`--slot` is optional; without it the account lands in the next free slot.
`--email` labels the account.

### Switch accounts

Rotate to the next account in slot order:

```
$ cswap switch
Switched to Account-2 (bob@example.com)
Accounts:
  1: alice@example.com [personal]
     usage unavailable (http-429)

  2: dev (bob@example.com) [personal] (active)
     usage unavailable (http-429)

  3: key@example.com [personal]
     API key (no quota)

  5: carol@example.com [personal] (disabled)
     usage unavailable (http-429)

New account is active on your next message — no restart needed.
```

Switch to a specific account by slot number, email, or alias:

```
$ cswap switch 1
$ cswap switch bob@example.com
$ cswap switch dev
```

Switch by remaining quota instead of slot order:

```
$ cswap switch --strategy best            # the account with the most headroom
$ cswap switch --strategy next-available  # rotate, skipping at-limit accounts
```

**Restart semantics.** The follow-up line states whether Claude Code needs a
restart, keyed to where the credential write landed:

- On Linux, WSL, and Windows the active credentials are file-based. The line
  reads `New account is active on your next message — no restart needed.` The
  running Claude Code session picks up the new account on its next message.
- On macOS the active credentials are stored in the login Keychain. The line
  reads `Restart Claude Code to apply immediately — otherwise the session can
  take up to ~30 seconds to pick up the new account.`

### Read the list dashboard

`cswap list` (alias `ls`) prints every managed account and its usage:

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

Markers, per line:

- The leading number is the slot.
- `alias (email)` — when an account has an alias, the alias precedes the email
  in parentheses (slot 2 above).
- `[personal]` or `[Organization Name]` — the account's organization label.
- `(active)` — the currently active login.
- `(disabled)` — held out of auto-rotation by `cswap disable`.
- `at limit: <windows>` — a relevant usage window is at or over its limit; the
  suffix names the limiting windows (for example `7d` or `Fable 5`).

The second line per account is usage. For an OAuth account with a healthy fetch
it shows the 5-hour and 7-day percentages and reset times; `usage unavailable
(http-NNN)` reports a failed fetch and its HTTP status; `API key (no quota)`
marks an account authenticated by API key, which has no measured quota.

Add `--token-status` for each OAuth account's token expiry and refresh state:

```
$ cswap list --token-status
Accounts:
  1: alice@example.com [personal] (active)
     usage unavailable (http-429)
     • oauth: unknown expiry, refresh token no
  ...
```

`cswap list --json` and `cswap status --json` emit one JSON document
(schemaVersion 1) instead of the human table; the schemas are in
[`docs/reference.md`](docs/reference.md). To extract the active account's email:

```
$ cswap status --json | jq -r '.active.email'
alice@example.com
```

The full-screen interactive dashboard is `cswap` with no arguments (also
`cswap tui` and `cswap watch`). It shows the same accounts with live usage and
refreshes on a timer.

### Alias accounts

An alias is a short name usable anywhere an account is named:

```
$ cswap alias 3 ops
Set alias 'ops' for Account 3

$ cswap alias
Aliases:
  2: dev (bob@example.com)
  3: ops (key@example.com)

$ cswap alias 3 --unset
Removed alias for Account 3
```

### Disable and enable accounts

A disabled account stays managed and switchable by hand but is skipped by
auto-switch:

```
$ cswap disable 2
Disabled Account-2 (bob@example.com).

$ cswap enable 2
Enabled Account-2 (bob@example.com).
  It is back in the rotation.
```

### Auto-switch before a limit

`cswap auto` runs a foreground loop that polls usage and switches to another
account before the active one reaches its rate limit:

```
$ cswap auto --threshold 80
Auto-switch running: threshold 80%, every 60s — Ctrl-C to stop
13:53:36  Account-1 (alice@example.com): usage unknown (http-429) (switch at 80%) | others: #2: ? (http-429), #3: ?, #5: ? (http-429)
13:53:36  no switch: active-usage-unknown (1/3 before failover)
```

`--threshold` is the headroom percentage at which a switch triggers; the polling
interval defaults to 60 seconds. Ctrl-C stops the loop and exits 130.

**Single tick, for cron.** `cswap auto --once` performs exactly one evaluation
and exits; `--json` prints the tick as JSON event lines:

```
$ cswap auto --once --json
{"active":{"email":"alice@example.com","number":1},"event":"poll","fetchErrors":{"1":"http-429","2":"http-429","5":"http-429"},"headroomPct":{"1":null,"2":null,"3":null,"5":null},"schemaVersion":1,"threshold":80.0,"ts":"2026-07-17T20:49:40Z"}
{"detail":"1/3 before failover","event":"no-switch","reason":"active-usage-unknown","schemaVersion":1,"ts":"2026-07-17T20:49:40Z"}
```

The process exit code reports the outcome, so a scheduler can branch on it:

| Exit | Meaning  | Condition                                                    |
|------|----------|-------------------------------------------------------------|
| 0    | Switched | A switch happened.                                          |
| 1    | Error    | Network trouble, lock contention, or a transient failure.  |
| 2    | NoAction | Nothing to do — below threshold, in cooldown, or idle.     |
| 3    | Blocked  | A switch was wanted but no viable target remained.         |

A crontab entry that ticks every five minutes and logs the result:

```cron
*/5 * * * * cswap auto --once --json >> cswap-auto.log 2>&1
```

cron runs with a minimal `PATH`; the entry finds `cswap` only if its install
directory is on cron's `PATH` (set `PATH=` at the top of the crontab, or place
`cswap` in a directory cron already searches).

**Per-model weekly limits.** By default auto-switch weighs only the account-wide
5-hour and 7-day windows. To also count a model's per-model weekly window, set
the model's display name in `autoswitch.model`:

```
$ cswap config set autoswitch.model Fable
autoswitch.model = Fable
```

The value is matched, case-insensitively, against the display name of the
account's scoped weekly window. An exhausted per-model weekly window then reads
as at-limit even when the account-wide 7-day window has headroom. A name that
matches no window is a silent no-op — use the display name exactly as it appears
in the account's per-model usage rows.

**Renewal-ordered switching.** By default auto-switch tries the qualifying
target with the most headroom first (`autoswitch.strategy best`). Setting the
strategy to `soonest-reset` orders qualifying targets by weekly renewal
instead: the account whose 7-day window — and any `autoswitch.model` weekly
windows — refills earliest is tried first, so quota is spent where it returns
soonest. Qualification itself is unchanged. On a proactive switch, a target
must still land under the threshold and beat the active account by the
hysteresis margin. On an at-limit or failover switch neither check applies,
but `soonest-reset` still never lets an early renewal beat the threshold: an
account at or above the threshold is tried only after every account below
it, regardless of how soon it renews.

```
$ cswap config set autoswitch.strategy soonest-reset
autoswitch.strategy = soonest-reset
```

### Run accounts in parallel

`cswap run` launches Claude Code as a chosen account in the current terminal
only, using a session profile, without changing the machine-wide active login.
A second terminal can run `cswap run` for a different account at the same time.

```
$ cswap run 2 -- --version
Launching Account-2 (bob@example.com) [session mode]
2.1.212 (Claude Code)
```

Arguments after `--` are forwarded to `claude`. When the requested account is
already the active default login, `cswap run` launches `claude` directly rather
than preparing a session profile.

**Directory mappings.** Map a directory to an account so a bare `cswap run` in
that directory resolves to it:

```
$ cswap map 2 ~/work/client-app
Mapped ~/work/client-app → Account-2 (bob@example.com)

$ cswap map
Directory mappings:
  ~/work/client-app → 2: bob@example.com [personal]

$ cd ~/work/client-app && cswap run     # runs as account 2
```

`cswap unmap [path]` removes a mapping.

**Pinning a shell.** `cswap env` prepares the same session profile `cswap run`
does, but instead of launching `claude` it prints an eval-able export that pins
the current shell's `CLAUDE_CONFIG_DIR` to the account. Every subsequent
`claude` in that shell runs as the pinned account, and no separate process is
launched:

```
$ eval "$(cswap env 2)"                 # pin this shell to account 2
$ eval "$(cswap env)"                   # resolve from this directory's mapping
$ eval "$(cswap env --unset)"           # drop the pin (back to the default login)
```

`cswap env` writes only the eval lines to standard output; every notice goes to
standard error:

```
$ cswap env 2
Prepared Account-2 (bob@example.com) [session mode]      # (stderr)
export CLAUDE_CONFIG_DIR='~/.local/share/claude-swap/sessions/2-bob_example.com'
```

When the chosen account is already the active default login, `cswap env` exports
nothing and writes a note to standard error, since an unpinned shell already uses
that account; the `eval` is a safe no-op.

Other shells take `--shell`:

```fish
cswap env 2 --shell fish | source
```
```powershell
cswap env 2 --shell pwsh | Invoke-Expression
```

A pinned shell keeps the profile it eval'd until it eval's again. After
switching accounts or a credential change, re-run the `eval` — or use
`cswap run`, which re-prepares the profile on every launch.

In a pinned shell, every cswap command except `cswap env` and `cswap run`
ignores the pin and operates on the default login, printing `This shell is
pinned via cswap env; operating on the default login.` to standard error.

### Export and import

`cswap export` writes accounts to a portable file for transfer to another
machine:

```
$ cswap export backup.cswap
Exported 4 account(s) to backup.cswap

$ cswap export bob-only.cswap --account 2
Exported 1 account(s) to bob-only.cswap
```

`--account` limits the export to one account. The export file is JSON with
`encrypted: false`; the format is identical to claude-swap's, so either
implementation imports the other's exports.

`cswap import` reads accounts back:

```
$ cswap import backup.cswap
Imported alice@example.com → slot 1
Imported bob@example.com → slot 2
Imported key@example.com → slot 3
Imported carol@example.com → slot 5
Done: 4 imported, 0 overwritten, 0 skipped
Note: alice@example.com is your current live login — activate the imported credentials with: cswap --switch-to 1 --force
```

Import skips a slot already holding a different account unless `--force` is
given, which overwrites it. Import rejects any file marked `encrypted: true`.

### Configure settings

`cswap config` prints the settings and their values, marking each unchanged one
`(default)`:

```
$ cswap config
autoswitch.threshold              80
autoswitch.intervalSeconds        60     (default)
autoswitch.cooldownSeconds        300    (default)
autoswitch.hysteresisPct          10     (default)
autoswitch.strategy               best   (default)
autoswitch.includeApiKeyAccounts  false  (default)
autoswitch.unhealthyTicks         3      (default)
autoswitch.model                  Fable
```

`cswap config set KEY VALUE` validates and stores one setting. An out-of-range
value is rejected and nothing is written:

```
$ cswap config set autoswitch.threshold 85
autoswitch.threshold = 85

$ cswap config set autoswitch.threshold 40
Error: autoswitch.threshold must be between 50 and 99.9
```

Each key's type, default, and range are listed in
[`docs/reference.md`](docs/reference.md).

### Upgrade

`cswap upgrade` (alias `update`) updates the binary in place. When cswap was
installed with `go install`, it detects the install layout and re-runs
`go install ...@latest`. When it cannot detect a `go install` layout, it prints
the manual command and the releases URL instead:

```
$ cswap upgrade
Could not detect a `go install` layout (looked for $GOBIN, $GOPATH/bin, $HOME/go/bin).
  binary: ~/go/bin/cswap
To upgrade manually, run:
  go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest
Or download a release from:
  https://git.dpemmons.com/dpemmons/cswap/releases
```

On Windows, `cswap upgrade` never self-replaces; it prints the upgrade command
for the user to run.

### Remove and purge

`cswap remove <num|email>` unmanages one account, deleting its stored
credentials and configuration from the backup store. It prompts for
confirmation first; any answer other than `y` cancels. `cswap disable` (above)
keeps the account but drops it from auto-rotation.

`cswap purge` removes all cswap data. It states what it will delete and prompts
for confirmation; any answer other than `y` cancels:

```
$ cswap purge
This will remove ALL claude-swap data from your system:
  - Backup directory: ~/.local/share/claude-swap
  - All stored account credential files

Note: This does NOT affect your current Claude Code login.

Are you sure you want to purge all data? [y/N] n
Cancelled
```

## Data locations

The backup store is the root of everything cswap persists.

| Platform    | Active credentials             | Backup store                                        |
|-------------|--------------------------------|-----------------------------------------------------|
| Linux / WSL | file, in Claude Code's store   | `${XDG_DATA_HOME:-~/.local/share}/claude-swap/`     |
| macOS       | login Keychain, service `claude-swap` | `~/.claude-swap-backup/`                     |
| Windows     | file, in Claude Code's store   | `~/.claude-swap-backup/`                             |

Inside the backup store:

| Path                      | Contents                                                       |
|---------------------------|---------------------------------------------------------------|
| `sequence.json`           | The account roster: slot, email, alias, kind, disabled state. |
| `credentials/`            | Each account's stored credential blob (`.enc`).               |
| `configs/`                | Each account's configuration snapshot.                        |
| `settings.json`           | Auto-switch settings (`cswap config`).                        |
| `mappings.json`           | Directory-to-account mappings (`cswap map`).                  |
| `cache/usage.json`        | Cached usage fetches.                                         |
| `cache/update_check.json` | Cached update-check result.                                  |
| `sessions/`               | Session profiles for `cswap run` and `cswap env`.            |
| `claude-swap.log`         | The switch log; rotates at 1 MB, keeping 3 backups.          |

The full per-command contract — arguments, defaults, exit codes, JSON schemas,
environment variables, and error conditions — is in
[`docs/reference.md`](docs/reference.md).

## NOTES

**Restart timing.** File-based platforms (Linux, WSL, Windows) apply a switch on
Claude Code's next message with no restart. macOS reads credentials from the
Keychain and a running session may take up to about 30 seconds to notice a
switch; restarting Claude Code applies it at once.

**Same-account drift.** cswap and Claude Code share the same credential store.
While an account is active, Claude Code may refresh its own token; cswap folds
such refreshes back into the account's backup on the next operation, so the
backup does not go stale against the live login.

**API-key accounts.** An account authenticated by API key has no measured usage
quota. It shows `API key (no quota)` in the list, never carries usage
percentages, and is excluded from auto-switch unless
`autoswitch.includeApiKeyAccounts` is set.

**macOS Keychain.** On macOS, active and session-profile credentials live in the
login Keychain. The first credential read or write in a session may prompt for
Keychain access. `cswap purge` deletes the session-profile Keychain entries
along with the backup store.

## License

MIT. Behavioral design and CLI surface derived from claude-swap, © Onur
Cetinkol, MIT.
</content>
</invoke>
