# 05 — Auto-Switch Engine (`autoswitch.py`)

## Overview

The auto-switch engine (`AutoSwitchEngine`) is a UI-agnostic threshold-policy
switcher layered over a `ClaudeAccountSwitcher`. On each `tick()` it reads the
live account's usage (via an adaptive, O(1)-baseline usage collector), and if
the active account's **binding window** (the higher of its 5h / 7d / configured
per-model utilizations) has reached `settings.threshold`, it selects the
candidate account with the most headroom, freshens that candidate's OAuth token,
switches to it proactively (before the old account is exhausted, so a running
Claude Code picks up the new default within the macOS ~30 s Keychain cache
latency), and records a cooldown timestamp. Every decision — poll, switch,
no-switch, quarantine, all-exhausted, sleep, error, config-warning — is reported
as a typed frozen-dataclass event handed to an `on_event` callback; the CLI
renders those as timestamped human lines or one JSON object per line. Cooldown
and quarantine state persist in `<backup_root>/autoswitch_state.json` (mutated
read-modify-write under a dedicated file lock) so cron-driven `cswap auto --once`
ticks behave consistently across processes. The engine also has a foreground
`run_loop()` with adaptive inter-tick delays and a `--once` mode whose
`TickOutcome` enum value doubles as the process exit code.

---

## 1. Module constants (exact values)

Defined in `autoswitch.py`:

| Name | Value | Meaning |
|---|---|---|
| `STATE_FILENAME` | `"autoswitch_state.json"` | Persisted cooldown/quarantine state file name |
| `STATE_SCHEMA_VERSION` | `1` | Written into state file as `"schemaVersion"` |
| `FRESHEN_BUFFER_MS` | `10 * 60 * 1000` = `600000` | Refresh a target token when it expires within this window (ms) — twice Claude Code's own 5-min refresh buffer |
| `MAX_SLEEP_S` | `6 * 3600.0` = `21600.0` | Cap on any single sleep around a known quota reset |
| `NO_RESET_FALLBACK_S` | `300.0` | Blocked/idle-hold cadence when no reset time is known |
| `IDLE_HOLD_MAX_S` | `30 * 60.0` = `1800.0` | Max elapsed time to hold an owned+expired active token before resuming unhealthy counting |
| `_logger` | `logging.getLogger("claude-swap")` | Shared logger name |

Imported constants used by the engine (from `poll_policy`):

| Name | Value | Source |
|---|---|---|
| `ESCALATION_MARGIN_PCT` | `15.0` | `poll_policy.py` — active within this margin of threshold triggers a full candidate refresh |
| `RESET_SLACK_S` | `60.0` | `poll_policy.py` — slack added past a known reset before re-polling |
| `SCHEMA_VERSION` | `1` | `json_output.py` — event envelope `schemaVersion` |
| `USAGE_TOKEN_EXPIRED` | `"token expired"` | `json_output.py` — sentinel for owned+expired active token |

`_refresh_fingerprint = oauth.credential_fingerprint` — aliased. Reset-math
aliases: `_limiting_reset_ts = poll_policy.limiting_reset_ts`,
`_earliest_future_reset_ts = poll_policy.earliest_future_reset_ts`,
`_parse_reset_ts = poll_policy.parse_reset_ts`.

---

## 2. Settings (`AutoSwitchSettings`, from `settings.py`)

Frozen dataclass; defaults and CLI/`settings.json` bounds:

| Field | JSON key (`autoswitch.*`) | Default | Kind | Bounds/choices |
|---|---|---|---|---|
| `threshold` | `threshold` | `90.0` | float | `50.0 … 99.9` |
| `interval_seconds` | `intervalSeconds` | `60.0` | float | `15.0 … 3600.0` |
| `cooldown_seconds` | `cooldownSeconds` | `300.0` | float | `0.0 … 86400.0` |
| `hysteresis_pct` | `hysteresisPct` | `10.0` | float | `0.0 … 50.0` |
| `strategy` | `strategy` | `"best"` | choice | only `"best"` in v1 |
| `include_api_key_accounts` | `includeApiKeyAccounts` | `False` | bool | — |
| `unhealthy_ticks` | `unhealthyTicks` | `3` | int | `1 … 100` |
| `model` | `model` | `None` | string | comma-separated names, or `"all"`, or unset |

- `settings.json` lives at `<backup_root>/settings.json`, camelCase keys inside
  an `"autoswitch"` section, with `"schemaVersion": 1`.
- Load is **forgiving**: missing/corrupt file → defaults; out-of-range numeric
  values are **clamped** to the bounds; a bad-type value reverts to the default;
  an unsupported `strategy` logs a warning and uses the default.
- `parse_model_names(value)` splits on `,`, trims each, dedupes
  case-insensitively (**first spelling wins**), returns a `tuple[str, ...]`;
  empty/`None` → `()`.
- CLI overrides (`merged_with_cli`): `--threshold`→threshold,
  `--interval`→interval_seconds, `--cooldown`→cooldown_seconds,
  `--include-api-key-accounts`→include_api_key_accounts, `--model`→model. Only
  non-`None` overrides applied, then re-clamped.

---

## 3. Events

Base class `AutoSwitchEvent` (frozen dataclass): field `ts` (kw-only,
default = `_now_iso()`), classvar `kind = "event"`. `to_json()` produces:

```json
{ "schemaVersion": 1, "event": "<kind>", "ts": "<iso>", ...extra }
```

`_now_iso()` = `datetime.now(timezone.utc).isoformat(timespec="seconds")` with
`+00:00` replaced by `Z` (e.g. `"2026-07-17T12:34:56Z"`). Payloads are
**additive**: consumers must ignore unknown `event` kinds and unknown fields.

Every event kind, its JSON `event` value, extra JSON fields, and human string:

### `poll` (`PollEvent`)
Fields: `active` (dict `{number, email}` or `None`), `headroom`
(`dict[str, float|None]`, account-number-string → headroom pct, `None`=unknown),
`threshold` (float), `fetch_errors` (`dict[str,str]`, default `{}`), `windows`
(`dict[str, dict[str,float]]`, default `{}`).

JSON `_fields()`:
```json
{
  "active": {"number": 1, "email": "a@example.com"},
  "headroomPct": {"1": 40.0, "2": 90.0},
  "threshold": 90.0,
  "fetchErrors": {"1": "http-429"},   // only if non-empty
  "windowsPct": {"2": {"5h": 3.0, "7d": 89.0, "Fable": 21.0}}  // only if non-empty
}
```
- `fetchErrors` included **only** for accounts whose `usage` is `None` this tick
  **and** whose entry has a `last_error`.
- `windowsPct` per account = ordered `_window_pcts(usage, models)`: label→pct for
  `5h`, `7d`, then each configured scoped model name; included only when the dict
  is non-empty. Scoped windows appear **only when a model is configured** (see
  `_window_pcts` note: showing an unconfigured 100% scoped window next to a switch
  onto that account would read as a bug).

Human line:
- If `active is None`: `"poll: no active account"`.
- Else: `"Account-{num} ({email}): {used} (switch at {pct_label(threshold)}%){tail}"`
  where `used` is `"{100-h:.0f}% used"` when headroom known, else
  `"usage unknown ({err})"` / `"usage unknown"`. `tail` = `" | others: {list}"`,
  each other account rendered `"#{n}: {describe}"`. `_describe(n)`:
  windows (`" · ".join(f"{name} {pct:.0f}%")`) if present, else
  `"{100-h:.0f}%"`, else `"? ({err})"` or `"?"`.

