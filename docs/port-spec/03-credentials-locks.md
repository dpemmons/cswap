# cswap Spec 03 — Credential Storage, Paths, and Locking

## Overview

This spec covers three leaf subsystems of `claude-swap` (cswap), a multi-account
switcher for Claude Code: **path resolution** (`paths.py` — where Claude Code's
config/credential files live and where cswap keeps its own backup tree),
**credential storage** (`credentials.py` + `macos_keychain.py` — how cswap reads
and writes Claude Code's *active* credential and its own *per-account backups*,
routing between the macOS Keychain and plaintext files with a sticky per-process
capability cache), and **two independent locking mechanisms** (`claude_locks.py`
— an npm-`proper-lockfile`-compatible directory lock that cooperates with a
running Claude Code's own advisory locks; and `locking.py` — cswap's own
cross-process `flock`/`msvcrt` file lock serializing cswap invocations against
each other). Because cswap reads and writes files owned by Claude Code, large
parts of this spec are *encodings of external Claude Code / npm behavior* that
the Go port must reproduce byte-for-byte — those are called out explicitly and
quoted verbatim throughout.

Everything here is behavior a Go port must preserve exactly: file paths, env
vars, keychain service/account names, JSON keys, subprocess argv, numeric
timeouts/thresholds, return codes, and error text.

---

## 1. Platform detection (dependency — from `models.py`)

`Platform` is an enum with variants `MACOS`, `LINUX`, `WSL`, `WINDOWS`,
`UNKNOWN`. `Platform.detect()` (used by `paths.get_backup_root`) resolves the
platform from `sys.platform` (NOT `platform.system()`, to avoid a WMI hang on
Windows):

```
sys.platform == "darwin"            -> MACOS
sys.platform == "win32"             -> WINDOWS
sys.platform startswith "linux":
    os.environ["WSL_DISTRO_NAME"] set (non-empty) -> WSL
    else                                          -> LINUX
otherwise                           -> UNKNOWN
```

Note: `CredentialStore` does **not** call `Platform.detect()` itself — it reads
`self._host.platform`, an attribute set once on the switcher (`switcher.platform
= Platform.detect()` normally, but tests override it, e.g. force `MACOS` on a
Linux host). Only `get_backup_root` calls `Platform.detect()` live.

Go port: `sys.platform`-equivalent is `runtime.GOOS` (`darwin`/`windows`/`linux`).
WSL detection is purely the `WSL_DISTRO_NAME` env var being non-empty — there is
**no** `/proc/version` sniffing anywhere in this codebase.

---

## 2. Path resolution (`paths.py`)

All functions return `pathlib.Path`. `Path.home()` = `$HOME` on POSIX,
`$USERPROFILE` on Windows (tests set both). Env lookups use `os.environ.get`.

### 2.1 Constants

- `LEGACY_BACKUP_DIRNAME = ".claude-swap-backup"`
- `_THROWAWAY_NAMES = {"cache"}` — directory/file names in the backup root that
  hold no real user data.
- `_THROWAWAY_PREFIXES = ("claude-swap.log",)` — filename prefixes that are
  throwaway (matches `claude-swap.log`, `claude-swap.log.1`, etc.).

### 2.2 `get_claude_config_home() -> Path`

The Claude config home directory.

```
env = os.environ.get("CLAUDE_CONFIG_DIR")
if env (truthy / non-empty):  return Path(env)
else:                         return Path.home() / ".claude"
```

### 2.3 `get_global_config_path() -> Path`

Path to Claude Code's global config file (`~/.claude.json`). **Mirrors
claude-code `utils/env.ts getGlobalClaudeFile`.**

```
legacy = get_claude_config_home() / ".config.json"
if legacy.exists():  return legacy                     # legacy layout wins
env = os.environ.get("CLAUDE_CONFIG_DIR")
base = Path(env) if env else Path.home()
return base / ".claude.json"
```

**Critical asymmetry (documented in-source and tested):** by default
`.claude.json` lives at the **home directory root**, NOT inside `.claude/`. But
the *credentials* file (§2.5) DOES live inside `.claude/`. When
`CLAUDE_CONFIG_DIR` is set, `.claude.json` resolves relative to it
(`<CCD>/.claude.json`). The legacy `<config_home>/.config.json` takes precedence
whenever it exists (whether config_home is `~/.claude` or a `CLAUDE_CONFIG_DIR`
override).

### 2.4 `get_default_global_config_path() -> Path`

Same legacy-vs-`.claude.json` fallback but **deliberately ignores
`CLAUDE_CONFIG_DIR`** — always resolves against `Path.home()`:

```
legacy = Path.home() / ".claude" / ".config.json"
if legacy.exists():  return legacy
return Path.home() / ".claude.json"
```

Rationale (in-source): session-sharing callers that mirror the user's *real*
profile must not source from a nested session's `CLAUDE_CONFIG_DIR`. (This
function is outside the core credentials path but shares the file; port it for
completeness.)

### 2.5 `get_credentials_path() -> Path`

```
return get_claude_config_home() / ".credentials.json"
```

So it honors `CLAUDE_CONFIG_DIR` (via config_home). Default:
`~/.claude/.credentials.json`.

### 2.6 `get_legacy_backup_root() -> Path`

```
return Path.home() / ".claude-swap-backup"
```

### 2.7 `get_backup_root() -> Path`  (cswap's own backup tree)

**Platform-conditional. Calls `Platform.detect()` live.**

```
if Platform.detect() in (LINUX, WSL):
    xdg = os.environ.get("XDG_DATA_HOME", "")
    if xdg:
        xdg_path = Path(os.path.expanduser(xdg))
        if xdg_path.is_absolute():
            return xdg_path / "claude-swap"
    return Path.home() / ".local" / "share" / "claude-swap"
# macOS / Windows / UNKNOWN:
return get_legacy_backup_root()      # ~/.claude-swap-backup
```

XDG rules (per the XDG Base Directory Specification, quoted URL in source:
`https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html`):
- `$XDG_DATA_HOME` is **ignored when unset, empty string, or non-absolute**
  (falls back to `~/.local/share/claude-swap`).
- A leading `~` is expanded (`os.path.expanduser`) so `~/custom-data` set in a
  systemd unit / Dockerfile (no shell expansion) still resolves. So
  `XDG_DATA_HOME=~/custom-data` → `~/custom-data/claude-swap`.

The switcher derives the credential-storage directory from this root:
`backup_dir = get_backup_root()`, then `credentials_dir = backup_dir /
"credentials"`. This `credentials_dir` is what `CredentialStore` reads as
`self._host.credentials_dir`. (Other siblings: `sequence_file = backup_dir /
"sequence.json"`, `configs_dir = backup_dir / "configs"`, `lock_file = backup_dir
/ ".lock"`, `cache = backup_dir / "cache"`.)

### 2.8 `migrate_legacy_backup_dir(target: Path) -> bool`

One-time migration of the pre-XDG `~/.claude-swap-backup` tree to the new
`target` (the XDG path). No-op on macOS/Windows (where target == legacy).
Returns `True` iff a move actually ran this call, `False` if a no-op. Raises
`MigrationError` (a subclass of `ClaudeSwitchError`) on a genuine collision or an
`OSError` during the move.

Flag file location: `flag = target.parent / f".{target.name}.migrating"` (e.g.
`~/.local/share/.claude-swap.migrating`).

Algorithm:

```
legacy = get_legacy_backup_root()
try: same_path = legacy.resolve() == target.resolve()
except OSError: same_path = (legacy == target)
if same_path: return False                     # macOS/Windows no-op

flag = target.parent / f".{target.name}.migrating"

if not legacy.exists():
    flag.unlink(missing_ok=True)               # prior run finished, died before flag cleanup
    return False

try:
    if flag.exists():
        # interrupted prior run: discard any partial target, retry the move
        if target.exists(): shutil.rmtree(target)
    elif target.exists():
        if _target_has_meaningful_data(target):
            raise MigrationError(
                f"Both legacy ({legacy}) and new ({target}) backup paths exist. "
                f"Refusing to merge or overwrite — inspect both and remove the "
                f"stale one manually before re-running."
            )
        _wipe_throwaway_artifacts(target)      # only cache/ + log files present → wipe

    target.parent.mkdir(parents=True, exist_ok=True)
    flag.touch()
    shutil.move(legacy, target)                # atomic rename same-FS; copy+unlink cross-FS
    flag.unlink()
except OSError as exc:
    raise MigrationError(f"Migration of {legacy} → {target} failed: {exc}") from exc

return True
```

