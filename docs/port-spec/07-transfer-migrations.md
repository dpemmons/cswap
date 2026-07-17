# claude-swap: Export/Import (`transfer.py`) and Data Migrations (`migrations.py`) — Behavioral Spec

## Overview

`claude_swap.transfer` implements the `.cswap` portable backup format: a single
JSON envelope containing one or more accounts' OAuth/API-key credentials and a
(slimmed or full) copy of `~/.claude.json`, used to move accounts between
machines or to back them up. `export_accounts()` reads from the local backup
store (files and/or macOS Keychain, whichever the switcher's per-account
credential backend resolves to) and serializes to a file or stdout; no
encryption is built in by design — the docstring explicitly tells users to
compose their own (`cswap --export - | gpg -c > out.gpg`). `import_accounts()`
reads the envelope, validates every account *before* writing anything (an
all-or-nothing validation pass followed by a best-effort write pass), and
either skips, overwrites (`--force`), or allocates a fresh slot for each
imported account, with special-cased handling for alias collisions, the
locally active account, live session-mode processes, and a stale-token
quarantine system. `claude_swap.migrations` is a separate, much narrower
system: a tiny registry of *one-time, idempotent, self-guarded* migrations
that relocate legacy per-account backup-credential storage (Windows
Credential Manager → files; macOS third-party `keyring` library → the
`security`-CLI-backed `SECURITY_SERVICE`) and that run automatically once per
un-applied migration at `ClaudeAccountSwitcher.__init__` time. Two *other*
migrations that a Go port must know about live outside `migrations.py`
entirely — an in-band `sequence.json` org-field backfill in `switcher.py`
(triggered transparently by both export and import), and a legacy
`~/.claude-swap-backup` → XDG-path directory relocation in `paths.py` (run
before any of the above, at the very top of switcher construction) — both are
documented here for completeness since a Go port's startup sequence must
reproduce the same ordering.

---

## 1. The `.cswap` export file format

### 1.1 Envelope schema (verbatim keys, camelCase)

```json
{
  "version": 1,
  "exportedAt": "2026-01-01T00:00:00Z",
  "exportedFrom": "linux",
  "swapVersion": "1.2.3",
  "encrypted": false,
  "activeAccountNumber": 2,
  "accounts": [ { "...": "see 1.2" } ]
}
```

Field semantics:

- `version` — integer, always `1` today (`FORMAT_VERSION = 1` in
  `transfer.py`). Import rejects anything else, including a missing key
  (`envelope.get("version")` is `None` and `None != 1`).
- `exportedAt` — `get_timestamp()`: `datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")`
  — UTC, second precision, literal trailing `Z`, no microseconds, no `+00:00`
  offset form. Purely informational; import never reads it.
- `exportedFrom` — one of the `_PLATFORM_TAG` strings: `"macos"`, `"linux"`,
  `"wsl"`, `"windows"`, `"unknown"` (mapped from the exporting switcher's
  `Platform` enum; falls back to `"unknown"` for any unmapped value).
  Informational only; import never reads it.
- `swapVersion` — the exporting tool's own version string, resolved via
  `importlib.metadata.version("claude-swap")` (`claude_swap.__version__`).
  Informational only.
- `encrypted` — always written as `false` by `export_accounts` (there is no
  encryption feature). Import hard-rejects any envelope where this is `true`
  (see §3.3).
- `activeAccountNumber` — integer or `null`. The **source-side** active slot
  number, but only if that slot's account actually made it into the
  `accounts` array (see §2.7); otherwise `null`. Import uses this only to
  *seed* the destination's active-account preference when the destination has
  none (see §3.10) — it never forces an activation.
- `accounts` — non-empty array of per-account objects (§1.2). Import rejects
  a missing, non-array, or empty value with `"export file has no accounts to
  import"`.

The whole envelope is a JSON object; import rejects any other JSON top-level
type with `"export file must be a JSON object"`.

### 1.2 Per-account entry schema

```json
{
  "number": 1,
  "email": "alice@example.com",
  "uuid": "acct-uuid",
  "organizationUuid": "org-a",
  "organizationName": "Acme",
  "added": "2024-01-01T00:00:00Z",
  "credentials": { "...": "OAuth JSON object, OR see kind:api_key below" },
  "config": { "oauthAccount": { "...": "see slim vs full, §2.4" } },
  "kind": "api_key",
  "alias": "dev"
}
```

- `number` — the account's slot number **on the exporting machine**. Only
  used as a *hint* for slot allocation on import when no matching account
  already exists locally (§3.6); never used as an identity key. Must be a
  positive integer (`>= 1`) when re-imported — see validation in §3.4.
- `email` — the account's OAuth email address (or `{kind}-{slot}@token.local`
  synthetic placeholder for tokens/API keys added without a real email —
  that convention lives in the account-add code, not transfer.py, but flows
  through unchanged). Must pass `switcher._validate_email()`:
  `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`.
- `uuid`, `organizationUuid`, `organizationName`, `added` — copied verbatim
  from the local `sequence.json` account record (`record.get(..., "") or ""`
  — `None`/missing normalizes to `""`, never `null`). `added` falls back to
  a fresh `get_timestamp()` at *import* time if absent/falsy in the source
  entry (i.e. hand-crafted envelopes need not supply it).
- `credentials` — **either**:
  - a JSON object (OAuth credential, e.g. `{"accessToken": ..., "refreshToken": ..., "expiresAt": ...}`)
    when the account is `kind: oauth` (the default, omitted `kind` key), **or**
  - a raw string starting with `sk-ant-api` when the account is
    `kind: api_key`. `looks_like_api_key()` (from `credentials.py`) is the
    single source of truth for this classification: `text.strip().startswith("sk-ant-api")
    and not text.strip().startswith("{")`.
  Export always sets the `"kind": "api_key"` key on the entry when the
  underlying credential is an API key (never present for OAuth accounts —
  the field is entirely absent, not `"oauth"`).