### `switch` (`SwitchEvent`)
Fields: `trigger` (`"proactive"` | `"at-limit"` | `"failover"`), `from_ref`
(dict|None), `to_ref` (dict|None), `warnings` (list, default `[]`), `dry_run`
(bool, default `False`).
```json
{ "trigger": "proactive", "from": {"number":1,"email":"a@example.com"},
  "to": {"number":3,"email":"c@example.com"}, "warnings": [], "dryRun": false }
```
Human: `"[dry-run] would switch Account-1 -> Account-3 (c@example.com) (proactive)"`
(prefix `"Switched"` when not dry-run; `src`=`"(none)"` if no from; `dst`=`"?"` if no to).

### `no-switch` (`NoSwitchEvent`)
Fields: `reason` (str), `detail` (str, default `""`).
```json
{ "reason": "below-threshold", "detail": "50% < 90%" }
```
Human: `"no switch: {reason}"` + `" ({detail})"` if detail non-empty.
**All `reason` values emitted** (exhaustive):
`"unmanaged-active-account"`, `"no-active-account"`, `"active-api-key"`,
`"below-threshold"`, `"active-idle"`, `"active-usage-unknown"`, `"cooldown"`,
`"no-candidates"`, `"no-comparison"`, `"no-qualifying-candidate"`,
`"no-viable-target"`, `"already-active"`.

### `account-quarantined` (`QuarantineEvent`)
Fields: `number` (str), `email` (str), `reason` (str).
```json
{ "number": "2", "email": "b@example.com", "reason": "invalid_grant" }
```
Human: `"Account-{number} ({email}) quarantined: {reason}. Log in with it and run 'cswap --add-account --slot {number}' to recover."`

### `account-unquarantined` (`UnquarantineEvent`)
Fields: `number`, `email`, `reason` (default `"credentials-replaced"`).
```json
{ "number": "2", "email": "b@example.com", "reason": "credentials-replaced" }
```
Human: `"Account-{number} ({email}) back in rotation ({reason})"`.
`reason` is `"account-replaced"` (email changed / slot re-added) or
`"credentials-replaced"` (refresh-token fingerprint changed).

### `all-exhausted` (`AllExhaustedEvent`)
Field: `earliest_reset_at` (str|None).
```json
{ "earliestResetAt": "2026-07-03T10:30:00Z" }
```
Human: `"all accounts exhausted; earliest reset {ts}"` or `"all accounts exhausted; no reset time known"`.

### `sleep` (`SleepEvent`)
Fields: `seconds` (float, **rounded to 1 decimal** in JSON), `until` (str).
```json
{ "seconds": 1800.0, "until": "2026-07-17T13:04:56Z" }
```
Human: `"sleeping {seconds/60:.0f}m (until {until})"`.

### `error` (`ErrorEvent`)
Fields: `message` (str), `transient` (bool, default `True`).
```json
{ "message": "could not freshen any candidate (network?)", "transient": true }
```
Human: `"error: {message}"` + `" (will retry)"` if transient.

### `config-warning` (`ConfigWarningEvent`)
Field: `message` (str).
```json
{ "message": "autoswitch.model: Fabel matches no account's usage windows — only the 5h/7d limits are being watched for it (typo?)" }
```
Human: `"warning: {message}"`.

---

## 4. `TickOutcome` enum → `--once` exit codes

```python
class TickOutcome(enum.Enum):
    SWITCHED  = 0   # a switch happened (or, in dry-run, would happen)
    ERROR     = 1   # network trouble, lock contention, transient freshen failure
    NO_ACTION = 2   # nothing to do (below threshold, cooldown, idle, api-key active, ...)
    BLOCKED   = 3   # wanted to switch but no viable target / all exhausted
```
The `.value` is the process exit code for `cswap auto --once` (`sys.exit(engine.tick().value)`).
CLI epilog documents them verbatim:
```
Exit codes with --once:
  0  switched to another account
  1  error (network trouble, lock contention, ...)
  2  no action needed
  3  blocked: wanted to switch but no viable target / all exhausted
```

---

## 5. State file: `autoswitch_state.json`

- Path: `state_path` (constructor arg) or default `switcher.backup_dir / "autoswitch_state.json"`.
- Lock: `FileLock(state_path.parent / ".autoswitch_state.lock")` (POSIX `fcntl.flock` /
  Windows `msvcrt`; 10 s default acquire timeout, 0.1 s poll; raises `LockError`
  on timeout — see §16). All mutations go read-modify-write under this lock.
- Written atomically via `atomic_write_json` (temp file + `os.replace`, `0600`
  file / `0700` parent on non-Windows, `json.dumps(..., indent=2)`).
- `_read_state()` swallows `OSError`/`JSONDecodeError`/`UnicodeDecodeError` and a
  non-dict top-level → returns `{}`.
- Every write sets `state["schemaVersion"] = 1`.

Schema (all keys optional; unknown keys survive round-trips):
```json
{
  "schemaVersion": 1,
  "lastSwitchAt": 1700000000.0,     // wall-clock epoch (self.clock()) of last real switch
  "lastSwitchTo": "3",              // slot number string of last switch target
  "quarantine": {
    "2": {
      "email": "b@example.com",
      "reason": "invalid_grant",              // or "identity-conflict"
      "at": "2026-07-17T12:00:00Z",           // _now_iso()
      "refreshTokenFingerprint": "sha256:..." // or null
    }
  }
}
```

**Concurrency note:** two engines (loop + cron `--once`) serialize on this lock.
`_mutate_state(mutator)` reads → sets schemaVersion → applies mutator → atomic
write → returns new state. Must **never** be called while any other lock is held.

---

## 6. The tick algorithm (`_tick_inner`), step by step

`tick()` wraps `_tick_inner()`: catches `ClaudeSwitchError` → emit
`ErrorEvent(str(e), transient=True)`, return `ERROR`; catches any other
`Exception` → emit `ErrorEvent(f"{type(e).__name__}: {e}", transient=True)`,
return `ERROR`. **`tick()` never raises.**

`_tick_inner()`:

1. Reset per-tick fields: `_sleep_until_ts=None`, `_blocked_wait_long=False`,
   `_idle_hold_slow=False`. Snapshot `settings = self.settings` (frozen).
2. `state = self._read_state()`. If **not** dry-run:
   `state = self._release_recovered_quarantines(state)` (dry-run never mutates
   state, so it never releases). Build `quarantined` set from
   `state["quarantine"]` keys (if a dict).
3. `current = switcher.current_account_number()`.
   - If `None`: emit `PollEvent(active=None, headroom={}, threshold)`. Then:
     - if `switcher.has_live_login()`: emit
       `NoSwitchEvent("unmanaged-active-account", "run 'cswap --add-account' to include it in rotation")`.
     - else: emit `NoSwitchEvent("no-active-account", "log in and run 'cswap --add-account' first")`.
     - Return `NO_ACTION`.
4. `current_email = switcher.account_email(current)`. Build
   `active_ref = {"number": int(current), "email": current_email or ""}`.
5. Collect usage: `entries, usage, headroom = _collect_scheduled_usage(current, quarantined, threshold=settings.threshold)` (§9).
6. Emit `PollEvent(active_ref, headroom, threshold, fetch_errors=..., windows=...)`
   (`fetch_errors` = accounts with `usage[num] is None` and non-empty `last_error`;
   `windows` = `_window_pcts(value, models)` per account with non-empty result).
