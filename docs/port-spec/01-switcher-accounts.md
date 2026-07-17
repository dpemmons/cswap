# claude-swap — Account Store & Account Lifecycle Spec

## Overview

This spec covers the **account store** and the **account lifecycle** inside
`switcher.py` (`ClaudeAccountSwitcher`): the on-disk backup directory layout and
every file in it, the `sequence.json` account-metadata format, the per-account
credential/config backup mechanism (file `.enc` vs macOS Keychain, `.prev`
retention, the "unclaimed credentials" stash), and the mutating operations `add`
(plain / `--slot` / `--alias`), `add-token` (setup-token and `sk-ant-api…`
API-key accounts, stdin form, email defaulting), `remove`, `move`
(relocate/swap), `swap`, `alias set/unset/list`, `disable/enable`, and `purge`.
It also covers identifier resolution, duplicate/collision detection, and the
one-time org-field migration, because those constrain the store format. Usage
polling, the switch/status/list-usage rendering, and the auto-switch engine are
**out of scope** except where they read or constrain the store; cross-references
are noted. All strings, JSON keys, numeric constants, file paths, and error
messages below are exact and must be reproduced verbatim in the Go port.

---

## 1. Backup directory layout

### 1.1 Backup root resolution (`paths.get_backup_root`)

| Platform | Backup root |
|---|---|
| Linux / WSL | `$XDG_DATA_HOME/claude-swap` if `$XDG_DATA_HOME` is set, non-empty, and (after `~` expansion) **absolute**; otherwise `~/.local/share/claude-swap` |
| macOS / Windows / UNKNOWN | `~/.claude-swap-backup` (the "legacy" layout) |

- `Platform.detect()` uses `sys.platform`: `darwin`→MACOS, `win32`→WINDOWS,
  `linux*`→WSL if env `WSL_DISTRO_NAME` is set else LINUX, else UNKNOWN.
- `$XDG_DATA_HOME` is `os.path.expanduser`-expanded first (so `~/data` from a
  systemd unit / Dockerfile works), then required to be absolute; a
  relative/empty value is ignored per the XDG spec.
- `LEGACY_BACKUP_DIRNAME = ".claude-swap-backup"`. `get_legacy_backup_root()` =
  `~/.claude-swap-backup`.

### 1.2 Files and subdirectories under the backup root

Set in `ClaudeAccountSwitcher.__init__`:

| Path | Constant / attribute | Purpose |
|---|---|---|
| `<backup>/sequence.json` | `self.sequence_file` | The account table (see §2). |
| `<backup>/configs/` | `self.configs_dir` | Per-account `~/.claude.json` snapshots. |
| `<backup>/credentials/` | `self.credentials_dir` | Per-account credential `.enc` files, `.prev`, swap staging, unclaimed stash. |
| `<backup>/.lock` | `self.lock_file` | The cross-process account lock (`FileLock`, §10.1). |
| `<backup>/cache/` | `UsageStore(self.backup_dir / "cache")` | Usage cache (`cache/usage.json` etc). Out of scope; treated as "throwaway" by migration. |
| `<backup>/sessions/<num>-<slug>/` | `session.session_dir_for` | Per-account session profile dir (§7). |
| `<backup>/claude-swap.log*` | logger | Log output. Treated as throwaway by migration. |

Per-account file names (email is embedded **raw**, unslugified):

| File | Format string |
|---|---|
| Config backup | `configs/.claude-config-{account_num}-{email}.json` |
| Credential backup (base64) | `credentials/.creds-{account_num}-{email}.enc` |
| Retained previous credential | `credentials/.creds-{account_num}-{email}.enc.prev` |
| Swap staging (same-email swap only) | `credentials/.swap-staging-{kind}-{num}.json` where `kind ∈ {creds, config}` |
| Unclaimed stash manifest | `credentials/.unclaimed-manifest.json` |
| Unclaimed stash entry | `credentials/.unclaimed-{entry_id}.enc` |

macOS Keychain item names (`security` generic-password items):

| Service (constant) | Account (username) | Holds |
|---|---|---|
| `claude-swap` (`SECURITY_SERVICE`) | `account-{account_num}-{email}` | per-account backup credential |
| `claude-swap` | `account-{account_num}-{email}.prev` | retained previous backup |
| `Claude Code-credentials` (`CLAUDE_CODE_KEYCHAIN_SERVICE`) | `keychain_account_name()` | Claude Code's **active** OAuth credential |
| `Claude Code` (`CLAUDE_CODE_MANAGED_KEYCHAIN_SERVICE`) | `keychain_account_name()` | Claude Code's **active** managed API key |
| `claude-code` (`KEYRING_SERVICE`) | `account-{num}-{email}` | LEGACY keyring backups (purge sweep only) |

- `keychain_account_name()` mirrors Claude Code's `getUsername()`: env `$USER`
  first, else the POSIX username (`pwd.getpwuid(os.geteuid()).pw_name`), else the
  literal `"claude-code-user"`. Matching this exactly matters on
  headless/launchd/cron hosts.

### 1.3 Directory creation & permissions (`_setup_directories`)

- Creates `backup_dir`, `configs_dir`, `credentials_dir` (via
  `mkdir(parents=True, exist_ok=True)`). Does **not** create `sessions/` or
  `cache/` (created lazily by their owners).