- `config` — a JSON object. By default (`--full` not passed) this is
  `{"oauthAccount": <the source's oauthAccount value>}` and nothing else
  (see §2.4). With `--full` it is the **entire** parsed `~/.claude.json` from
  the source (or the source's stored per-account backup config).
- `alias` — present only if the source account had one set; a lowercase
  string matching `^[a-z0-9_.-]+$`, non-empty, not purely numeric, not
  leading with `-` (see `normalize_alias()` in `models.py`). Absent (key not
  present) for aliasless accounts — never `null` or `""`.

### 1.3 Format constants

| Constant | Value | Location |
|---|---|---|
| `FORMAT_VERSION` | `1` | `transfer.py` |
| JSON serialization | `json.dumps(envelope, indent=2)` + trailing `"\n"` when writing to a file; **no** trailing newline logic difference for stdout beyond an explicit extra `\n` write | `export_accounts` |
| File permissions | `0o600` (owner read/write only) on the export file, non-Windows only | `_atomic_write_file` |
| Directory permissions | `0o700` on `backup_dir`, `configs_dir`, `credentials_dir` | `switcher._setup_directories` |

---

## 2. Export (`export_accounts`)

```python
def export_accounts(
    switcher: ClaudeAccountSwitcher,
    destination: str,       # file path, or "-" for stdout
    account: str | None = None,  # NUM|EMAIL to limit to one account
    full: bool = False,     # include entire ~/.claude.json vs. oauthAccount only
) -> None
```

Raises `TransferError` (malformed/missing data, unknown account),
`CredentialReadError` (failed credential read), or `ConfigError` (missing
config file / missing `oauthAccount`).

### 2.1 Preconditions

Calls `switcher._get_sequence_data_migrated()` first (this transparently
triggers the org-field backfill migration described in §6.1 as a side
effect, mutating local `sequence.json` if needed). If there is no sequence
data or no accounts: `TransferError("no accounts to export — run cswap
--add-account first")`.

### 2.2 Account selection

- Explicit `account` (`NUM|EMAIL`, resolved via
  `switcher._resolve_account_identifier`): if it doesn't resolve to a known
  slot, `TransferError(f"account not found: {account}")`. Exactly one
  account is targeted, and a missing backup for it is a **hard failure**
  (§2.6).
- No `account`: every slot in `sequence_data["accounts"]`, sorted by
  `int(key)` ascending. A slot with missing backup data is **skipped with a
  stderr warning**, not a hard failure (§2.6, issue #41).

### 2.3 Active-account special case

`switcher._get_current_account()` reads the **live** `~/.claude.json`'s
`oauthAccount.emailAddress` / `organizationUuid` (returns `None` if the file
is missing/unparseable/has no email). For each candidate slot, it is
considered "the active/live one" iff `current_identity == (record["email"],
record["organizationUuid"] or "")`.

- **If active**: credentials come from `switcher._read_credentials()` (the
  *live* credential store, not the backup), and config from
  `switcher._get_claude_config_path().read_text()` (the live
  `~/.claude.json`, not the backup config file). Rationale: the live vault
  has fresher tokens than the backup copy. Missing live credentials →
  `CredentialReadError(f"failed to read live credentials for active account
  {email}")`. Missing live config file → `ConfigError("Claude config file
  not found")`.
- **If not active**: credentials/config come from the stored backup via
  `switcher._read_account_credentials(num, email)` /
  `switcher._read_account_config(num, email)`.

### 2.4 Slim vs. full config (`--full`)

`_slim_config(config_obj, label)`:
- Requires `config_obj["oauthAccount"]` to be a dict; else
  `TransferError(f"{label} is missing oauthAccount — cannot export")` where
  `label = f"config for {email}"` (full message: `"config for alice@x.com is
  missing oauthAccount — cannot export"`).
- Returns **only** `{"oauthAccount": oauth}` — every other top-level key
  (`userID`, `anonymousId`, `projects`, `tipsHistory`,
  `cachedGrowthBookFeatures`, `appleTerminalBackupPath`, `numStartups`, etc.)
  is stripped. Rationale stated in the source: keep transfers small and avoid
  leaking source-machine identity (userID, anonymousId, absolute paths,
  cached feature flags) to the destination. This is the **default**
  (`full=False`).

With `full=True`, `_slim_config` is never called — the entire parsed config
object is embedded verbatim (same-PC backup use case, machine state is
intentional).

### 2.5 API-key vs. OAuth credential serialization

```python
is_api_key = looks_like_api_key(creds_text)
entry["credentials"] = (
    creds_text.strip() if is_api_key
    else _parse_payload(creds_text, f"credentials for {email}")  # JSON-parsed object
)
if is_api_key:
    entry["kind"] = "api_key"
```
`_parse_payload` raises `TransferError(f"{label} is not valid JSON: {exc}")`
or `TransferError(f"{label} must be a JSON object")` if the stored
credential text isn't a JSON object (this would indicate local on-disk
corruption, not a user input error).

### 2.6 Broken-slot tolerance (issue #41)

For each candidate slot (not the active-account case, which always reads
live):
- If **both** creds and config backups are readable → included.
- If **either** is missing/empty and `explicit_account` is `False` (bulk
  export): **skip with a stderr warning**, never fail the whole export:
  ```
  Skipping Account-{num} ({email}): no stored credentials/config — re-add with: cswap --add-account --slot {num}
  ```
  (single message regardless of whether it was creds, config, or both that
  were missing).
- If `explicit_account` is `True` (user asked for exactly this one): **hard
  fail**. Missing creds → `CredentialReadError(f"no backup credentials found
  for account {num} ({email})")`. Missing config (creds present) →
  `ConfigError(f"no backup config found for account {num} ({email})")`.
- If **every** candidate slot ends up skipped (bulk mode): `TransferError("no
  exportable accounts — all managed slots are missing stored
  credentials/config. Re-add with: cswap --add-account --slot <number>")`.

### 2.7 `activeAccountNumber` computation

```python
recorded_active = sequence_data.get("activeAccountNumber")
exported_nums = {a["number"] for a in accounts_payload}
active_in_payload = recorded_active if recorded_active in exported_nums else None
```
If the recorded active slot's backup was broken and thus skipped (§2.6), the
envelope's `activeAccountNumber` becomes `null` — it must never reference an
account absent from the payload, or import would dangle.

### 2.8 Output

- `destination == "-"`: writes the JSON (indent=2) directly to `sys.stdout`,
  then an explicit `"\n"`, then flushes. **No** summary line is printed at
  all in this mode (not even to stderr) — stdout must stay pure JSON for pipe
  consumers, and the "Exported N account(s)..." summary is entirely
  suppressed.
- otherwise: `Path(destination).expanduser()`, written via
  `_atomic_write_file` — writes to a sibling temp file
  (`path.with_suffix(f".{os.getpid()}.tmp")`, i.e. `Path.with_suffix`
  **replaces** any existing trailing suffix on `path`, not appends), chmods
  it `0o600` (non-Windows), `shutil.move`s it onto the final path, then
  chmods the final path `0o600` again (belt-and-suspenders — the move can in
  principle land the temp file's mode, but the final chmod is explicit).
  Content is `serialized + "\n"`. Then prints to **stderr**:
  `f"Exported {len(accounts_payload)} account(s) to {out_path}"`.

Per-slot skip warnings (§2.6) always go to **stderr**, in both stdout and
file-destination modes, so `cswap --export -` piped into a JSON consumer
never sees them mixed into stdout.

---

## 3. Import (`import_accounts`)

```python
def import_accounts(
    switcher: ClaudeAccountSwitcher,
    source: str,        # file path, or "-" for stdin
    force: bool = False,
) -> None
```

### 3.1 Source reading

- `source == "-"`: `sys.stdin.read()`.
- otherwise: `Path(source).expanduser()`; missing file →
  `TransferError(f"import file not found: {in_path}")`; else
  `read_text(encoding="utf-8")`.

### 3.2 Envelope-level validation (checked in this exact order)

1. `json.loads(text)` — parse failure → `TransferError(f"export file is not
   valid JSON: {exc}")`.
2. Top-level must be a dict → else `TransferError("export file must be a
   JSON object")`.
3. `version` must equal `FORMAT_VERSION` (`1`) → else `TransferError(f"unsupported
   export version: {version!r} (expected {FORMAT_VERSION})")`. A **missing**
   `version` key (`None != 1`) hits this same branch with `version!r ==
   'None'`.
4. `encrypted is True` → `TransferError("encrypted exports are not supported
   in this version — decrypt before piping (e.g. gpg -d backup.gpg | cswap
   --import -)")`. (Checked *after* version, so a version mismatch on an
   encrypted file reports the version error first.)
5. `accounts` must be a non-empty list → else `TransferError("export file has
   no accounts to import")`.

### 3.3 Pass 1 — per-account validation (ALL accounts validated before ANY write)

The docstring/comment is explicit about the invariant: *"A malformed account
later in the list must not leave earlier accounts half-imported."* This is
enforced structurally — pass 1 builds a `normalized: list[dict]` in memory
and does zero filesystem/keyring writes; pass 2 (§3.6) only starts once every
entry in `accounts` has passed validation.

Before the loop: `switcher._get_sequence_data_migrated()` is called on the
**local** destination store (triggering the org-field migration, §6.1, as a
side effect) to build a case-folded `local_aliases: {alias_lower:
(email, org_uuid)}` map used for collision detection.

Per-entry, `_validate_imported_account(switcher, account)` runs first
(defends against path traversal — email and slot number later flow into
f-string filenames like `.creds-{num}-{email}.enc`):

```python
def _validate_imported_account(switcher, account) -> tuple[str, str]:
    # account must be a dict
    # email must be a str AND switcher._validate_email(email) must pass
    #   -> TransferError(f"invalid or missing email in imported account: {email!r}")
    # number: must be int, NOT bool (isinstance(x, bool) rejected explicitly
    #   even though bool is an int subclass in Python), and >= 1
    #   -> TransferError(f"invalid slot number in imported account ({email}): {raw_number!r}")
    # organizationUuid / organizationName / uuid / added / alias: if present
    #   and non-None, MUST be a str
    #   -> TransferError(f"{field} for {email} must be a string, got {type(...).__name__}")
    # alias (if a string): must pass normalize_alias() (ValueError -> TransferError
    #   f"invalid alias for {email}: {e}")
    return email, str(raw_number)
```

Then, back in `import_accounts`, still within pass 1, per entry:

- `org_uuid = raw.get("organizationUuid", "") or ""`.
- `config_obj = raw.get("config")` must be a dict → else
  `TransferError(f"config for {email} must be a JSON object")`.
- Kind detection: `is_api_key = raw.get("kind") == "api_key" or
  isinstance(creds_obj, str)` — i.e. a string `credentials` value is
  *always* treated as an API-key attempt even without an explicit `kind`
  field.
  - If API key: `creds_obj` must be a string AND `looks_like_api_key(creds_obj)`
    → else `TransferError(f"API-key credentials for {email} must be a raw
    sk-ant-api… string")`. Stored as `creds_obj.strip()`.
  - Else (OAuth): `creds_obj` must be a dict → else `TransferError(f"credentials
    for {email} must be a JSON object")`. Stored as `json.dumps(creds_obj)`
    (re-serialized, **not** the original source text — whitespace/key-order
    from the input file is not preserved).
- **Duplicate-within-envelope check**: key `(email, org_uuid)`; a repeat →
  `TransferError(f"duplicate account in export: {email} (org={org_uuid or
  'personal'})")`.
- **Alias handling** (only if `raw.get("alias")` truthy):
  - `alias_key = normalize_alias(alias)` (already validated for format in
    `_validate_imported_account`, so this only re-derives the lowercase
    form; the comment explicitly notes it's "already validated in pass-1
    above").
  - Duplicate alias *within this envelope* → `TransferError(f"duplicate
    alias in export: {alias_key}")`.
  - Collision with a **local** alias belonging to a *different* identity
    (`owner != (email, org_uuid)`): the alias is **silently dropped** (set to
    `None`) with a stderr warning — this is **not** a hard error:
    ```
    Warning: alias '{alias_key}' for {email} already used by an existing account, dropping the imported alias
    ```
  - If the local alias owner **is** this same `(email, org_uuid)` (i.e.
    re-importing/re-exporting the same account that already carries this
    alias locally, e.g. under `--force`), it's kept — not treated as a
    collision with itself.
- Normalized record appended to `normalized`:
  ```python
  {
      "email", "exported_num" (str), "org_uuid", "org_name",
      "uuid", "added" (falls back to get_timestamp() if raw missing/falsy),
      "kind": "api_key" | "oauth",
      "alias" (possibly None),
      "creds_text" (raw API key string, or json.dumps(oauth-object)),
      "config_text": json.dumps(config_obj, indent=2),
  }
  ```

### 3.4 Pass 2 — writes

```python
switcher._setup_directories()
switcher._init_sequence_file()
```
(so import into a completely fresh `$HOME` — no `.claude-swap-backup` /
XDG dir at all — works: directories and an empty `sequence.json` skeleton
are created on demand.)

Running counters: `imported`, `skipped`, `overwritten` (all start at 0).
`written_slots: set[str]` accumulates every slot actually written this run
(both `"imported"` and `"overwrote"` outcomes — **not** `"skipped"`).

`envelope_active_str = str(envelope["activeAccountNumber"])` if that field
`isinstance(..., int)`, else `None`. (Note: in Python `bool` is an `int`
subclass, so a pathological `"activeAccountNumber": true` would pass this
`isinstance` check and stringify to `"True"` — a Python-ism a Go port need
not reproduce; see §9.) `resolved_active_slot: str | None = None` tracks
where the envelope's active account ends up locally, however it got there
(imported / overwrote / already-existed-and-was-skipped).

For each normalized entry, **in envelope order**:

1. `is_envelope_active = entry["exported_num"] == envelope_active_str` (when
   the latter is not `None`).
2. **Re-read** `switcher._get_sequence_data_migrated()` fresh on every
   iteration (falling back to a synthetic empty skeleton
   `{"activeAccountNumber": None, "lastUpdated": ..., "sequence": [],
   "accounts": {}}` if `None`) — so each account's write observes every
   prior account's write in *this same import run*. There is **no other
   locking** around this read-modify-write cycle (see §9 on concurrency).
3. `existing_slot = switcher._find_account_slot(data, entry["email"],
   entry["org_uuid"])` — identity match is the **composite key**
   `(email, organizationUuid)`, never the exported slot number.

**Case A — account already exists locally (`existing_slot is not None`):**

- **No `--force`**: **skip**.
  - stderr: `f"Skipped {entry['email']} (already exists, use --force)"`.
  - **Dead-token quarantine hint** (issue #136): if
    `switcher._usage_store.entries({existing_slot: (email, org_uuid)})[existing_slot].token_dead()`
    is true, an additional stderr line:
    ```
      └ currently quarantined — refresh token dead; --force replaces the backup and lifts the old verdict
    ```
    (Guarded by identity — a stale usage-store row for a *different*
    identity at that slot number returns an empty/non-dead entry, so the
    hint does not misfire.) No credential material is touched in this
    branch — the stored verdict is left exactly as-is.
  - `skipped += 1`. If this entry is the envelope's active account,
    `resolved_active_slot = existing_slot` is still recorded **even though
    nothing was written** — "the envelope's active account exists locally
    — record where so we can seed activeAccountNumber" even on skip.
  - `continue` (no sequence.json write, no credential write, this entry is
    fully done).
- **With `--force`**: `target_num = existing_slot`; `outcome = "overwrote"`.
  - **Live session-mode warning**: `switcher._live_session_pids(target_num,
    email)` — if any live PIDs are running against this slot's session-mode
    profile, stderr:
    ```
    Warning: {email} (slot {target_num}) has a live session-mode instance (PID {pid, pid, ...}); its session profile keeps the pre-import credentials until it is restarted via 'cswap run'.
    ```
    (comma-joined PIDs). This is a warning only — the import proceeds
    regardless; the live process's own in-memory/session-profile copy of
    credentials is deliberately left untouched (pulling credentials out from
    under a running process would be worse than the drift).

**Case B — no existing local account (`existing_slot is None`):**

- `outcome = "imported"`.
- **Slot allocation**: if the envelope's own `exported_num` is **not**
  already occupied in the local `sequence.json`'s `accounts` map, reuse it
  (`target_num = entry["exported_num"]`) — i.e. import tries to preserve the
  source machine's slot number when it's free locally. Otherwise, allocate
  the next number via `switcher._get_next_account_number()` — **`max(existing
  slot numbers) + 1`; gaps are never filled** (mirrors `add_account`
  semantics exactly).

**Both cases then execute the same write sequence:**

```python
switcher._write_account_credentials(target_num, entry["email"], entry["creds_text"])
switcher._write_account_config(target_num, entry["email"], entry["config_text"])
switcher._usage_store.clear_dead_token([target_num], {target_num: (entry["email"], entry["org_uuid"])})
```

- `_write_account_credentials` is the credential-backend write (macOS
  Keychain-vs-`.enc`-file routing lives in `credentials.py`, out of this
  mandate) and, as a side effect on the switcher wrapper, calls
  `_post_backup_write` which **invalidates the slot's session-mode
  profile**: if a live session is running, the profile is marked *stale*
  (`.cswap-stale-credentials` sentinel file — session directory's
  `.claude.json` **survives**, only `.credentials.json` and the stale marker
  get cleaned on the *next* stale check); if **no** live session, the
  profile's `.credentials.json` is deleted outright (and any macOS session
  Keychain entry) while `.claude.json` (profile history: projects, MCP
  config, etc.) is preserved. Net effect either way: the next `cswap run`
  for that slot re-bootstraps credentials from the freshly imported backup.
- `_usage_store.clear_dead_token(...)` **unconditionally** clears the
  dead-token quarantine (`authDeadStrikes = 0`, `consecutiveFailures = 0`,
  `lastError = None`, `backoffUntil = None`) for `target_num`, for **both**
  `"imported"` and `"overwrote"` outcomes. This matters even for a brand-new
  slot number: if the identity `(email, org_uuid)` was previously *removed*
  (`cswap remove` does **not** prune `usage.json`) and is now being
  re-imported into a numerically fresh slot, the orphaned dead-token row
  from the old removed slot-number would otherwise still quarantine the
  reused slot number — `clear_dead_token` is keyed by slot **number** in
  `usage.json`, so this call is what lifts it (issue #138). A brand-new slot
  number with no prior row is an unconditional-but-harmless no-op (no
  spurious quarantine is ever created).

Then the sequence.json record is built and written:
```python
new_record = {
    "email": entry["email"],
    "uuid": entry["uuid"],
    "organizationUuid": entry["org_uuid"],
    "organizationName": entry["org_name"],
    "added": entry["added"],
}
if entry["kind"] == "api_key": new_record["kind"] = "api_key"
if entry.get("alias"): new_record["alias"] = entry["alias"]
data["accounts"][target_num] = new_record
if int(target_num) not in data["sequence"]:
    data["sequence"].append(int(target_num)); data["sequence"].sort()
data["lastUpdated"] = get_timestamp()
switcher._write_json(switcher.sequence_file, data)
```
(`data["sequence"]` is a sorted list of ints; a re-imported/overwritten slot
that's already in it is not re-appended.)

If this entry is the envelope's active account: `resolved_active_slot =
target_num`. `written_slots.add(target_num)`.

Finally, per-entry stderr line:
- overwrote: `f"Overwrote {entry['email']} (slot {target_num})"`.
- imported: `f"Imported {entry['email']} → slot {target_num}"` (Unicode
  right-arrow `→`, not `->`).

### 3.5 `activeAccountNumber` seeding (clean-home migration UX)

After the per-account loop, `final = switcher._get_sequence_data()` (a plain
re-read, **not** the `_migrated` variant, but by this point the migration
already ran earlier so it's equivalent). If:
```python
final is not None
and final.get("activeAccountNumber") in (None, 0)
and resolved_active_slot is not None
```
then `final["activeAccountNumber"] = int(resolved_active_slot)`,
`final["lastUpdated"] = get_timestamp()`, write back. **Only** fires when
the destination had *no* prior active-account preference (`None` or the
literal `0` — `0` is treated as "unset" here, not a real slot). If the
destination already has its own active selection, import **never**
overwrites it, even if the imported envelope carried a different active
account. `resolved_active_slot` is deliberately the **resolved local slot**
of the envelope's active account (which may differ from the envelope's raw
slot number if that number collided with an unrelated local account, or if
the envelope's active account was skipped-but-already-present locally under
a different slot) — never the raw envelope number.

### 3.6 Final summary line (always printed, stderr)

```
Done: {imported} imported, {overwritten} overwritten, {skipped} skipped
```

### 3.7 Live-login-overwritten hint (issue #79 follow-up)

After everything else:
```python
identity = switcher._get_current_account()   # live ~/.claude.json's (email, orgUuid)
if identity is not None and final is not None:
    live_slot = switcher._find_account_slot(final, identity[0], identity[1])
    if live_slot is not None and live_slot in written_slots:
        _eprint(
            f"Note: {identity[0]} is your current live login — activate the "
            f"imported credentials with: cswap --switch-to {live_slot} --force"
        )
```
Fires whenever the currently-live logged-in account's slot was **written**
this run (imported *or* overwrote — not merely skipped). Rationale: a plain
`cswap --switch` would back the (possibly stale) live credentials up over
the freshly imported ones; the user needs the explicit
`--switch-to <slot> --force` path to force-activate without that backup
step. `--force` here refers to the *switch* command's own force flag (skip
backing up current live login), unrelated to `import_accounts`'s `force`
parameter — same flag name, different call site, different semantics.

---

## 4. CLI surface

Subcommand → legacy flag translation (`_SUBCOMMAND_FLAGS` in `cli.py`):
`"export"` → `--export`, `"import"` → `--import`. So `cswap export PATH` and
`cswap --export PATH` are identical; likewise `cswap import PATH` / `cswap
--import PATH`.

Flags (argparse):
- `--export PATH` (metavar `PATH`) — mutually exclusive with every other
  top-level verb (`--list`, `--switch`, `--purge`, etc., via one
  `add_mutually_exclusive_group`).
- `--import PATH` — `dest="import_"` (avoids shadowing the `import` keyword);
  same mutually-exclusive group.
- `--account NUM|EMAIL` — "Limit export to one account (use with 'export')".
  Parser-level guard: if `--account` is supplied without `--export`... *(not
  explicitly guarded in the excerpt reviewed — only `--force`/`--full` have
  explicit misuse checks; treat `--account` outside export as accepted by
  argparse but ignored by `export_accounts`'s signature, since it's a keyword
  arg other subcommands never read)*.
- `--force` — shared flag: "Overwrite existing accounts during import; with
  'switch <num|email>', activate the stored credentials without backing up
  the current login first." Parser-level guard:
  ```python
  if args.force and not (args.import_ or args.switch_to):
      parser.error("--force can only be used with 'import' or 'switch <num|email>'")
  ```
- `--full` — "Include full ~/.claude.json in export (default: oauthAccount
  only)." Parser-level guard:
  ```python
  if args.full and not args.export:
      parser.error("--full can only be used with 'export'")
  ```

Dispatch (`cli.py` `main()`):
```python
elif args.export:
    export_accounts(switcher, args.export, account=args.account, full=args.full)
elif args.import_:
    import_accounts(switcher, args.import_, force=args.force)
```
Neither branch produces a `--json` payload (`payload` stays `None`); `--json`
combined with `--export`/`--import` has no special JSON-envelope output for
the *result* — only the error path (below) is JSON-aware.

### Error handling / exit codes

Both `export_accounts` and `import_accounts` raise subclasses of
`ClaudeSwitchError` (`TransferError`, `ConfigError`, `CredentialReadError`,
all ultimately `ClaudeSwitchError`). `main()` wraps the whole dispatch in:
```python
except ClaudeSwitchError as e:
    if args.json:
        print(json.dumps(error_envelope(e), indent=2))   # to STDOUT, exit 1
    else:
        error(f"Error: {e}")                              # to stderr, exit 1
    sys.exit(1)
```
`error_envelope(e)`:
```json
{"schemaVersion": 1, "error": {"type": "TransferError", "message": "<str(e)>"}}
```
(`type` is the Python exception class name — a Go port should pick a stable
equivalent tag per error kind.) `KeyboardInterrupt` during either command →
exit code `130`. Successful export/import → whatever `main()`'s normal
fall-through exit code is (0), after which a **passive update-check** notice
may print to stderr (skipped only after `--purge`/`--upgrade`, not skipped
after export/import).

---

## 5. `migrations.py` — one-time data migrations

Docstring's own contract, verbatim (this is the exact behavioral contract
every migration function must satisfy):

> Each migration:
> - is **idempotent** and **self-guarded** (safe to run twice, safe even if
>   the state file is missing or corrupt),
> - returns `True` when it *completed* (runner records it as applied),
> - returns `False` when it was *skipped / not applicable* (runner records
>   nothing, so a later-restored backup can still trigger it),
> - raises `MigrationIncomplete` (or any other exception) when it *partially
>   failed* — the runner logs it and leaves it unmarked so the next run
>   retries.

### 5.1 State file

Path: `<backup_dir>/.migrations.json`. Format:
```json
{"version": 1, "applied": {"windows_keyring_to_files": "<iso-timestamp>"}}
```
`STATE_VERSION = 1` (this file's own schema version — **unrelated** to the
`.cswap` export `FORMAT_VERSION`, and never itself checked/enforced on read
— `_load_applied` never inspects `data["version"]`, only `data["applied"]`).

- `_load_applied(switcher)`: missing file → `{}`. Any parse failure
  (`JSONDecodeError`, `OSError`, `UnicodeDecodeError`) → `{}` (never raises —
  "a corrupt file can never permanently block a migration"). Non-dict
  `applied` value → `{}`.
- `_mark_applied(switcher, migration_id)`: re-loads current applied map,
  sets `applied[migration_id] = get_timestamp()`, writes atomically via
  `tempfile.mkstemp(dir=state_path.parent, suffix=".tmp")` +
  `os.write`/`os.close`/`os.replace` (**not** `shutil.move` — a distinct
  atomic-write helper from the two others in this codebase, but same net
  effect), then `chmod 0o600` (non-Windows). On any exception mid-write: best
  effort `os.unlink(tmp_path)` cleanup, then re-raise. **Content preserves
  every previously-recorded migration** (re-reads before writing) — the
  runner marks one migration at a time, so concurrent writes across
  migrations in the same run don't clobber each other.

### 5.2 Migration function signature & registry

```python
MIGRATIONS: list[tuple[str, Callable[[ClaudeAccountSwitcher], bool]]] = [
    ("windows_keyring_to_files", migrate_windows_keyring_to_files),
    ("macos_keyring_to_security", migrate_macos_keyring_to_security),
]
```
Order matters *only* if migrations ever depend on each other (comment notes
this as a forward-looking concern; today they're platform-disjoint and
independent).

### 5.3 `migrate_windows_keyring_to_files`

**Purpose**: Windows used to store per-account *backup* credentials (not the
live/active credential — that's a separate store) in Windows Credential
Manager via the third-party `keyring` library, keyed under service
`KEYRING_SERVICE = "claude-code"` with per-account usernames
`account-{num}-{email}`. Windows now always uses base64 `.enc` files under
`credentials_dir` (Credential Manager rejects entries over ~2,500 bytes,
issue #45). This migration relocates any pre-existing entries.

**Skip conditions (return `False`, never marked)**:
- `switcher.platform != Platform.WINDOWS`.
- `sequence.json` doesn't exist yet ("No managed accounts yet — let a later
  restore migrate.").
- `sequence.json` exists but is unparseable (`_get_sequence_data()` returns
  `None`) — "Never mark applied: a user who repairs or restores it must
  still get the migration."

**Trivial-complete (return `True` immediately)**: `accounts` dict is empty
→ "Readable sequence, nothing to migrate → done."

**Backend-availability guard**: attempts `import keyring` and touches
`keyring.errors.PasswordDeleteError` (forces a broken backend to surface
here rather than later). Any exception →
```python
raise MigrationIncomplete(f"keyring backend unavailable, deferring Windows migration: {e}")
```
— explicitly **not** treated as "nothing to migrate" (that would
permanently skip real entries); forces a retry on next launch.

**Setup**: `credentials_dir.mkdir(parents=True, exist_ok=True)` +
`chmod(0o700)` (non-Windows only, so effectively a no-op on the real Windows
target, but exercised as written on POSIX test doubles) — "Existing Windows
keyring users may have sequence.json + configs but no credentials/ dir yet
(it never held files before this change)."

**Per-account relocation** (`email_counts = Counter(email for every account)`
computed once, up front, over **all** accounts — used for the
`account-None` disambiguation below):

For each `(account_num, info)` in `accounts.items()`:
1. `canonical = f"account-{account_num}-{email}"`,
   `none_user = f"account-None-{email}"`.
2. Read `keyring.get_password(KEYRING_SERVICE, canonical)`. On exception:
   log a warning, `failed += 1`, `continue` (this account is left for a
   retry; does **not** abort the whole migration).
3. **`account-None` fallback**: if `canonical` read empty/missing AND
   `str(account_num) != "None"` AND `email_counts[email] == 1` (the email is
   **unique** across all managed accounts) → also try
   `keyring.get_password(KEYRING_SERVICE, none_user)`; if found, `source_username
   = none_user`. This fallback exists for a legacy artifact where an
   account's keyring username was written with a literal `"None"` in place
   of the slot number (presumably from an older bug/format where the slot
   number wasn't known at write time) — it's only safely attributable to a
   slot when the email is unambiguous. On exception reading `none_user`: same
   warn/`failed += 1`/`continue` pattern.
   - When the email is **not** unique (two accounts share it, e.g. same
     email under two different orgs), `account-None-{email}` is **never**
     touched — left alone as an unattributable, ambiguous artifact (verified
     by test: neither slot receives it, and it is **not deleted**, "to avoid
     destroying possibly-only-copy data we can't attribute").
4. If still no `creds` found for this slot at all: **benign skip**, not a
   failure — `continue` (covers "added on the new version" and "ambiguous
   account-None we won't touch" cases).
5. **Write + verify before delete** (no unsafe window — old and new coexist
   until the new copy is proven):
   ```python
   switcher._write_account_credentials(account_num, email, creds)
   readback = switcher._read_account_credentials(account_num, email)
   ```
   On exception, or if `readback != creds` (byte-for-byte mismatch): log a
   warning, **`switcher._delete_account_credentials(account_num, email)`**
   (discard the just-written partial/garbage file so it can never shadow the
   still-intact keyring source), `failed += 1`, `continue`. The still-intact
   keyring source is left in place for the retry.
6. On success: `_delete_keyring_quietly(keyring, switcher, source_username)`
   (best-effort — swallows `keyring.errors.PasswordDeleteError` silently,
   logs any *other* exception as a warning but never raises), and if a
   distinct `none_user` fallback was also read (i.e. `source_username !=
   none_user` and `account_num != "None"`), also best-effort delete
   `none_user` — cleans up the redundant legacy entry even when the canonical
   one was the actual source. `migrated += 1`.

**Post-loop**:
- `migrated > 0` → stdout (**not** stderr — `print(..., file=sys.stderr)` is
  what's actually used here, so this *is* stderr; verify below) — exact
  message: `"claude-swap: migrated {migrated} Windows credential(s) from
  Credential Manager to files"` printed to **stderr**.
- `failed > 0` → `raise MigrationIncomplete(f"{failed} account(s) could not
  be migrated from Credential Manager; will retry on next run")` — this
  happens **after** processing every account (so successfully-migrated
  accounts in the same run stay migrated; only the migration-as-a-whole is
  left unmarked, meaning the *whole* migration retries next time, re-probing
  already-migrated accounts too — cheap since their keyring entries are
  already gone and the fallback logic naturally no-ops).
- Otherwise (`failed == 0`): `return True`.

### 5.4 `migrate_macos_keyring_to_security`

**Purpose**: macOS used to store per-account backup credentials via the
third-party `keyring` library (service `KEYRING_SERVICE = "claude-code"`,
hits the real Keychain under the hood). It now stores them via the
`security` CLI wrapper (`claude_swap.macos_keychain`) under
`SECURITY_SERVICE = "claude-swap"` — a **different service name in the same
Keychain**, so source and destination coexist safely during write → verify →
delete, identical safety shape to the Windows migration.

**Skip conditions (return `False`)**: `platform != Platform.MACOS`;
`sequence_file` doesn't exist; `sequence_file` unparseable
(`_get_sequence_data()` is `None`).

**Trivial-complete**: empty `accounts` dict → `True`.

**Pre-check** (this migration has a step the Windows one doesn't): before
touching `keyring` at all,
```python
pending = {num: info for num, info in accounts.items()
           if not switcher._kc_read_backup(num, info.get("email", ""))}
```
— reads the **`security`-service-only** backend directly (`_kc_read_backup`,
**not** the transparent `.enc`-wins read helper `_read_account_credentials`)
because this migration's job is specifically the Keychain, and a fallback
`.enc` file must never be mistaken for "already migrated" here. If **every**
account already has a security-service entry, `pending` is empty →
`return True` immediately, having **never imported or touched `keyring`
at all** (verified by test: `fake.get_calls == []`). A `KeychainError`/
`KEYCHAIN_ERRORS` raised during this pre-check (locked/denied/missing
Keychain) is **not** treated as "nothing to migrate" — re-raised as
`MigrationIncomplete(f"Keychain unavailable, deferring macOS keyring
migration: {e}")` (a genuine programming error is *not* caught here — only
`macos_keychain.KEYCHAIN_ERRORS`).

**Source backend selection** — the interesting divergence from Windows:
```python
keyring = None
try:
    import keyring as _keyring
    keyring = _keyring
except Exception as e:
    switcher._logger.warning("... keyring unavailable, using security fallback ...")
```
If `keyring` is importable, all legacy reads/deletes go through it
(silent, same-app). If it's **not** importable, legacy reads fall back to
`macos_keychain.get_password(KEYRING_SERVICE, username)` (the `security` CLI
against the *legacy service name*) — described as "dormant while `keyring`
is a dependency; it exists so a future `keyring` removal can't strand a
long-absent user."

`_read_old(username)` — prefers `keyring`, downgrades to the `security`
fallback **only** when the backend is judged genuinely unavailable via
`_keyring_backend_unavailable(keyring, exc)`:
```python
def _keyring_backend_unavailable(keyring, exc) -> bool:
    candidates = tuple of keyring.errors.{NoKeyringError, InitError} that exist as types
    return bool(candidates) and isinstance(exc, candidates)
```
Any **other** exception (e.g. a locked/denied Keychain prompt) is a real
failure and is **re-raised**, not downgraded — "a locked/denied Keychain
would hit `security` identically, so that must stay a genuine failure
(retry), not a fallback that just re-prompts." Once a fallback triggers,
`keyring` is set to `None` for the remainder of the run (subsequent reads all
use `security`).

`_delete_old(username)` — **only** deletes via `keyring` when `keyring is
not None`; in the keyring-unavailable fallback state, the legacy item is
**deliberately left behind** ("deleting it via `security` could raise a
*second* Keychain prompt... The orphan is harmless cruft (`purge` mops it
up)").

Per pending `(account_num, info)`, same shape as Windows §5.3 steps 1–4
(canonical-wins, `account-None` fallback gated on unique email, benign skip
on no creds found) but using `_read_old`/`canonical`/`none_user` instead of
raw `keyring.get_password`.

**Write + verify** uses the Keychain-only helpers (not the transparent
backup accessor):
```python
switcher._kc_write_backup(account_num, email, creds)
readback = switcher._kc_read_backup(account_num, email)
```
On failure or mismatch: warn, `switcher._delete_backup_keychain_quiet(account_num,
email)` (discard the bad security-service item so it can't shadow the still-
intact keyring source), `failed += 1`, `continue`.

**On success**: `_delete_old(source_username)`, and if a distinct
`none_user` fallback was used, `_delete_old(none_user)` too. Then a
**verification step Windows doesn't have**:
```python
if macos_keychain.item_exists(KEYRING_SERVICE, source_username):
    switcher._logger.warning(f"... {source_username} was left behind (delete failed or was denied); harmless — remove manually or via purge")
```
Rationale: `keyring`'s macOS backend raises the **same**
`PasswordDeleteError` both when an entry doesn't exist *and* when the user
**denies** the delete in a Keychain permission prompt — so without this
explicit existence check, a denied delete would be silently indistinguishable
from a successful/no-op one. `item_exists` is attribute-only (never
decrypts, never prompts). `migrated += 1` regardless of whether the orphan
check found a leftover — the copy itself already succeeded and is
authoritative.

**Post-loop**: `migrated > 0` → stderr: `"claude-swap: migrated {migrated}
macOS credential(s) from the keyring into the Keychain via security"`.
`failed > 0` → `raise MigrationIncomplete(f"{failed} account(s) could not be
migrated to the security service; will retry on next run")`. Else `return
True`.

### 5.5 Runner (`run_migrations`)

```python
def run_migrations(switcher) -> None:
    if not switcher.backup_dir.exists():
        return   # fresh-install no-op; preserves lazy-dir invariant
    applied = _load_applied(switcher)
    for migration_id, fn in MIGRATIONS:
        if migration_id in applied:
            continue
        try:
            completed = fn(switcher)
        except Exception as e:
            switcher._logger.warning(f"Migration {migration_id} did not complete (will retry): {e}")
            continue
        if completed:
            try:
                _mark_applied(switcher, migration_id)
            except Exception as e:
                switcher._logger.warning(f"Migration {migration_id} ran but recording it failed (will re-run next time): {e}")
```
**Never raises** — every migration function's exception (including
`MigrationIncomplete` and anything else) is caught, logged as a warning, and
the loop moves to the next migration. A migration that returns `False` is
simply left unrecorded (no log line at all — silent skip). `_mark_applied`
failing is itself caught and logged (leaves that migration to re-run next
time even though it logically completed this time — its side effects were
NOT rolled back, only the "applied" bookkeeping failed).

### 5.6 When migrations run (construction order)

In `ClaudeAccountSwitcher.__init__` (order matters for a Go port's
bootstrap sequence):
1. `self.home = Path.home()`; `self.platform = Platform.detect()`.
2. `self.backup_dir = get_backup_root()`.
3. `migrate_legacy_backup_dir(self.backup_dir)` — the **directory-layout**
   migration (§6.2), run **before** any logger or directory setup touches
   the new location. Prints `f"claude-swap: migrated data from {legacy} to
   {self.backup_dir}"` to stderr if it actually moved something.
4. `self.sequence_file/configs_dir/credentials_dir/lock_file` paths derived.
5. Logger set up (`setup_logging(self.backup_dir, ...)`), `UsageStore`
   constructed.
6. `self._store = CredentialStore(self)` — constructed **before**
   `run_migrations()` because the macOS migration performs storage ops
   through it.
7. `from claude_swap.migrations import run_migrations; run_migrations(self)`
   — the registry migrations (§5.3/§5.4) run **last**, lazily imported to
   avoid a circular import, and are fully self-contained/non-aborting.

Note that **directory creation itself is not guaranteed** at this point —
`run_migrations` no-ops if `backup_dir` doesn't exist yet, and neither
`__init__` nor `run_migrations` calls `_setup_directories()`. The org-field
migration (§6.1) and the registry migrations therefore only ever fire for a
`backup_dir` that some **other** operation already materialized (e.g. a
previous `add_account`, or — for import specifically — `import_accounts`
calling `_setup_directories()`/`_init_sequence_file()` itself in pass 2).

---

## 6. Related migrations NOT in `migrations.py` (essential context)

### 6.1 `sequence.json` org-field backfill (`switcher._migrate_org_fields`)

Not part of the `MIGRATIONS` registry, has no entry in `.migrations.json`,
and is **not** idempotency-tracked by a state file at all — it's a
self-guarded **on-read** migration re-evaluated every time it's invoked,
gated by `_get_sequence_data_migrated()`:
```python
def _get_sequence_data_migrated(self) -> dict | None:
    data = self._get_sequence_data()
    if not data: return data
    needs_migration = any("organizationUuid" not in acc for acc in data.get("accounts", {}).values())
    if needs_migration:
        self._migrate_org_fields()
        data = self._get_sequence_data()  # re-read after migration
    return data
```
**Both `export_accounts` and `import_accounts` call this** (export at the
very top on the source data; import on the local/destination data before
building the alias map) — so a Go port's export/import entry points must
run the equivalent backfill first, every time, not just at startup.

`_migrate_org_fields()` backfills `organizationUuid`/`organizationName` on
any account record from before org support existed (pre-v0.6.0
`sequence.json`, per the `sample_sequence_data_pre_v06` fixture — those
records only had `email`/`uuid`/`added`, no org fields at all):
- Reads the **live** `~/.claude.json`'s `oauthAccount` once, up front
  (`live_email`/`live_org_uuid`/`live_org_name`) — read failures are
  swallowed (`try/except Exception: pass`), leaving those blank.
- For each account already missing `organizationUuid`: if its `email`
  matches the live email (and live email is non-empty), backfill from the
  **live** config (rationale: "backup may lack org fields," live is
  authoritative for the active identity). Otherwise, backfill from that
  slot's own **backup** config file (`_read_account_config`), parsed as
  JSON; on any parse failure or absent config, both fields default to
  `""`.
- Skips (via `continue`) any account that already has `organizationUuid`
  present (even if it's `""`) — the migration is per-field-presence, not
  per-value.
- If anything changed: `data["lastUpdated"] = get_timestamp()`, write back
  via `_write_json`.

This migration is also invoked directly (not just via the `_migrated`
accessor) at the top of `add_account()`.

### 6.2 Legacy backup directory → XDG path (`paths.py::migrate_legacy_backup_dir`)

Also outside `migrations.py`; runs unconditionally at the very start of
every `ClaudeAccountSwitcher()` construction (§5.6 step 3), **before** the
registry migrations. Included here because a Go port's startup sequence
must reproduce this ordering and because it is the single most-triggered
migration for any real upgrading user on Linux/WSL.

- **Legacy root**: `~/.claude-swap-backup` (`LEGACY_BACKUP_DIRNAME`).
- **New root** (`get_backup_root()`): Linux/WSL only —
  `$XDG_DATA_HOME/claude-swap` if `XDG_DATA_HOME` is set, non-empty, and
  resolves (after `~` expansion) to an **absolute** path; else
  `~/.local/share/claude-swap`. macOS/Windows/`Platform.UNKNOWN` keep using
  the legacy path directly (`get_backup_root() == get_legacy_backup_root()`,
  so `migrate_legacy_backup_dir` is a same-path no-op there — this migration
  is Linux/WSL-only in practice).
- Guarded by a **flag file** `<target>.parent>/.{target.name}.migrating`
  (e.g. `~/.local/share/.claude-swap.migrating`) touched *before* the move
  and removed *after*, so an interrupted migration is distinguishable from a
  genuine collision on the next run:
  - flag absent, `legacy` doesn't exist → no-op (either already migrated, or
    never had legacy data).
  - flag present, `legacy` doesn't exist → a prior run completed the move
    but crashed before deleting the flag; just unlink the flag, return
    `False`.
  - flag present, `legacy` still exists → prior run was interrupted
    mid-move; if `target` exists it's discarded (`shutil.rmtree`) as
    potentially partial, then retry the move.
  - flag absent, both `legacy` and `target` exist: **genuine collision**.
    If `target` holds anything beyond "throwaway artifacts" (a `cache/`
    subdir, or files whose name starts with `claude-swap.log` — i.e.
    anything a prior *cswap run itself* might have laid down before legacy
    data reappeared, e.g. synced in from another machine) →
    `raise MigrationError(f"Both legacy ({legacy}) and new ({target}) backup
    paths exist. Refusing to merge or overwrite — inspect both and remove
    the stale one manually before re-running.")`. If `target` holds **only**
    throwaway artifacts, they're wiped (`_wipe_throwaway_artifacts`) and the
    migration proceeds.
  - Otherwise: `target.parent.mkdir(parents=True, exist_ok=True)`; touch
    flag; `shutil.move(legacy, target)` (atomic rename on same filesystem,
    copy+unlink across filesystems); unlink flag. Any `OSError` during this
    →`MigrationError(f"Migration of {legacy} → {target} failed: {exc}")`.
- Returns `True` only when an actual move happened in *this* call (used
  solely to decide whether to print the "migrated data from X to Y" stderr
  notice).
- `MigrationError` (a distinct exception class from `MigrationIncomplete`)
  propagates all the way out of `ClaudeAccountSwitcher.__init__` — it's
  caught by the CLI's top-level `ClaudeSwitchError` handler like any other
  init-time failure (exit 1, `"Error: ..."`), **not** swallowed/retried like
  the registry migrations.

---

## 7. External-system knowledge encoded here (verbatim, high-value)

- **`~/.claude.json`'s relevant shape** (Claude Code's own live config file):
  only the top-level `oauthAccount` key is ever read back by a switch/export;
  export's default slim mode is built entirely around this fact
  (`_slim_config`). `oauthAccount` itself is expected to carry (at least)
  `emailAddress`, `accountUuid`, `organizationUuid`, `organizationName`.
- **Claude Code credential shapes**: OAuth credentials are a JSON object
  (elsewhere described as `{"claudeAiOauth": {...}}` at the live-credentials
  layer, though the *exported* `credentials` field here is the inner OAuth
  JSON object directly, e.g. `{"accessToken", "refreshToken", "expiresAt"}`).
  A managed **API key** credential is a bare string prefixed `sk-ant-api`
  (never JSON) — `looks_like_api_key()` in `credentials.py` is the single
  classifier used by both export and import.
- **Windows Credential Manager entry-size limit**: ~2,500 bytes — the
  documented reason (issue #45) the Windows backend abandoned Credential
  Manager for base64 `.enc` files entirely, motivating
  `migrate_windows_keyring_to_files`.
- **`keyring` library error semantics on macOS**: `PasswordDeleteError` is
  raised **both** for "entry doesn't exist" and for "user denied the Keychain
  delete prompt" — indistinguishable without a follow-up `item_exists()`
  check via the `security` CLI. This is why
  `migrate_macos_keyring_to_security` does an explicit post-delete
  verification that Windows's migration doesn't need (Credential Manager has
  no equivalent "prompt the user" semantics for delete).
- **XDG Base Directory Specification** (cited directly in `paths.py`):
  `$XDG_DATA_HOME` must be **ignored** when unset, empty, or set to a
  non-absolute path, falling back to `~/.local/share`. `claude-swap`'s data
  dir under that spec is `$XDG_DATA_HOME/claude-swap`.
- **Base64 encoding of on-disk credential backups**: every `.enc` file under
  `credentials_dir` is `base64.b64encode(credentials_text.encode("utf-8"))`
  — a reversible, non-cryptographic obfuscation layer only (explicitly *not*
  encryption — matches the module-level docstring's "No encryption is built
  in" disclaimer for the export format too).

---

## 8. Edge cases & subtleties (from tests)

- **Round-trip identity is `(email, organizationUuid)`, never the exported
  slot number.** A hand-edited envelope can claim any `number`; import only
  uses it as an allocation *hint* when the account doesn't already exist
  locally (`test_force_overwrites_existing_slot_in_place`,
  `test_slot_allocation_when_exported_slot_taken`).
- **`--force` overwrites the local matching slot *in place*, never the
  exported slot number.** Concretely: local slot 3 = alice, local slot 1 =
  bob (unrelated); an envelope claiming alice was slot 1 elsewhere, imported
  with `--force`, still updates **slot 3** and leaves bob at slot 1 fully
  untouched (`test_force_overwrites_existing_slot_in_place`).
- **Fresh-slot allocation never fills gaps** — `max(existing) + 1`, matching
  `add_account`'s own allocator exactly (`test_slot_allocation_when_exported_slot_taken`).
- **Cross-backend transparency**: exporting from a macOS (Keychain-backed)
  switcher and importing into a Linux (file-backed) switcher works
  transparently — the `.cswap` envelope is entirely backend-agnostic
  (`test_export_macos_keychain_import_linux_files`).
- **Path traversal defense is airtight even on validation failure mid-list**:
  a second account in the envelope with `email="../../evil"` fails
  validation, and the **first** (valid) account in the same envelope is
  verified to have **zero** files written anywhere, including checking the
  parent directories one and two levels up
  (`test_path_traversal_email_rejected`,
  `test_malformed_later_account_does_not_partial_write`).
- **`bool` is explicitly excluded from the "number" int check** —
  `isinstance(raw_number, bool)` is checked *before* `isinstance(raw_number,
  int)` specifically because Python's `bool` is an `int` subclass; without
  this guard `"number": true` would pass as slot `1`. A Go port using
  `encoding/json` into `interface{}` gets this correctness "for free" (a
  JSON `true` unmarshals to Go `bool`, never `float64`), but should still
  write an explicit type-switch test for it since Go's dynamic typing
  differs in mechanism, not just outcome.
- **Alias self-collision is not a collision**: re-importing (with `--force`)
  an account that already carries the *same* alias locally is allowed —
  collision detection compares against the **owning identity**, not just
  string equality of the alias (`test_import_reexport_of_same_account_keeps_own_alias`).
- **A hand-edited envelope can smuggle a duplicate alias** that `export`
  itself could never produce (export-time uniqueness is enforced at
  set-time) — import must still catch it
  (`test_import_duplicate_alias_within_export_rejected`).
- **`--full` vs. default is a hard privacy boundary**, not just a size
  optimization: `userID`, `anonymousId`, `projects`, `tipsHistory`,
  `cachedGrowthBookFeatures`, `appleTerminalBackupPath`, `numStartups` (the
  exact bloat-config test fixture) are all verified **absent** from a
  default-mode export and **all present** verbatim under `--full`.
- **Missing `oauthAccount` at export time is a hard failure even for the
  slim path**, never silently omitted — `TransferError` with "missing
  oauthAccount" (`test_export_missing_oauthAccount_raises`).
- **Bulk export tolerates individually-broken slots; single-account export
  does not** — same missing-credentials condition is a stderr-warned skip in
  one mode and a hard `CredentialReadError`/`ConfigError` in the other
  (`TestExportSkipsBrokenSlots`, issue #41).
- **A broken *active* slot with no live session to fall back on** silently
  drops out of both the accounts array *and* `activeAccountNumber` (set to
  `null`, not left dangling) — `test_skipped_active_slot_clears_envelope_active`.
- **stdout pipe mode is byte-for-byte pure JSON** even when a slot was
  skipped with a warning — the warning is unconditionally stderr regardless
  of destination (`test_stdout_pipe_mode_keeps_stdout_pure_json`).
- **Importing into a completely uninitialized `$HOME`** (no backup dir at
  all, `sequence.json` doesn't exist) self-bootstraps via
  `_setup_directories()` + `_init_sequence_file()` inside `import_accounts`
  itself (`TestEmptyHome`).
- **`--force` overwriting the backup of the *currently live logged-in*
  account** prints the activation hint pointing at `cswap --switch-to <slot>
  --force`; this hint is **absent** when the live login isn't among the
  written slots this run (`TestConflictPolicy` live-login tests). It fires
  for *both* "overwrote" and "imported" outcomes, not just overwrite
  (`written_slots` tracks both).
- **Session-mode interaction**: force-overwriting an account with an
  **inactive** session profile deletes `.credentials.json` from the profile
  but preserves `.claude.json` (profile history); with a **live** (PID-alive)
  session profile, `.credentials.json` is left completely untouched and a
  warning is printed instead, but the import itself still succeeds and the
  backup is still updated (`TestImportSessionInvalidation`).
- **Dead-token quarantine interactions are three-way**: (1) `--force` over
  an existing quarantined slot rewrites creds and unconditionally lifts the
  quarantine; (2) re-importing (no `--force`, fresh slot) after a `cswap
  remove` that left an orphaned `usage.json` row for that *slot number*
  **also** lifts it, because `clear_dead_token` is unconditional on every
  successful write regardless of `imported` vs. `overwrote`; (3) a **plain
  skip** (no `--force`, account already exists) touches *no* credential
  material and therefore leaves the quarantine verdict completely
  untouched, while printing an extra hint line only when that specific slot
  is actually quarantined (`TestImportClearsDeadTokenQuarantine`, issues
  #136/#138).
- **Migration `account-None` disambiguation is a three-way outcome**:
  canonical entry always wins when present; `account-None-{email}` is used
  as a fallback **only** when the slot's email is globally unique across all
  managed accounts; when the email is shared by ≥2 slots, `account-None` is
  left completely alone (migrated into **neither** slot, and **not
  deleted**) since it can't be safely attributed
  (`TestAccountNoneFallback`).
- **A denied Keychain delete during the macOS migration is silently
  survivable** (the migration still completes and returns `True`) but is
  logged as a warning containing the literal phrase `"left behind"` and the
  orphaned username — this is only detectable via the explicit
  `macos_keychain.item_exists` post-delete check, since `keyring` itself
  can't distinguish "already gone" from "denied"
  (`test_denied_legacy_delete_warns_but_completes`).
- **A locked/denied *source* keyring during the macOS migration does NOT
  fall back to `security`** — only a genuinely *unavailable backend*
  (`NoKeyringError`/`InitError`) triggers the `security` fallback; a runtime
  error from a locked Keychain is a hard failure that retries next run
  (`test_locked_keyring_does_not_fall_back`).
- **An unusable *destination* (security-service) Keychain during the macOS
  migration's pre-check must defer, not misclassify or write around it** —
  a `KeychainError` from `_kc_read_backup` during the pending-accounts
  pre-check raises `MigrationIncomplete` before any `.enc` file or Keychain
  write happens (`test_security_keychain_unusable_defers_and_writes_no_enc`).
- **Migration idempotency is enforced at the runner level via short-circuit,
  not by the migration function re-checking its own state** — a second
  `run_migrations()` call after a successful first pass makes **zero**
  additional keyring calls at all (`test_idempotent_second_run_touches_no_keyring`);
  contrast with the macOS migration's *own* pre-check (§5.4), which is a
  belt-and-suspenders optimization for the *first* run when some accounts
  are already migrated (e.g. mixed old/new installs) and is orthogonal to
  the runner's applied-state short-circuit.
- **A partially-failing migration migrates everything it safely can and
  reports failure only for the broken subset** — one bad account among
  several doesn't block the others, and the *whole migration* stays
  unmarked (not "partially marked") so the healthy accounts get harmlessly
  re-probed (cheaply, since their keyring source is already gone) on the
  next run (`test_partial_failure_migrates_rest_and_stays_unmarked`).
- **`purge()` performs a *separate*, best-effort sweep of leftover legacy
  keyring/Credential-Manager entries** independent of the migration state
  file — relevant because a user who never triggers a normal migration path
  (e.g. purges immediately) can still have their legacy entries cleaned up
  (`TestPurgeWindows`). This is outside `migrations.py` proper (lives in
  `switcher.purge()`) but is the other consumer of the same
  `KEYRING_SERVICE`/`SECURITY_SERVICE` constants.

---

## 9. Go port notes

- **No threading/goroutines in either module.** Both `transfer.py` and
  `migrations.py` are single-threaded, synchronous, run-to-completion
  functions. `run_migrations` runs once, synchronously, inside
  `ClaudeAccountSwitcher.__init__` — a Go port's equivalent constructor
  should do the same (not spawn a background goroutine — the whole point is
  that migrations must be *done* before any read/write of managed accounts
  proceeds).
- **No cross-process locking in `transfer.py` or `migrations.py`.** The
  codebase *has* a `FileLock` (`locking.py`, fcntl on POSIX / `msvcrt` on
  Windows, default 10s timeout) used extensively elsewhere in `switcher.py`
  (`add_account`, `remove_account`, `switch`, `switch_to`, etc.), but
  **neither `export_accounts` nor `import_accounts` nor any migration
  function acquires it.** Import's pass-2 loop does a plain read-modify-write
  of `sequence.json` per account with no lock held across the read and the
  write — concurrent `cswap import` invocations, or an import racing a
  `cswap switch`, are **not** safe against interleaving in the current
  Python implementation. A Go port should either (a) faithfully preserve
  this gap for behavioral parity, or (b) deliberately improve it by taking
  `FileLock` around each write (or the whole import) — but this is a real
  design decision to flag to the porting team, not an accidental omission to
  silently "fix" without discussion, since observable behavior (e.g. timing
  of partial-import states) could shift.
- **Lazy `import keyring` inside migration functions is load-bearing for
  testability**, not just style: the test suite explicitly notes that
  patching `claude_swap.switcher.keyring` would **not** affect
  `migrations.py`, because it does its own local `import keyring` — tests
  inject a fake module via `sys.modules`. This is pure Python-ism; a Go port
  has no equivalent lazy-import mechanism and should instead take the
  keyring/Credential-Manager access as an injectable interface (already the
  natural Go idiom) — no special handling needed beyond normal DI, but call
  out that the *test* strategy (module-level fake injection) has no direct
  Go analog and should be redesigned as constructor injection instead of
  transliterated.
- **Two atomic-write helpers with near-identical but not-identical
  mechanics coexist** in the codebase: `transfer.py`'s `_atomic_write_file`
  (temp path via `Path.with_suffix(f".{pid}.tmp")`, plain `write_text` +
  `shutil.move`) vs. `migrations.py`'s `_mark_applied` (temp path via
  `tempfile.mkstemp`, `os.write`/`os.close`/`os.replace`) vs.
  `switcher._write_json` (temp path via `Path.with_suffix`, content
  validated by re-parsing before the move) vs. `credentials.py`'s
  `_atomic_b64_write` (`tempfile.mkstemp` again). All four achieve the same
  *outcome* (atomic same-directory rename, 0600 final perms on POSIX,
  cleanup-on-failure), so a Go port can and should **unify these into one
  helper** (e.g. write-to-`os.CreateTemp`-in-same-dir +
  `os.Chmod(0o600)` + `os.Rename`) rather than reproduce four subtly
  different Python code paths. The one behaviorally-relevant divergence to
  preserve: `switcher._write_json` **round-trips the freshly written temp
  file through a JSON parse before committing** ("Generated invalid JSON" →
  `ConfigError`, unlinking the temp file) as a self-check that none of the
  other three helpers perform — worth keeping as a defensive check in
  whichever unified helper replaces it, or at least applying to the
  `sequence.json` write path specifically.
- **Windows/non-Windows perms branching is a straight `sys.platform !=
  "win32"` guard repeated at every write site** (`os.chmod(..., 0o600)` /
  `0o700` skipped entirely on Windows, which has no POSIX mode bits). A Go
  port should gate the equivalent `os.Chmod` calls behind `runtime.GOOS !=
  "windows"` (or simply let `os.Chmod` no-op/degrade gracefully on Windows,
  if the chosen Go idiom tolerates that — Windows `os.Chmod` in Go does
  partially respect the read-only bit via mode `0200`, which is *not* the
  same as POSIX 0600 semantics, so an explicit platform guard mirroring the
  Python is the safer choice).
- **`Platform.detect()` deliberately avoids `platform.system()`/`uname()`
  on Windows** (documented reason: `uname()` triggers a WMI query that can
  hang indefinitely when the WMI service is slow) and uses `sys.platform ==
  "win32"`/`"darwin"` directly instead. A Go port's platform detection
  (`runtime.GOOS`) has no equivalent WMI-hang risk and needs no special
  handling here — noted only so the port doesn't "helpfully" reintroduce a
  slower detection path.
- **JSON number decoding gotcha for a Go port specifically**: Python's
  `json.loads` gives back a real `int` for `"number": 1`, so `isinstance(x,
  int)` is a meaningful, exact check. Go's `encoding/json` unmarshaling into
  `interface{}` (or a loosely-typed map) always yields `float64` for JSON
  numbers — a Go port validating the imported `number`/`activeAccountNumber`
  fields must explicitly check "is this float64 an exact non-negative
  integer with no fractional part" (e.g. `f == math.Trunc(f)` plus a range
  check) to reproduce the Python behavior of rejecting `1.5`, huge
  out-of-`int` values, `NaN`/`Inf` (not valid JSON anyway), etc. Prefer
  decoding into a typed struct with `json.Number` or an explicit `int`
  field via `json.Unmarshal` with `UseNumber()` to get closer to Python's
  strictness for free.
- **`str(raw_number)` for `exported_num`**: Python's `str()` on a validated
  positive `int` is always a clean base-10 decimal with no sign, no leading
  zeros, no exponent. A Go equivalent (`strconv.Itoa`/`FormatInt`) matches
  this exactly once the value is confirmed to be an integer per the note
  above — no special-casing needed.
- **The `bool`-is-an-`int` quirk for `activeAccountNumber`** (§3.4): in
  Python, `envelope.get("activeAccountNumber")` being JSON `true` passes
  `isinstance(x, int)` and stringifies via `str(True) == "True"`, which then
  trivially fails to equal any real `exported_num` string — a functionally
  inert quirk (it can never accidentally match a real slot, since
  `exported_num` is always a decimal-digit string) but worth a one-line
  regression test in Go to confirm the port's type handling produces an
  equally inert (never-matching) outcome for a boolean
  `activeAccountNumber`, rather than, say, a Go panic on type assertion.
- **Ordering-sensitive migration bootstrap** a Go port's
  `NewSwitcher()`/equivalent constructor must reproduce exactly: (1) resolve
  `home`/`platform`; (2) resolve `backupDir` via the XDG-aware path
  function; (3) run the legacy-backup-dir-relocation migration (§6.2) — this
  can return a hard error that aborts construction, unlike everything after
  it; (4) derive `sequenceFile`/`configsDir`/`credentialsDir`/`lockFile`
  paths; (5) set up logging/usage-store; (6) construct the credential-store
  abstraction; (7) run the registry migrations (§5), which must **never**
  abort construction — every error is caught, logged, and left for retry.
  Getting steps (3) and (7) swapped, or making (7) fallible, would be an
  observable behavioral regression.
- **The org-field backfill (§6.1) has no persistent "applied" marker at
  all** — unlike the registry migrations, it's cheap enough (and safe
  enough, being purely additive/idempotent per-field) to just re-check
  `"organizationUuid" not in acc` on every `_get_sequence_data_migrated()`
  call. A Go port can reproduce this as a plain function called at the top
  of the Go equivalents of `export_accounts`/`import_accounts`/`add_account`
  — no state file, no registry entry needed for it.
- **Decide explicitly, per the task's framing, which registry migrations the
  Go port needs to *keep* running vs. can retire**, given users may upgrade
  from *any* prior Python version: both `windows_keyring_to_files` and
  `macos_keyring_to_security` exist to rescue data from storage backends
  (`keyring` library / Windows Credential Manager) that a **from-scratch Go
  binary would never have used in the first place** — a Go port has no
  legacy `keyring`-backed installs of its own to migrate *from*. However,
  a user upgrading **from an old Python `claude-swap` release straight to
  the new Go binary** may still be sitting on exactly this legacy state (an
  old Windows Credential Manager entry, or an old-format macOS `keyring`
  entry under service `"claude-code"`) — so the Go port likely needs to
  **keep both migrations' read/relocate logic** (reading `keyring`/
  Credential-Manager-equivalent APIs one more time to pull any leftover
  legacy credential into the new binary's file/Keychain-`security` store),
  even though the Go binary itself will never *write* to those legacy
  backends. The **`.migrations.json` state-tracking format itself should be
  preserved** (same path, same `{"version":1,"applied":{...}}` shape) so a
  machine that has already run these migrations under the Python tool
  doesn't redundantly re-probe legacy backends after switching to the Go
  binary — the Go port's runner should honor an existing `.migrations.json`
  it did not itself write. The legacy-backup-dir migration (§6.2) similarly
  must be kept: any Linux/WSL user still on the pre-XDG layout when they
  switch to the Go binary is exactly the long-upgrade-gap case this whole
  system exists to protect, and the Go binary is the one that must perform
  the one-time relocation for them since the Python tool may never run
  again on that machine.
