# claude-swap — Switching & Reporting Spec (`switcher.py`)

## Overview

This spec covers the SWITCHING and REPORTING surface of `ClaudeAccountSwitcher`
(`src/claude_swap/switcher.py`): the `switch` (rotate-to-next), `switch_to`
(by number/email/alias, with `--force`), and the usage-aware `best` /
`next-available` strategies; the physical mechanics of a switch (which files and
Keychain entries are written, in what order, under which locks, with rollback);
the `list_accounts` and `status` human-readable renderers and their `--json`
payloads; the usage-data pull path that feeds `list`/`status` (a shared,
identity-guarded, poll-planned usage store fed by parallel OAuth fetches);
`disable`/`enable`; the `upgrade` self-upgrade command (installer detection);
and exit codes / error envelopes. Account add/remove/import internals are out of
scope (covered by another spec) except where switching reads them. All exact
strings, JSON keys, numeric constants, file paths, and external-system contracts
below are the source of truth — omitting any is a port bug.

---

## 1. Data model, files, and identity

### 1.1 Backup root & files

`ClaudeAccountSwitcher.__init__` derives (`paths.py`):

- `backup_dir` = `get_backup_root()`:
  - **Linux/WSL**: `$XDG_DATA_HOME/claude-swap` if `XDG_DATA_HOME` is set, non-empty,
    and (after `~` expansion) absolute; else `~/.local/share/claude-swap`.
  - **macOS/Windows/UNKNOWN**: `~/.claude-swap-backup` (legacy layout).
- On Linux/WSL, `migrate_legacy_backup_dir(backup_dir)` moves a legacy
  `~/.claude-swap-backup` into the XDG path (no-op on macOS/Windows where they are
  equal). If it actually moved, prints to **stderr**:
  `claude-swap: migrated data from {legacy} to {backup_dir}`.
- `sequence_file` = `backup_dir/sequence.json`
- `configs_dir` = `backup_dir/configs`
- `credentials_dir` = `backup_dir/credentials`
- `lock_file` = `backup_dir/.lock` (the cswap account lock; `FileLock`)
- usage cache = `backup_dir/cache/usage.json` (via `UsageStore(backup_dir/"cache")`)

Per-account backup file names:

- Credentials (Linux/WSL/Windows always; macOS fallback): `credentials_dir/.creds-{account_num}-{email}.enc` (base64 of the raw credential string).
- Config: `configs_dir/.claude-config-{account_num}-{email}.json` (raw `~/.claude.json` text of that account).
- macOS Keychain backup item: service `claude-swap` (`SECURITY_SERVICE`), account name `account-{account_num}-{email}`.

Directory permissions: `_setup_directories()` creates `backup_dir`, `configs_dir`,
`credentials_dir` with `mkdir(parents=True, exist_ok=True)` and `chmod 0o700`
(skipped on `win32`). JSON files are written `0o600` (temp file chmod, then atomic
`shutil.move`). Config backups also `chmod 0o600`.

### 1.2 `sequence.json` schema

```json
{
  "activeAccountNumber": 1,
  "lastUpdated": "2026-07-17T12:00:00Z",
  "sequence": [1, 2, 3],
  "accounts": {
    "1": {
      "email": "a@example.com",
      "uuid": "acct-uuid",
      "organizationUuid": "",
      "organizationName": "",
      "added": "2026-01-01T00:00:00Z",
      "alias": "dev",
      "kind": "api_key",
      "disabled": true
    }
  }
}
```

- `sequence` is a sorted list of **int** slot numbers; it drives rotation and list order and is kept sorted everywhere.
- `accounts` keys are **string** slot numbers.
- `activeAccountNumber` is an **int** or `null`.
- `lastUpdated` = `get_timestamp()` = `datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")`.
- Optional per-account fields: `alias` (present only when set), `kind` (`"api_key"`; absent/other reads as `"oauth"`), `disabled` (present only as `true` when parked out of rotation).
- Org migration: `_get_sequence_data_migrated()` backfills `organizationUuid`/`organizationName` for accounts lacking `organizationUuid`, reading the live `~/.claude.json` for the active account and the per-account backup config otherwise.

### 1.3 Live Claude Code files (external contract — see `paths.py`)

- Config home: `CLAUDE_CONFIG_DIR` if set, else `~/.claude`.
- Global config: `<config_home>/.config.json` if it exists (legacy), else `(CLAUDE_CONFIG_DIR || $HOME)/.claude.json`. **Asymmetry**: `.claude.json` sits at home root by default, not inside `.claude/`.
- Credentials file: `<config_home>/.credentials.json`.
- Active identity is `~/.claude.json`'s `oauthAccount.emailAddress` + `oauthAccount.organizationUuid` (`""` for personal).

### 1.4 Account identity / resolution

- Identity key is the composite `(email, organizationUuid)`. `_find_account_slot(data, email, org_uuid)` matches on both (org compared as `account.get("organizationUuid","") == org_uuid`).
- `_resolve_account_identifier(identifier)` precedence: **number → alias → email**.
  - Pure-digit string returns itself unchanged (note: not normalized, so `"01"` stays `"01"` here; `move_account`/`swap` normalize separately).
  - Alias match is case-insensitive (`account.alias.lower() == identifier.lower()`); empty alias never matches.
  - Email that matches multiple accounts raises `ConfigError`:
    `Email '{identifier}' is ambiguous — matches accounts: {num [OrgName|personal], ...}. Use account number instead (e.g., cswap --switch-to 1).`
- Alias validation (`models.normalize_alias`): lowercased+stripped; must be non-empty, not purely numeric, not start with `-`, and match `^[a-z0-9_.-]+$`. Violations raise `ValueError` (wrapped as `ValidationError` by callers).

---

## 2. Constants & thresholds (exact)

From `switcher.py`, `poll_policy.py`, `usage_store.py`, `credentials.py`, `claude_locks.py`, `update_check.py`, `oauth.py`:

| Constant | Value | Meaning |
|---|---|---|
| `_FETCH_STAGGER_S` | `0.25` | Delay `idx * 0.25s` before each parallel usage fetch start |
| `_USAGE_AGE_NOTE_S` | `= SERVE_TTL_S` = `180.0` | Show `· Xm ago` age note when served data older than this |
| `SETUP_TOKEN_SCOPES` | `("user:inference",)` | Scopes stamped on add-token OAuth blobs |
| `SERVE_TTL_S` | `180.0` | Freshness floor: younger → serve without fetching |
| `MIN_INTERVAL_S` | `180.0` | Poll cadence floor |
| `URGENT_INTERVAL_S` | `60.0` | Active account moving inside escalation band |
| `ACTIVE_MAX_INTERVAL_S` | `300.0` | Decay ceiling, active |
| `CANDIDATE_DEFAULT_INTERVAL_S` | `300.0` | Default cadence, idle candidate |
| `CANDIDATE_MAX_INTERVAL_S` | `600.0` | Decay ceiling, candidate |
| `MOVEMENT_DELTA_PCT` | `1.0` | Binding-pct delta that counts as "moving" |
| `JITTER_FRAC` | `0.1` | ± cadence jitter (tests zero it) |
| `EDGE_BACKOFF_S` | `300.0` | Backoff after `Retry-After: 0` |
| `POST_429_MIN_INTERVAL_S` | `360.0` | Cadence floor while a 429 is recent |
| `RECENT_429_WINDOW_S` | `3600.0` | How long a 429 stays "recent" |
| `ESCALATION_MARGIN_PCT` | `15.0` | Escalation band width |
| `RESET_SLACK_S` | `60.0` | Never poll later than window reset + slack |
| `STALE_OK_S` | `300.0` | Decision-grade trust window for last-good |
| `CLAIM_TTL_S` | `10.0` | In-flight claim window |
| `TRUST_MAX_AGE_S` | `3600.0` | Hard ceiling on trust-extended staleness |
| `BACKOFF_BASE_S` / `BACKOFF_CAP_S` | `30.0` / `600.0` | Failure backoff `30·2^(n-1)` capped |
| `RETRY_AFTER_FLOOR_CAP_S` | `900.0` | Cap honoring server Retry-After |
| `AUTH_DEAD_STRIKES` | `1` | `invalid_grant` count → quarantine |
| `PERMANENT_AUTH_ERRORS` | `{"invalid_grant"}` | Errors advancing the dead-token strike |
| `KEYCHAIN_RECHECK_COOLDOWN_S` | `60.0` | Long-running-process Keychain re-probe |
| `_ACTIVE_READ_ATTEMPTS` / `_ACTIVE_READ_RETRY_DELAY` | `2` / `0.3` | Active OAuth Keychain read retry |
| `STALENESS_S` (claude locks) | `10.0` | proper-lockfile stale threshold |
| `TOUCH_INTERVAL_S` | `3.0` | Lock mtime touch cadence |
| `DEFAULT_TIMEOUT_S` | `9.0` | Claude Code lock acquire timeout |
| `FileLock` default timeout | `10.0` | cswap account lock acquire timeout |
| `OAUTH_EXPIRY_BUFFER_MS` | `5*60*1000` | Token treated expired this early |
| `CACHE_TTL` (update check) | `24*3600` | PyPI version cache TTL |
| PyPI request timeout | `2s` | Update check |
| Usage API request timeout | `5s` | `request_usage_data` |