7. If `not self._model_check_done`: run `_check_model_names(quarantined, usage)` (§11).
8. If active is an api-key account **and** `not include_api_key_accounts`:
   emit `NoSwitchEvent("active-api-key", "API-key accounts have no quota to watch")`,
   return `NO_ACTION`.
9. `active_headroom = headroom.get(current)`.
   - **If known (not None):** reset `_unhealthy_ticks = 0`, `_idle_hold_since = None`.
     `utilization = 100.0 - active_headroom`.
     - If `utilization < settings.threshold`: emit
       `NoSwitchEvent("below-threshold", f"{pct_label(utilization)}% < {pct_label(threshold)}%")`,
       return `NO_ACTION`.
     - Else `trigger = "at-limit" if active_headroom <= 0 else "proactive"`.
   - **If unknown (None):** see §7 (idle-hold + unhealthy counting). Either
     returns `NO_ACTION` early, or sets `trigger = "failover"`.
10. **Cooldown gate (proactive only):** if `trigger == "proactive"` and
    `_in_cooldown(state)`: emit `NoSwitchEvent("cooldown")`, return `NO_ACTION`.
    (`at-limit` and `failover` bypass cooldown.)
11. Candidate selection (§10). Produces `ordered` list or an early BLOCKED return.
12. Freshen + switch (§12).

---

## 7. Active usage unknown: idle-hold + unhealthy counting

When `active_headroom is None`:

- **If `usage.get(current) == USAGE_TOKEN_EXPIRED`** (`"token expired"` sentinel —
  active token locally expired while an *owner* (default-profile Claude Code or a
  live `cswap run` session) holds the credential; produced by the collector, never
  by a network fetch):
  - `now = self.clock()`. If `_idle_hold_since is None`: set it to `now`.
  - If `now - _idle_hold_since <= IDLE_HOLD_MAX_S` (1800 s): set
    `_unhealthy_ticks = 0`, `_idle_hold_slow = True`, emit
    `NoSwitchEvent("active-idle", "token expired while Claude Code is idle; resumes on next use")`,
    return `NO_ACTION`.
  - Else (held longer than the cap — likely a *dead* refresh token with an active
    user): `_logger.warning(...)` and **fall through** to unhealthy counting.
- **Else** (`usage[current]` is `None` — genuine fetch failure / dead creds, not
  the idle sentinel): `_idle_hold_since = None`.
- Then `_unhealthy_ticks += 1`.
  - If `_unhealthy_ticks < settings.unhealthy_ticks`: emit
    `NoSwitchEvent("active-usage-unknown", f"{_unhealthy_ticks}/{settings.unhealthy_ticks} before failover")`,
    return `NO_ACTION`.
  - Else `trigger = "failover"`.

**Key distinction:** the idle sentinel `USAGE_TOKEN_EXPIRED` resets the unhealthy
counter and holds; a plain `None` increments it. A healthy reading resets both
`_unhealthy_ticks` and `_idle_hold_since` (§6 step 9 known-branch).

---

## 8. Threshold & binding-window semantics; `--model` folding

- **Binding window / headroom** (`oauth.account_headroom(usage, models)`):
  `100 - max(pct over relevant windows)`. Returns `None` when usage is
  unavailable or has no window data.
- **`relevant_windows(usage, models)`** returns `[(label, pct, resets_at), ...]`:
  - Always `("5h", pct, resets_at)` and `("7d", pct, resets_at)` from
    `usage["five_hour"]` / `usage["seven_day"]` (only when the window is a dict
    with a numeric `pct`).
  - When `models` non-empty: each `usage["scoped"]` entry `{name, pct, resets_at}`
    whose `name.lower()` is in the wanted set (case-insensitive) is appended with
    label = its display name. The sentinel `"all"` (case-insensitive) matches
    **every** scoped window regardless of name.
  - `spend` (pay-as-you-go) is **deliberately excluded**.
- `_models = parse_model_names(settings.model)` computed **once at construction**
  and passed everywhere usage windows are read (decisions, cadence, reset math) so
  all axes agree. `settings.model` supports `"Fable"`, `"Opus,Sonnet"`, `"all"`.
- **Model folding effect:** an account whose 5h=5% but Fable=100% has headroom 0
  when `model="Fable"` → the engine leaves it even though session windows have
  room. The most-Fable-headroom candidate wins (`test_model_maxed_switches_despite_session_headroom`).
- `threshold` is compared against `utilization = 100 - active_headroom`; switch
  when `utilization >= threshold`.
- **`apply_threshold(threshold)`** (TUI session override): atomically swaps
  `self.settings = replace(self.settings, threshold=...)` and calls
  `switcher.set_poll_policy_inputs(threshold, self._models)`. **Threshold only** —
  the model axes are fixed at construction. Each tick snapshots `self.settings`
  once so a mid-tick threshold change is consistent; no locking needed.

---

## 9. Adaptive usage collection (`_collect_scheduled_usage`)

Two-phase, O(1) baseline. Signature:
`(current, quarantined=frozenset(), *, threshold=None) -> (entries, usage, headroom)`.