- On non-`win32`, `chmod 0o700` each of the three dirs.
- Every credential/config file written by the store is `chmod 0o600` on
  non-`win32` (via a temp file that is chmod'd before the atomic rename).

### 1.4 Legacy backup migration (`paths.migrate_legacy_backup_dir`)

Run in `__init__` **before** any logger/dir setup, so Linux/WSL installs move
`~/.claude-swap-backup` → the XDG path once. No-op on macOS/Windows (roots are
equal). On a real move it prints to **stderr**:
`claude-swap: migrated data from {legacy} to {backup_dir}`.

Protocol (flag file `<target.parent>/.<target.name>.migrating`):

- `legacy` and `target` resolve to the same path → return False (no-op).
- `legacy` absent → unlink a stale flag, return False.
- Flag present + legacy present → interrupted prior run: `rmtree` any partial
  target, retry the move.
- No flag + target exists + target has **meaningful data** → raise
  `MigrationError` (surfaces as `ClaudeSwitchError`): "Both legacy (…) and new
  (…) backup paths exist. Refusing to merge or overwrite — inspect both and
  remove the stale one manually before re-running."
- No flag + target exists but only **throwaway** artifacts → wipe them and
  migrate. Throwaway = names in `{"cache"}` or prefixed by `claude-swap.log`.
- The move uses `shutil.move` (atomic rename same-FS; copy+unlink cross-FS)
  bracketed by `flag.touch()` … `flag.unlink()`. Any `OSError` → `MigrationError`
  "Migration of {legacy} → {target} failed: {exc}".

---

## 2. Account metadata format (`sequence.json`)

### 2.1 Top-level schema

```json
{
  "activeAccountNumber": 1,
  "lastUpdated": "2024-01-01T00:00:00Z",
  "sequence": [1, 2],
  "accounts": {
    "1": { "...account record..." }
  }
}
```

- `activeAccountNumber`: integer slot number, or `null`. This is cswap's
  *recorded* active slot; the *live* active slot is derived from
  `~/.claude.json` (§5.4). Written as an **int** (`int(account_num)`).
- `lastUpdated`: UTC timestamp, format `%Y-%m-%dT%H:%M:%SZ` (`get_timestamp()`).
  Rewritten on every mutation.
- `sequence`: list of **int** slot numbers, kept **sorted ascending** everywhere
  (every insert does `.append` then `.sort()`). This is rotation/list order.
- `accounts`: map of **string** slot number → account record. Keys are strings
  (`"1"`), values below.

Initial file (`_init_sequence_file`, only written if the file does not exist):

```json
{ "activeAccountNumber": null, "lastUpdated": "<ts>", "sequence": [], "accounts": {} }
```

### 2.2 Account record schema

```json
{
  "email": "user@example.com",
  "uuid": "account-uuid-or-empty",
  "organizationUuid": "org-uuid-or-empty",
  "organizationName": "Acme Corp",
  "added": "2024-01-01T00:00:00Z",
  "alias": "dev",
  "kind": "api_key",
  "disabled": true
}
```

Field semantics:

| Key | Type | Required | Notes |
|---|---|---|---|
| `email` | str | yes | From `oauthAccount.emailAddress`, or a synthesized `…@token.local` for token accounts. Half the composite identity. |
| `uuid` | str | yes | `oauthAccount.accountUuid`; **`""`** for `add-token` accounts (empty placeholder). Never rewritten once non-empty except via `backfill_account_uuid` which only fills an empty one. |
| `organizationUuid` | str | yes (post-migration) | `""` for personal accounts. The other half of the composite identity. Absent on pre-v0.6.0 records → triggers migration (§9). |
| `organizationName` | str | yes (post-migration) | `""` for personal. Display only. |
| `added` | str | yes | Timestamp when first added; not updated on refresh-in-place. |
| `alias` | str | **optional** | Present only when set. Lowercased, validated (§8.1). Absent (key deleted) when unset. |
| `kind` | str | **optional** | Present and equal to `"api_key"` **only** for managed-API-key accounts. Absent ⇒ treated as `"oauth"` (back-compat; setup-tokens are also `"oauth"`). |
| `disabled` | bool | **optional** | Present and `true` only when the slot is held out of auto-rotation. `pop`'d entirely when re-enabled. |

Additivity rule: `alias`, `kind`, `disabled` are **absence-signals** — never
written as `""`/`false`/`"oauth"`. A Go port must omit them, not zero-fill them,
or duplicate-detection and back-compat reads break.

**Composite identity key** = `(email, organizationUuid)`. Two accounts may share
an email if their `organizationUuid` differs. `_find_account_slot(data, email,
org_uuid)` returns the first slot whose record matches both
(`account.get("organizationUuid", "") == organization_uuid`). This is why a
missing `organizationUuid` field compares equal to `""`.

### 2.3 JSON write discipline (`_write_json`)

1. `content = json.dumps(data, indent=2)` (2-space indent, default separators).
2. Write to temp path `path.with_suffix(f".{os.getpid()}.tmp")`.
3. Re-read + `json.loads` the temp file; on `JSONDecodeError` unlink it and raise
   `ConfigError("Generated invalid JSON")`.
4. On non-`win32`, `chmod 0o600` the **temp** file (so the rename is the atomic
   commit; a chmod on the final path could fail post-publish).
5. `shutil.move(temp, path)`.

`_read_json`: returns `None` if the file doesn't exist; on
`JSONDecodeError`/`UnicodeDecodeError` logs `Invalid JSON in {path}` (warning) and
returns `None`.

---

## 3. Credential storage backends (`credentials.py` `CredentialStore`)

The switcher delegates all credential I/O to `CredentialStore` via thin proxy
methods. Two axes: the **active** credential (Claude Code's own store) and the
**per-account backup** credential (cswap's store). This mandate is the backup
store plus enough of the active store to serve add/switch.

### 3.1 Backup backend routing

- **Linux / WSL / Windows / UNKNOWN**: backup credentials are always base64
  `.enc` files under `credentials_dir`. (Windows moved off Credential Manager
  because it rejects entries >~2500 bytes.)
- **macOS**: backup writes go to the Keychain (`SECURITY_SERVICE`) while it is
  usable; fall back to `.enc` files when it isn't (headless/SSH/locked).
- Backup **reads are `.enc`-wins on every platform**: a fallback `.enc` beats a
  possibly-stale Keychain copy.

Keychain usability cache (per-process, on the store):

- `_keychain_usable_cache`: `None` (unprobed) → `True`/`False` after the first
  real `security` call. Flips `None→True` on success, `→False` on
  `KEYCHAIN_ERRORS`; **never** `False→True` within a process except via cooldown.
- `KEYCHAIN_ERRORS = (KeychainError, subprocess.TimeoutExpired, OSError)` — only
  these three are treated as "Keychain unusable"; anything else propagates.
- On failure, `_keychain_disabled_until = time.monotonic() +
  KEYCHAIN_RECHECK_COOLDOWN_S` (`60.0`s). `_use_keychain()` re-probes (sets cache
  back to `None`) once monotonic time passes that deadline — so a sub-second CLI
  never re-probes (no backend split-brain) but a long-running daemon self-heals.
- `_pin_file_mode()` sets cache `False` and deadline `0.0` (sticky, no re-probe)
  — used after an active-credential **write** falls back to file, because a
  best-effort Keychain-delete may have failed and re-probing could read the
  residual.
- `item_exists` is deliberately **not** routed through the usability cache
  (returns False for both "absent" and "failed").

### 3.2 Backup write (`_write_account_credentials` in the store)

Pure I/O; the **switcher** wrapper runs `_post_backup_write` (session
invalidation) exactly once, only after a successful store write.

1. `_retain_previous_backup`: read the current backup; if it is non-empty **and**
   differs from the new value, copy it to `.prev` (Keychain when in use, else
   `.enc.prev`). Best-effort. A same-value rewrite does **not** clobber `.prev`.
2. macOS + Keychain usable: `set_password(SECURITY_SERVICE, account-{num}-{email},
   creds)`. On success, `_reconcile_enc_after_keychain_write`: delete the `.enc`;
   if delete fails, rewrite the `.enc` with the fresh creds; if that also fails,
   **raise** (never serve stale). Return.
   On `KEYCHAIN_ERRORS`: log warning, fall through to file mode.
3. File mode: atomically write the `.enc` (base64-encode the UTF-8 bytes, temp
   file + `os.replace`, chmod 0600). On write failure log + **raise**. On macOS,
   best-effort delete the stale Keychain copy afterward.

### 3.3 Backup read (`_read_account_credentials`)

1. If the `.enc` exists (guarding `OSError` from `exists()` on an unsearchable
   dir → treated as missing): read + strip; `base64.b64decode(..., validate=True)`
   + UTF-8 decode. If it decodes to a **non-empty** string, return it. Corrupt
   (`validate=True` rejects `"!!!!"`), empty, or whitespace → fall through.
2. macOS: `get_password(SECURITY_SERVICE, account-{num}-{email})` (via `_kc_call`,
   which updates the usability cache). `KEYCHAIN_ERRORS` → log + `""`.
3. Otherwise `""`.

Reading a healthy-Keychain backup must **not** materialize an `.enc` file.

### 3.4 Backup delete (`_delete_account_credentials`)

- `nums = [account_num]`; append `"None"` if `account_num != "None"` (sweeps the
  legacy `account-None-{email}` alias left by old buggy runs).
- For each num: unlink the `.enc` (best-effort, logged), macOS
  `_delete_backup_keychain_quiet` (best-effort), and `delete_previous_backup`
  (drops `.prev` file + `.prev` Keychain item).

`delete_account_credentials_strict(num, email)` (used by swap/move pre-commit
clears): best-effort sweep first (as above), then **unconditionally**
`.enc.unlink(missing_ok=True)` + macOS `_kc_delete_backup` with errors
**propagating** as `CredentialError("Could not clear stored credentials for slot
{num} ({email}) — aborting before commit: {e}")`; finally a read-back — if it
still reads non-empty, raise the same `CredentialError` (no `: {e}` suffix).
Absence counts as success on both backends (missing `.enc`; Keychain rc 44). The
Keychain delete runs even when routing says file mode.

### 3.5 `.prev` retention

- One generation per slot, routed by the same rule as the backup itself
  (Keychain item `account-{num}-{email}.prev` when Keychain in use, else
  `.enc.prev` file). Best-effort.
- `_read_previous_backup`: `.enc.prev`-wins, same decode rules as §3.3.
- `delete_previous_backup` is also called on its own after a renumber (swap/move)
  writes another account's material through a key, so recovery can't resurrect a
  displaced generation onto the key's new owner.

### 3.6 Unclaimed-credentials stash (write-only forensic store)

Created only when a **switch** displaces live credential bytes it positively
attributes to someone other than the outgoing slot. Not consumed by any lifecycle
op here, but the store format matters:

- Files are **0600 base64 files on every platform** (never Keychain), so a stash
  failure — which aborts a switch by design — can't inherit Keychain flakiness.
- Manifest `credentials/.unclaimed-manifest.json`:
  `{"schemaVersion": 1, "entries": {<id>: {"createdAt": "<ISO Z>", ...context}}}`.
- Entry files `credentials/.unclaimed-{entry_id}.enc`; entry id =
  `{YYYYMMDDTHHMMSS}-{sha256(creds)[:12]}-{secrets.token_hex(3)}`.
- `list_unclaimed_credentials()` merges manifest rows with orphan `.enc` files
  (glob `.unclaimed-*.enc`), defaulting orphans to `{"createdAt": None}`. Surfaced
  only in `--list --json` (`unclaimedCredentials`, sorted) and logs — never in the
  human list.

### 3.7 API-key detection & Claude Code active-store format (external knowledge)

- `looks_like_api_key(creds)`: `True` iff the stripped string starts with
  `sk-ant-api` **and not** with `{`. A JSON blob that merely *contains* a key, or
  a raw `sk-ant-oat…` setup token, is **not** an API key.
- `approved_form(api_key)` = `api_key.strip()[-20:]` — mirrors Claude Code's
  `normalizeApiKeyForConfig` (`apiKey.slice(-20)`). Stored in
  `customApiKeyResponses.approved` so Claude Code's "is this key approved?" check
  passes without re-prompting.
- Active OAuth credential lives in Keychain `Claude Code-credentials` (macOS) or
  the plaintext `<config_home>/.credentials.json` (Claude Code's own fallback,
  every platform).
- Active managed API key lives in Keychain `Claude Code` (macOS) or
  `~/.claude.json`'s `primaryApiKey` string (mirrors
  `getApiKeyFromConfigOrMacOSKeychain`).
- OAuth and API-key are **mutually exclusive** auth axes; activating one clears
  the other (mirrors Claude Code `saveApiKey`/`removeApiKey`). Relevant to add
  because `_read_active_credentials` reads OAuth fully first (so a Keychain-empty
  OAuth-file login is never misread as an API key).

---

## 4. Claude Code file paths cross-reference (`paths.py`)

Used by add/refresh to snapshot the live account:

- **Config home** (`get_claude_config_home`): env `CLAUDE_CONFIG_DIR` if set, else
  `~/.claude`.
- **Global config** (`get_global_config_path`, i.e.
  `_get_claude_config_path()`): if `<config_home>/.config.json` exists (legacy)
  use it; else `(CLAUDE_CONFIG_DIR || $HOME)/.claude.json`. Note the asymmetry:
  `.claude.json` sits at **home dir** by default, not inside `.claude/`.
- **Credentials file** (`get_credentials_path`):
  `<config_home>/.credentials.json`.

`~/.claude.json` `oauthAccount` object fields cswap reads: `emailAddress`,
`accountUuid`, `organizationUuid`, `organizationName` (and, elsewhere,
`organizationRole`, `displayName`).

---

## 5. Account lifecycle: `add` (`add_account`)

Signature: `add_account(slot: int | None = None, assume_yes: bool = False,
alias: str | None = None)`.

Preamble (always): `_setup_directories()`, `_init_sequence_file()`,
`_migrate_org_fields()`. Then, if `alias is not None`, `normalize_alias(alias)`
(ValidationError on failure — the raised `ValueError` message is wrapped).

Read the live identity: `identity = _get_current_account()` = `(email,
organization_uuid)` from `~/.claude.json` `oauthAccount` (email required, else
`None`). If `None` → `ConfigError("No active Claude account found. Please log in
first.")`.

### 5.1 Refresh-in-place (no `slot`, identity already managed)

Condition: `slot is None and _account_exists(email, org_uuid)`.

1. Resolve `account_num` via `_find_account_slot`.
2. If `alias is not None`, check `_alias_in_use(alias, exclude_num=account_num)`;
   conflict → `ValidationError("Alias '{alias}' is already used by account
   {conflict}")`.
3. `current_creds = _read_credentials()`. `None` →
   `CredentialReadError("Failed to read credentials for current account")`; `""`
   → `CredentialReadError("No credentials found for current account")`.
4. `_reject_live_api_key_capture(current_creds)`: if `looks_like_api_key` →
   `ValidationError("Active login is an API-key account. Add it with 'cswap
   --add-token sk-ant-api...' instead of --add-account.")`.
5. Read live config text (`config_path.read_text`). `FileNotFoundError` →
   `ConfigError("Claude config file not found")`; `PermissionError` →
   `ConfigError("Permission denied reading Claude config")`.
6. `_write_account_credentials(account_num, email, current_creds)`;
   `_write_account_config(account_num, email, current_config)`.
7. `_usage_store.clear_dead_token([account_num], {account_num: (email,
   org_uuid)})` — lifts any dead-token quarantine so the refreshed credential is
   re-polled.
8. If `alias is not None`, set `record["alias"] = alias`.
9. `activeAccountNumber = int(account_num)`, `lastUpdated` bumped, write.
10. Print `Updated credentials for Account {account_num} ({email} [{tag}]).`
    (log `Updated credentials for account {account_num}: {email}`). Return.

`tag` = `_get_display_tag(email, org_name, org_uuid)` = `org_name` if truthy else
`"personal"`.

### 5.2 New/slotted add

Slot decision & confirmation are collected **before** any destructive op (new
account must be verified readable first).

- `displace_slot`: set when an explicit `slot` is occupied by a **different**
  account (different composite identity) → after confirmation, that slot's files
  are deleted and its mappings pruned.
- `migrate_from`: set when the same identity already lives in a **different**
  slot → that old slot is cleaned up (the account moves).

If `slot is not None`:
- `slot < 1` → `ConfigError("Slot number must be >= 1")`.
- `account_num = str(slot)`.
- If identity exists elsewhere (`old_num != account_num`), `migrate_from =
  old_num`.
- If `account_num` occupied by a different identity: `warning("Slot {slot}
  already occupied")`, print `{existing_email} [{existing_tag}]`; unless
  `assume_yes`, prompt `Overwrite slot {slot}? [y/N] `. On `EOF`/`KeyboardInterrupt`
  print `\nCancelled` and **return**; answer not in `{"y","yes"}` → print
  `Cancelled`, return. Then `displace_slot = (account_num, existing_email,
  existing_org)`.

Else (`slot is None`): `account_num = str(_get_next_account_number())` =
`max(existing int keys, default 0) + 1` (or `1` when empty).

Alias carry-forward (`existing_alias`): if the prior record in the target slot is
the same identity, take its `alias`; if `migrate_from`, take that record's alias
(preferring migrate_from's). Then, if `alias is not None`, re-check
`_alias_in_use(alias, exclude_num=account_num)` → ValidationError on conflict.

Read new credentials **before** destructive ops: `_read_credentials()` (None/""
→ same `CredentialReadError`s), `_reject_live_api_key_capture`, live config text
(same `ConfigError`s), and parse `oauthAccount` for `accountUuid`,
`organizationUuid`, `organizationName`.

Destructive cleanup (now safe):
- `displace_slot`: `_delete_account_files(d_num, d_email)`; remove `int(d_num)`
  from `sequence`; `del accounts[d_num]`; write; `_prune_mappings(d_email,
  d_org)`.
- `migrate_from`: `_delete_account_files(migrate_from, old_email)`; remove from
  `sequence`; `del accounts[migrate_from]`; write. (Mappings kept — same
  identity.)

Store & record:
- `_write_account_credentials(account_num, email, current_creds)`,
  `_write_account_config(account_num, email, current_config)`,
  `_usage_store.clear_dead_token([account_num], {account_num: (email, org_uuid)})`.
- New record:
  ```json
  { "email": email, "uuid": account_uuid, "organizationUuid": org_uuid,
    "organizationName": org_name, "added": "<ts>" }
  ```
  Then `carried_alias = alias if alias is not None else existing_alias`; if truthy
  set `record["alias"]`.
- Append `int(account_num)` to `sequence` if absent, then sort.
- `activeAccountNumber = int(account_num)`, bump `lastUpdated`, write.
- Log `Added account {account_num}: {email} (org: {org_uuid or 'personal'})`.
- If `migrate_from`: print `Moved from slot {migrate_from} → {slot}` (dimmed).
- Print `Added Account {account_num}: {email} [{tag}]`.

### 5.3 First-run setup (`_first_run_setup`, entered from `list` when no
sequence file)

Interactive: if no live identity → `No active Claude account found. Please log in
first.`; else prompt `No managed accounts found. Add current account ({email}) to
managed list? [Y/n] `. Response `"n"` → `Setup cancelled. You can run 'cswap
--add-account' later.`; otherwise call `add_account()`.

### 5.4 Live-identity helper

`_get_current_account()` reads `~/.claude.json`; returns `None` if the file is
absent/unparseable or `oauthAccount.emailAddress` is empty; otherwise `(email,
organizationUuid or "")`.

---

## 6. Account lifecycle: `add-token` (`add_account_from_token`)

Signature: `add_account_from_token(token, email=None, slot=None,
assume_yes=False)`. Registers a raw OAuth **setup-token** or a managed
**`sk-ant-api…` API key** as a new account with no Anthropic API calls.

### 6.1 Token acquisition & validation

- `token == "-"` → read one line from stdin: `sys.stdin.readline().rstrip("\n")`.
- `token == ""` (falsy) → `getpass.getpass("Token: ")`.
- `token = token.strip()`; empty → `ValidationError("Token cannot be empty")`.
- `is_api_key = looks_like_api_key(token)`.
- If `email` given and `not _validate_email(email)` →
  `ValidationError("Invalid email format: {email}")`. Email regex:
  `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`.

Preamble: `_setup_directories()`, `_init_sequence_file()`, `_migrate_org_fields()`.

### 6.2 Email defaulting

If `email` is falsy: if `slot is None`, `slot = _get_next_account_number()`
(**note: this mutates `slot` for the rest of the function**); `label = "api-key"
if is_api_key else "setup-token"`; `email = f"{label}-{slot}@token.local"`. The
slot number guarantees uniqueness of the placeholder.

### 6.3 Cross-kind collision guard

`_reject_cross_kind_collision(email, is_api_key)`: matches identity `(email, "")`
(token accounts are always personal). If a slot with that identity exists as the
**other** kind → `ValidationError("'{email}' already exists as an {existing_label}
account (slot {slot}); cannot add it as an {new_label} account. Pass a distinct
--email.")` where labels are `API-key` / `OAuth`.

### 6.4 Credential & config payloads

- API key: credential is the **raw token** stored verbatim.
- Setup-token: credential is
  ```json
  {"claudeAiOauth": {"accessToken": "<token>", "scopes": ["user:inference"]}}
  ```
  (`SETUP_TOKEN_SCOPES = ("user:inference",)`; serialized as a JSON list).
- Config (identical for both kinds — no real org metadata):
  ```json
  {"oauthAccount": {"emailAddress": "<email>", "accountUuid": "",
    "organizationUuid": null, "organizationName": null}}
  ```
  Note `organizationUuid`/`organizationName` are JSON **`null`** here (the config
  blob), while the sequence.json record stores `""` (see §6.6).

### 6.5 Refresh-in-place (no slot, identity `(email,"")` exists)

Condition: `slot is None and _account_exists(email, "")`. Resolve `account_num`;
if `None` (corrupt lookup) → `ConfigError("Existing account metadata for {email}
is inconsistent")` and nothing is written. Otherwise write creds+config,
`clear_dead_token([account_num], {account_num: (email, "")})`, bump `lastUpdated`,
write, print `Updated {kind_label} for Account {account_num} ({email}
[personal]).` (`kind_label = "API key" | "token"`). Return.

### 6.6 New/slotted add

Same displace/migrate logic as §5.2 but identity is always `(email, "")`:
- `slot < 1` → `ConfigError("Slot number must be >= 1")`.
- Occupied-by-different prompt: identical `Overwrite slot {slot}? [y/N] ` flow.
- Else `account_num = str(_get_next_account_number())`.

Store creds+config, `clear_dead_token`. Record:
```json
{ "email": email, "uuid": "", "organizationUuid": "", "organizationName": "",
  "added": "<ts>" }
```
If `is_api_key`, add `record["kind"] = "api_key"` (setup-tokens get **no** kind
key). Append to `sequence` (sorted), bump `lastUpdated`, write.
- Log `Added account {account_num} from {source_label}: {email}` (`source_label =
  "API key" | "token"`).
- If `migrate_from`: print `Moved from slot {migrate_from} → {slot}`.
- Print `Added Account {account_num}: {email} [personal] (from {source_label})`.

`add-token` does **not** set aliases (no `alias` parameter).

---

## 7. Session profile interaction (constrains destructive ops)

Session profiles live at `sessions/{account_num}-{slugify_email(email)}/`.
`slugify_email` NFC-normalizes then replaces any char that isn't ASCII
alphanumeric or `._-` with `_`.

Every op that removes or relocates a slot goes through `_delete_account_files` /
`_ensure_no_live_session`, which **refuse while a session-mode `cswap run`
process is live** against that slot:

- `_ensure_no_live_session(num, email, action)` → `SessionError("Account-{num}
  ({email}) has a live session-mode Claude instance (PID {pids}). Exit it first,
  then retry {action}.")`. `action` strings: `"the operation"` (from
  `_delete_account_files`), `"--remove-account"`, `"--swap-accounts"`,
  `"--move-account"`.
- `_delete_account_files(num, email)`: ensure-no-live-session → delete creds →
  unlink config file → `_delete_session_profile` (deletes the profile dir and its
  macOS keychain entry; keychain first, since its hashed service name derives from
  the dir path).
- `_post_backup_write(num, email)` (run after every successful backup write via
  the switcher wrapper): if a live session exists, `mark_session_stale(...)`; else
  `_invalidate_session_credentials(...)` (drops the profile's
  `.credentials.json`, `STALE_MARKER` = `.cswap-stale-credentials`, and macOS
  keychain entry, keeping history).

Add-token/API-key accounts are rejected by session mode entirely
(`SessionError("... does not support API-key accounts")`) — enforced in
`session.py`, not here.

---

## 8. Alias, resolution, disable/enable

### 8.1 Alias validation (`models.normalize_alias`)

- `normalized = name.strip().lower()`.
- Empty → `ValueError("alias cannot be empty")`.
- Purely numeric → `ValueError("alias '{name}' cannot be purely numeric (reserved
  for slot numbers)")`.
- Leading `-` → `ValueError("alias '{name}' cannot start with '-' (would be read
  as a command flag)")`.
- Must match `^[a-z0-9_.-]+$` (regex `_ALIAS_RE`) else `ValueError("alias '{name}'
  may only contain letters, digits, '-', '_', and '.'")`.
- Valid examples: `dev`, `work-1`, `client_a`, `team.b`, `DEV` (→ `dev`).
- Invalid: `123`, `dev@work`, `dev work`, ``, `dev/work`, `-dev`.
- Aliases are stored lowercased; matched case-insensitively.

### 8.2 Identifier resolution (`_resolve_account_identifier`)

Precedence: **number → alias → email**.

- `identifier.isdigit()` → return it unchanged (as a string) — number always wins,
  even over an alias.
- Else `_find_account_by_alias(identifier)` (case-insensitive; empty never
  matches) → return that slot.
- Else match records by exact `email`. 0 matches → `None`; 1 → that slot; ≥2 →
  `ConfigError("Email '{identifier}' is ambiguous — matches accounts: {details}.
  Use account number instead (e.g., cswap --switch-to 1).")` where `details` is
  `{num} [{organizationName or 'personal'}]` comma-joined.

`resolve_account(identifier)` (public, for map/run/disable): migrates org fields,
resolves, and returns `(account_num, email, organizationUuid)`; unknown →
`AccountNotFoundError("No account found with identifier: {identifier}")`; missing
record → `AccountNotFoundError("Account-{num} does not exist")`. Ambiguity is a
hard `ConfigError` (no interactive prompt).

### 8.3 `alias` command

- `set_alias(identifier, alias)` → `(account_num, normalized)`. Normalizes
  (ValidationError on bad), resolves identifier (which may itself be an existing
  alias — supports rename), conflict-check `_alias_in_use(normalized,
  exclude_num=account_num)` → `ConfigError("Alias '{normalized}' is already used by
  account {conflict}")`. Sets `record["alias"]`, bumps `lastUpdated`, writes.
- `unset_alias(identifier)` → `account_num`. **Idempotent**: if `"alias"` key
  absent, succeeds silently without writing. Deletes the key otherwise.
- `list_aliases()` → `[(account_num, alias, email)]` for records with a truthy
  alias, **sorted by `int(account_num)`**.
- All three: no sequence file check beyond `_get_sequence_data_migrated`; unknown
  identifier → `AccountNotFoundError`.

### 8.4 disable / enable (`set_account_disabled(identifier, disabled)`)

- Requires the sequence file: else `ConfigError("No accounts are managed yet")`.
- Resolves via `resolve_account` (hard error on ambiguity, migrates org fields).
- No-op if already in the requested state: print `Account-{num} ({email}) is
  already {verb}.` (`verb = "disabled" | "enabled"`), return without writing.
- Mutate: `disabled=True` sets `record["disabled"] = True`; `disabled=False` does
  `record.pop("disabled", None)`. Bump `lastUpdated`, write. Log
  `{Verb} account {num}: {email}`.
- Print `{Verb} Account-{num} ({email}).` (accent on the capitalized verb).
- If disabling and it is the active slot (`str(active) == account_num`): print
  `  It is the active account — it stays live until you switch away; it just
  won't be an automatic switch target.`
- If disabling and `switchable_account_numbers()` is now empty: `warning("  No
  accounts remain in rotation — auto-switch and bare switch have nothing to pick.
  Re-enable one with cswap enable <num|email>.")`.
- If enabling: print `  It is back in the rotation.`

Disabled semantics (constraining rotation/strategies — read-only here):
- `is_account_disabled(num)`, `disabled_account_numbers()` (sequence order),
  `switchable_account_numbers()` = sequence slots that are switchable **and not
  disabled**.
- `_disabled_from_data(data, num)` = `bool(record and record.get("disabled"))`.
- Disabled slots stay managed and remain valid **explicit** `switch <num|email>`
  targets; only auto-selection, bare-`switch` rotation, and `best`/`next-available`
  skip them. Re-enabling restores the original sequence position (the number never
  moved). Removing & re-adding a slot clears the flag (fresh record).

### 8.5 `_account_is_switchable(num)` and `_account_kind`

- Switchable iff the slot's record exists **and** it has both a non-empty stored
  credential backup and a non-empty stored config backup. Tolerates stale
  sequence entries pointing at a removed record (returns False).
- `_account_kind(num)`: `"api_key"` iff `record.get("kind") == "api_key"`, else
  `"oauth"` (including missing record / `None` slot). Public: `account_kind_for`,
  `account_email`, `account_identity` (returns `{"email","organizationUuid","uuid"}`).

---

## 9. Org-field migration (`_migrate_org_fields` / `_get_sequence_data_migrated`)

Back-compat for pre-v0.6.0 records lacking `organizationUuid`. Triggered lazily
whenever any record is missing that key.

- For the **active** account (email matches live `~/.claude.json`): take
  `organizationUuid`/`organizationName` from the live config (authoritative).
- For inactive accounts: parse the account's backup config
  (`configs/.claude-config-{num}-{email}.json`) `oauthAccount` fields; on absence
  or parse error, set both to `""`.
- If anything changed, bump `lastUpdated` and write. Idempotent; skips records
  already carrying `organizationUuid`.

`add_account` and `add_account_from_token` call `_migrate_org_fields()` in their
preamble; `remove`, `swap`, `move`, `alias`, `disable/enable` call
`_get_sequence_data_migrated()`.

---

## 10. `remove`, `move`, `swap` (relocation semantics)

### 10.1 The account lock

`FileLock(self.lock_file)` — a cross-process advisory lock on `<backup>/.lock`
(POSIX `fcntl.flock LOCK_EX|LOCK_NB`, Windows `msvcrt.locking`; poll every 0.1s;
default timeout **10.0s**; timeout returns False → `LockError`). **Non-reentrant**
within a process — never nest acquisitions. `swap`, `move`, `persist_backup_credentials`,
and the usage-refresh persist all take it; the whole resolve-validate-mutate span
of swap/move runs under one acquisition (a slot number resolved outside the lock
could be renumbered by a concurrent swap/move).

### 10.2 `remove_account(identifier, assume_yes=False)`

- No sequence file → `ConfigError("No accounts are managed yet")`.
- `_get_sequence_data_migrated()`.
- Identifier gate: if not a digit, it must be a known alias **or** a
  format-valid email (`_validate_email`), else `ValidationError("Invalid account
  identifier: {identifier}")`. For a non-alias email with **multiple** matches:
  print `Multiple accounts found for '{identifier}':` and each `  {num}:
  {identifier} [{tag}]`, then prompt `Enter account number to remove: `; a
  non-digit or out-of-set choice → `Cancelled` (return); otherwise `identifier`
  becomes the chosen number.
- Resolve; unknown → `AccountNotFoundError("No account found with identifier:
  {identifier}")`; missing record → `AccountNotFoundError("Account-{num} does not
  exist")`.
- `_ensure_no_live_session(num, email, "--remove-account")` **before** the prompt.
- If the slot is active: `warning("Warning: Account-{num} ({email}) is currently
  active")`.
- Unless `assume_yes`: prompt `Are you sure you want to permanently remove
  Account-{num} ({email})? [y/N] `; any answer whose lower() != `"y"` → `Cancelled`
  and return.
- `_delete_account_files(num, email)`; `del accounts[num]`; `sequence = [n for n
  in sequence if n != int(num)]`; bump `lastUpdated`; write. Log `Removed account
  {num}: {email}`. Print `Removed Account-{num} ({email})`.
- `_prune_mappings(email, organizationUuid)` — drops directory mappings for the
  now-gone identity, printing `Removed {n} directory mapping(s) for this account`
  if any.

### 10.3 `move_account(account, target) -> (source_num, target_num, swapped)`

The general form of swap. `account` = any `NUM|EMAIL|ALIAS`; `target` = a
destination **slot number**.

- No sequence file → `ConfigError("No accounts are managed yet")`.
- `target = target.strip()`; must be `isdigit()` and `>= 1`, else
  `ValidationError("Target slot must be a positive slot number, got: {target!r}
  (use `swap` to trade two accounts by identifier)")`. Normalize `"01"→"1"` via
  `str(int(target))`.
- All work under one `FileLock`:
  - Migrate; resolve `account` → `num_src`; unknown → `AccountNotFoundError`;
    missing record → `AccountNotFoundError("Account-{num_src} does not exist")`.
  - Cap check: `max_slot = max(int(n) for digit keys, default 0)`, `cap =
    max(99, max_slot)`. `int(target) > cap` → `ValidationError("Target slot
    {target} is out of range (1-{cap}): new accounts are numbered from the highest
    slot, so a large target would inflate future account numbers")`.
  - `num_src == target` → no-op, return `(num_src, target, False)`.
  - Target occupied → `_swap_accounts_locked(num_src, target)`, return
    `(num_src, target, True)`.
  - Target empty → `_relocate_locked(num_src, target)`, return `(num_src, target,
    False)`.

### 10.4 `_relocate_locked(num_src, target)` (one-way move to empty slot)

Re-checks the record exists (`AccountNotFoundError`) and target is unoccupied
(`ValidationError("Slot {target} is already occupied — retry the move")`).
`_ensure_no_live_session(num_src, email, "--move-account")`. Reads
creds+config up front (missing → `""`). Then:

1. Best-effort move the session profile `sessions/{src}-{slug}` →
   `sessions/{target}-{slug}` (only if src exists and dst doesn't).
2. **Write-or-clear** the target key: if `creds` → `_write_account_credentials`,
   else `delete_account_credentials_strict` (fails-closed, aborts on
   inability to clear a stale key — see §3.4); if `config` →
   `_write_account_config`, else `_delete_config_backup`.
3. `accounts[target] = record`; `del accounts[num_src]`; renumber `sequence`
   (`int_src`→`int_target`) then **sort**; if `activeAccountNumber == int_src` set
   it to `int_target`; bump `lastUpdated`; **write (commit point)**.
4. On any pre-commit exception: best-effort clear the target key
   (`_delete_account_credentials`, `_delete_config_backup`) and move the session
   profile back; re-raise. Records still point at `num_src`, which is untouched.
5. Post-commit: `_delete_account_files(num_src, email)` (best-effort, logged
   loudly on failure — a stray under the freed number would poison a future
   same-email account). If `creds`, `delete_previous_backup(target, email)`. Log
   `Moved slot: {num_src} ({email}) -> {target}`.

### 10.5 `swap_accounts(first, second) -> (num_a, num_b)`

- No sequence file → `ConfigError("No accounts are managed yet")`. All under one
  `FileLock` → `_swap_accounts_locked`.
- Resolve both; unknown → `AccountNotFoundError("No account found with
  identifier: {first|second}")`; `num_a == num_b` → `ValidationError("Cannot swap
  an account with itself")`; missing records → `AccountNotFoundError("Account-{n}
  does not exist")`.
- `_ensure_no_live_session` on both (`"--swap-accounts"`).
- Read both slots' creds+config up front (missing → `""`).
- **Same-email overlap** (`email_a == email_b`, e.g. same email different orgs):
  the two backup keys fully overlap, so each write destroys the other's material.
  `_stage_overlap_material` parks durable `credentials/.swap-staging-{kind}-{num}.json`
  copies (0600, `O_EXCL`, never overwriting). A leftover staging file →
  `ConfigError("Found leftover staging from an interrupted swap: {path}. It holds
  that slot's pre-swap credentials and may be the only surviving copy. Verify both
  accounts still work (`cswap list`), then delete the file and retry.")`. Staging
  `OSError` → `ConfigError("Could not stage swap material, nothing was changed:
  {e}")`.
- `_swap_session_dirs`: exchange the two profile dirs (via a `.swapping` staging
  rename); best-effort.
- **Write-or-clear each destination** to its owner's exact state (write material
  that exists, `delete_account_credentials_strict` / `_delete_config_backup` what
  doesn't) — an empty source must never leave the destination serving the other
  account's material.
- Swap the two records; renumber `sequence` then **sort**; swap
  `activeAccountNumber` if it was either; bump `lastUpdated`; **write (commit)**.
- On any exception before/at commit → `_rollback_swap` (best-effort restore of
  both slots to their **old** keys; on same-email, an originally-empty key is
  cleared strict; staged copies are kept on disk if any restore fails, with a
  `warning` naming them; on a fully clean rollback the staged copies are
  discarded) then re-raise.
- Post-commit (best-effort): if emails differ, `_delete_account_files` on both old
  keys (logged loudly on failure). Clear the retained `.prev` generations for keys
  that received material (`delete_previous_backup`). `_discard_staging`. Log
  `Swapped slots: {num_a} ({email_a}) <-> {num_b} ({email_b})`.
- `_delete_config_backup(num, email)` = unconditional
  `config_file.unlink(missing_ok=True)` (never `exists()`-guarded — fails open on
  an inaccessible dir).

Aliases, credential/config backups, session profiles, and membership in
`sequence` all travel with the slot number in both move and swap. Directory
mappings key on `(email, org)` and are **unaffected** (no pruning) by swap or
slot-migration. Usage-cache rows and auto-switch quarantine key on slot number but
carry identity, so a moved row fails its identity check and self-heals on the next
poll.

---

## 11. `purge()`

Removes all cswap data. Refuses while any session-mode instance is live.

- Enumerate `sessions/*` dirs; if any has live PIDs → `SessionError("Live
  session-mode Claude instance(s) found: {name (PID …); …}. Exit them first, then
  retry --purge.")`.
- Print a `warning("This will remove ALL claude-swap data from your system:")`
  header, then:
  - `  - Backup directory: {backup_dir}`
  - `  - Legacy backup directory: {legacy}` **only if** legacy is a distinct path
    that exists.
  - macOS: `  - All stored account credentials (macOS Keychain and/or files)`;
    else `  - All stored account credential files`.
  - `  - All session profiles and their Keychain entries` if any session dirs.
  - `Note: This does NOT affect your current Claude Code login.` (dimmed).
- Prompt `Are you sure you want to purge all data? [y/N] `; answer `.lower() != "y"`
  → `Cancelled`, return.
- For every account record: for `nums = [num] (+ "None")`, unlink each
  `credentials/.creds-{num}-{email}.enc` (collect `Credential file: {name}`);
  macOS `security delete_password(SECURITY_SERVICE, account-{num}-{email})` (collect
  `Credential: {username}`); on macOS/Windows also `_sweep_legacy_keyring` (import
  `keyring`, `delete_password(KEYRING_SERVICE="claude-code", username)`, collect
  `Legacy keyring credential: {username}`). All best-effort (swallow exceptions).
- Delete session-profile keychain entries **before** the dir (`delete_macos_keychain_entry`
  for each session dir; collect `Session profiles: {names}`).
- Close all log handlers (required on Windows), then `shutil.rmtree(backup_dir)`
  (collect `Directory: {backup_dir}`).
- If a distinct legacy dir still exists, `shutil.rmtree(legacy)` (best-effort,
  collect `Legacy directory: {legacy}`).
- Print `Removed:` + `  - {item}` lines, or `No claude-swap data found to remove.`
  if nothing; then `Purge complete.`.

---

## 12. Duplicate & collision detection

Impossible-by-construction offline signals surfaced in `list`/`list --json`
(read-only; do not mutate the store):

- `_duplicate_account_warnings(accounts_info)`:
  - Identical credential **fingerprint** (`oauth.credential_fingerprint`) across
    two slots → `"Account-{other} and Account-{snum} hold the same credential
    ({email}) — one slot's backup was overwritten. Log in with the missing account
    and re-add it: cswap add --slot N"`.
  - Same non-empty `uuid` + org across two slots → `"Account-{other} and
    Account-{snum} both authenticate as {email} — remove or re-login one of them."`
    (empty uuids — add-token placeholders — never match each other).
- `_lockstep_usage_warnings`: heuristic for two generations of the same account —
  identical 5h & 7d pct + reset timestamps → `"Account-{other} and Account-{snum}
  report identical usage and reset times — they may be the same account (issue
  #117). If it persists, log in with the missing account and re-add it: cswap add
  --slot N"`. Only compares rows where both windows carry non-null `resets_at` and
  `pct`.

The *preventive* guards (§5.1 step 4, §6.3, and the true-duplicate refusal that
falls into refresh-in-place) are the write-side counterparts.

---

## 13. Edge cases & subtleties (from tests)

- **Sequence stays sorted**: `add`, `add-token`, `move`, `swap` all `.append`
  then `.sort()` `sequence`. Renumber-then-sort in move/swap means rotation and
  `cswap list` order follow the **new** numbers, not old visual positions
  (`test_move_keeps_sequence_sorted`, `test_swap_keeps_sequence_sorted`).
- **Sparse slots are legal**: `remove` leaves gaps; `add` numbers from `max+1`;
  `move` accepts any `1..cap` where `cap = max(99, existing_max_slot)`. A table
  grown past 99 keeps its full range (`test_move_cap_stretches_to_existing_max_slot`);
  `100`/`151` rejected as "out of range".
- **`move`/`swap` invalid targets** (`"abc"`, `"0"`, `"-1"`, `"1.5"`, `""`) →
  `ValidationError`. `"05"` normalizes to `"5"`.
- **`move` no-op** (`move X X`) returns `(X, X, False)` and touches nothing.
- **Occupied `move` == `swap`**: `move a <b's slot>` lands identical state to
  `swap a b` (`test_move_is_general_form_of_swap`), returns `swapped=True`.
- **Unbacked slot relocation**: an account with no stored credential must not
  adopt stale foreign material left under its target key by an earlier crash — the
  write-or-clear step **actively clears** the target key
  (`test_move_unbacked_account_clears_stale_target_key`,
  `test_swap_clears_stale_destination_key`). If the clear can't be verified
  (locked Keychain / unreadable dir / injected unlink failure), the op **aborts
  pre-commit** with `CredentialError` "aborting before commit" and the account
  stays intact under its original number
  (`test_move_failed_required_clear_aborts_commit`,
  `test_move_strict_clear_fails_closed_on_unreadable_dir`,
  `test_move_strict_clear_fails_closed_on_locked_keychain`).
- **Metadata write is the commit point**: a `_write_json` failure on
  `sequence.json` leaves the account fully usable under its original number, the
  old keys uncleared, target-key strays cleaned
  (`test_move_metadata_failure_leaves_account_intact`).
- **Same-email swap durability**: with fully overlapping keys, a mid-swap write
  failure rolls both slots back (`test_swap_same_email_partial_failure_rolls_back`);
  a *persistent* backend outage keeps the pre-swap material in the 0600
  `.swap-staging-creds-{num}.json` copies rather than only in dying memory
  (`test_swap_same_email_persistent_failure_keeps_staged_copy`); a leftover
  staging file causes a loud refusal, never an overwrite
  (`test_swap_refuses_leftover_staging`); clean swaps leave no `.swap-staging-*`
  and no `*.enc.prev` behind (`test_swap_same_email_clears_prev_generations`).
- **`_write_json` publishes only after chmod**: chmod runs on the temp file, so a
  chmod failure aborts without publishing (`test_write_json_publishes_only_after_chmod`).
- **Active number follows the account** through move/swap
  (`test_move_active_account_to_empty_slot_follows_active`,
  `test_swap_moves_active_number_with_account`).
- **Alias travels with its account** in move/swap and is preserved on
  slot-migration via `add --slot`; a plain refresh-in-place `add` without `--alias`
  keeps the existing alias, and `add --alias X` on an existing slot replaces it
  (`TestAddAccountAlias`). Duplicate alias at add time → `ValidationError`.
- **Alias resolution precedence**: number > alias > unrelated email; empty
  identifier never matches an aliasless account; alias match is case-insensitive
  (`TestResolveByAlias`).
- **`unset_alias` is idempotent** — clearing an unset alias never raises.
- **`add-token` email defaulting**: omitted email →
  `setup-token-{slot}@token.local` (OAuth) / `api-key-{slot}@token.local`
  (API key); defaulted email propagates into the config blob's
  `oauthAccount.emailAddress`; two default-email registrations to different slots
  coexist; explicit `--email` wins. Stdin (`"-"`) reads one line and strips the
  trailing newline. `slot=0` → `ConfigError >= 1`; empty/whitespace token →
  `ValidationError empty`; malformed `--email` → `ValidationError Invalid email`.
- **`add-token` credential blob** always seeds `scopes: ["user:inference"]`, even
  on refresh-in-place; the raw `sk-ant-api…` key is stored **verbatim** (not
  wrapped) and tagged `kind: "api_key"`.
- **Cross-kind collision**: an email registered as OAuth rejects a later API-key
  add of the same email and vice-versa (`TestCrossKindCollision`). Default
  `…@token.local` labels never collide (slot-unique); the guard only bites a forced
  `--email`.
- **`add_account` refuses to capture a live API-key login** as a kindless OAuth
  account (`TestAddAccountGuard`): even with a lingering `oauthAccount` identity, a
  `primaryApiKey` present → `ValidationError "Active login is an API-key account"`.
- **Same email, different org** is a legal *second* account
  (`test_allows_same_email_different_org`); identical `(email, org)` is a true
  duplicate → refresh-in-place ("Updated credentials"), never a second record
  (`test_blocks_true_duplicate`).
- **Dead-token quarantine** is lifted whenever a fresh credential is written to a
  slot — via `add`, `add-token` refresh-in-place, or a new write into a slot whose
  lingering usage row still carries an `invalid_grant` strike
  (`clear_dead_token([num], {num: (email, org)})`). API-key/OAuth token accounts
  are always personal (`org == ""`).
- **`.enc`-wins reads**: a fresh `.enc` beats a stale Keychain copy; a
  corrupt/empty/whitespace `.enc` (`"corrupt"`, `""`, `"!!!!"`, `"   "`, `"\n"`)
  falls back to the Keychain (base64 `validate=True`); a Keychain backup write
  deletes the shadow `.enc` (or rewrites it fresh if the delete fails); file-mode
  writes clear the stale Keychain copy; delete removes both backends; a healthy-Mac
  read never materializes an `.enc` (`Test... backup` tests around lines 4631-4718).
- **`.prev` lifecycle**: retains exactly one prior generation on a
  changed-value write; a same-value rewrite preserves the retained generation;
  deleting the account drops `.prev` too (`test_prev_removed_with_account`).
- **Purge sweeps legacy `account-None-{email}`** on the new `claude-swap` service
  and best-effort on the old `claude-code` keyring service
  (`test_purge_removes_legacy_none_keychain_entry`).
- **Remove/switch gates accept aliases**: `remove_account("dev")` /
  `switch_to("dev")` reach resolution; a well-formed-but-unknown alias →
  `AccountNotFoundError` (not `ValidationError`); junk like `"not an email or
  alias!"` → `ValidationError`.
- **Directory-mapping pruning**: `remove` and slot-*overwrite* (displacing a
  different account) prune mappings; slot-*migration* (same identity, new slot)
  and swap **keep** them (mappings key on `(email, org)`).
- **JSON list** carries `disabled: true` only on disabled rows (additive, absent
  on enabled rows).

---

## 14. Go port notes

- **Platform-conditional storage** is the single biggest branch. macOS routes
  backups to the Keychain via the `/usr/bin/security` CLI (pinned absolute path —
  never PATH-resolved, a credential-tool hardening requirement) with `.enc` files
  as fallback and `.enc`-wins reads; every other platform is `.enc`-only. Port the
  usability cache (`None`/`True`/`False`), the 60s `KEYCHAIN_RECHECK_COOLDOWN_S`
  cooldown on a **monotonic** clock, and the sticky "pin file mode" (no re-probe)
  posture. `KEYCHAIN_ERRORS` catches exactly `KeychainError` (non-44 non-zero exit
  / timeout), a subprocess timeout, and a missing binary — nothing else. The
  active-OAuth read retries `_ACTIVE_READ_ATTEMPTS = 2` times with
  `_ACTIVE_READ_RETRY_DELAY = 0.3s` between attempts.
- **Keychain account name** must replicate Claude Code's `getUsername()` exactly:
  `$USER` → POSIX username → literal `"claude-code-user"`. A divergent default
  keys a different item and breaks interop on headless hosts.
- **`security` item values**: `get_password` uses `find-generic-password -w` and
  strips exactly one trailing `\n` (`removesuffix("\n")`), preserving meaningful
  internal/edge whitespace. rc **44** = "not found" → `None`, not an error.
  `set_password` uses `-U` (create-or-update) and prefers stdin (`-i`) to keep the
  secret out of argv. Port these exit-code and quoting details.
- **Concurrency / locks** (three distinct lock protocols):
  1. `FileLock` on `<backup>/.lock`: POSIX `fcntl.flock(LOCK_EX|LOCK_NB)`, Windows
     `msvcrt.locking(LK_NBLCK, 1)`, 0.1s poll, 10s default timeout. **Non-reentrant
     in-process** — the Go port must not nest acquisitions (the public
     `write_account_credentials`/`persist_backup_credentials` split exists precisely
     to avoid double-locking). Prefer `flock` semantics (advisory, released on fd
     close/process death).
  2. **npm `proper-lockfile` interop** (`claude_locks.py`) for cooperating with a
     running Claude Code while touching its files — a **directory** lock at
     `<target>.lock` (`~/.claude.lock`, `~/.claude.json.lock`), `mkdir` atomicity =
     mutex, stale after `STALENESS_S = 10.0s`, live holder touches mtime every
     `TOUCH_INTERVAL_S = 3.0s` (Claude Code touches every 5s = stale/2), our wait
     `DEFAULT_TIMEOUT_S = 9.0s`, take over a stale lock by `rmdir`+`mkdir`, run a
     daemon thread touching the mtime while held, `rmdir` on release. Claude Code
     retries a held credentials lock 5× with 1–2s jittered sleeps. Timeout →
     `ClaudeCodeLockTimeout` (nothing mutated; safe to retry). The Go port needs a
     background goroutine for the touch loop.
  3. Usage fetches use a `ThreadPoolExecutor` with a `_FETCH_STAGGER_S = 0.25s`
     per-index start stagger — Go: a goroutine pool with the same stagger. (Usage
     is another agent's mandate; noted because it shares the account lock via the
     persist callbacks.)
- **Atomic writes** everywhere: write to a temp file in the same directory, then
  `os.replace` (rename). `_write_json` additionally validates the JSON round-trip
  before commit and chmods the *temp* file so the rename is the last fallible step.
  Replicate with `os.CreateTemp` + `os.Rename` and 0600/0700 modes gated on
  non-Windows.
- **JSON shape fidelity**: `json.dumps(..., indent=2)` with default separators.
  `sequence` is a list of ints; `accounts` keys are strings; `activeAccountNumber`
  is an int or null. Optional keys (`alias`, `kind`, `disabled`) must be **omitted**
  when unset, never zero-valued — use `omitempty`/pointer/`map[string]any` in Go so
  absence is preserved (duplicate detection and back-compat depend on it). The
  add-token **config** blob writes JSON `null` for org fields while the sequence
  **record** writes `""` — keep both.
- **Timestamps**: UTC, `strftime("%Y-%m-%dT%H:%M:%SZ")` (seconds precision, `Z`
  suffix). Go: `time.Now().UTC().Format("2006-01-02T15:04:05Z")`.
- **base64**: standard encoding; decode with strict validation (`validate=True`)
  and reject empty/whitespace results — a corrupt `.enc` must fall through, not
  win. Go `base64.StdEncoding.DecodeString` already errors on non-alphabet input;
  add the non-empty guard.
- **Python-isms to translate**:
  - `getpass.getpass("Token: ")` for interactive token entry;
    `sys.stdin.readline().rstrip("\n")` for `"-"`.
  - `input(...)` prompts return the raw string; comparisons are `.strip().lower()`
    against `{"y","yes"}` (add/overwrite) or `.lower() == "y"` (remove/purge). Cancel
    handling catches `EOFError`/`KeyboardInterrupt` (add paths) → print `\nCancelled`
    and return.
  - `str`/`int` slot duality: keys are strings, `sequence`/`activeAccountNumber`
    are ints; `str(int("01"))` normalization; `n.isdigit()` guards.
  - `record.pop("disabled", None)` / `del record["alias"]` — key removal, not
    zeroing.
  - Lazy imports (`keyring`, `mappings`, `session`, `migrations`) are for
    circular-import avoidance and best-effort optionality; in Go they are ordinary
    package deps, but `keyring` (the legacy backend) is *optional* — its absence
    during purge is a silent no-op.
- **Error → exit code**: all custom exceptions derive from `ClaudeSwitchError`
  (`ConfigError`, `CredentialError`/`CredentialReadError`/`CredentialWriteError`,
  `SwitchError`, `SessionError`, `LockError`/`ClaudeCodeLockTimeout`,
  `AccountNotFoundError`, `ValidationError`, `TransferError`, `MigrationError`). The
  CLI catches `ClaudeSwitchError` and prints an error / (in `--json`) emits
  `{"schemaVersion":1,"error":{"type":<ClassName>,"message":<str>}}`. The Go port
  should carry a comparable typed-error hierarchy so the `type` name round-trips
  (external contract for scripts). `SCHEMA_VERSION = 1`.
- **Best-effort vs fail-closed** is load-bearing and must be preserved per call
  site: transactional pre-commit clears (`delete_account_credentials_strict`) and
  the swap/move required-clears **raise** and abort the commit; post-commit
  cleanup, `.prev` handling, session-profile moves, and Keychain deletes are
  best-effort (log, continue). Getting this backwards either bricks switching on a
  flaky Keychain or leaks a stale credential onto a reused slot.