Helper `_target_has_meaningful_data(target)`: iterate `target.iterdir()`;
returns `False` if `FileNotFoundError`/`NotADirectoryError`; returns `True` on
the first entry whose name is not in `_THROWAWAY_NAMES` and does not start with
any `_THROWAWAY_PREFIXES`. So a target holding only `cache/` and
`claude-swap.log*` counts as empty.

Helper `_wipe_throwaway_artifacts(target)`: iterate entries; `shutil.rmtree` for
a real directory (not a symlink), else `entry.unlink()`; finally `target.rmdir()`
(so `shutil.move` can create it fresh).

State-machine summary (from docstring + tests):
| Flag | Legacy exists | Target exists | Action |
|------|---------------|---------------|--------|
| present | yes | (partial) | discard partial target, redo move |
| present | no  | complete    | just `flag.unlink()`, return False (target untouched) |
| absent  | yes | yes, real data | `MigrationError` "Refusing to merge" |
| absent  | yes | only throwaway | wipe throwaway, move |
| any     | no  | —           | `flag.unlink(missing_ok)`, return False |
| —       | target==legacy | —  | return False (macOS/Windows) |

`shutil.move` preserves file modes across a same-FS rename and, per test
`test_preserves_file_modes`, a cross-FS copy must preserve `0o600` file mode and
`0o700` dir mode.

---

## 3. External knowledge: where Claude Code stores credentials

This section quotes/derives what cswap knows about **Claude Code's own** storage,
which the Go port must reproduce to interoperate. Source references cited in the
Python are `utils/secureStorage/*.ts`, `utils/env.ts`, `utils/auth.ts`,
`utils/config.ts`, `utils/lockfile.ts` in claude-code.

### 3.1 The active OAuth credential

Two backends, checked in order (macOS):

1. **macOS Keychain**, generic-password item:
   - service = `"Claude Code-credentials"` (constant
     `CLAUDE_CODE_KEYCHAIN_SERVICE`)
   - account = `keychain_account_name()` (see §4.1; mirrors Claude Code's
     `getUsername()`)
   - value = the OAuth credential JSON string.
2. **Plaintext file** `<config_home>/.credentials.json` (Claude Code's own
   fallback; every platform). On Linux/WSL/Windows this is the only backend.

The credential value is a JSON object. cswap's classifier only cares that it is
NOT a raw API key — i.e. it starts with `{`. The canonical OAuth shape (from
Claude Code + the `oauth` module) is:

```json
{ "claudeAiOauth": { "accessToken": "...", "refreshToken": "...", "...": "..." } }
```

The conftest test fixture `mock_credentials_file` seeds a simplified
`{"accessToken": "test-token", "refreshToken": "test-refresh"}`. Treat the value
as an opaque string end-to-end; only §4 classification (`looks_like_api_key`)
inspects the first characters.

### 3.2 The active managed API key (`/login` with `sk-ant-api…`)

A **separate auth axis** from OAuth. Two backends (checked in order, mirroring
Claude Code's `getApiKeyFromConfigOrMacOSKeychain`):

1. **macOS Keychain**, generic-password item:
   - service = `"Claude Code"` (constant `CLAUDE_CODE_MANAGED_KEYCHAIN_SERVICE`
     — note: NO `-credentials` suffix)
   - account = `keychain_account_name()`
   - value = the raw `sk-ant-api…` key string.
2. **`~/.claude.json`** key `"primaryApiKey"` (a bare string). On non-macOS this
   is the only managed-key backend.

Claude Code's `~/.claude.json` also carries `"customApiKeyResponses"` (see §5.6):

```json
{
  "primaryApiKey": "sk-ant-api...",
  "customApiKeyResponses": { "approved": ["<last-20-chars>"], "rejected": [] },
  "oauthAccount": { "...": "..." }
}
```

**External behavior cswap mirrors exactly:**
- `normalizeApiKeyForConfig` = `apiKey.slice(-20)` → the value stored in
  `customApiKeyResponses.approved` is the **last 20 characters** of the key.
  Storing anything else makes Claude Code's "is this key approved?" check miss
  and re-prompt. (`approved_form(api_key) = api_key.strip()[-20:]`.)
- `saveApiKey`: keychain-then-config fallback (write Keychain "Claude Code" when
  possible, else `primaryApiKey`); always records the approved form even on
  Keychain success.
- `removeApiKey`: deletes the Keychain "Claude Code" item and drops
  `primaryApiKey`, but **leaves `customApiKeyResponses.approved` intact**.
- Activating one axis clears the other (mutual exclusion) — Claude Code does
  this and so must cswap.

### 3.3 Keychain read constraint (external caveat, quoted)

> `find-generic-password -w` prints the stored data raw only when it is
> printable; data with non-printable bytes comes back *hex-encoded*, so a
> write/read round-trip would not be identity. Fine for this codebase
> (credentials are ASCII JSON). Claude Code's `-w` reads share the same
> constraint.

### 3.4 Why `security` and not in-process Keychain APIs (quoted)

> Keychain items are created and read by the same stable `security` binary, so
> reads stay silent across upgrades. `keyring` (and any in-process
> Security.framework call) anchors the item's access to the *Python
> interpreter*, which `uv tool upgrade` rebuilds — at which point macOS can show
> the "wants to use your keychain" prompt. `security` never changes, so creator
> == reader and there is no prompt.

Go port note: a Go binary is itself stable across its own upgrades, but the same
principle (creator == reader) means the Go port MUST also shell out to
`/usr/bin/security` for interop — items written by the Python `security` path
must be readable by Go and vice-versa, and both must match Claude Code's own
`security`-created items. Do **not** use a CGo Security.framework binding.

---

## 4. macOS Keychain wrapper (`macos_keychain.py`)

A thin wrapper over `/usr/bin/security`. Import-safe on every platform (only
shells out at call time). Mirrors Claude Code's
`utils/secureStorage/macOsKeychainStorage.ts`.

### 4.1 Constants

- `SECURITY_STDIN_LINE_LIMIT = 4096 - 64` = **4032** bytes. (`security -i` reads
  stdin with a 4096-byte `fgets()`/BUFSIZ buffer on darwin; a longer command
  line is truncated mid-argument. 64 bytes of headroom guards line-terminator
  accounting.)
- `_NOT_FOUND_RC = 44` — `errSecItemNotFound`, surfaced by
  `find-generic-password` and `delete-generic-password`.
- `_TIMEOUT = 5.0` seconds — bounds every `security` spawn (a locked login
  keychain on a headless/SSH host can prompt for an unlock that never comes). A
  healthy Keychain answers in well under 100ms.
- `_SECURITY = "/usr/bin/security"` — **absolute path pinned**, never PATH
  resolution (a credential tool must not let an attacker-controlled `security`
  earlier on PATH intercept secrets). Present on every macOS.
- `class KeychainError(Exception)` — a `security` invocation failed for a reason
  other than "not found".
- `KEYCHAIN_ERRORS = (KeychainError, subprocess.TimeoutExpired, OSError)` — the
  tuple callers catch to mean "Keychain unusable → fall back to file". Catching
  exactly this tuple (never bare `Exception`) keeps a real programming bug loud
  instead of silently routing to the file backend.

### 4.2 `keychain_account_name() -> str`

Mirrors Claude Code's `getUsername()`
(`utils/secureStorage/macOsKeychainHelpers.ts`):

```
user = os.environ.get("USER")
if user:  return user
try:
    import pwd
    return pwd.getpwuid(os.geteuid()).pw_name
except Exception:
    return "claude-code-user"
```

**Critical:** the final fallback is the literal string `"claude-code-user"`,
**never** `"user"`. On headless/launchd/cron hosts where `$USER` is unset a
divergent default would key a *different* Keychain item than Claude Code, so the
two could not see each other's active credential.

Go port: `os.Getenv("USER")`, then `os/user.Current().Username` (or
`user.LookupId(strconv.Itoa(os.Geteuid()))`), then `"claude-code-user"`.

### 4.3 `_quote(value) -> str`

For the `security -i` stdin command line (re-parsed shell-style per line):

```
escaped = value.replace("\\", "\\\\").replace('"', '\\"')
return f'"{escaped}"'
```

I.e. backslash-escape `\` then `"`, wrap in double quotes. (The active-credential
service name `"Claude Code-credentials"` contains a space, so quoting is
required.)