External URLs:
- Usage API: `https://api.anthropic.com/api/oauth/usage`, headers `Authorization: Bearer <token>`, `anthropic-beta: oauth-2025-04-20`, `User-Agent: claude-swap/1.0`.
- OAuth profile: `https://api.anthropic.com/api/oauth/profile`.
- OAuth token endpoint: `https://platform.claude.com/v1/oauth/token`.
- PyPI: `https://pypi.org/pypi/claude-swap/json`.

---

## 3. Locking model

Three lock layers, all cooperating; **never hold any lock across network I/O**.

1. **cswap account lock** — `FileLock(self.lock_file)` = `backup_dir/.lock`.
   POSIX `fcntl.flock(LOCK_EX|LOCK_NB)` retried every `0.1s` up to `10s`; Windows
   `msvcrt.locking(LK_NBLCK,1)`. On timeout `__enter__` raises
   `LockError("Failed to acquire lock - another instance may be running")`.
   **Non-reentrant** — never re-acquire within the same held span (v0.7.3 deadlock
   history). Guards every mutation of `sequence.json` and per-account backups.

2. **Claude Code advisory locks** (`claude_locks.py`) — mirror npm `proper-lockfile`:
   - Lock artifact is a **directory** `<target>.lock`: `~/.claude.lock`
     (credentials, dir = `<config_home>` parent + `<config_home>.name + ".lock"`)
     and `~/.claude.json.lock` (global config). `mkdir` atomicity is the mutex.
   - **Staleness = 10s**; live holders `os.utime` the dir every `3s` (a daemon thread);
     a lock whose mtime is older than 10s may be `rmdir`'d and retaken.
   - Acquire waits up to `9s`, sleeping `0.25 + rand*0.25s` between attempts; on
     timeout raises `ClaudeCodeLockTimeout` (a `LockError` subclass) — **nothing is
     mutated when this raises**; safe to retry.
   - Held during the whole switch mutation (incl. rollback) via
     `with FileLock(self.lock_file), claude_credentials_lock(), claude_config_lock():`
     so a mid-refresh Claude Code either finishes first (backup captures rotated
     token) or re-reads under the lock and aborts its own refresh.

3. **Usage store lock** — `FileLock(cache/.usage.lock)`, held only for
   read-modify-write of `cache/usage.json` (atomic replace); reads are lock-free.

The usage store follows the protocol: (a) lock→read→reserve/claim (stamp
`lastAttemptAt`)→unlock; (b) fetch with no lock; (c) lock→merge→write→unlock.

---

## 4. `switch(strategy=None, json_output=False, models=(), model_source=None)`

Rotate to the next account, or run a usage-aware strategy. Returns the switch
JSON payload (§10.3) in JSON mode, else `None`.

`strategy_label = strategy if strategy in ("best","next-available") else "rotation"`.
If `strategy_label == "rotation"`, `models` is forced to `()` (model limits only
steer usage-aware strategies). If `models` and not JSON: prints
`Using configured model limits: {', '.join(models)}` plus ` (from {source})`
where `source = "--model"` when `model_source=="cli"` else `model_source`.

**Precondition**: `sequence.json` must exist, else `ConfigError("No accounts are managed yet")`.

Then `_get_sequence_data_migrated()` runs (org backfill).

### 4.1 Fresh-machine path (no live login)

If `_get_current_account()` is `None` (no `~/.claude.json` oauthAccount email):

- `preferred = activeAccountNumber` or, if falsy and `sequence` non-empty, `sequence[0]`.
- If no `preferred`: `ConfigError("No accounts are managed yet")`.
- If `preferred` is disabled or not switchable, skip it:
  - Disabled → reason `(disabled)`.
  - Not switchable → console reason `(no stored credentials/config, re-add with cswap --add-account --slot {target})`; JSON warning `Skipped Account-{target} (no stored credentials/config)`.
  - JSON: append `Skipped Account-{target} {reason}` to warnings; human: `{accent('Skipping')} Account-{target} {console_reason}`.
  - Fallback = first `num != target` in `sequence` that is enabled and switchable.
  - If no fallback:
    - Some slot is switchable (all remaining disabled) → `ConfigError("No accounts remain in rotation. Re-enable one with: cswap enable <num|email>")`.
    - Else → `ConfigError("No managed accounts have valid stored credentials/config. Re-add a slot with: cswap --add-account --slot <number>")`.
- Calls `_perform_switch(target, emit_output=not json_output)`; returns `_switch_result_from_op(op, strategy_label, warnings)` in JSON mode.
- **Strategies are ignored on this path** (documented).

### 4.2 Unmanaged live account

If the live `(email, org_uuid)` is not a managed account:

- **JSON**: no auto-add. Returns `_switch_noop(strategy=strategy_label, reason="unmanaged-account", from_ref=to_ref=account_ref(None, current_email), message="Active account is not managed; run cswap --add-account")`.
- **Human**: prints `{accent('Notice:')} Active account '{current_email}' was not managed.`, calls `add_account()`, then prints `It has been automatically added as Account-{activeAccountNumber}.` and `Please run the switch command again to switch to the next account.`; returns `None`.

### 4.3 Only one account

If `len(sequence) < 2`:
- JSON: `_switch_noop(reason="only-one-account", to_ref=account_ref(int(num), current_email) if num else None, message="Only one account is managed. Add more accounts to switch between.")`.
- Human: prints `Only one account is managed. Add more accounts to switch between.` (dimmed), returns `None`.

### 4.4 Anchor & current identity

- `active_account = data["activeAccountNumber"]`.
- `current_num = _find_account_slot(data, current_email, current_org_uuid)`; if `None`, fall back to `str(active_account)` (or `None`).
- `current_ref = account_ref(int(current_num), current_email)` when `current_num`.

### 4.5 Strategy `best` (§5 for scoring)

`best_usage = _usage_by_account()`; `_warn_inert_models(best_usage, models, json_output, warnings)`;
`target, note = _select_best_switchable(current_num, models, best_usage)`.

- `target is not None` → `_perform_switch(target, emit_output=not json_output)`; result via `_switch_result_from_op(op, strategy_label, warnings)`.
- Otherwise the `note` maps to a no-op (all stay on `current_num`), each with human + JSON forms:

| note | JSON reason | Human message |
|---|---|---|
| `current-unavailable` | `usage-unavailable` | `Current account usage is unavailable — staying on Account-{n}. Run cswap --switch to rotate.` |
| `no-comparison` | `usage-unavailable` | `No other account has usage data to compare — staying on Account-{n}. Run cswap --switch to rotate.` |
| `incomplete-comparison` | `usage-unavailable` | `No account with known usage has more remaining quota; some usage is unavailable — staying on Account-{n}.` |
| `stay` | `already-best` | `Already on the account with the most remaining quota (Account-{n}).` (accent, human) |
| `exhausted` | `candidates-exhausted` | `All accounts are at their {limits_label} — staying on Account-{n}.` (`warning()`) |
| `none` | (falls through to rotation) | — |