**Candidate set:** `switchable_account_numbers()` minus `current` minus `quarantined`.
Quarantined accounts are excluded from ever consuming a poll slot (wasted — they
can't be targets).

**Phase A (baseline):**
1. `pre = switcher.usage_entries_by_account(fetch=set())` (store-only read).
2. Nominate the **active** account into the fetch `plan` when any of:
   - never fetched (`active_pre is None` or `age_s is None`), or
   - a **stale candidate plan** override (`stale_candidate_plan`): the slot carries
     a candidate-style plan (interval > `ACTIVE_MAX_INTERVAL_S`=300) left over from
     a role change the switcher never saw (e.g. a manual login), and `age_s >=
     ACTIVE_MAX_INTERVAL_S`, **and** its binding pct < 100 (an exhausted account
     stays parked at its reset — the age cap must not defeat reset parking), or
   - poll-due (`next_poll_at is not None and now >= next_poll_at`), or
   - no plan yet but `age_s >= poll_policy.MIN_INTERVAL_S` (180).
3. **If not in an idle-hold** (`_idle_hold_since is None`): pick **one** due
   candidate via `due_candidate(candidates, pre, now)` (stalest-first —
   never-fetched before oldest-fetch) and add to `plan`. During an idle-hold,
   **no candidate is polled at all** (slow crawl for everything).
4. `entries = switcher.usage_entries_by_account(fetch=plan)`; `usage = {num: entry.decision_value()}`.

**Phase B (escalation):** Compute `active_headroom` from `usage[current]`.
`threshold` defaults to `self.settings.threshold` (the caller passes the
tick-snapshotted value so fetch and decision use the same threshold even if
`apply_threshold` lands mid-tick). **Escalate** when there is at least one
candidate AND:
- active headroom unknown AND `active_value != USAGE_TOKEN_EXPIRED` (a failover
  must not run on stale candidate data; an idle-hold does **not** escalate), OR
- active headroom known AND `100 - active_headroom >= threshold - ESCALATION_MARGIN_PCT`
  (active within 15 pct of threshold).

On escalation: re-fetch `{current, *candidates}` and recompute `usage`.
**Candidate selection never runs on the pre-escalation snapshot.**

Finally `headroom = {num: account_headroom(value, models)}` for every entry.

**Backoff is enforced by the collector** even for the active account (a
`Retry-After` must never be defeated). `decision_value()` returns the sentinel if
set, else last-good while trusted (age ≤ `STALE_OK_S`=300 or `trust_extended`),
else `None`.

---

## 10. Target-selection rule (candidate selection)

```
candidates = [n for n in switchable_account_numbers()
              if n != current and n not in quarantined]
oauth_candidates   = [n for n in candidates if account_kind_for(n) != "api_key"]
api_key_candidates = ([n for n in candidates if account_kind_for(n) == "api_key"]
                      if settings.include_api_key_accounts else [])
```

- If **no** oauth and **no** api-key candidates: set `_blocked_wait_long = True`,
  emit `NoSwitchEvent("no-candidates")`, return `BLOCKED`.

Build `qualifying` from oauth candidates:
```
any_known = False
for num in oauth_candidates:
    h = headroom.get(num)
    if h is None:              continue          # unreadable this tick
    any_known = True
    if h <= 0:                 continue          # candidate itself at its limit
    if trigger == "proactive" and active_headroom is not None:
        if (100.0 - h) >= settings.threshold:  continue   # would re-trigger next tick
        if h - active_headroom < settings.hysteresis_pct: continue  # not provably better
    qualifying.append((h, num))
qualifying.sort(key=lambda t: -t[0])   # best headroom first; list order breaks ties
ordered = [num for _, num in qualifying]
if not ordered and api_key_candidates:
    ordered = api_key_candidates       # last resort (unmeasurable headroom)
```

### The hysteresis rule (verbatim from source comment)

> Hysteresis guards only the proactive case: two accounts hovering at the line
> must not ping-pong. The gate is relative — the candidate must beat the active
> account by the full margin (a one-way move like 99%→89% qualifies; near-line
> pairs can't flap back) — and the landing must be healthy: an account at/over
> the threshold would re-trigger on the very next tick. At-limit and failover are
> escapes — any account with real headroom beats a blocked or dead one (and you
> can't flap back onto an account at 100%).

Two proactive gates, both must pass: **(a)** landing below threshold
(`(100 - h) < threshold`), and **(b)** better by full margin
(`h - active_headroom >= hysteresis_pct`). `at-limit` and `failover` apply
**neither** gate (only `h > 0`).

### When `ordered` is empty

- If `not any_known`: emit `NoSwitchEvent("no-comparison", "no candidate has readable usage")`, return `BLOCKED`.
- Else compute `truly_exhausted = all(h is not None and h <= 0 for h in [headroom.get(n) for n in oauth_candidates])`:
  - If **not** truly exhausted (some candidate merely failed the hysteresis gate,
    or its usage is unreadable this tick): emit
    `NoSwitchEvent("no-qualifying-candidate", "no candidate is below the threshold and better than the active account by the hysteresis margin, or usage is unreadable this tick")`,
    return `BLOCKED`. **No all-exhausted event, no reset sleep** — keep normal
    cadence so the at-limit escape isn't missed.
  - If **truly exhausted** (every candidate's usage known and `<= 0`): set
    `_blocked_wait_long = True`; `earliest = _earliest_recovery(usage)`; if not
    None set `_sleep_until_ts = earliest.timestamp() + RESET_SLACK_S`; emit
    `AllExhaustedEvent(earliest_reset_at = earliest.isoformat().replace("+00:00","Z") or None)`,
    return `BLOCKED`.

---

## 11. All-exhausted recovery time (`_earliest_recovery`)

Per account with a dict usage value:
- `blocked = [resets_at for label, pct, resets_at in relevant_windows(value, models) if pct >= 100.0]`.
- If no `blocked` windows → account not exhausted, skip it.
- `usable_at = _limiting_reset_ts(value, models)` = **latest** reset among its
  ≥100% relevant windows (an account blocked on both 5h and a scoped weekly limit
  isn't usable until the later resets). If `usable_at is None` (a blocked window
  carries no parseable reset) → **return `None`** immediately (recovery unprovable;
  don't oversleep toward another account's later known reset).
- Overall answer = the **minimum** `usable_at` across all exhausted accounts (the
  active account included, since its recovery also ends the blocked state), returned
  as a UTC `datetime` (or `None`).

`limiting_reset_ts`/`parse_reset_ts`: parse ISO (`Z`→`+00:00`) via
`datetime.fromisoformat`; a `ValueError` → `None`.

---

## 12. Freshen + switch

For each `num` in `ordered`:
```
email = switcher.account_email(num)
if dry_run:  return self._perform(num, email, trigger)   # stop at decision
status = self._freshen_target(num, email)
  "identity-conflict" -> _quarantine(num, email, "identity-conflict");  continue
  "invalid_grant"     -> _quarantine(num, email, "invalid_grant");      continue
  "transient"         -> transient_failure = True;                      continue
  "skip-live-session" -> continue
  otherwise ("ok")    -> return self._perform(num, email, trigger)
```
After the loop:
- if `transient_failure`: emit
  `ErrorEvent("could not freshen any candidate (network?)", transient=True)`, return `ERROR`.
- else: emit `NoSwitchEvent("no-viable-target")`, return `BLOCKED`.

### `_freshen_target(number, email) -> str`

Ensures the candidate's stored token outlives Claude Code's 5-min refresh buffer
before activation. **Only ever touches the slot's backup store** (the active
credential belongs to Claude Code). Returns one of `"ok"`, `"invalid_grant"`,
`"identity-conflict"`, `"transient"`, `"skip-live-session"`.

1. `account_kind_for(number) == "api_key"` → `"ok"` (API keys don't expire/refresh).
2. `live_session_pids_for(number, email)` non-empty → `"skip-live-session"`
   (a live `cswap run` session owns that account's token in its own profile;
   auto-activating it as the default too would put one rotating refresh token in
   two config dirs with nobody reading the warning, and its quota is already
   being consumed). *Manual* `switch_to` keeps warn-and-proceed; auto skips.
3. `creds = read_account_credentials(number, email)`; if falsy → `"transient"`.
4. `data = oauth.extract_oauth_data(creds)`; if falsy → `"invalid_grant"`.
5. `expires_at = data.get("expiresAt")`; `now_ms = self.clock() * 1000`;
   `near_expiry = isinstance(expires_at, (int,float)) and now_ms + FRESHEN_BUFFER_MS >= expires_at`.
   If **not** near_expiry → `"ok"` (fresh token, no refresh).
6. `outcome = oauth.try_refresh_oauth_credentials(creds)`:
   - On success (`outcome.error is None and outcome.credentials`):
     **persist first, unconditionally** —
     `switcher.persist_backup_credentials(number, email, outcome.credentials)`
     (the grant consumed a generation; not writing the successor would kill the
     lineage). Then `_note_token_identity(number, outcome.token_account)`:
     - returns `True` (conflict) → `"identity-conflict"`.
     - returns `False` → `"ok"`.
   - `outcome.error in ("invalid_grant", "no_refresh_token")` → `"invalid_grant"`.
   - otherwise → `"transient"`.

### `_note_token_identity(number, token_account) -> bool` (True = conflict)

Uses the token endpoint's optional free identity `{"uuid","email","organizationUuid"}`.
Opportunistic and defensive (the successor credential is already persisted).
- `token_account` not a dict → `False`.
- `ta_uuid = token_account.get("uuid")`; not a non-empty str → `False`; else strip.
- `slot_identity = switcher.account_identity(number)` = `{email, organizationUuid, uuid}`.
- `ta_org = token_account.get("organizationUuid")`; `slot_org = slot_identity["organizationUuid"] or ""`.
- **Org compared first:** if both `ta_org` and `slot_org` are non-empty strings
  and differ → return `True` (conflict). *(Same-uuid-different-org is still a
  conflict — org is part of identity.)*
- If `slot_identity["uuid"]` is empty: `backfill_account_uuid(number, ta_uuid)`
  (wrapped in try/except → debug log, never breaks the freshen), return `False`.
  **Backfill happens only when there is no org conflict** (a wrong-org credential
  would poison the slot's identity; backfill never rewrites a non-empty uuid).
- Else return `slot_identity["uuid"] != ta_uuid`.

### `_perform(number, email, trigger) -> TickOutcome`

- **Dry-run:** emit `SwitchEvent(trigger, from_ref=current ref or None, to_ref=_ref(number,email), dry_run=True)`, return `SWITCHED`. No writes.
- **Real:** hold the state lock across the whole recheck→switch→record sequence
  (so two concurrent engines make one serialized decision: the loser re-reads the
  winner's `lastSwitchAt` and backs off). No deadlock cycle — the switch path's
  own locks never take the state lock.
  1. `state = _read_state()`. If `trigger == "proactive" and _in_cooldown(state)`:
     emit `NoSwitchEvent("cooldown")`, return `NO_ACTION` (re-check under lock).
  2. `result = switcher.switch_to(number, json_output=True)`.
  3. If `not result or not result.get("switched")`: emit
     `NoSwitchEvent("already-active", detail=result.get("reason",""))`, return `NO_ACTION`.
  4. Else set `state["schemaVersion"]=1`, `state["lastSwitchAt"]=self.clock()`,
     `state["lastSwitchTo"]=number`, `atomic_write_json(state_path, state)`.
  5. (Outside lock) emit `SwitchEvent(trigger, from_ref=result["from"], to_ref=result["to"], warnings=result.get("warnings",[]))`, return `SWITCHED`.

`switch_to(...)` returns a dict with keys `schemaVersion, switched (bool),
from ({number,email}), to ({number,email}), strategy, reason, message, warnings`.
`_ref(number, email) = {"number": int(number), "email": email}`.

---

## 13. Cooldown

- `_in_cooldown(state)`: `last = state.get("lastSwitchAt")`; if not int/float →
  `False`; else `(self.clock() - last) < settings.cooldown_seconds`.
- Default `cooldown_seconds = 300.0`.
- **Set** only on a real (non-dry-run) switch, to `self.clock()` (wall time).
- **Applies only to `proactive`.** `at-limit` (active headroom `<= 0`) and
  `failover` bypass it entirely.
- Checked twice: once before candidate selection, once again under the state lock
  in `_perform` (to serialize concurrent engines).

---

## 14. Quarantine lifecycle

**Triggered** (both in `_freshen_target` path, real ticks only):
- `"invalid_grant"` — the slot's refresh-token lineage is dead (token endpoint
  answered `invalid_grant`/`no_refresh_token`).
- `"identity-conflict"` — the slot's credential is alive but authenticates as a
  different account (or different org).

`_quarantine(number, email, reason)`:
1. `creds = read_account_credentials(number, email)`; `fingerprint = credential_fingerprint(creds) if creds else None`.
2. `_mutate_state` adds `state["quarantine"][number] = {email, reason, at: _now_iso(), refreshTokenFingerprint: fingerprint}`.
3. emit `QuarantineEvent(number, email, reason)`.

`credential_fingerprint(credentials)`: `"sha256:" + sha256(refreshToken)` when a
non-empty refresh token exists (survives access-token rotation, so two
generations of one lineage compare equal); else `"sha256-full:" + sha256(full
credentials)`; `None` only for empty input.

**Cleared** by `_release_recovered_quarantines(state)` (real ticks only, at the
top of each tick):
- For each quarantined `number`: `email_now = account_email(number)`.
  - If `email_now` is empty or `!= entry["email"]` → release, reason `"account-replaced"`.
  - Else compute current `fingerprint`; if `!= entry["refreshTokenFingerprint"]` →
    release, reason `"credentials-replaced"` (the user re-logged in / re-captured).
- Releases drop the entries under `_mutate_state` and emit `UnquarantineEvent` per release.

**Persistence:** in `autoswitch_state.json["quarantine"]`. Survives across engine
instances and processes (`test_quarantine_persists_across_engine_instances`).

**Migration note (verbatim from source):** older setup-token quarantines stored
`None` where the shared helper now yields a full-content hash — those release once
on first recheck and re-quarantine on the next dead freshen (one harmless extra
cycle, migration only).

---

## 15. Polling loop & inter-tick timing

### `run_loop() -> int`
```
while True:
    self._wake.clear()          # clear at TOP, not after the wait (a wake racing
                                # a wait timeout is never lost)
    if self._stop.is_set():  return 0
    try:  outcome = self.tick()
    except Exception:  emit ErrorEvent(...); outcome = ERROR   # tick() already guards
    delay = self._next_delay(outcome)
    if delay > settings.interval_seconds * 1.5:
        until = now_utc + delay;  emit SleepEvent(seconds=delay, until=<iso Z>)
    self._wake.wait(delay)
```
- `stop()` sets both `_stop` and `_wake` events; safe before the loop starts
  (never cleared → loop exits without a tick). Wired to `SIGTERM` in the CLI.
- `wake()` sets `_wake` — cuts the current sleep short and ticks now (used by
  `apply_threshold`).
- A `SleepEvent` is emitted only when the chosen delay exceeds `interval * 1.5`.

### `_next_delay(outcome) -> float`
```
interval = settings.interval_seconds
if outcome is BLOCKED:
    if _sleep_until_ts is not None:
        delay = _sleep_until_ts - self.clock()
        return min(max(delay, interval), MAX_SLEEP_S)      # clamp [interval, 21600]
    if _blocked_wait_long:
        return max(interval, NO_RESET_FALLBACK_S)          # exhausted/no-candidates, no reset
    # else: blocked on a resolvable condition -> fall through to jittered normal
elif outcome is NO_ACTION and _idle_hold_slow:
    return max(interval, NO_RESET_FALLBACK_S)              # idle-hold crawl
return interval * (0.9 + 0.2 * random.random())            # ±10% jitter
```

**Timing summary:**
- Normal tick delay: `interval * U(0.9, 1.1)` (default 54–66 s).
- Blocked with a known reset: sleep to `reset + RESET_SLACK_S`, clamped
  `[interval, MAX_SLEEP_S]` (21600 s / 6 h cap).
- Blocked, exhausted, no reset / no candidates: `max(interval, 300)`.
- Idle-hold NO_ACTION: `max(interval, 300)`.
- Blocked on a resolvable condition (hysteresis fail / unreadable candidate):
  normal jittered cadence (so the at-limit escape isn't missed).

---

## 16. Interaction with account kinds & disabled accounts

- **Active api-key account** (`account_kind_for(current) == "api_key"`) and
  `include_api_key_accounts` False → `NoSwitchEvent("active-api-key")`, `NO_ACTION`.
- **API-key candidates** are excluded unless `include_api_key_accounts`; when
  included they are only a **last resort** (`ordered = api_key_candidates` only
  when no oauth candidate qualified). They have unmeasurable headroom and are
  never refreshed (`_freshen_target` returns `"ok"` immediately for api_key).
- **Disabled accounts** (`cswap disable`): excluded by
  `switchable_account_numbers()` (which filters `_account_is_switchable` and
  `disabled`). They never appear as candidates and never consume a poll slot. A
  disabled *active* account stays live but is not an automatic target.
- **Quarantined accounts:** excluded from candidates and from poll scheduling.

---

## 17. `pct_label` display helper

`pct_label(value) = f"{value:.10g}"`. Ten significant digits: `90.0`→`"90"`,
`99.9`→`"99.9"`, `85.555555`→`"85.555555"`, `100.0 - 37.4`→`"62.6"`,
`99.85000000000001`→`"99.85"`. Used on **both sides** of the `below-threshold`
detail and the poll header's `switch at X%` so a `.0f`-rounded left side never
renders an impossible `"100% < 99.9%"`.

---

## 18. Model-name typo guard (`_check_model_names`)

One-shot per engine run. `_model_check_done` starts `True` when `_models` is
empty. When a model filter is configured:
- `wanted = {m.lower(): m for m in _models if m.lower() != "all"}`. If empty
  (bare `"all"`): set `_model_check_done = True`, return (no name match needed).
- `relevant` = switchable, non-quarantined, non-api-key accounts.
- `values = [usage.get(n) for n in relevant]`; `readable = [dict values]`.
- If not every relevant account has a readable dict this tick
  (`not readable or len(readable) != len(values)`): return without setting done
  (re-check next tick — adaptive polling legitimately leaves gaps).
- Else collect `seen` = lowercased scoped window names across all readable usages;
  set `_model_check_done = True`; `missing = [name for low, name in wanted.items() if low not in seen]`;
  if any missing, emit `ConfigWarningEvent("autoswitch.model: {names} matches no
  account's usage windows — only the 5h/7d limits are being watched for it (typo?)")`.
- Warns **at most once per run**, never per tick; never triggers a forced refresh.

---

## 19. CLI wiring (`_auto_command`)

- Flags: `--once`, `--json`, `--interval SECONDS`, `--threshold PCT`,
  `--cooldown SECONDS`, `--model NAMES`,
  `--include-api-key-accounts` (`BooleanOptionalAction`, default `None`),
  `--dry-run`, `--debug`. `auto` must be the **first** argument (pre-dispatched).
- Settings = `merged_with_cli(load_settings(switcher.backup_dir), args)`.
- Root guard (non-Windows): `os.geteuid() == 0 and not _is_running_in_container()`
  → `"Error: Do not run this script as root (unless running in a container)"`, exit 1.
- `--once`: `sys.exit(engine.tick().value)`.
- Loop mode: install `SIGTERM → engine.stop()`; if not `--json`, print a dimmed
  banner `"Auto-switch running: threshold {:.0f}%, every {:.0f}s{ (dry-run)?} — Ctrl-C to stop"`;
  `sys.exit(engine.run_loop())` (returns 0).
- Emit callbacks:
  - `jsonl_emit`: `print(json.dumps(event.to_json()), flush=True)` — **one JSON
    object per line on stdout**.
  - `human_emit`: `print(f"{HH:MM:SS}  {line}", flush=True)`, where `line` =
    `event.human()`, colorized — `switch`→accent, `error`/`account-quarantined`→
    yellowed, `poll`/`no-switch`/`sleep`→dimmed.
- `ClaudeSwitchError` → JSON `error_envelope(e)` (`{"schemaVersion":1,"error":{"type","message"}}`)
  or `"Error: {e}"`, exit 1. `KeyboardInterrupt` → `"Auto-switch stopped"`, exit 130.

---

## 20. Edge cases & subtleties (from the tests)

1. **Tie-break by sequence order** (`test_tie_resolves_to_earliest_slot`): equal
   headroom → the earlier slot in `switchable_account_numbers()` order wins (the
   `qualifying.sort(key=-h)` is stable, preserving list order).
2. **Strictly-better-candidate (#115)** (`test_issue_115...`): active bound by 5h
   99%, candidate bound by 7d 89% → `89 < 90` and `99-89 >= 10` → switch. The old
   absolute bar (`<= 80% used`) is gone; the gate is relative.
3. **Proactive never lands at/over threshold** (`test_proactive_never_lands...`):
   threshold 80, hysteresis 5, active 90% / candidate 85% → candidate is 5 better
   but sits at/over 80 → **BLOCKED** (`no-qualifying-candidate`).
4. **Stable landing doesn't switch back** (`test_stable_landing...`): even with
   cooldown 0, after 99→89 the roles reverse and the old 99% can never beat the new
   89% → one-way move, no flap.
5. **Mixed unknown + exhausted is NOT all-exhausted**
   (`test_mixed_unknown_and_exhausted...`): one candidate at 100%, one unreadable →
   BLOCKED `no-qualifying-candidate`, `_sleep_until_ts is None`, normal cadence.
6. **Stale-beyond-trust blocks without all-exhausted**
   (`test_stale_beyond_trust_blocks_all_exhausted`): an aged-out entry reads as
   unknown → treated like case 5.
7. **Trusted-stale exhausted set still fires all-exhausted**
   (`test_trusted_stale_exhausted_set_still_fires_all_exhausted`): entries in
   failure state with `trust_extended=True` still count as "known and exhausted";
   `earliest_reset_at` comes from their stored reset.
8. **`all-exhausted` earliest reset = min across accounts**
   (`test_all_exhausted_carries_earliest_reset`): three at 100% with resets
   12:00/10:30/11:00 → `earliestResetAt == "2026-07-03T10:30:00Z"`, `_sleep_until_ts` set.
9. **Dual-exhausted candidate recovers at its LATER window**
   (`test_dual_exhausted_candidate_recovers_at_its_later_reset`): a candidate
   blocked on both 5h (12:00) and Fable (15:00) is only usable at 15:00; the
   engine picks 15:00, not the earlier 12:00.
10. **Unknown recovery → unprovable**
    (`test_unknown_recovery_falls_back_instead_of_oversleeping`): a blocked window
    with no `resets_at` → `earliest_reset_at is None`, `_sleep_until_ts is None`,
    `_next_delay == NO_RESET_FALLBACK_S` (300).
11. **Scoped-only exhaustion drives the wake time**
    (`test_scoped_only_exhaustion_drives_the_wake_time`): candidates blocked only
    by Fable → wake from the scoped reset (a 5h/7d-only scan finds no ≥100% window).
12. **At-limit bypasses cooldown & hysteresis** (`test_at_limit_bypasses_cooldown`,
    `test_at_limit_escapes_hysteresis_bar`): active at 100% takes any candidate with
    real headroom even one above the proactive bar. Trigger reported `"at-limit"`.
13. **At-limit never targets another at-limit account**
    (`test_at_limit_never_targets_another_at_limit_account`): all three at 100% →
    BLOCKED.
14. **Failover requires N consecutive unknown ticks**
    (`test_unknown_active_usage_waits_then_fails_over`): with `unhealthy_ticks=3`,
    ticks 1&2 → NO_ACTION, tick 3 → SWITCHED (trigger `"failover"`). A healthy read
    in between **resets** the counter (`test_known_active_usage_resets_unhealthy_counter`).
15. **Failover ignores the hysteresis bar** (`test_failover_ignores_hysteresis_bar`).
16. **All candidates unknown → `no-comparison`** (not all-exhausted).
17. **Unmanaged live login is never touched** (`test_unmanaged_live_login_is_never_touched`):
    `current_account_number()` returns `None` while `has_live_login()` is True →
    `unmanaged-active-account`, credentials file byte-identical afterward.
18. **Idle-hold** (`TestIdleHold`): owned+expired `USAGE_TOKEN_EXPIRED` sentinel →
    holds indefinitely (up to `IDLE_HOLD_MAX_S`) with reason `"active-idle"`,
    `_unhealthy_ticks` stays 0, cadence crawls (`_next_delay >= 300`). Past the cap
    the sentinel counts as unhealthy again → failover after `unhealthy_ticks`. A
    healthy read **resets the hold clock** (`test_recovery_resets_the_hold_clock`).
    A plain `None` is **not** the idle sentinel — it increments unhealthy and clears
    `_idle_hold_since` (`test_plain_fetch_failure_still_counts_unhealthy`).
19. **Expired active enters idle-hold even during backoff**
    (`test_expired_active_enters_idle_hold_even_during_backoff` /
    `test_idle_hold_skips_candidate_polling`): the owned+expired sentinel is derived
    by the collector locally (no request), so it is visible even when the active
    row is in a `Retry-After` failure backoff; during a hold **no candidate is
    polled at all** (`counts == {}`).
20. **Backoff keeps trusted headroom** (`test_active_in_backoff_keeps_trusted_headroom`):
    active in a 429 backoff with last-good aged past `STALE_OK_S` but `trust_extended`
    → headroom stays known, no unhealthy ticks, no escalate-all burst, active not re-fetched.
21. **Adaptive scheduler** (`TestAdaptiveScheduler`): baseline fetches active +
    exactly one due candidate (stalest first); escalates to full refresh when
    active within `ESCALATION_MARGIN_PCT` of threshold or active unknown; urgent
    60 s cadence when the active account is *moving inside the band*; unmoved usage
    decays ×1.5 toward the ceiling; exhausted accounts park at their reset; polls
    are clamped to `reset + RESET_SLACK_S`; quarantined candidates never consume the
    poll slot.
22. **Escalation keys on the tick-snapshot threshold**
    (`test_collect_escalates_on_the_tick_snapshot_threshold`): the `threshold`
    argument, not a re-read of `self.settings` — active 80% escalates against 90 but
    not against 99.9.
23. **API-key accounts** (`TestApiKeyAccounts`): excluded by default; last-resort
    when included; used when all oauth exhausted; an active api-key idles the engine.
24. **Freshening** (`TestFreshening`): near-expiry target is refreshed and the
    rotated token ends up live after the switch; a fresh target is not refreshed
    (`mock_refresh.assert_not_called()`); `invalid_grant` quarantines then tries the
    next candidate; `transient` skips without quarantine and returns **ERROR** (with
    an `ErrorEvent`); a live-session target is skipped (BLOCKED) even with a fresh
    token and even when near-expiry.
25. **Token identity** (`TestTokenIdentity`): uuid backfill onto a blank-uuid slot;
    conflicting uuid → identity-conflict (but the rotated generation is still
    persisted); same-uuid-different-org → conflict; malformed token identity is
    ignored (returns `"ok"`, credential persisted); org conflict is checked before
    the blank-uuid backfill (foreign uuid NOT backfilled); a dead slot is
    quarantined outright even when a safety-copy credential exists (no auto-promotion).
26. **Dry-run** (`TestDryRunAndNoOp`): reports the would-switch as SWITCHED with
    `dryRun: true`, mutates nothing (active unchanged, live creds byte-identical,
    `state() == {}`), never freshens or quarantines, never releases quarantines
    (a still-recorded quarantine keeps the slot out → BLOCKED).
27. **`already-active` result → NO_ACTION**, no `lastSwitchAt` recorded.
28. **State lock preserves concurrent writes** (`test_state_lock_preserves_concurrent_writes`):
    an interleaved quarantine write and a `lastSwitchAt` write both survive.
29. **Event envelope** (`TestEventsShape`): every event `to_json()` has
    `schemaVersion == 1`, `event == kind`, `ts` ending in `"Z"`. Switch `from`/`to`
    are `{"number": int, "email": str}`.
30. **Poll windows match the decision set** (`test_poll_event_windows_match_the_decision_set`):
    scoped windows shown only when a model is configured;
    `"#2: 5h 3% · 7d 89%"` without model, `"… · Fable 21%"` with `model="Fable"`.
31. **Run loop** (`TestRunLoop`): loop ticks until `stop()`; survives a raising
    `_tick_inner` (emits ErrorEvent, keeps going); a `stop()` before start yields 0
    ticks; a `wake()` during a tick cuts the following sleep short (clear-at-top
    ordering); blocked-with-reset sleeps to reset; blocked-exhausted-without-reset
    uses 300; blocked-resolvable keeps normal cadence; normal delay is jittered
    54–66; sleep is capped at 6 h.
32. **Model-aware switch** (`TestModelAwareSwitch`): switches off a
    model-exhausted account despite session headroom; comma lists switch on any
    named model; `"all"` binds every scoped window; unmatched name warns once per
    run (not per tick) and never while an account is unreadable.
33. **`pct_label`** (`TestPctLabel`): exact rendering rules and the
    `below-threshold` detail never showing `"100% < 99.9%"`.

---

## 21. External-system knowledge (quote-exact — most valuable for the port)

### OAuth token refresh endpoint (`oauth.py`)
```
OAUTH_EXPIRY_BUFFER_MS = 5 * 60 * 1000        # 300000
OAUTH_TOKEN_URL  = "https://platform.claude.com/v1/oauth/token"
OAUTH_CLIENT_ID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
```
`try_refresh_oauth_credentials(credentials)` POSTs JSON body:
```json
{ "grant_type": "refresh_token", "refresh_token": "<rt>",
  "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e" }
```
Headers: `Content-Type: application/json`, `User-Agent: claude-swap/1.0`.
`urlopen(..., timeout=10)`. On success reads `access_token`, `expires_in`
(seconds → `expiresAt = now_ms + expires_in*1000`), optional `refresh_token`
(rotation — replaces the stored one when present), optional `scope` (`.split()` →
`scopes`). The rotated credentials JSON preserves the outer envelope and rewrites
`data["claudeAiOauth"]`.

**Failure classification (verbatim intent):** permanent **only** when the server
itself rejected the grant — an HTTP 400/401/403 whose body contains
`"invalid_grant"` or `"invalid_client"` → `error="invalid_grant"`. Anything else
(other HTTP codes, network, parse) → `error="transient"`. `no_refresh_token` when
the credential has no usable refresh token. *A misclassified transient costs one
retry; a misclassified permanent would wrongly quarantine a live token.*

**`token_account` (`_parse_token_account`):** the response may carry
`account` / `organization` objects; usable identity requires a non-empty string
`account.uuid`; result `{"uuid": <stripped>, "email": account.email_address or
None, "organizationUuid": organization.uuid or None}`; anything malformed → None.
Opportunistic — never rely on it, never discard it.

### Claude Code credential file formats
- Live login credentials (Linux/WSL): `~/.claude/.credentials.json`, shape
  `{"claudeAiOauth": {"accessToken", "refreshToken", "expiresAt"?, "scopes"?}}`.
- Live identity: `~/.claude.json`, shape `{"oauthAccount": {"emailAddress", "accountUuid", "organizationUuid"?, "organizationName"?, ...}}`.
- Stored account backups carry the same `{"claudeAiOauth": {...}}` blob. `expiresAt`
  is **epoch milliseconds**.

### Anthropic usage API (poll_policy / oauth)
- `GET https://api.anthropic.com/api/oauth/usage` — 5h / 7d / per-model scoped
  utilization windows. Normalized shape used internally:
  `{"five_hour": {"pct", "resets_at"?}, "seven_day": {...}, "scoped": [{"name","pct","resets_at"?}], "spend": {...}}`.
- **Rate-limit model (verbatim from `poll_policy` docstring):** the endpoint
  enforces a per-access-token budget on non-first-party clients — *a rolling
  ~60-minute window of ~28-30 requests per token × UA-class*. It is **not** a
  refilling bucket: capacity returns only as old requests age out of the trailing
  hour, so a burst saturates for up to a full hour. The budget target is *an
  average of at most ~1 request / 3 minutes per token* (`SERVE_TTL_S = 180`).
  - `Retry-After: 0` = saturated-budget edge → wait ≥ `EDGE_BACKOFF_S = 300` s
    before probing; while any 429 was seen within `RECENT_429_WINDOW_S = 3600` s,
    floor cadence at `POST_429_MIN_INTERVAL_S = 360`.
  - `Retry-After: N>0` = burst rule → honor as the wait, capped at
    `RETRY_AFTER_FLOOR_CAP_S = 900`.
- Poll cadence constants (`poll_policy.py`): `SERVE_TTL_S=180`, `MIN_INTERVAL_S=180`,
  `URGENT_INTERVAL_S=60`, `ACTIVE_MAX_INTERVAL_S=300`, `CANDIDATE_DEFAULT_INTERVAL_S=300`,
  `CANDIDATE_MAX_INTERVAL_S=600`, `MOVEMENT_DELTA_PCT=1.0`, `JITTER_FRAC=0.1`,
  `ESCALATION_MARGIN_PCT=15.0`, `RESET_SLACK_S=60`. Usage-store trust:
  `STALE_OK_S=300`, `TRUST_MAX_AGE_S=3600`, `AUTH_DEAD_STRIKES=1`.
- Reset timestamps: ISO 8601 with `Z`; parsed via `datetime.fromisoformat` after
  `Z`→`+00:00`; unparseable → None.

### Claude Code refresh-buffer coupling (verbatim intent)
`FRESHEN_BUFFER_MS = 10 min = 2 ×` Claude Code's own 5-minute refresh buffer. A
running Claude Code re-reads credentials under its own lock and *aborts its own
refresh if the token is not expired*; freshening a target to outlive **twice**
that buffer means CC's post-lock re-read sees a fresh token and stands down —
which is what makes a proactive swap safe under the macOS ~30 s Keychain cache latency.

---

## 22. Go port notes

1. **Frozen dataclasses → immutable structs / value types.** Events are frozen;
   `AutoSwitchSettings` is frozen and `apply_threshold` does
   `dataclasses.replace(...)` (copy-with-one-field-changed) then atomically swaps
   `self.settings`. In Go, store settings behind an atomic pointer (`atomic.Pointer`)
   or a mutex; a tick must snapshot it once.

2. **Threading model.** Two `threading.Event`s: `_stop` and `_wake`. `run_loop`
   waits with `self._wake.wait(delay)` (timeout **or** a set-event wakeup). Go
   equivalent: a `time.Timer`/`time.After(delay)` selected against a buffered
   `wake chan struct{}` and a `stop chan struct{}` (or `context.Context`). Preserve
   the **clear-at-top** ordering: drain/clear the wake signal at the top of the loop
   before checking stop and ticking, so a `wake()` racing a timeout is never lost.
   `stop()` must be latching (idempotent, never cleared) — engines are single-use.

3. **File locking is cross-process, advisory, blocking-with-timeout.** `FileLock`
   uses POSIX `fcntl.flock(LOCK_EX|LOCK_NB)` in a retry loop (0.1 s sleep, 10 s
   default timeout → `LockError`) / Windows `msvcrt.locking`. The state lock file is
   `.autoswitch_state.lock` **beside** the state file. Go: `github.com/gofrs/flock`
   or raw `syscall.Flock`; replicate the poll+timeout and the two-platform split.
   The lock is a **separate** file from the data file. `LockError` surfaces as a
   `ClaudeSwitchError` → tick returns ERROR (exit 1). Note the invariant: the state
   lock is never held across the switch path's own locks (no deadlock cycle), and
   `_mutate_state` is never called while another lock is held.

4. **Atomic JSON writes** (`atomic_write_json`): `tempfile.mkstemp` in the target
   dir → write → `os.replace` (atomic rename) → `chmod 0600` file / `0700` parent
   (skipped on Windows). `json.dumps(data, indent=2)`. Go: write temp in same dir,
   `os.Rename`, `os.Chmod`; guard Windows.

5. **Wall-clock vs monotonic.** `self.clock` defaults to `time.time` (wall clock) —
   persisted `lastSwitchAt`/cooldown must survive processes, so it **must** be wall
   time, not monotonic. The lock's internal retry uses `time.monotonic`. Keep these
   two clocks distinct in Go (`time.Now()` for persisted state, a monotonic source
   for the lock retry). Tests inject a `FakeClock` — the port should keep `clock`
   injectable for determinism.

6. **`expiresAt` is epoch milliseconds**; `now_ms = self.clock() * 1000`. The
   near-expiry test is `now_ms + FRESHEN_BUFFER_MS >= expires_at`. Be careful with
   int/float — Python compares floats; Go should use `float64` or careful int64 ms.

7. **`pct_label` = `%.10g`.** Go's `strconv.FormatFloat(v, 'g', 10, 64)` is the
   closest match; verify `90.0`→`"90"`, `99.9`→`"99.9"`, `62.60000000000001`→`"62.6"`.
   The `%.0f` renderings in human/`_describe` (`f"{pct:.0f}%"`, `f"{100-h:.0f}%"`)
   round-half-to-even in Python — but only used for display.

8. **Randomness.** `random.random()` for ±10% jitter in `_next_delay`; `poll_policy`
   jitter uses an injectable rng (`JITTER_FRAC`, zeroed in tests via monkeypatch).
   Go: `math/rand`; make it injectable for tests.

9. **Dict iteration order.** Python 3.7+ dicts preserve insertion order.
   `switchable_account_numbers()` returns rotation order; `qualifying.sort` is
   **stable** so ties keep sequence order. Go maps are unordered — the port must
   carry candidate order as a slice throughout and use a stable sort.

10. **Enum values double as exit codes** — model `TickOutcome` as typed int
    constants (SWITCHED=0, ERROR=1, NO_ACTION=2, BLOCKED=3) and `os.Exit(int(outcome))`.

11. **Exception → ErrorEvent mapping.** `tick()` catches `ClaudeSwitchError`
    (`transient=True`, `message=str(e)`) and any other `Exception`
    (`message="{TypeName}: {e}"`). Go has no exceptions — thread errors up and
    classify at the tick boundary (a domain error type vs. anything else) to
    reproduce both message formats.

12. **`on_event` exceptions are NOT caught** (a broken frontend should fail loudly
    in tests). In Go, either let a callback panic propagate or document that the
    callback must not error.

13. **JSONL output ordering & flushing.** `print(..., flush=True)` — one compact
    JSON object per line, flushed immediately. Go: `json.Marshal` (compact) +
    `\n` + flush the writer each event. Keep key order stable within objects if
    scripts depend on it (they should key by name, but the tests read via parsed
    dicts).

14. **Idempotent/no-op sentinels are strings.** `USAGE_TOKEN_EXPIRED == "token
    expired"` etc. compared by equality against `usage[num]`. Model the collector's
    decision value as a sum type `dict | string-sentinel | nil`.

15. **`_release_recovered_quarantines` and freshening are skipped in dry-run** —
    dry-run must perform **zero** mutations (no state write, no token refresh, no
    quarantine, no unquarantine). The dry-run short-circuit sits *before*
    `_freshen_target` in the candidate loop.

16. **`account_headroom` returns `None` for "unknown"** (unavailable or no window
    data) — callers must treat `None` as unknown and never auto-skip; only a numeric
    `<= 0` is "at limit". Preserve the three-way None / `<=0` / `>0` distinction.