### 4.4 `get_password(service, account) -> str | None`

```
subprocess.run(
    ["/usr/bin/security", "find-generic-password", "-a", account, "-w", "-s", service],
    capture_output=True, text=True, timeout=5.0)
```

- On `TimeoutExpired`: raise `KeychainError("security find-generic-password timed
  out after 5.0s")` (chained `from e`).
- rc == 0: return `result.stdout.removesuffix("\n")` — strip **exactly one**
  trailing newline (the `-w` terminator), so leading/trailing whitespace in the
  value survives. (Do NOT `.strip()`.)
- rc == 44: return `None` (not found).
- any other rc: raise `KeychainError(f"security find-generic-password failed
  (rc={rc}): {stderr.strip()}")`.

### 4.5 `item_exists(service, account) -> bool`

Attribute-only lookup, **no `-w`** (never decrypts → never prompts, even for
items owned by another app):

```
subprocess.run(
    ["/usr/bin/security", "find-generic-password", "-a", account, "-s", service],
    capture_output=True, text=True, timeout=5.0)
```

- `except (TimeoutExpired, OSError): return False`
- return `result.returncode == 0`.

**Deliberately non-raising.** rc 44, error rcs, timeout, and a missing binary all
return `False`. Used for cleanup verification only; MUST NOT feed the capability
cache (a timeout means "couldn't tell", not "Keychain works"). The source
explicitly warns: *do NOT route `item_exists` through `_kc_call`*.

### 4.6 `set_password(service, account, password) -> None`

Create-or-update (`-U`). Secret is hex-encoded and kept out of argv when
possible.

```
hex_value = password.encode("utf-8").hex()
command = f"add-generic-password -U -a {_quote(account)} -s {_quote(service)} -X {hex_value}\n"

if len(command.encode("utf-8")) <= 4032:      # SECURITY_STDIN_LINE_LIMIT
    subprocess.run(["/usr/bin/security", "-i"], input=command,
                   capture_output=True, text=True, timeout=5.0)      # stdin path
else:
    subprocess.run(["/usr/bin/security", "add-generic-password", "-U",
                    "-a", account, "-s", service, "-X", hex_value],
                   capture_output=True, text=True, timeout=5.0)      # argv fallback
```

- The trailing `\n` in the stdin command is required.
- `-X` passes the value as hex (avoids escaping issues for the secret bytes).
- Threshold test is on the UTF-8 **byte** length of the full command string,
  compared with `<=` 4032. Note hex doubles the payload length, so a raw secret
  around ~2000 bytes can trip the fallback.
- Stdin path: the secret is NOT in argv; argv is exactly `["/usr/bin/security",
  "-i"]`. Argv fallback: `input`/stdin is not used; the hex value is a raw list
  element (no shell, no quoting).
- On `TimeoutExpired`: raise `KeychainError("security add-generic-password timed
  out after 5.0s")`.
- rc != 0: raise `KeychainError(f"security add-generic-password failed (rc={rc}):
  {stderr.strip()}")`.

### 4.7 `delete_password(service, account) -> None`

```
subprocess.run(
    ["/usr/bin/security", "delete-generic-password", "-a", account, "-s", service],
    capture_output=True, text=True, timeout=5.0)
```

- On `TimeoutExpired`: raise `KeychainError("security delete-generic-password
  timed out after 5.0s")`.
- rc in (0, 44): return (already-absent counts as success).
- any other rc: raise `KeychainError(f"security delete-generic-password failed
  (rc={rc}): {stderr.strip()}")`.

### 4.8 Real-keychain interop shapes (from contract tests)

- Claude Code seeds an item with `add-generic-password -a $USER -s "Claude
  Code-credentials" -w <token> -A <keychain>`. cswap reads it via the search
  list with the §4.4 shape (no `-k`/explicit keychain), so it must be findable by
  `(account=$USER, service="Claude Code-credentials")`.
- A wrapper-created item (no `-A` any-app access) is read back via the keychain
  **search list** with no explicit keychain argument. So the Go port's read must
  also omit any keychain-file argument and rely on the default search list.

---

## 5. CredentialStore (`credentials.py`)

`CredentialStore` owns *where* credentials live and *how* they are read/written.
It is a leaf collaborator: imports only `macos_keychain` and `paths`; never
imports the switcher. It reads live config from a **host view** `_StoreHost`
(data only — never a method):

```
_StoreHost:
    platform: Platform            # read at call time (tests override post-construction)
    credentials_dir: Path
    _logger: logging.Logger
```

Its only owned mutable state: `_keychain_usable_cache` (sticky, process-local),
`_keychain_disabled_until` (monotonic re-probe deadline), and
`_last_active_credentials_backend` (`"keychain"`|`"file"`|`None`, for the
post-switch follow-up message).

### 5.1 Storage-layer constants

- `SECURITY_SERVICE = "claude-swap"` — service for **per-account backup**
  credentials (distinct from the active-credential services and from the old
  `keyring` service, so old keyring items and new security items coexist during
  migration).
- `CLAUDE_CODE_KEYCHAIN_SERVICE = "Claude Code-credentials"` — active OAuth.
- `CLAUDE_CODE_MANAGED_KEYCHAIN_SERVICE = "Claude Code"` — active managed key.
- `_ACTIVE_READ_ATTEMPTS = 2` — attempts for the active OAuth Keychain read.
- `_ACTIVE_READ_RETRY_DELAY = 0.3` seconds between those attempts.
- `KEYCHAIN_RECHECK_COOLDOWN_S = 60.0` seconds — after a Keychain failure the
  store drops to file mode; a long-running daemon re-probes once this cooldown
  elapses (a sub-second CLI command never re-probes within its own lifetime).

### 5.2 Classifiers

- `looks_like_api_key(credentials) -> bool`: `False` for falsy input; else strip
  and return `text.startswith("sk-ant-api") and not text.startswith("{")`.
  Strict on purpose: a managed key is a bare `sk-ant-api…` string; every
  OAuth/setup-token credential is a JSON object (`{...}`). Requiring the
  `sk-ant-api` prefix keeps a raw/garbled `sk-ant-oat…` setup token from being
  misread as an API key.
- `approved_form(api_key) -> str`: `api_key.strip()[-20:]` (last 20 chars; see
  §3.2).

### 5.3 The capability cache (sticky Keychain fallback)

Three-state cache `_keychain_usable_cache: bool | None`
(`None` = unprobed, `True` = usable, `False` = failed this run).

`_kc_call(fn, *args)` — run a `macos_keychain` wrapper call and learn usability:
- On success (including `get_password` returning `None` for a missing item): if
  the cache is `None`, flip it to `True`. **Never** flips `False → True` — once a
  call failed this run, stay in file mode so one invocation can't split-brain.
- On `KEYCHAIN_ERRORS` (`KeychainError` / `TimeoutExpired` / `OSError`): set
  cache `False`, set `_keychain_disabled_until = time.monotonic() +
  KEYCHAIN_RECHECK_COOLDOWN_S`, and **re-raise**. Only that tuple is caught; a
  programming error propagates.
- `item_exists` must never route through `_kc_call` (returns `False` for both
  absent and failed).

`_use_keychain() -> bool` — whether ops target the Keychain right now:
- `False` off macOS (`self._host.platform != MACOS`).
- On macOS: if cache is `False` AND `_keychain_disabled_until` is set AND
  `time.monotonic() >= _keychain_disabled_until`, reset cache to `None` and
  deadline to `0.0` (cooldown elapsed → re-probe). Then return
  `_keychain_usable_cache is not False`.
- So within one sub-second CLI command the deadline never passes; a daemon
  re-probes after 60s.

`_pin_file_mode()` — pin file mode for the rest of the process with **no
re-probe**: set cache `False`, deadline `0.0`. Used after a **write** falls back
to file, because a write's best-effort delete of the old Keychain item may have
failed, leaving a stale entry; re-probing later could resurrect the wrong
account. (A read timeout is safe to recover from — it schedules a re-probe; a
write fallback pins.)