`limits_label` = `"usage limits"` if `models` else `"5h/7d limit"`. JSON messages
mirror human wording (without the "Run cswap --switch to rotate." tail on the
already-best/exhausted cases; the current-unavailable/no-comparison JSON drop the
tail too — see verbatim strings in code lines 3596–3672).

### 4.6 Rotation & `next-available`

`anchor = current_num if strategy=="next-available" else active_account` (usage-aware
rotation anchors on live account to avoid self-no-ops under drift; plain rotation
keeps anchoring on `active_account` byte-for-byte). `current_index = sequence.index(int(anchor))`,
falling back to `sequence.index(active_account)` then `0` on `TypeError/ValueError`.

`usage = _usage_by_account() if strategy=="next-available" else {}`; for `next-available`
also `_warn_inert_models(...)`.

Loop `offset` in `1..len(sequence)-1`, `candidate = str(sequence[(current_index+offset) % len])`:
- Disabled → skip. JSON warning `Skipped Account-{candidate} (disabled)`; human `{accent('Skipping')} Account-{candidate} (disabled)`.
- Not switchable → skip. JSON `Skipped Account-{candidate} (no stored credentials/config)`; human `{accent('Skipping')} Account-{candidate} (no stored credentials/config, re-add with cswap --add-account --slot {candidate})`.
- `next-available`: `headroom = oauth.account_headroom(usage.get(candidate), models)`; if `headroom is not None and headroom <= 0`, mark exhausted-skip. `label = "5h/7d"` by default; with `models`, `label = "/".join(name for name, pct, _ in oauth.relevant_windows(...) if pct >= 100.0)` if any (names the binding window, e.g. `Fable`, `5h/Fable`). JSON warning `Skipped Account-{candidate} (at {label} limit)`; human `{accent('Skipping')} Account-{candidate} (at {label} limit)`.
- Otherwise `next_account = candidate`, break.

After the loop:
- `next_account is None` and some were `skipped_exhausted` → stay: JSON `_switch_noop(reason="candidates-exhausted", to_ref=current_ref, message="All other accounts are at their {limits_label} — staying on Account-{n}.")`; human `warning("All other accounts are at their {limits_label} — staying on Account-{n}.")`.
- `next_account is None` (no exhausted) → JSON `_switch_noop(reason="no-valid-target", to_ref=current_ref, message="No other accounts have valid stored credentials/config.")`; human prints (dimmed) `No other accounts have valid stored credentials/config.\nRe-add a skipped slot with: cswap --add-account --slot <number>`.

### 4.7 Self-switch guard on rotation

If `next_account == current_num` (rotation anchored on a drifted `activeAccountNumber`
can land on the live slot): `action, provenance = _self_switch_action(next_account, current_email)`.
- `action != "reconcile"` → no-op. JSON `_switch_noop(reason="already-active", from_ref=to_ref=current_ref, message="Already on Account-{n} ({email})")`; human `{accent('Already on')} Account-{n} ({email})`.
- `action == "reconcile"` → fall through with `provenance`.

Finally `_perform_switch(next_account, emit_output=not json_output, provenance=provenance)`;
returns `_switch_result_from_op(op, strategy_label, warnings)`.

---

## 5. Usage-aware scoring (`_select_best_switchable`, headroom)

`oauth.account_headroom(usage, models)` = `100 - max(pct over relevant_windows)`;
`None` when usage is not a dict / carries no window data (treated as "unknown",
never auto-skipped). `relevant_windows` = always `("5h", five_hour.pct, resets_at)`
and `("7d", seven_day.pct, resets_at)`; when `models` non-empty, each named
`scoped` window (case-insensitive display-name match; `"all"` sentinel matches
every scoped window). `spend` (pay-as-you-go) is **excluded**.

`_select_best_switchable(current_num, models, usage)`:

1. `others` = every `sequence` slot `!= current_num` that is switchable and not disabled.
   Empty → `(None, "none")`.
2. `usage = _usage_by_account()` if not passed.
3. `current_headroom = account_headroom(usage[current_num], models)`. `None` → `(None, "current-unavailable")` (can't prove any target better; never move onto worse/unverifiable).
4. `scored = [(account_headroom(usage[num], models), num) for num in others]`; `known = [(h,num) for ...  h is not None]`. Empty → `(None, "no-comparison")`.
5. `best_headroom, best_num = max(known, key=headroom)` — **first maximal wins**, and `known` preserves rotation (sequence) order, so **ties resolve to the earliest slot**.
6. `best_headroom > current_headroom` → `(best_num, "")` **switch** (strictly greater only).
7. Else current is ≥ every measured candidate → stay:
   - any candidate's headroom is `None` → `(None, "incomplete-comparison")` (can't claim best/exhausted).
   - `current_headroom <= 0` → `(None, "exhausted")`.
   - else → `(None, "stay")`.

`_warn_inert_models(usage, models, json_output, warnings)`: one-shot `--model` typo
guard. Only fires when **every** account's usage is a dict (an unreadable account
could carry the window). `wanted` = model names (excluding `all`, lowercased). `seen`
= all `scoped[].name` across accounts. `missing` names → message
`model(s) {', '.join(missing)} match no account's usage windows (typo?)`; appended to
`warnings` (JSON) or printed via `warning()` (human).

---

## 6. `switch_to(identifier, json_output=False, force=False)`

Switch to a specific account by NUM/EMAIL/ALIAS. Returns switch JSON (§10.3) or `None`.

- Precondition: `sequence.json` exists, else `ConfigError("No accounts are managed yet")`. Runs org migration.
- Identifier resolution:
  - Non-digit and not a known alias and not a valid email → `ValidationError("Invalid account identifier: {identifier}")`.
  - Ambiguous email in **human** mode (not alias): prints `Multiple accounts found for '{identifier}':` and one line per match `  {num}: {identifier} [{tag}]`, then `input("Enter account number to switch to: ")`; non-digit or non-matching → prints `Cancelled` (dimmed), returns `None`. In **JSON** mode no prompt — falls through to `_resolve_account_identifier`, which raises the ambiguous-email `ConfigError` → error envelope.
- `target_account = _resolve_account_identifier(identifier)`; not found → `AccountNotFoundError("No account found with identifier: {identifier}")`. If not in `accounts` → `AccountNotFoundError("Account-{target_account} does not exist")`.

### 6.1 Already-active short-circuit (issue #79 / #117)

If **not** `force` and there is data and the live identity resolves to `target_account`:
`action, provenance = _self_switch_action(target_account, live_email)`.
- If still `target_account` and `action != "reconcile"` → no-op:
  - Human: `{accent('Already on')} Account-{n} ({email})` then dimmed
    `To rewrite the live login from the stored backup (e.g. after --import), run: cswap --switch-to {n} --force`; returns `None`.
  - JSON: `_switch_noop(strategy="direct", reason="already-active", from_ref=to_ref=account_ref(int(n), email), message="Already on Account-{n} ({email})")`.
- `reconcile` falls through (so `_perform_switch` reconciles a resolved divergence).

`--force` **skips this guard entirely** — its job is to rewrite the live login from
the stored backup.

### 6.2 Perform & force reason rewrite

`op = _perform_switch(target_account, emit_output=not json_output, force_activate=force, provenance=provenance)`;
`result = _switch_result_from_op(op, "direct")` (JSON only).

If JSON, `force`, and `not result["switched"]` (i.e. a **forced self-activation**):
overwrite `result["reason"]="activated"` and
`result["message"]="Activated Account-{to.number} ({to.email}) from stored backup"`.
A **cross-slot** force stays `switched:true` / `reason:"switched"`.

---

## 7. Self-switch provenance helpers

- `_live_matches_slot_backup(slot, email)`: `True` when live credential equals the slot's
  backup byte-for-byte **or** `oauth.credential_fingerprint(live) == fingerprint(backup)`.
  Unreadable/empty live → `True` (keep the no-op). Missing backup → `False`.
- `_self_switch_action(slot, email)` → `(action, provenance)`:
  - matches backup → `("noop", None)`.
  - diverged, then `_prefetch_live_identity()`; if `resolved is None` → `("noop-diverged", None)` (silent pre-fix no-op, logged INFO); else `("reconcile", provenance)`.
- `_prefetch_live_identity()` (runs **before** locks, may hit network): returns
  `{"live": str|None, "resolved": dict|None}`. Reads live creds; if equal/fingerprint-equal
  to slot backup, returns unresolved (local provenance established). Else extracts the
  access token and calls `oauth.fetch_oauth_profile(access_token)` → `resolved` = `{uuid, email, organizationUuid}`-ish. All failures swallowed (advisory oracle must never fail a switch).

---

## 8. `_perform_switch(target_account, emit_output=True, force_activate=False, provenance=None)`

Returns `{"from": ref|None, "to": ref, "warnings": [...]}`. `emit_output=False` (JSON
mode) suppresses all human prints; the live-session warning rides back in `warnings`.

### 8.1 Pre-lock steps

1. **Session-mode drift warning** (warn, never block): if the target slot has live
   `cswap run` session PIDs, message:
   `Account-{n} ({email}) has a live session-mode Claude instance (PID {ids}). Running the same account as both the default login and a session can make one copy's token go stale if the server rotates it. If the session later fails to authenticate, exit it and re-run 'cswap run {n}'.`
   → `warning()` (human) or appended to `warnings_out`.
2. **Provenance**: if `provenance is None`, set to `{"live": None, "resolved": None}` when
   `force_activate`, else `_prefetch_live_identity()`.

### 8.2 Under the triple lock (`FileLock(lock_file), claude_credentials_lock(), claude_config_lock()`)

Re-reads sequence data, resolves `current_account` (from live identity, falling back to
recorded `activeAccountNumber`), `target_email`, `to_ref`.

**Direct-activation path** — taken when `force_activate` OR `current_identity is None`
OR `current_account is None` (fresh machine / unmanaged live / --force):