**Monotonic clock** is used for the cooldown deadline (`time.monotonic()`), so a
wall-clock jump can't expire it early/late.

### 5.4 Reading the active credential

`ActiveCredentials = NamedTuple(value: str | None, keychain_unavailable: bool)`:
- `value` = credential string, `""` when none exists in any backend, or `None`
  on a plaintext-file read error.
- `keychain_unavailable` = `True` only when the macOS OAuth Keychain read failed
  (locked/denied/timeout) and nothing else covered it — lets the UI say
  "keychain unavailable" rather than a misleading "no credentials".

`_read_credentials() -> str | None` — thin wrapper returning
`_read_active_credentials().value` (historic contract: string / `""` / `None`).

`_read_active_oauth_keychain() -> (value, failed)` — bounded retry:
```
for attempt in range(2):                      # _ACTIVE_READ_ATTEMPTS
    try:
        value = _kc_call(macos_keychain.get_password,
                         "Claude Code-credentials", keychain_account_name())
        return (value, False)
    except KEYCHAIN_ERRORS as e:
        last_error = e
        if attempt + 1 < 2:  time.sleep(0.3)  # _ACTIVE_READ_RETRY_DELAY
# every attempt raised:
logger.warning(f"Keychain read failed after 2 attempt(s), trying file: {last_error}")
return (None, True)
```
A genuinely absent item (rc 44 → `None` without raising) reports `failed=False`
and is NOT retried. The retry rides out a transient lock/contention.

`_read_active_credentials() -> ActiveCredentials` — ordered resolution:
1. **OAuth Keychain** (macOS when `_use_keychain()`), via the bounded retry.
   `keychain_failed` captured. If a truthy value → return `(val, False)`.
   Else-branch: if already in file mode but platform is macOS, set
   `keychain_failed = True` (a prior op failed → absence below = "keychain
   unavailable").
2. **OAuth plaintext file** `get_credentials_path()` (every platform). If it
   exists: `read_text(encoding="utf-8")`; on any read exception log
   `f"Failed to read credentials file: {e}"` and return `(None, False)`. If the
   text is non-blank (`.strip()`) → return `(text, False)` (returns the raw text,
   NOT stripped).
3. **Managed key** via `_read_managed_key()`. If truthy → return `(key, False)`.
4. Nothing anywhere → return `("", keychain_failed)`.

**Ordering rationale (in-source):** trying OAuth fully first means a macOS OAuth
login that only has a file fallback (Keychain empty) is never misread as an API
key.

`_read_managed_key() -> str` (mirrors `getApiKeyFromConfigOrMacOSKeychain`):
1. If `_use_keychain()`: `_kc_call(get_password, "Claude Code",
   keychain_account_name())`; on `KEYCHAIN_ERRORS` log `f"Managed-key Keychain
   read failed: {e}"` and treat as `None`. If truthy → return it.
2. `_read_global_config()`; if `primaryApiKey` is a non-empty `str` → return it.
3. Else `""`.

`_read_global_config() -> dict | None`: path = `get_global_config_path()`; if
absent → `None`; parse JSON; on exception log `f"Failed to read global config:
{e}"` → `None`; return `data` only if it is a `dict`, else `None`.

### 5.5 Writing the active credential

`_write_credentials(credentials)` — enforce a single auth axis:
```
if looks_like_api_key(credentials):
    _write_managed_credentials(credentials.strip())
else:
    _write_oauth_credentials(credentials)
    _clear_managed_key()
```

`_write_oauth_credentials(credentials)`:
- If `_use_keychain()`: `_kc_call(set_password, "Claude Code-credentials",
  keychain_account_name(), credentials)`.
  - On `KEYCHAIN_ERRORS`: log `f"Keychain write failed, falling back to file:
    {e}"` and fall through to file mode.
  - On success: call `_refresh_stale_credentials_file(credentials)`, set
    `_last_active_credentials_backend = "keychain"`, **return**.
- File mode (non-macOS, Keychain known-unusable, or a just-failed write):
  `_write_active_credentials_file(credentials)` (raising
  `CredentialWriteError(f"Failed to write credentials: {e}")` on failure), then
  `_delete_active_keychain_entry()` (best-effort), then on macOS `_pin_file_mode()`,
  then set `_last_active_credentials_backend = "file"`.

`_refresh_stale_credentials_file(credentials)` — the **#86 hot-reload fix**
(rewrite-when-present / never-create): if `get_credentials_path()` does not
exist, return. Else `_write_active_credentials_file(credentials)`; on failure log
`f"Could not refresh .credentials.json after Keychain write ({e}); a running
session may not hot-reload until restart"` (best-effort, never fails the switch).
Quoted external rationale:

> Claude Code invalidates its memoized OAuth token only when this file's mtime
> changes or the file is absent; a Keychain-only switch leaves a stale file's
> mtime frozen, so a running session serves the old token until restart.
> Rewriting the existing file with the same fresh creds bumps the mtime (atomic
> `os.replace`, so it bumps even when content is unchanged) and keeps a
> file-reading consumer (#1414 shared `~/.claude`) consistent. We never create
> the file when absent — Keychain-only users keep their fileless posture and
> their absent-file (~30s Keychain-TTL) path already hot-reloads.

`_write_active_credentials_file(credentials)` — atomic plaintext write:
```
cred_dir = get_claude_config_home()
cred_dir.mkdir(parents=True, exist_ok=True)
cred_file = cred_dir / ".credentials.json"
fd, tmp = tempfile.mkstemp(dir=cred_dir, suffix=".tmp")
os.write(fd, credentials.encode("utf-8")); os.close(fd)
os.replace(tmp, cred_file)
if sys.platform != "win32": os.chmod(cred_file, 0o600)
# on any BaseException: close fd if open, unlink tmp (ignore OSError), re-raise
```
Note: the payload is written **raw** (no JSON re-encoding) — the credential
string is stored verbatim. Path is always `get_claude_config_home() /
".credentials.json"` (equivalent to `get_credentials_path()`).

`_delete_active_keychain_entry()` — best-effort (macOS only): if platform !=
MACOS return; else `macos_keychain.delete_password("Claude Code-credentials",
keychain_account_name())` inside a bare `try/except Exception: pass`. Needed so
Claude Code (which reads the Keychain before the file) can't resurrect a stale
entry (#30337).

### 5.6 Managed API key write / clear

`_write_managed_credentials(api_key)`:
1. `wrote_to_keychain = False`. If `_use_keychain()`: `_kc_call(set_password,
   "Claude Code", keychain_account_name(), api_key)`; on `KEYCHAIN_ERRORS` log
   `f"Managed-key Keychain write failed, falling back to config: {e}"`; else set
   `wrote_to_keychain = True`.
2. `approved = approved_form(api_key)` (last 20 chars).
3. `_update_global_config(_mutate)` where `_mutate(cfg)`:
   - `responses = cfg.get("customApiKeyResponses")`; if not a dict → `{}`.
   - `approved_list = responses.get("approved")`; if not a list → `[]`; append
     `approved` if not already present; `responses["approved"] = approved_list`.
   - `responses.setdefault("rejected", [])`.
   - `cfg["customApiKeyResponses"] = responses`.
   - if `wrote_to_keychain`: `cfg.pop("primaryApiKey", None)` (keep the key out
     of plaintext); else `cfg["primaryApiKey"] = api_key`.
   - Errors: `_update_global_config` raising `CredentialWriteError` re-raises;
     any other exception → `raise CredentialWriteError(f"Failed to write managed
     API key: {e}")`.
4. `_clear_oauth_credential()` (mutual exclusion).
5. If platform == MACOS and not `wrote_to_keychain`: `_pin_file_mode()` (stale
   "Claude Code" Keychain item may remain; managed-key reads check Keychain
   before `primaryApiKey`).
6. `_last_active_credentials_backend = "keychain" if wrote_to_keychain else "file"`.

`_clear_managed_key()` (Claude Code `removeApiKey` semantics):
- On macOS: `macos_keychain.delete_password("Claude Code",
  keychain_account_name())` in `try/except Exception: pass`.
- `cfg = _read_global_config()`; if `cfg is not None` and `cfg.get("primaryApiKey")
  is not None`: `_update_global_config(lambda c: c.pop("primaryApiKey", None))`;
  on exception log `f"Failed to clear primaryApiKey: {e}"`.
- **Leaves `customApiKeyResponses.approved` untouched.** No-op (no config
  rewrite) when no key present.

`_clear_oauth_credential()`: `_delete_active_keychain_entry()` then unlink
`get_credentials_path()` if it exists; on `OSError` log `f"Failed to remove
credentials file: {e}"`.

`_update_global_config(mutator)` — atomic, key-scoped RMW of `~/.claude.json`:
```
path = get_global_config_path()
data = _read_global_config() or {}            # exception → CredentialWriteError(...)
mutator(data)
path.parent.mkdir(parents=True, exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=path.parent, suffix=".tmp")
os.write(fd, json.dumps(data, indent=2).encode("utf-8")); os.close(fd)
os.replace(tmp, path)
if sys.platform != "win32": os.chmod(path, 0o600)
# on BaseException: close fd, unlink tmp, re-raise
```
`json.dumps(..., indent=2)`. Preserves every unmanaged key (`oauthAccount`,
projects, settings).

### 5.7 Per-account backup credentials

cswap keeps a per-slot backup of each account's credential. Two backends:
base64-encoded `.enc` files under `credentials_dir`, and the macOS Keychain under
`SECURITY_SERVICE = "claude-swap"`.

Path/name schema:
- `.enc` file: `credentials_dir / f".creds-{account_num}-{email}.enc"`
- Keychain username: `f"account-{account_num}-{email}"`
- `.prev` file: `credentials_dir / f".creds-{account_num}-{email}.enc.prev"`
- `.prev` Keychain username: `f"account-{account_num}-{email}.prev"`

`_uses_file_backup_backend() -> bool` = `not _use_keychain()`. Linux/WSL/Windows
always use `.enc` files (Windows moved off the Credential Manager, which rejects
entries over ~2500 bytes, #45); UNKNOWN uses files; macOS uses the Keychain while
usable.

**Reads are `.enc`-wins on macOS** (a fallback `.enc` written while the Keychain
was down is authoritative over a possibly-stale Keychain copy). Base64 read uses
`validate=True` to reject non-alphabet junk.

`_kc_read_backup / _kc_write_backup / _kc_delete_backup / _kc_delete_backup_prev`
— route through `_kc_call` with `SECURITY_SERVICE` and the username schemas;
raise on failure. `_kc_read_backup` returns `""` for an absent item.
`_delete_backup_keychain_quiet` wraps `_kc_delete_backup` in `try/except
Exception: log warning "Failed to delete credentials from Keychain: {e}"`.

`_write_backup_enc(account_num, email, credentials)` → `_atomic_b64_write(enc_path,
credentials)`.

`_atomic_b64_write(target, credentials)`:
```
credentials_dir.mkdir(parents=True, exist_ok=True)
encoded = base64.b64encode(credentials.encode("utf-8")).decode("utf-8")
fd, tmp = tempfile.mkstemp(dir=credentials_dir, suffix=".tmp")
os.write(fd, encoded.encode("utf-8")); os.close(fd)
os.replace(tmp, target)
if sys.platform != "win32": os.chmod(target, 0o600)
# on BaseException: close fd, unlink tmp, re-raise
```

`_read_account_credentials(account_num, email) -> str` (`""` when missing):
1. `enc_file = _backup_enc_path(...)`. `enc_present = enc_file.exists()` — wrap
   in `try/except OSError`: log `f"Failed to read credentials file: {e}"`, set
   `enc_present = False`. (Python 3.12's `Path.exists()` raises on an
   unsearchable directory where 3.13+ returns `False`; normalize to "missing".)
2. If `enc_present`: `encoded = read_text().strip()`; `decoded =
   base64.b64decode(encoded, validate=True).decode("utf-8")`. On any exception
   log `f"Failed to read credentials file: {e}"` and fall through. If `decoded`
   truthy → return it. (Empty/whitespace `.enc` → fall through to Keychain.)
3. On macOS: `_kc_read_backup(...)` inside `try/except KEYCHAIN_ERRORS: log
   f"Failed to read credentials from Keychain: {e}"`.
4. `""`.

`_write_account_credentials(account_num, email, credentials)`:
1. `_retain_previous_backup(...)` (see §5.8).
2. If `_use_keychain()`: `_kc_write_backup(...)`; on `KEYCHAIN_ERRORS` log
   `f"Keychain backup write failed, falling back to file: {e}"`; else
   `_reconcile_enc_after_keychain_write(...)` and **return**.
3. File mode: `_write_backup_enc(...)`; on any exception log `f"Failed to write
   credentials file: {e}"` and **re-raise**. On macOS then
   `_delete_backup_keychain_quiet(...)` (drop stale Keychain copy).

Raises on a file-write failure **before** returning, so the switcher wrapper runs
its `_post_backup_write` exactly once and only after a successful write.

`_reconcile_enc_after_keychain_write(account_num, email, credentials)` —
**correctness-critical** (not best-effort), because reads are `.enc`-wins:
```
enc_file = _backup_enc_path(...)
if not enc_file.exists(): return
try:
    enc_file.unlink(); return
except Exception as e:
    log warning f"Could not delete .enc after Keychain backup write ({e}); rewriting it with the fresh credentials to keep both consistent"
_write_backup_enc(account_num, email, credentials)     # if this raises, it propagates
```

`_delete_account_credentials(account_num, email)`:
- `nums = [account_num]`; if `str(account_num) != "None"` append `"None"` (legacy
  `account-None-{email}` alias).
- For each `num`: unlink the `.enc` if it exists (on exception log `f"Failed to
  delete credentials file: {e}"`); on macOS `_delete_backup_keychain_quiet(num,
  email)`; then `delete_previous_backup(num, email)`.

`delete_account_credentials_strict(account_num, email)` — **fail-closed**
transactional clear (used by swap/move pre-commit write-or-clear and rollback):
1. `_delete_account_credentials(...)` (best-effort sweep: legacy alias, `.prev`,
   quiet Keychain).
2. `try: _backup_enc_path(account_num, email).unlink(missing_ok=True)`; on macOS
   `_kc_delete_backup(account_num, email)`. `except (OSError,
   *KEYCHAIN_ERRORS) as e:` → `raise CredentialError(f"Could not clear stored
   credentials for slot {account_num} ({email}) — aborting before commit: {e}")
   from e`. The Keychain delete runs even in file mode; absence counts as success
   (missing `.enc`; Keychain rc 44).
3. Final belt: if `_read_account_credentials(account_num, email)` still truthy →
   `raise CredentialError(f"Could not clear stored credentials for slot
   {account_num} ({email}) — aborting before commit")`.

### 5.8 Previous-generation retention (`.prev`)

One retained generation per slot, routed by the same rule as the backup itself
(Keychain when in use, `.enc.prev` file otherwise). **Best-effort by design.**
Retention must not *weaken* storage posture — a Keychain-backed Mac must not grow
a plaintext `.prev` copy (test
`test_write_retains_prev_generation_in_keychain_not_a_file`).

`_retain_previous_backup(account_num, email, new_credentials)`:
```
current = _read_account_credentials(account_num, email)   # exception swallowed w/ warning
if not current or current == new_credentials: return      # nothing to retain / unchanged
try:
    if _use_keychain():
        _kc_call(set_password, "claude-swap", f"account-{n}-{email}.prev", current)
    else:
        _atomic_b64_write(_prev_backup_path(...), current)
except Exception as e:
    log warning f"Failed to retain previous credential generation for account {account_num}: {e}"
```

`_read_previous_backup(account_num, email) -> str` — `.enc.prev`-wins, same
shape as the main read (base64 `validate=True`, then macOS Keychain
`.prev` username via `_kc_call`; on `KEYCHAIN_ERRORS` log `f"Failed to read .prev
from Keychain: {e}"`); `""` when absent/corrupt.

`delete_previous_backup(account_num, email)`: unlink `.prev` file if present (on
exception log `f"Failed to delete .prev file: {e}"`); on macOS
`_kc_delete_backup_prev(...)` in `try/except Exception: log f"Failed to delete
.prev from Keychain: {e}"`. Called from full deletion and standalone on a
renumber (swap/move) so recovery never resurrects a displaced generation onto a
key's new owner.

### 5.9 Unclaimed-credential stash (forensic safety copies)

Write-only preservation of live credential bytes a switch positively attributed
to someone other than the outgoing slot. **Always 0600 base64 files on every
platform — never the Keychain** (a flaky Keychain must not start blocking
switches, #101/#106). Append-only; nothing consumes them automatically (recovery
is a documented `/login` + `cswap add [--slot N]`).

Paths:
- Manifest: `credentials_dir / ".unclaimed-manifest.json"`
- Entry: `credentials_dir / f".unclaimed-{entry_id}.enc"`

`entry_id` = `f"{ts}-{digest}-{secrets.token_hex(3)}"` where:
- `ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S")`
- `digest = hashlib.sha256(credentials.encode("utf-8")).hexdigest()[:12]`
- `secrets.token_hex(3)` = 6 hex chars nonce (uniqueness for identical bytes in
  the same second).

Manifest JSON (written by `_write_stash_manifest`):
```json
{ "schemaVersion": 1, "entries": { "<entry_id>": { "createdAt": "<iso>", "...context": "..." } } }
```
- `createdAt` = `datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")`,
  merged with the caller's `context` dict (`{**context}`).
- Written via `settings.atomic_write_json(path, {"schemaVersion": 1, "entries":
  entries})` — which does `mkdir(parents=True)`, `chmod(parent, 0o700)`
  (non-win32), atomic tmp+`os.replace`, `chmod(path, 0o600)` (non-win32),
  `json.dumps(indent=2)`.

`_write_unclaimed_credential(credentials, context) -> entry_id` — **entry file
written before the manifest** (an entry without metadata is recoverable; a
manifest row without bytes is not). Raises on any failure (a successful stash is
the license to overwrite the live store).

`_read_stash_manifest() -> dict`: returns the inner `entries` dict (or `{}`),
`{}` if absent, `{}` + warning `f"Failed to read unclaimed manifest: {e}"` on
error.

`_write_stash_manifest(entries)`: if a manifest file already exists and does NOT
parse as JSON, rename it aside to `f"{name}.corrupt-{int(time.time())}"` (log
`f"Unreadable unclaimed manifest preserved as {aside.name}"`; on `OSError` log
`f"Could not preserve corrupt unclaimed manifest: {e}"`) before overwriting — a
corrupt manifest holds classification evidence and must not be silently
clobbered.

`_list_unclaimed_credentials() -> dict[str, dict]`: manifest entries plus any
orphaned `credentials_dir.glob(".unclaimed-*.enc")` entry files (id parsed as
`path.name[len(".unclaimed-"):-len(".enc")]`, defaulted to `{"createdAt":
None}`); `OSError` during glob is swallowed.

---

## 6. Claude Code cooperative locks (`claude_locks.py`)

cswap holds Claude Code's **own** advisory locks while mutating its files, to
close the one real race with a running Claude Code (its OAuth refresh reads
credentials, refreshes over the network, and saves — all under `~/.claude.lock`).

### 6.1 External protocol (quoted verbatim — npm `proper-lockfile`)

> - The lock artifact is a **directory** at `<target>.lock` (`~/.claude.lock`,
>   `~/.claude.json.lock`); `mkdir` atomicity is the mutex.
> - A lock is considered stale when its mtime is older than 10s; live holders
>   touch the mtime every 5s to prove liveness, and a stale lock may be removed
>   and taken over.
> - Claude Code retries a held credentials lock 5 times with 1-2s jittered
>   sleeps before giving up, so briefly holding it is fully cooperative.

References (claude-code source): `utils/auth.ts
checkAndRefreshOAuthTokenIfNeededImpl`, `utils/config.ts saveConfigWithLock`,
`utils/lockfile.ts`.

### 6.2 Constants

- `STALENESS_S = 10.0` — a lock is stale when its mtime is older than this.
- `TOUCH_INTERVAL_S = 3.0` — cswap touches the held lock's mtime this often
  (Claude Code uses `stale/2 = 5s`; cswap touches faster for margin).
- `DEFAULT_TIMEOUT_S = 9.0` — default max wait to acquire (comfortably outlasts a
  sub-second-to-few-second credential/config hold without stalling the CLI
  forever).

### 6.3 Lock directory paths

- `credentials_lock_dir()` = `home.parent / (home.name + ".lock")` where `home =
  get_claude_config_home()`. Default `~/.claude.lock`; with
  `CLAUDE_CONFIG_DIR=/x/custom-claude` → `/x/custom-claude.lock`.
- `config_lock_dir()` = `path.parent / (path.name + ".lock")` where `path =
  get_global_config_path()`. Default `~/.claude.json.lock`; honors
  `CLAUDE_CONFIG_DIR` (→ `<CCD>/.claude.json.lock`) and the legacy `.config.json`
  resolution.

### 6.4 `proper_lockfile(lock_dir, *, timeout=None, staleness=STALENESS_S)` (context manager)

```
if timeout is None: timeout = DEFAULT_TIMEOUT_S     # resolved at CALL time (tests shorten)
lock_dir.parent.mkdir(parents=True, exist_ok=True)
start = time.monotonic()
while True:
    try:
        os.mkdir(lock_dir); break                   # acquired
    except FileExistsError:
        pass
    if time.monotonic() - start > timeout:
        raise ClaudeCodeLockTimeout(
            f"Could not acquire {lock_dir.name} — Claude Code appears "
            "to be refreshing credentials. Retry in a few seconds.")
    try:
        held_mtime = os.stat(lock_dir).st_mtime
    except FileNotFoundError:
        continue                                    # holder released between mkdir & stat; retry now
    if time.time() - held_mtime > staleness:        # WALL clock vs mtime
        try:
            os.rmdir(lock_dir)                       # dead holder → remove & retake
        except OSError:
            time.sleep(0.05)                         # can't remove; don't spin hot
        continue
    time.sleep(0.25 + random.random() * 0.25)       # jittered backoff [0.25, 0.50)
```
After acquiring, start a **daemon toucher thread**:
```
stop_touching = threading.Event()
def _touch():
    while not stop_touching.wait(TOUCH_INTERVAL_S):   # wakes every 3.0s
        try: os.utime(lock_dir)                        # bump mtime to now
        except OSError: return                         # lock stolen/removed → stop
toucher = threading.Thread(target=_touch, daemon=True); toucher.start()
try:
    yield
finally:
    stop_touching.set()
    toucher.join(timeout=1.0)
    try: os.rmdir(lock_dir)
    except FileNotFoundError:
        logger.warning("Lock %s vanished while held (taken over as stale?)", lock_dir)
    except OSError as e:
        logger.warning("Failed to release lock %s: %s", lock_dir, e)
```

Key details:
- Staleness comparison uses **`time.time()` (wall clock)** vs the lock's mtime
  (must match `os.utime`/filesystem mtime). Acquire-timeout uses
  **`time.monotonic()`**.
- Jittered backoff is `0.25 + random.random()*0.25` → uniform in `[0.25, 0.50)`.
- The toucher wakes via `Event.wait(3.0)`; `set()` makes `wait()` return `True`
  and ends the loop promptly on release.
- Release tolerates a stolen lock: `os.rmdir` raising `FileNotFoundError` (taken
  over) or `OSError` just logs a warning; no exception escapes.

### 6.5 Named helpers

- `claude_credentials_lock(*, timeout=None)` → `proper_lockfile(credentials_lock_dir(),
  timeout=timeout)` — Claude Code's credential-refresh lock (`~/.claude.lock`).
- `claude_config_lock(*, timeout=None)` → `proper_lockfile(config_lock_dir(),
  timeout=timeout)` — Claude Code's global-config write lock
  (`~/.claude.json.lock`).

`ClaudeCodeLockTimeout` is a subclass of `LockError` (→ `ClaudeSwitchError`).
Nothing is mutated when it raises; the operation is safe to retry.

### 6.6 Why hold these locks (quoted external rationale)

> Holding these locks while swapping credentials closes the one real race with a
> running Claude Code: its refresh reads credentials, refreshes over the network,
> and saves — all under `~/.claude.lock` — so a swap landing inside that window
> would be overwritten by the refreshed old-account token (and the just-taken
> backup would keep a pre-rotation refresh token). Under the lock, Claude Code's
> own double-checked re-read sees the swapped (non-expired) credential and aborts
> the refresh instead.

---

## 7. cswap's own file lock (`locking.py`)

`FileLock` — a cross-process advisory lock serializing cswap invocations against
**each other** (distinct from §6, which cooperates with Claude Code). Uses
`fcntl.flock` on POSIX, `msvcrt.locking` on Windows.

### 7.1 Class contract

`FileLock(lock_path: Path, timeout: float = 10.0)`. Default timeout **10.0s**.

`acquire(timeout=None) -> bool`:
```
if timeout is None: timeout = self.timeout
lock_path.parent.mkdir(parents=True, exist_ok=True)
self._lock_file = open(lock_path, "w")           # truncating open of the lock file
start = time.monotonic()
while True:
    try:
        if win32: msvcrt.locking(fileno, msvcrt.LK_NBLCK, 1)      # 1 byte, non-blocking
        else:     fcntl.flock(fileno, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self._locked = True; return True
    except (BlockingIOError, OSError):
        if time.monotonic() - start > timeout:
            self._lock_file.close(); self._lock_file = None
            return False                          # TIMEOUT → returns False (no raise)
        time.sleep(0.1)                           # poll every 100ms
```

`release()`:
```
if self._lock_file and self._locked:
    if win32:
        try: msvcrt.locking(fileno, msvcrt.LK_UNLCK, 1)
        except OSError: pass
    else:
        fcntl.flock(fileno, fcntl.LOCK_UN)
    self._lock_file.close(); self._lock_file = None; self._locked = False
```
Safe to call when not locked and safe to call twice (guarded by `self._locked`).

Context manager: `__enter__` calls `self.acquire()` (default timeout) and if it
returns `False` raises `LockError("Failed to acquire lock - another instance may
be running")`; else returns `self`. `__exit__` calls `release()`.

### 7.2 Semantics

- **Non-reentrant** across `FileLock` instances in one process (`flock`/`msvcrt`
  are process- or fd-scoped; cswap's callers explicitly note "FileLock is
  non-reentrant" and never re-acquire while held — e.g. `persist_active` must not
  nest inside another `FileLock(self.lock_file)`).
- Timeout returns `False` from `acquire`; only the context-manager form raises
  `LockError`.
- The lock file is opened with mode `"w"` (truncates), so its *content* is
  meaningless — only the advisory lock matters.

### 7.3 Where FileLock is used (protects what)

- `switcher.lock_file = backup_dir / ".lock"` — the primary cswap mutation lock,
  wrapped around `swap_accounts`, add/remove, renumber, and the switch body
  (`with FileLock(self.lock_file): ...`). Default 10s timeout.
- `session.py` bootstrap: `FileLock(switcher.lock_file,
  timeout=_BOOTSTRAP_LOCK_TIMEOUT)` where `_BOOTSTRAP_LOCK_TIMEOUT = 30.0`.
- `autoswitch.py`: `FileLock(state_path.parent / ".autoswitch_state.lock")`
  guarding the autoswitch state file.
- `usage_store.py`: `FileLock(self._lock_path)` guarding the usage cache.

### 7.4 Lock ordering across operations (from switcher.py)

When a switch also needs Claude Code's cooperative locks, the acquisition order
is **cswap FileLock first, then Claude Code credentials lock, then Claude Code
config lock**:
```
with FileLock(self.lock_file), claude_credentials_lock(), claude_config_lock():
    ...   # e.g. switcher.py:4282, and persist_active at switcher.py:2552-2554
```
This order is consistent everywhere the three combine; the Go port must preserve
it to avoid deadlock. Note the deliberate non-nesting: the network refresh runs
*outside* the FileLock (FileLock is non-reentrant and `persist_active` re-acquires
it), then persistence re-takes all three under a fresh double-checked re-read.

---

## 8. Edge cases & subtleties (from tests)

**macos_keychain (`test_macos_keychain.py`):**
- `get_password` returns `None` **only** on rc 44; rc 51 (locked/denied) raises
  `KeychainError` — must NOT be masked as "not found".
- `item_exists` returns `True` only on rc 0; `False` on rc 44, rc 51, timeout,
  and `FileNotFoundError` (missing binary). Never raises. Its argv omits `-w`.
- `set_password` small payload: argv is exactly `["/usr/bin/security", "-i"]`,
  secret rides on stdin as hex `-X`; the stdin string starts with
  `"add-generic-password -U"` and contains `-X <hex>`, `-a "acct"`, `-s "svc"`
  (quoted). Large payload (`"x" * SECURITY_STDIN_LINE_LIMIT`, since hex doubles
  length): argv path `["/usr/bin/security", "add-generic-password", "-U", ...]`
  with no `input` kwarg; hex value appears as a raw argv element.
- Round-trip: the hex written on `set` must `bytes.fromhex(...).decode("utf-8")`
  back to the original secret, including `"quotes"`, `\` backslash, and `é`.
- `delete_password`: rc 0 and rc 44 both succeed; rc 51 raises.
- All three (`get`/`set`/`delete`) convert `subprocess.TimeoutExpired` into
  `KeychainError`; `subprocess.run` is called with `timeout=_TIMEOUT` (5.0).
- `keychain_account_name()` prefers `$USER`; with `$USER` unset the result is
  non-empty and **never the literal `"user"`**.

**Backup path (`test_macos_keychain_contract.py`):**
- Backup read/write use service `"claude-swap"` and account
  `"account-<num>-<email>"` (e.g. `"account-1-user@example.com"`).
- With no existing backup (`get_password` → `None`), a write is a single
  `set_password` call (retention has nothing to keep).
- With an existing backup, `.prev` retention writes the OLD value first, in
  order: `set_password("claude-swap", "account-2-...@....prev", "old-generation")`
  THEN `set_password("claude-swap", "account-2-...@...", "secret-token")`. On a
  Keychain-backed Mac the `.prev` goes to the Keychain and **no `.prev` file
  exists on disk**.
- Delete issues, in order: `delete_password` for the slot, the slot `.prev`, the
  legacy `account-None-<email>`, and `account-None-<email>.prev`.
- Real-keychain (macOS GHA only): `_read_credentials()` finds a Claude-Code-seeded
  `"Claude Code-credentials"` item keyed by `$USER`; `_write_credentials(token)`
  creates an item findable by `find-generic-password -a $USER -s "Claude
  Code-credentials" -w`; wrapper round-trip `set→get→delete→get==None`.

**Locks (`test_locking.py`, `test_claude_locks.py`):**
- `FileLock`: basic acquire/release toggles `_locked`; context manager sets/clears
  `_locked`; creates parent dirs; a second lock on a held path times out
  (`acquire(0.5) is False`); reacquirable after release; context manager raises
  `LockError` when `acquire` returns `False`; double `release()` is safe.
  Concurrency: a subprocess holding the lock blocks another process's
  `acquire(0.5)` (→ `False`); lock is acquirable once the holder process exits.
- `proper_lockfile`: acquire creates the dir, release removes it; reacquire after
  release; **contention with a fresh (live) lock times out** in under 5s and
  leaves the holder's lock intact; a lock with a 30s-old mtime is taken over
  (and the new mtime is fresh, `< 5s`); release tolerates the dir being `rmdir`'d
  out from under it (no exception, nothing left behind); the toucher keeps mtime
  fresh even after the test back-dates it 30s (with `TOUCH_INTERVAL_S` monkeypatched
  to 0.1); creates missing parent dirs.
- Lock paths honor `CLAUDE_CONFIG_DIR`: default `~/.claude.lock` /
  `~/.claude.json.lock`; with CCD → `<CCD-name>.lock` at the CCD parent and
  `<CCD>/.claude.json.lock`. Named helpers create/remove their dirs and nest
  (credentials lock outside, config lock inside).

**Paths (`test_paths.py`):**
- `get_global_config_path` default is `$HOME/.claude.json` (NOT inside `.claude/`).
- Legacy `<config_home>/.config.json` (or `<CCD>/.config.json`) wins whenever it
  exists.
- XDG: default `~/.local/share/claude-swap`; respects an absolute
  `$XDG_DATA_HOME`; **ignores** empty and relative `$XDG_DATA_HOME`; expands a
  leading `~`. WSL uses the XDG layout; macOS/Windows use
  `~/.claude-swap-backup`.
- Migration: no legacy → no-op; target==legacy → no-op (untouched); moves legacy
  (including nested dirs) to target; collision with real data → `MigrationError`
  matching `"Refusing to merge"`; target with only throwaway (`cache/`,
  `claude-swap.log`, `claude-swap.log.1`) → wiped then migrated; real data
  *alongside* throwaway → still a collision; flag + legacy present → discard
  partial target and redo; flag + legacy gone → just clean the flag (target
  untouched), return `False`; `shutil.move` raising `PermissionError` → wrapped as
  `MigrationError` matching `"failed"`; file modes (`0o600` file, `0o700` dir)
  preserved (skipped on Windows).

**conftest fixtures (test harness contract, informs the Go test harness):**
- `block_real_keychain` (autouse) replaces the `macos_keychain` module functions
  with an in-memory `(service, account) -> secret` map; `delete_password` on an
  absent key is a no-op (rc 44 parity). Opt out via `@pytest.mark.no_keychain_fake`.
- `_isolate_real_home` (autouse, runs first) redirects `$HOME`/`$USERPROFILE` and
  `Path.home()` to a temp dir and **always unsets `CLAUDE_CONFIG_DIR` and
  `XDG_DATA_HOME`** (both bypass `$HOME` in path resolution).

---

## 9. Go port notes

### 9.1 Platform-conditional logic

- **Keychain is macOS-only.** All `macos_keychain` calls and the `_use_keychain()`
  branch are gated on `host.platform == MACOS`. On Linux/WSL/Windows/UNKNOWN,
  every credential op goes to files (active OAuth → `.credentials.json`; managed
  key → `~/.claude.json primaryApiKey`; backups → `.enc`).
- **`chmod 0600/0700` are POSIX-only** — every write helper guards `if
  sys.platform != "win32"`. Skip `os.Chmod` on Windows.
- **Backup root is XDG on Linux/WSL, legacy on macOS/Windows/UNKNOWN.**
- **File-lock primitive differs by OS**: `fcntl.flock(LOCK_EX|LOCK_NB)` on POSIX
  vs `msvcrt.locking(LK_NBLCK, 1)` on Windows. In Go use
  `golang.org/x/sys/unix.Flock` (POSIX) and `LockFileEx` via
  `golang.org/x/sys/windows` (Windows) — locking exactly 1 byte on Windows to
  match `msvcrt.locking(..., 1)`.

### 9.2 Concurrency / threading

- `proper_lockfile` spawns a **daemon background thread** (`_touch`) that bumps
  the lock dir's mtime every `TOUCH_INTERVAL_S = 3.0s` while held, coordinated by
  a `threading.Event`. Go: a goroutine with a `time.Ticker(3*time.Second)` and a
  `done chan struct{}` (or `context.Context`); on release, signal done and
  `join` with a 1.0s bound before `os.Remove`. The toucher must stop on the first
  `os.utime`/`os.Chtimes` error (lock stolen).
- `_kc_call` / `_use_keychain` state is **per-process, not thread-safe by
  design** — one `CredentialStore` per switcher, single-threaded credential ops
  under the FileLock. If the Go port shares a store across goroutines, guard
  `_keychain_usable_cache` / `_keychain_disabled_until` with a mutex.
- **Two clocks, deliberately:** monotonic (`time.monotonic()`) for acquire
  timeouts and the Keychain re-probe cooldown (immune to wall-clock jumps); wall
  clock (`time.time()`) for lock **staleness** (must compare against filesystem
  mtime). Go: `time.Now()` for both, but derive staleness from
  `fileinfo.ModTime()` and elapsed-timeout from a monotonic baseline
  (`time.Since(start)` uses the monotonic reading Go embeds in `time.Time`).

### 9.3 Python-isms needing deliberate Go equivalents

- **Atomic writes**: `tempfile.mkstemp(dir=...)` + `os.write` + `os.close` +
  `os.replace` (atomic rename within the same dir) + `os.chmod`, with a
  `BaseException` cleanup that unlinks the temp file. Go: `os.CreateTemp(dir,
  "*.tmp")`, write, `Close`, `os.Rename`, `os.Chmod`, `defer`-cleanup on error.
  The temp file **must** be created in the same directory as the target so the
  rename is atomic (same filesystem).
- **`str.removesuffix("\n")`** in `get_password` strips exactly one trailing
  newline — use `strings.TrimSuffix(out, "\n")`, NOT `TrimSpace` (leading/trailing
  whitespace in the value is meaningful).
- **`bytes.hex()`** for the `-X` value → `hex.EncodeToString` (lowercase, no
  separators).
- **base64**: `base64.b64encode/b64decode(..., validate=True)` = Go
  `base64.StdEncoding`; the `validate=True` decode must reject non-alphabet junk
  (Go `StdEncoding.DecodeString` already errors on invalid input — do not use a
  lenient decoder).
- **`sha256(...).hexdigest()[:12]`** = first 12 hex chars of `sha256.Sum256`.
  **`secrets.token_hex(3)`** = 6 hex chars from 3 crypto-random bytes
  (`crypto/rand`).
- **UTC timestamps**: `strftime("%Y%m%dT%H%M%S")` → `t.UTC().Format("20060102T150405")`;
  `strftime("%Y-%m-%dT%H:%M:%SZ")` → `t.UTC().Format("2006-01-02T15:04:05Z")`.
- **JSON**: `json.dumps(data, indent=2)` → `json.MarshalIndent(data, "", "  ")`
  (2-space indent). Preserve unknown keys in `~/.claude.json` by decoding into
  `map[string]interface{}` (or `json.RawMessage` per key), mutating only the
  managed keys, and re-encoding — do NOT model it as a fixed struct or unmanaged
  keys will be dropped.
- **`Path.exists()` version quirk**: `_read_account_credentials` wraps
  `enc_file.exists()` in `try/except OSError` because Python 3.12 raised on an
  unsearchable dir while 3.13+ returns `False`. Go's `os.Stat` returns an error
  in that case — treat any stat error as "missing" (log + `enc_present=False`)
  for the read path, matching the normalized best-effort behavior.
- **`Path.unlink(missing_ok=True)`** in the strict clear = `os.Remove` treating
  `os.IsNotExist` as success but propagating other errors — the fail-closed
  contract depends on permission/I/O errors aborting the commit.
- **`subprocess.run(..., text=True, capture_output=True, timeout=5.0)`** →
  `exec.CommandContext` with a 5s `context.WithTimeout`; a context-deadline
  cancellation must be mapped to the `KeychainError` "timed out" message, and the
  process's exit code read from `exec.ExitError` to distinguish rc 44 / rc 0 /
  other. For `security -i`, feed the command string to the process's stdin.
- **Error taxonomy**: `KeychainError`, `subprocess.TimeoutExpired`, `OSError`
  form `KEYCHAIN_ERRORS` — the "Keychain unusable, fall back" set. Model as a
  Go error type/interface the fallback code checks with `errors.As`; keep it
  disjoint from programming errors so those still surface. Exceptions used:
  `CredentialError`, `CredentialWriteError` (subclasses of `ClaudeSwitchError`);
  `MigrationError`; `LockError` and its subclass `ClaudeCodeLockTimeout`.

### 9.4 Exact interop invariants the port must not break

- Keychain service strings verbatim: active OAuth `"Claude Code-credentials"`,
  active managed `"Claude Code"`, cswap backups `"claude-swap"`.
- Keychain account name resolution: `$USER` → OS username → `"claude-code-user"`.
- `customApiKeyResponses.approved` entry = last 20 chars of the key.
- `.credentials.json` is written **raw** (the credential string verbatim), not
  re-serialized JSON.
- Backup `.enc` files are base64 of the raw credential string; filename
  `.creds-<num>-<email>.enc`; `.prev` sibling `.creds-<num>-<email>.enc.prev`.
- Lock dirs `<target>.lock` as directories, mtime-based staleness at 10s.
- `security` binary is the pinned absolute path `/usr/bin/security`.