- `from_ref`: `None` (fresh machine) / `account_ref(None, live_email)` (unmanaged live) / `account_ref(int(current_account), live_email)` (--force with managed login).
- Read `target_creds` / `target_config`; missing creds → `SwitchError("Account-{n} has no stored credentials. Re-add with: cswap --add-account --slot {n}")`; missing config → `SwitchError("Account-{n} has no stored config backup. Re-add with: cswap --add-account --slot {n}")`; bad JSON → `SwitchError("Invalid backup config: {exc}")`; missing `oauthAccount` → `SwitchError("Invalid oauthAccount in backup")`.
- Snapshot live state for rollback when a live identity exists: `rollback_creds = _read_credentials()`; `None` → `CredentialReadError("Cannot snapshot live credentials before activation")`; config read failure → `ConfigError("Cannot snapshot live config before activation: {e}")`.
- **Invariant II stash** (issue #117): if `rollback_creds` exists, differs from `target_creds`, and there is a live identity, `_stash_live_credential(rollback_creds, "displaced-live-login", current_account or "unmanaged", None)`. On stash failure: without `--force` → `SwitchError("Could not preserve the live credential before activation (safety-copy write failed: {e}); aborting rather than destroying it")`; with `--force` → warn `Could not preserve the replaced live credential (safety-copy write failed: {e}) — proceeding because --force explicitly rewrites the live login.` and proceed.
- Write order (each recorded for rollback): `_write_credentials(target_creds)` → splice `oauthAccount` into existing `~/.claude.json` (preserving local settings/projects) or write full imported config when none → set `activeAccountNumber` + `lastUpdated` and `_write_json(sequence_file)`. On any exception, roll back config then credentials (best-effort, logged), re-raise.
- Logs `Activated account {n} (forced, backup of current login skipped)` or `Activated account {n} (no prior live account)`.
- Human: `{accent('Activated')} Account-{n} ({target_email})`, blank line, `_print_switch_followup()`, blank line.
- `_replan_new_active(...)`, return `{"from": from_ref, "to": to_ref, "warnings": warnings_out}`.

**Normal switch path** — a managed live login exists:

- `from_ref = account_ref(int(current_account), current_email)`.
- Read `original_creds = _read_credentials()`; `None` → `CredentialReadError("Failed to read current credentials")`; empty string → `CredentialReadError("Current account credential is empty (Keychain unreadable?); refusing to overwrite its backup")` (a Keychain timeout returns `""`, must NOT clobber the departing backup). `original_config = config_path.read_text()`; missing → `ConfigError("Claude config file not found")`; permission → `ConfigError("Permission denied reading Claude config")`.
- Build `SwitchTransaction(original_credentials, original_config, original_account_num=current_account, original_email, config_path)`.
- **Step 1 — back up the outgoing slot**, classified by `_classify_outgoing_credential` (§9):
  - `foreign` / `alien` → `_stash_live_credential(original_creds, kind, current_account, resolved)` (raises on failure → aborts before overwriting live), then warn:
    - `foreign`: `Credential ownership mismatch detected. The live credential was preserved and was not written into Account-{current}. If Account-{foreign_slot} later cannot authenticate, log in as it and run: cswap add --slot {foreign_slot}`.
    - `alien`: `The live login does not match a managed account. It was preserved and not written into Account-{current}. If you need that account, log in as it and run: cswap add`.
  - `foreign-synced` → warn only (no write): `Credential ownership mismatch detected. The live credential already matches Account-{foreign_slot}'s stored backup, so nothing was written into Account-{current}.`
  - `unresolved` → **pre-fix backup**: `_write_account_credentials(current, email, original_creds)` + `_write_account_config(...)`, INFO-log only (no warning).
  - `own-bytes` → config-only backup: `_write_account_config(...)`, INFO `Backed up account {current} (config only; credentials unchanged)`.
  - `own-family` / `own-rotated` → `_write_account_credentials(...)` + `_write_account_config(...)`. For `own-rotated`, backfill a missing slot `uuid` from `resolved.uuid`.
- **Step 2** — read `target_creds`/`target_config`; missing → same `SwitchError` messages as above.
- **Step 3** — `_write_credentials(target_creds)`; record step `credentials_written`.
- **Step 4** — splice `oauth_section = target_config["oauthAccount"]` (missing → `SwitchError("Invalid oauthAccount in backup")`) into the current `~/.claude.json`; write; record `config_written`.
- **Step 5** — `activeAccountNumber = int(target_account)`, `lastUpdated`, `_write_json(sequence_file)`; record `sequence_updated`.
- On any exception: if `transaction.completed_steps`, `transaction.rollback(self)` (reverse order: restore credentials, restore config text + chmod 0600, restore `activeAccountNumber`). Success → `SwitchError("Switch failed and was rolled back: {e}")`; failure → `SwitchError("Switch failed and rollback also failed: {e}. Manual recovery may be needed.")`. No completed steps → re-raise original.

### 8.3 After the lock releases (network-safe display)

If `emit_output`: `{accent('Switched to')} Account-{n} ({target_email})`, then a nested
`self.list_accounts()` (post-switch usage display; on exception logs warning and prints
`  (usage display unavailable — run cswap --list to retry)`), blank line,
`_print_switch_followup()`, blank line. Then `_replan_new_active(...)` regardless of
`emit_output`, and return `{"from", "to", "warnings"}`.

### 8.4 `_print_switch_followup()`

Keyed to where the active credential write landed (`_last_active_credentials_backend`,
fallback `"keychain" if _use_keychain() else "file"`):
- `keychain`: `Restart Claude Code to apply immediately — otherwise the session can take up to ~30 seconds to pick up the new account.`
- `file`: `New account is active on your next message — no restart needed.`

### 8.5 `_replan_new_active(number, email, org_uuid)`

Best-effort (switch already committed; failure logged as
`Post-switch poll re-plan failed (switch itself succeeded): {e}`). Pulls the just-activated
account's poll plan to the active floor: `next_poll = max(now, entry.fetched_at + MIN_INTERVAL_S)`,
only ever pulled earlier (never later); a never-measured account is left plan-less.

### 8.6 `_switch_result_from_op` / `_switch_noop`

`_switch_result_from_op(op, strategy, extra_warnings=None)`:
`switched = op["from"] != op["to"]`. If switched → `reason="switched"`,
`message="Switched to Account-{to.number} ({to.email})"`; else `reason="already-active"`,
`message="Already on Account-{to.number} ({to.email})"`. Payload keys: `schemaVersion`,
`switched`, `from`, `to`, `strategy`, `reason`, `message`, `warnings` (= `extra_warnings + op["warnings"]`).

`_switch_noop(strategy, reason, message, from_ref=None, to_ref=None, warnings=None)`:
`switched=False`, `from_ref` defaults to `to_ref`, so every no-op reports `from == to`.

---

## 9. `_classify_outgoing_credential` (issue #117 identity oracle)

Returns `(kind, foreign_slot|None)`; the switch-time backup uses it to decide whether
the live credential bytes may be written into the outgoing slot. Precedence:

1. `backup == original_creds` → `own-bytes`.
2. `fingerprint(backup) == fingerprint(original_creds)` → `own-family` (access token rotated, same lineage).
3. `provenance.resolved is None` **or** `provenance.live != original_creds` (bytes moved since prefetch) → `unresolved` (fail open — exact pre-fix backup).
4. Outgoing-slot **uuid** match: `resolved.uuid == own.uuid` (and org agrees when both recorded) → `own-rotated`.
5. Find slot by `(resolved.email, resolved.org)`; drop it if both sides carry a uuid and they conflict (recycled email); fall back to org-scoped uuid match. If that slot `== current_account` → `own-rotated`.
6. No slot: `alien` only if a **structurally complete** identity (`resolved.email` and `organizationUuid is not None`); else `unresolved`.
7. Cross-slot must be **uuid-positive**: if `not r_uuid or stored_uuid != r_uuid` → `alien`.
8. If the foreign slot's backup already holds this lineage (`==` or fingerprint) → `foreign-synced`; else → `foreign`.

`_stash_live_credential(original_creds, reason, current_account, resolved)` writes a
write-only "unclaimed credentials" safety copy (base64) via `self._store`, capturing
`{reason, configSlot, fingerprint, liveOauthAccount, resolvedIdentity, credentialsMtime}`;
raises on write failure (a successful stash is the license to overwrite live). Logs a
WARNING. These are surfaced only in JSON `--list` (`unclaimedCredentials`) and logs,
never in the human list.

---

## 10. JSON output schemas (verbatim)

`SCHEMA_VERSION = 1`. All payloads carry `"schemaVersion": 1`. The CLI serializes with
`json.dumps(payload, indent=2)` — commands print nothing themselves.

### 10.1 `--list --json` (`_build_list_payload`)

```json
{
  "schemaVersion": 1,
  "activeAccountNumber": 1,
  "accounts": [ <account_row>, ... ]
}
```

Empty when `sequence.json` absent: `{"schemaVersion":1,"activeAccountNumber":null,"accounts":[]}` (never prompts).

Additive top-level fields (present only when non-empty):
- `"duplicateAccountWarnings"`: `[str,...]` (`_duplicate_account_warnings`).
- `"lockstepUsageWarnings"`: `[str,...]` (`_lockstep_usage_warnings`).
- `"unclaimedCredentials"`: sorted `[str,...]` of stash entry ids.

**`account_row`** (`json_output.account_row`):

```json
{
  "number": 1,
  "email": "a@example.com",
  "organizationName": "",
  "organizationUuid": "",
  "isOrganization": false,
  "active": true,
  "usageStatus": "ok",
  "usage": { <usage projection> | null },
  "alias": "dev",
  "disabled": true,
  "usageFetchedAt": "2026-07-17T12:00:00Z",
  "usageAgeSeconds": 12.3
}
```

- `alias` present only when non-empty; `disabled` present only when `true`.
- `usageFetchedAt`/`usageAgeSeconds` present only alongside a non-null `usage` (and only when `fetched_at` is not None). `usageFetchedAt` = UTC ISO8601 seconds precision with `Z` suffix; `usageAgeSeconds` = `round(age_s, 1)`.
- `usageStatus` ∈ `{"ok","token_expired","api_key","keychain_unavailable","relogin_required","no_credentials","unavailable"}`. JSON carries the **decision-grade** value (`entry.decision_value()`): last-good only while `age_s <= STALE_OK_S` or `trust_extended` (≤ `TRUST_MAX_AGE_S`); older → `usage:null` / `usageStatus:"unavailable"` even though the human view still shows the numbers with an age note.

**`usage_fields` mapping**: dict→`("ok", usage_to_json)`; `USAGE_TOKEN_EXPIRED`→`("token_expired",None)`;
`USAGE_API_KEY`→`("api_key",None)`; `USAGE_KEYCHAIN_UNAVAILABLE`→`("keychain_unavailable",None)`;
`USAGE_RELOGIN_REQUIRED`→`("relogin_required",None)`; any other str→`("no_credentials",None)`; `None`→`("unavailable",None)`.

**`usage_to_json`** projects the internal usage dict (camelCase; sub-keys emitted only when present):

```json
{
  "fiveHour": {"pct": 25.0, "resetsAt": "<iso>", "countdown": "4h", "clock": "02:00"},
  "sevenDay": {"pct": 16.0},
  "spend": {"used": 12.5, "limit": 300.0, "pct": 4.0, "currency": "USD", "resetsAt": "<iso>", "countdown": "...", "clock": "..."},
  "scoped": [{"name": "Fable", "pct": 100.0, "resetsAt": "<iso>", "countdown": "3h", "clock": "21:59"}]
}
```

`countdown`/`clock` are **recomputed from `resets_at` at serialization time** via
`oauth.fresh_reset_strings` (drift-correct); entries without `resets_at` fall back to
the cached fetch-time `clock`/`countdown`; unparseable `resets_at` also falls back.
Windows without any reset carry only `pct`.

### 10.2 `--status --json` (`_build_status_payload`)

- No live login: `{"schemaVersion":1,"active":null}`.
- Live but unmanaged (no sequence data, or slot not found): `{"schemaVersion":1,"active":{"email":"<email>","managed":false}}`.
- Managed:

```json
{
  "schemaVersion": 1,
  "active": {
    "number": 1,
    "email": "a@example.com",
    "organizationName": "",
    "organizationUuid": "",
    "isOrganization": false,
    "managed": true,
    "usageStatus": "ok",
    "usage": { ... | null },
    "alias": "dev",
    "usageFetchedAt": "...",
    "usageAgeSeconds": 1.0
  },
  "totalManagedAccounts": 2
}
```

`alias` present only when set. Freshness fields present only alongside non-null `usage`.
Same decision-grade projection as list.

### 10.3 `--switch` / `--switch-to --json` (switch result)

```json
{
  "schemaVersion": 1,
  "switched": true,
  "from": {"number": 1, "email": "a@example.com"},
  "to": {"number": 2, "email": "b@example.com"},
  "strategy": "rotation",
  "reason": "switched",
  "message": "Switched to Account-2 (b@example.com)",
  "warnings": []
}
```

- `strategy` ∈ `{"rotation","best","next-available","direct"}`. `switch_to` uses `"direct"`; `switch` uses `strategy_label`.
- `reason` ∈ `{"switched","already-active","activated","unmanaged-account","only-one-account","usage-unavailable","already-best","candidates-exhausted","no-valid-target"}`.
- `account_ref(number, email)` = `{"number": int|null, "email": str}`. `number` is `null` for an unmanaged live `from`.
- `from`/`to` may be `null` in specific no-ops (e.g. only-one-account with unresolved slot, no-valid-target with no `current_ref`).
- **CLI adds** (in `cli.py`, after the call) when `models` is non-empty: `payload["models"] = list(models)` and `payload["modelSource"] = model_source`. These are *not* set by `switcher.py`.

### 10.4 Error envelope

```json
{
  "schemaVersion": 1,
  "error": {"type": "SwitchError", "message": "boom"}
}
```

`type` = the exception class name (`type(exc).__name__`); `message` = `str(exc)`.

---

## 11. Human rendering — `list_accounts`

`list_accounts(show_token_status=False, json_output=False, fetch=None)`.

- No `sequence.json`: JSON → empty payload; human → prints `No accounts are managed yet.` (dimmed) then `_first_run_setup()` (prompts to add current account; may add).
- Otherwise: `accounts_info = _build_accounts_info()`, `entries = _collect_usage_entries(accounts_info, fetch=fetch)`; JSON → `_build_list_payload`; human render:

```
Accounts:
  1: dev (test@example.com) [personal] (active)
     ├ 5h:  10%   resets 20:39  in 1h 30m
     └ 7d:  50%   resets Jul 5 08:59  in 1d 19h
     • oauth: fresh, refresh token yes        # only with show_token_status

  2: account2@example.com [Acme Corp] (disabled)
     no credentials
```

- Header `Accounts:` (bold).
- Row: `  {num}: {label} {muted('['+tag+']')}{markers}` where
  `label = f"{accent(alias)} ({email})"` if alias else `email`;
  `tag = org_name or "personal"`;
  markers append ` {bold_accent('(active)')}` when active and ` {muted('(disabled)')}` when disabled (in that order).
- Usage lines from `_usage_entry_lines(entry)`, each printed indented 5 spaces (`     {line}`).
- `show_token_status`: after usage lines, `     • {token_status}` from `oauth.build_token_status(creds)` when non-empty.
- Blank line between accounts (not after the last).
- Then duplicate/lockstep warnings (blank line, then each via `warning()`). Unclaimed credentials are **deliberately not shown** in the human list.
- **Running instances** block (from `process_detection.get_running_instances()`; exceptions swallowed/logged):
  ```
  Running instances:
    ● {label}   {cwd}  (2 sessions, IDE)
  ```
  Grouped by `(entrypoint_label, abbreviate_path(cwd))`; session labels via `entrypoint_label` (`cli`→`CLI`, `claude-vscode`→`VS Code`, `claude-desktop`→`Desktop`, `sdk-*`→`SDK`, `mcp`→`MCP`, `local-agent`→`Agent`, `remote`→`Remote`); IDE names via `ide_short_name` (`Visual Studio Code`→`VS Code`); parts joined `"{n} session[s]"` and/or `"IDE"`.

### 11.1 `_usage_entry_lines(entry)` (styled, indent-less)

- **Sentinel present**: first line `dimmed(SENTINEL_NOTES.get(sentinel, sentinel))`; if a `last_seen_note` exists and sentinel is not `USAGE_API_KEY`, append `{dimmed('└')} {muted(last_seen)}`.
  - `SENTINEL_NOTES`:
    - `token expired` → `token expired — Claude Code refreshes the active account`
    - `api key` → `API key (no quota)`
    - `keychain unavailable` → `keychain unavailable — locked or in use; try again`
    - `re-login needed` → `re-login needed — refresh token dead; log in with Claude Code, then run: cswap add`
    - (`no credentials` has no entry → renders the raw sentinel string `no credentials`.)
- **Measurement (`last_good`) present**: `lines = _format_usage_lines(last_good)`; if the served data is older than `_USAGE_AGE_NOTE_S` (=180s) and `fetched_at` set, append ` · {format_age(int(fetched_at*1000))}` to the **last** line. Then each line becomes `{dimmed('├' or '└')} {muted(line)}` (`└` on the last).
- **Neither**: `dimmed("usage unavailable")` plus ` ({last_error})` when a last error exists.

`last_seen_note(entry)` = `last seen {100-headroom:.0f}% used · {format_age(int(fetched_at*1000))}` when `last_good` and `fetched_at` are present and headroom is computable, else `None`.

`format_age(started_at_ms)` (`printer.py`): `< 60s → "just now"`, `< 3600 → "{m}m ago"`, `< 86400 → "{h}h ago"`, else `"{d}d ago"`.

### 11.2 `_format_usage_lines(usage)` (raw text; column alignment)

Builds `(label, body)` rows then left-pads every label to the widest + `:`. Rows in
order: `spend` (`$$`), `5h`, `7d`, then each `scoped` window (by its `name`).

- Spend row (`$$`): `f"{pct:>3.0f}%   resets {clock:<12}  ${used:,.2f} / ${limit:,.2f}"` when a reset cell exists, else `f"{pct:>3.0f}%   ${used:,.2f} / ${limit:,.2f}"`.
- 5h/7d row: `f"{pct:>3.0f}%   resets {clock:<12}  in {countdown}"` with a cell, else `f"{pct:>3.0f}%"`.
- scoped row: `marker = "  (!)"` when `pct >= 100` else `""`; with a cell `f"{pct:>3.0f}%   resets {clock:<12}  in {countdown}{marker}"`, else `f"{pct:>3.0f}%{marker}"`.
- Final format per row: `f"{label + ':':<{width}} {body}"` where `width = max(len(label)) + 1`.

Reset `(countdown, clock)` comes from `oauth.fresh_reset_strings(window)`:
recompute from `resets_at` (`oauth.format_reset`), else cached `clock`/`countdown`,
else `None`. `format_reset`: `countdown` = `"{d}d {h}h"` / `"{h}h {m}m"` / `"{m}m"`;
`clock` (local time) = `"%H:%M"` same-day, else `"%b {day} %H:%M"`.

Examples verified by tests:
- `usage={"five_hour":{"pct":7.0,"clock":"20:39","countdown":"1h 30m"}}` → `["5h:   7%   resets 20:39         in 1h 30m"]`.
- Scoped alignment: labels pad to `Fable:`, so `%` columns align: `"5h:      0%"`, `"7d:     62%   resets Jul 5 08:59"`, `"Fable: 100%   resets Jul 5 08:59"`.
- `{"scoped":[{"name":"Fable","pct":100.0}]}` → `["Fable: 100%  (!)"]`.

---

## 12. Human rendering — `status`

`status(json_output=False)`:
- No live login: `{bolded('Status:')} {dimmed('No active Claude account')}` → `None`.
- No sequence data / unmanaged slot: `{bolded('Status:')} {current_email} {dimmed('(not managed)')}`.
- Managed:
  ```
  Status: Account-1 (test@example.com [personal])
    Total managed accounts: 2
    ├ 5h:  25%   resets ...  in ...
    └ 7d: ...
  ```
  Header line `{bolded('Status:')} {accent('Account-'+n)} ({email} {muted('['+tag+']')})`,
  `  {dimmed('Total managed accounts: '+total)}`, then `_usage_entry_lines(entry)` each indented 2 spaces.

`_active_account_usage(account_num, current_email, org_uuid)` builds a single-account
info row and runs the shared collector (`_collect_usage_entries([info])`), so freshness/
backoff/claim gating and the shared `cache/usage.json` behave exactly as in list.

---

## 13. Usage data pull for list/status (`_collect_usage_entries`)

Feeds both human and JSON reporting. Shared, identity-guarded, poll-planned.

1. Build `identities = {num: (email, org_uuid)}` and `info_by_num`.
2. **Static sentinels** (`_static_usage_sentinel`, re-derived every pass, never persisted):
   - API-key creds (`looks_like_api_key`) → `USAGE_API_KEY`.
   - No creds / no access token → `USAGE_KEYCHAIN_UNAVAILABLE` if active-and-keychain-unavailable, else `USAGE_NO_CREDENTIALS`.
   - Active + locally expired token + an owner running (Claude Code or a live session) → `USAGE_TOKEN_EXPIRED` (must be visible even when the fetch is gated).
3. **Dead-token quarantine**: for slots without a static sentinel whose `entry.token_dead()` (≥ `AUTH_DEAD_STRIKES` `invalid_grant` strikes) → `USAGE_RELOGIN_REQUIRED` (also stops the endless 401/429 fetch loop).
4. `requested` = slots with no sentinel and (`fetch is None` or in `fetch`).
5. `to_fetch = store.reserve(requested, identities, respect_plans=(fetch is None))` — atomic eligibility+claim (§ usage_store).
6. If any to fetch: `_run_usage_fetches` (parallel), `store.record(...)`, refresh `entries`, `_persist_poll_plans(...)`, then re-check `token_dead` post-fetch (surface `USAGE_RELOGIN_REQUIRED` same pass on a fresh `invalid_grant`).
7. Return `{num: with_sentinel(entries[num], sentinels.get(num))}`.

`_run_usage_fetches`: `ThreadPoolExecutor().map`, each fetch delayed `idx * _FETCH_STAGGER_S`
(0.25s) so N accounts never burst the endpoint in one instant.

**Active vs inactive fetch routing** (`_fetch_account_usage`):
- Active/default account → `_fetch_active_usage`: only refreshes the token when **no owner** is detected (`_active_cc_running()` or a live `cswap run` session). With an owner + expired token → `USAGE_TOKEN_EXPIRED` (would 401). Provenance guard (issue #117): only refresh when the live bytes lineage-match the stored backup; on mismatch, read usage as-is (don't consume a generation). No-owner refresh persists the rotated credential to **both** the active store and the backup (never holding the cswap lock across the network refresh; the persist callback re-acquires `FileLock` + Claude Code locks and re-checks owner/refresh-token lineage before writing; on a mid-refresh owner-appears or lineage change, discards and returns `USAGE_TOKEN_EXPIRED`).
- Inactive account → prefers a live **session profile** credential if fresh (read-only, no refresh); expired-with-live-session → `USAGE_TOKEN_EXPIRED`; drifted profile identity → falls back to backup; otherwise fetch with the backup (refreshing + persisting via `FileLock`-guarded callback).

`_usage_by_account()` (used by strategies) = `{num: entry.decision_value()}` — a dict
(last-good while ≤ `STALE_OK_S` / trust-extended), a sentinel string, or `None`.

`UsageEntry.decision_value()`: sentinel wins; else last-good if `age_s <= STALE_OK_S` or
`trust_extended`; else `None`.

---

## 14. `disable` / `enable` (`set_account_disabled`)

`set_account_disabled(identifier, disabled)`:
- Precondition: `sequence.json` exists (`ConfigError("No accounts are managed yet")`); resolves via `resolve_account` (hard-errors on ambiguous email, `AccountNotFoundError` on miss).
- Already in target state → prints `Account-{n} ({email}) is already {disabled|enabled}.` (dimmed), returns.
- Sets/pops `disabled` in the record, `lastUpdated`, writes. Logs `Disabled account {n}: {email}` / `Enabled account {n}: {email}`.
- Prints `{accent(verb.capitalize())} Account-{n} ({email}).` (`Disabled`/`Enabled`).
- On disable: if the target is the active account, dimmed note `  It is the active account — it stays live until you switch away; it just won't be an automatic switch target.`; if no switchable accounts remain, `warning("  No accounts remain in rotation — auto-switch and bare switch have nothing to pick. Re-enable one with cswap enable <num|email>.")`.
- On enable: dimmed `  It is back in the rotation.`

Accessors: `is_account_disabled`, `disabled_account_numbers()` (sequence order),
`switchable_account_numbers()` (sequence order, excludes non-switchable **and** disabled),
`_disabled_from_data(data, num)` = `bool(record.get("disabled"))`.

Effect: disabled slots are held out of the auto engine, bare rotation, and `best`/
`next-available`, but remain valid explicit `switch <num|email>` targets. Re-enabling
restores the original sequence position (the `disabled` flag is removed, not re-appended).
Re-adding a removed slot clears any stale `disabled`.

`_account_is_switchable(num)`: slot must have a record, non-empty stored **credentials**,
and non-empty stored **config**. Tolerates stale sequence entries (returns `False`).

---

## 15. `upgrade` command (`update_check.run_self_upgrade`)

CLI aliases: `upgrade` and `update` both map to `--upgrade`. Dispatched **before** switcher
init (so the tool can upgrade without touching config/Keychain); `sys.exit(run_self_upgrade())`.

**Installer detection** (`_detect_install_method`) → `"uv"`, `"pipx"`, or `None`:
- Inspect `Path(sys.prefix).parts` (lowercased) for the adjacent pair `("uv","tools")` → `uv`, or `("pipx","venvs")` → `pipx`.
- Env override: if `UV_TOOL_DIR`/`PIPX_HOME` set and `sys.prefix` is under it (`is_relative_to`) → that method.
- Else `None`.

`run_self_upgrade()` → int exit code:
- `uv` → `["uv","tool","upgrade","claude-swap"]`; `pipx` → `["pipx","upgrade","claude-swap"]`.
- `None`: `error(...)` with the multi-line manual instructions (lists `uv tool upgrade claude-swap`, `pipx upgrade claude-swap`, `{sys.executable} -m pip install --upgrade claude-swap`, and the editable-install `git pull` hint) plus `sys.prefix`/`sys.executable`; return `1`.
- **Windows** (`sys.platform=="win32"`): the running `cswap.exe` is locked, so it does NOT run the upgrade — prints `To upgrade claude-swap on Windows, run:\n  {accent(' '.join(cmd))}` and returns `1`.
- Else `subprocess.run(cmd, check=False)`; return its `returncode`. `FileNotFoundError` (manager missing from PATH) → `error("Detected {method} install but `{cmd[0]}` is not on PATH. Run the upgrade manually from a shell where it is available.")`, return `1`.

**Passive update notice** (`check_for_update`, separate from `upgrade`; runs after any
non-purge, non-upgrade, non-JSON command): reads/writes cache `CACHE_DIR/update_check.json`
(TTL 24h), fetches PyPI (2s timeout), compares tuple versions. When newer:
`A newer version of claude-swap is available ({latest}). You are using {current}. {hint}`
where hint is `Run `cswap upgrade` to update.` (uv/pipx, non-Windows), `Run `{direct}` to update.`
(uv/pipx on Windows), or `Run `cswap upgrade` for upgrade instructions.` (unknown method).

---

## 16. Exit codes & error handling (`cli.py`)

- Success: `0`.
- Handled `ClaudeSwitchError` (base of all `exceptions.py` errors): JSON mode →
  `print(json.dumps(error_envelope(e), indent=2))` to stdout; else `error(f"Error: {e}")`
  to stderr (red). `sys.exit(1)`.
- `KeyboardInterrupt`: JSON mode → cancellation note to **stderr** (keeps stdout parseable);
  else to stdout: `\n{dimmed('Operation cancelled')}`. `sys.exit(130)`.
- `--upgrade`: `sys.exit(run_self_upgrade())` (may be non-0 per §15).
- Root guard (POSIX, non-container): `error("Error: Do not run this script as root (unless running in a container)")`, `sys.exit(1)`.
- JSON payload for JSON-capable commands is printed by the CLI once (`json.dumps(payload, indent=2)`), never by the command.
- Arg validation (argparse `parser.error`, exit 2): `--strategy can only be used with bare 'switch'`; `--model can only be used with 'switch --strategy best' or 'switch --strategy next-available'`.

---

## 17. Edge cases & subtleties (from tests)

- **JSON never prompts / never leaks**: `list`/`status`/`switch`/`switch_to` in JSON mode
  print nothing to stdout (`capsys.readouterr().out == ""`); the empty-list payload is
  returned without calling `_first_run_setup` or `input`. Ambiguous email in JSON mode
  raises `ConfigError` (→ envelope), never prompts.
- **`switched` is identity-based** (`from != to`), covering recorded/live drift, not just
  the explicit already-active case. A forced self-activation keeps `switched:false` but
  reports `reason:"activated"`; a cross-slot force is `switched:true`/`reason:"switched"`.
- **Force self-activation really rewrites live creds** from the stored backup and does
  **not** back up the current login first (slot's stored creds untouched).
- **Every `switched:false` payload has `from == to`** (both the current account); tested for
  single-account, only-one-account, unmanaged.
- **Switch-to onto active is a total no-op**: `_perform_switch` is not called at all (guard
  short-circuits before any write).
- **`from.number` is `null`** when switching away from an unmanaged live account.
- **Rotation skips broken slots**: a slot missing creds or config is skipped with
  `Skipping Account-N` and rotation lands on the next valid one; if none valid,
  prints `No other accounts have valid...` and leaves `activeAccountNumber` unchanged
  (no exception). `switch_to` to a broken slot still raises `SwitchError` with
  `has no stored credentials` / `has no stored config backup`.
- **Fresh-machine (no live login)** picks first switchable slot when the recorded active is
  broken or disabled; all-broken → `No managed accounts have valid`; all-disabled →
  `No accounts remain in rotation`.
- **`best` never moves onto a worse account** (real bug: 89% current vs 100% other stays);
  strictly-greater headroom required; ties → stay / earliest slot. Notes distinguish
  `current-unavailable`, `no-comparison`, `incomplete-comparison`, `stay`, `exhausted`, `none`.
- **`next-available` anchors on the live account** under drift (so it never no-ops onto the
  slot you're already on); unknown usage is **not** skipped (given a chance); with `models`,
  the skip message names the binding window (`at Fable limit`, `at 5h/Fable limit`).
- **Inert `--model` warning survives into JSON** even on `best` no-ops (`already-best`,
  `candidates-exhausted`); only fires when every account's usage is readable.
- **`--model` source announced** up front: `Using configured model limits: Fable (from --model)`
  or `(from autoswitch.model)`; without `models`, scoped windows are invisible (default unchanged).
- **Usage store gating**: fresh entries (≤180s) served without API calls; entries >180s and
  poll-due are refetched (both live-fetched when one is stale + one missing); a stale entry
  whose `nextPollAt` is in the future is served, not refetched (on-demand callers can't
  out-poll the plan). Every on-demand pass persists poll plans so all surfaces inherit them.
- **Stale-but-decision-gated JSON**: with a failing refetch, `age_s=100/400 → "ok"` (serves
  25.0), `age_s=4000 → "unavailable"` with `usage:null` and no `usageFetchedAt` (past
  `TRUST_MAX_AGE_S`, even though the human view still shows the numbers with an age note).
- **Owned+expired sentinel wins over a fresh stored entry** and skips the fetch entirely
  (derived statically), while keeping the last-good measurement (`last seen ...`).
- **List never writes live creds while Claude Code is running**: active row stays hands-off;
  only inactive backups refresh (verified `write_live` not called, backup written for the
  inactive slot).
- **Reset strings are recomputed from `resets_at`** in both the human and JSON views (a
  store measurement served hours later must not print a frozen countdown/clock);
  fall back to cached strings only without/unparseable `resets_at`.
- **Column alignment**: labels padded to the widest (`Fable:`), so all `%` columns line up;
  scoped windows at ≥100% get a trailing ` (!)`.
- **Alias renders first**: `1: dev (test@example.com) [personal] (active)`; unaliased rows
  stay plain (`2: account2@example.com`, no spurious parens).
- **Disabled marker** attaches only to the disabled row (`(disabled)`), JSON `disabled:true`
  present only on disabled rows (absent, not `false`, on enabled).
- **Switch followup**: macOS shows the `~30 seconds` / `apply immediately` note; Linux/WSL/
  Windows show `no restart needed` (keyed by `_last_active_credentials_backend`).
- **Empty-current-creds guard**: a `""` read (Keychain timeout) must NOT overwrite the
  departing slot's backup → `CredentialReadError` aborts the switch.
- **Purge with unset active account** must not write `account-None-*` backups (direct
  activation path).
- **Dead-token quarantine**: one `invalid_grant` → `re-login needed`, no more fetches; a
  re-add/refresh (`clear_dead_token`) lifts it; surfaced same pass on a fresh `invalid_grant`.

---

## 18. Go port notes

- **Platform-conditional logic** everywhere: backup root (XDG on Linux/WSL vs
  `~/.claude-swap-backup` elsewhere), `chmod 0o700`/`0o600` skipped on Windows, Keychain
  (macOS only, service names `Claude Code-credentials` / `Claude Code` / `claude-swap`),
  the switch followup message, `Platform.detect()` via `sys.platform` (`darwin`/`win32`/
  `linux` + `WSL_DISTRO_NAME`), the upgrade-on-Windows print-only behavior, and
  `_is_running_in_container` (env `CONTAINER`/`container`, `/.dockerenv`, `/proc/1/cgroup`,
  `/proc/self/mountinfo`).
- **Concurrency**: usage fetches run in a `ThreadPoolExecutor` with a `0.25s * idx` stagger
  → in Go use a goroutine pool / errgroup with the same staggered start, collecting
  `map[string]FetchRecord`. The Claude Code lock toucher is a **daemon thread** doing
  `os.utime` every 3s while held → a background goroutine stopped via a channel/context on
  release. The active-refresh persist callback re-acquires locks from inside the fetch — the
  cswap `FileLock` is **non-reentrant**, so the network refresh must happen with no lock
  held and the persist callback re-locks; preserve this ordering exactly (a reentrant
  mutex would silently drop the refreshed token / deadlock).
- **File locking**: POSIX `fcntl.flock(LOCK_EX|LOCK_NB)` with 0.1s poll to 10s; Windows
  `msvcrt.locking`. In Go, use `flock(2)` (`golang.org/x/sys/unix`) / `LockFileEx` with the
  same non-blocking poll loop and timeout. `LockError` on timeout.
- **proper-lockfile protocol** (external contract — Claude Code interop): a **directory**
  `<target>.lock` created with `mkdir` (atomic mutex), stale after 10s (mtime), touched
  every 3s by the holder, taken over by `rmdir`+`mkdir` when stale, acquire timeout 9s with
  `0.25+rand*0.25s` sleeps. Must be byte-compatible so a running Claude Code and cswap
  cooperate. Guards `~/.claude.lock` (credential refresh) and `~/.claude.json.lock` (config
  writes). References: claude-code `utils/auth.ts checkAndRefreshOAuthTokenIfNeededImpl`,
  `utils/config.ts saveConfigWithLock`, `utils/lockfile.ts`.
- **Atomic JSON writes**: write to `path.with_suffix(f".{pid}.tmp")`, re-parse to validate
  (`ConfigError("Generated invalid JSON")` on failure), chmod 0600, `shutil.move` (rename)
  as the commit point. Replicate: temp file + rename; permissions on the temp file so the
  rename is the last fallible op.
- **`Path.home()` / env resolution**: `HOME`/`USERPROFILE`, `CLAUDE_CONFIG_DIR`,
  `XDG_DATA_HOME` (ignored when unset/empty/non-absolute; `~` expanded). The `.claude.json`
  at home-root asymmetry and the legacy `<config_home>/.config.json` precedence are external
  Claude Code contracts — preserve exactly.
- **Number/string slot duality**: `sequence` holds **ints** (sorted), `accounts` keys are
  **strings**; `activeAccountNumber` is an int. Port must keep both representations and the
  int↔str conversions at the boundaries (rotation indexes by int, backups key by str). Note
  `_resolve_account_identifier` returns a digit string unchanged (no `"01"`→`"1"` normalize);
  `move_account`/`swap` normalize separately.
- **Decimal/format specifics**: `pct:>3.0f`, `${used:,.2f}` (thousands separators, 2 dp),
  `clock:<12` left-justified, ISO8601 `Z` timestamps, `usageAgeSeconds` rounded to 1 dp.
  Go equivalents need explicit formatting (thousands grouping is not built-in — implement it).
- **Local-time reset clock**: `format_reset` renders in the machine's local timezone
  (`.astimezone()`), same-day `HH:MM` else `Mon D HH:MM`. Port must use local TZ, and the
  5-minute OAuth expiry buffer (`OAUTH_EXPIRY_BUFFER_MS`) in ms.
- **Sentinels are strings** (`"no credentials"`, `"token expired"`, `"api key"`,
  `"keychain unavailable"`, `"re-login needed"`), derived every pass and never persisted;
  keep them as a typed enum with the exact wire strings for the JSON status mapping.
- **Usage store schema**: `cache/usage.json` `{"schemaVersion":2,"accounts":{...}}`; a
  version-less or future-version file reads as empty. Rows are identity-guarded on
  `(email, organizationUuid)` — a row whose stored identity differs is invisible and
  replaced on write. `trust_extended` and `age_s` are computed at read time, not stored.
- **`ThreadPoolExecutor()` default worker count** is `min(32, cpu+4)` — unbounded relative
  to account count in practice; in Go, either unbounded goroutines or a generous pool are
  fine, but keep the per-index stagger so the request-hygiene property holds.
- **Errors are a typed hierarchy** (`ClaudeSwitchError` base). The JSON error envelope uses
  the **class name** as `error.type`; port needs a stable name per error kind matching the
  Python class names (`SwitchError`, `ConfigError`, `AccountNotFoundError`,
  `ValidationError`, `CredentialReadError`, `SessionError`, `LockError`,
  `ClaudeCodeLockTimeout`, `MigrationError`, `TransferError`, ...). Exit 1 for handled
  errors, 130 for interrupt, 2 for argparse misuse.
