# claude-swap: Session Mode & Directory Mappings — Behavioral Spec

Source: `src/claude_swap/session.py`, `src/claude_swap/mappings.py`,
`src/claude_swap/process_detection.py` (plus the minimum of `switcher.py`,
`paths.py`, `claude_locks.py`, `locking.py`, `macos_keychain.py`, `oauth.py`,
`models.py`, `settings.py`, `printer.py`, `cli.py` needed to make those three
modules' contracts precise). Line numbers refer to the read source at the time
of writing.

## Overview

`cswap run NUM|EMAIL` launches Claude Code as a *stored account* inside the
current terminal only, by pointing `CLAUDE_CONFIG_DIR` at a persistent
per-account profile directory under `<backup_dir>/sessions/<num>-<email-slug>/`
and `exec`ing (POSIX) or subprocess-wrapping (Windows) the real `claude`
binary — the default `~/.claude` login, every other terminal, and the VS Code
extension are untouched. The profile is lazily bootstrapped from cswap's
backup store (credentials + config), validated with a local `claude auth
status --json` probe, and reused on subsequent launches until something
invalidates it. By default a curated set of user customizations
(`settings.json`, `keybindings.json`, `CLAUDE.md`, `skills/`, `commands/`,
`agents/`) and the default profile's user-scope `mcpServers` are mirrored in
on every launch — symlinked on macOS/Linux so in-session `/config` edits write
straight through to `~/.claude`, re-synced by copy on Windows. An opt-in
`--share-history` flag additionally unifies conversation history
(`projects/`, `history.jsonl`) across every account via the same symlink
mechanism (POSIX-only), merging in any history the profile already
accumulated so nothing is lost. `cswap map`/`unmap` maintain a
per-machine, non-exported `<backup_dir>/mappings.json` associating absolute
directories with a stored account identity, so a bare `cswap run` (no
account argument) can resolve the current working directory — walking up to
the nearest mapped ancestor — and auto-launch the right account.
`process_detection.py` reads Claude Code's own `~/.claude/sessions/*.json`
PID files and `~/.claude/ide/*.lock` IDE lockfiles (filtering to processes
that are still alive) to answer "is anything running against this profile
right now?" — the guard that stops cswap from deleting, invalidating, or
otherwise pulling storage out from under a live `claude` process.

---

## 1. Session mode: `cswap run`

### 1.1 CLI surface

Pre-dispatched in `cli.py` before the main argparse tree is built (`run` must
be the *first* argv token — `cswap --debug run 2` is not supported; use
`cswap run 2 --debug`). Argument split: everything after the first literal
`--` token is forwarded to `claude` verbatim (`claude_args`); everything
before it is parsed by `cswap run`'s own parser.

```
cswap run [NUM|EMAIL] [--no-share] [--share-history | --no-share-history] [--debug] [-- <claude args>]
```

- `NUM|EMAIL` (positional, optional): account to run. Omitted → resolve from
  the current working directory's mapping (§3).
- `--no-share`: don't mirror `settings.json`/`keybindings.json`/`CLAUDE.md`/
  `skills`/`commands`/`agents` from `~/.claude` into the profile, **and**
  remove any such items a previous launch shared in.
- `--share-history` / `--no-share-history` (`argparse.BooleanOptionalAction`,
  default `False`): share `projects/` + `history.jsonl`. Independent of
  `--no-share` — `cswap run 2 --no-share --share-history` gives a bare
  profile with unified history.
- `--debug`: enable debug logging.

Examples from the parser's own epilog:
```
cswap run 2
cswap run user@example.com
cswap run 2 --no-share
cswap run 2 --share-history
cswap run 2 -- --resume
```

Root guard shared with `map`/`unmap` (`_guard_root`): on non-Windows, if
`os.geteuid() == 0` and the process is not inside a container, prints
`Error: Do not run this script as root (unless running in a container)` and
exits 1.

Errors from `run`/`map`/`unmap` surface as `Error: {message}` with exit code
1 (`ClaudeSwitchError` subclasses); `Ctrl+C` prints `\n{dimmed("Operation
cancelled")}` and exits 130.

### 1.2 Bare `cswap run` (directory-mapping resolution)

When no `NUM|EMAIL` is given, `cli.py` calls
`switcher.slot_for_directory(os.getcwd())` → `(slot, email)`:

- `(None, None)`: no mapping covers `cwd` → print
  `No account mapped for {cwd} — launching the default account.` (dimmed)
  and call `manager.exec_default(tail)`.
- `(None, email)`: a mapping exists but its account no longer has a slot →
  `warning(f"Mapped account {email} no longer exists — launching the default account.")`
  then `manager.exec_default(tail)`.
- `(slot, email)`: resolved → `manager.run(slot, tail, share=not args.no_share, share_history=args.share_history)`.

`exec_default` (`SessionManager.exec_default`) is the "just run plain
`claude`" path: no session profile, **no** `AUTH_OVERRIDE_ENV_VARS`
scrubbing, `env=dict(os.environ)` verbatim — identical to typing `claude`
directly. Raises `SessionError("'claude' was not found on PATH. Install
Claude Code first.")` if `shutil.which("claude")` is falsy.

### 1.3 `SessionManager.run()` control flow

1. Resolve `claude_bin = shutil.which("claude")`; raise `SessionError` (same
   message as above) if not found.
2. If `share_history` and platform is `Platform.WINDOWS`, raise:
   `"--share-history is not supported on Windows yet: sharing uses re-synced copies there, which would fork the history instead of sharing it."`
3. `account_num, email, org_uuid = switcher.resolve_account(identifier)` —
   raises `AccountNotFoundError` for an unknown NUM/EMAIL, `ConfigError` if an
   email matches multiple accounts. Unlike `switch_to`, ambiguity is a hard
   error (no interactive prompt) because session mode ends in an `exec`.
4. `_ensure_not_api_key(account_num, email)`: if the account's stored `kind`
   is `"api_key"`, raise:
   `f"Account-{account_num} ({email}) is an API-key account; 'cswap run' (session mode) does not support API-key accounts yet. Use 'cswap --switch-to' to make it your default login instead."`
   Called *before* the same-account fast path and again inside
   `setup_session` (defense in depth).
5. **Same-account fast path** (only when `CLAUDE_CONFIG_DIR` is *not* already
   set in the environment): if `switcher._get_current_account()` returns a
   tuple equal to `(email, org_uuid)`, print
   `dimmed(f"Account-{account_num} ({email}) is already the active default login — launching claude directly.")`
   and `_exec(claude_bin, claude_args, env=dict(os.environ))` — **never
   returns**. Rationale: two live copies of one account's credentials can
   drift on refresh-token rotation.
   - If `CLAUDE_CONFIG_DIR` *is* already set, print
     `warning(f"CLAUDE_CONFIG_DIR is already set ({config_dir_preset}); overriding it for this launch.")`
     and **skip** the fast path even if the identity matches — "current
     default account" is meaningless when possibly already inside a session.
6. Compute `scrubbed = [v for v in AUTH_OVERRIDE_ENV_VARS if os.environ.get(v)]`;
   if non-empty, warn:
   `f"Ignoring {', '.join(scrubbed)} for this session — it would override the selected account inside Claude Code."`
7. `session_dir, account_num, email = self.setup_session(identifier, share, share_history)` (§1.4).
8. Print `f"{accent('Launching')} Account-{account_num} ({email}) {muted('[session mode]')}"`.
9. Build `env = {k: v for k, v in os.environ.items() if k not in AUTH_OVERRIDE_ENV_VARS}`,
   set `env["CLAUDE_CONFIG_DIR"] = str(session_dir)`.
10. `self._exec(claude_bin, claude_args, env=env)` — never returns (POSIX).

**`AUTH_OVERRIDE_ENV_VARS`** (env vars that make `claude` bypass account
OAuth entirely — verified against `claude 2.1.175`):
```
ANTHROPIC_API_KEY
ANTHROPIC_AUTH_TOKEN
CLAUDE_CODE_OAUTH_TOKEN
CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR
CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR
```
Scrubbed from: the session launch env (step 9 above), and the auth-status
probe env (`_probe_env`, §1.6). **Not** scrubbed from: the same-account
fast-path env (step 5) or `exec_default`'s env — both are "just run plain
claude" and must reproduce ordinary `claude` behavior exactly.

### 1.4 `setup_session()` — bootstrap/reuse decision

```python
def setup_session(self, identifier: str, share: bool, share_history: bool = False) -> tuple[Path, str, str]
```

1. `resolve_account` + `_ensure_not_api_key` (as above).
2. `session_dir = session_dir_for(switcher.backup_dir, account_num, email)`
   (§1.5).
3. **Deferred-invalidation check** (lock-free): `stale = (session_dir / STALE_MARKER).exists() and not live_sessions_for(session_dir)`.
   The marker is honored only when no `claude` is currently live against the
   profile — a second `cswap run` joining an already-live session must never
   invalidate credentials out from under it; the marker simply survives for
   later.
4. **Cheap reuse check** (no lock): if `not stale and self._is_session_valid(session_dir, email, org_uuid)` (§1.7), call `_sync_sharing` (§2) and return `(session_dir, account_num, email)`. This is the hot path for most launches.
5. Otherwise, acquire `FileLock(switcher.lock_file, timeout=_BOOTSTRAP_LOCK_TIMEOUT)` — **`_BOOTSTRAP_LOCK_TIMEOUT = 30.0`** seconds (larger than the switch paths' default 10s because bootstrap may hold the lock across one token refresh — a 10s network call — plus auth-status probes).
6. Under the lock: re-check the stale marker + re-check `_is_session_valid` (another concurrent `cswap run` may have already bootstrapped while this one waited). If the marker still applies and no session is live, call `switcher._invalidate_session_credentials(account_num, email)` then unlink the marker. If now valid, sync sharing and return.
7. Otherwise `self._bootstrap(session_dir, account_num, email, org_uuid)` (§1.6), then `_sync_sharing`.
8. Re-validate with `_is_session_valid`; if still invalid, `_cleanup_failed_session(session_dir)` (deletes the macOS keychain entry then `shutil.rmtree(session_dir, ignore_errors=True)`) and raise:
   `SessionError(f"Session profile for Account-{account_num} ({email}) failed validation. Log in with that account and re-add it: cswap --add-account --slot {account_num}")`
9. Lock is released before any `exec` — an exec'd `claude` must never inherit a held flock.

### 1.5 Session profile directory naming

```python
def slugify_email(email: str) -> str
```
NFC-normalize the email, then map each character: keep it if `ch.isascii() and (ch.isalnum() or ch in "._-")`, else replace with `"_"`. Not required to be injective (uniqueness comes from the `<num>-` prefix), only filesystem-safe including Windows-forbidden characters.

Examples (from tests):
- `"user@example.com"` → `"user_example.com"`
- `"user+tag@example.com"` → `"user_tag_example.com"`
- `"bø@x.com"` → `"b__x.com"` (both non-ASCII bytes of `ø` become `_`, and the result is pure ASCII)
- `'a<>:"/\\|?*b@x.com'` → none of `<>:"/\|?*` survive

```python
def session_dir_for(backup_dir: Path, account_num: str, email: str) -> Path:
    return backup_dir / "sessions" / f"{account_num}-{slugify_email(email)}"
```
E.g. `session_dir_for(tmp, "2", "user@example.com") == tmp / "sessions" / "2-user_example.com"`.

**Note**: Claude Code itself writes its own PID files at
`<profile>/sessions/<pid>.json` (see §4) — so a full real path looks like
`<backup>/sessions/2-user_x.com/sessions/1234.json`. This double `sessions/`
nesting is intentional/unavoidable, not a bug.

### 1.6 Bootstrap (`_bootstrap`) — external Claude Code file-format knowledge

Caller holds `switcher.lock_file`. Steps, in order:

1. `delete_macos_keychain_entry(session_dir)` — **must** run before seeding:
   Claude reads its Keychain entry *before* the plaintext `.credentials.json`
   fallback, so a stale hashed entry left by an earlier profile at this exact
   path would silently shadow the fresh seed.
2. `creds = switcher.read_account_credentials(account_num, email)`; if falsy, raise `SessionError(f"Account-{account_num} has no stored credentials. Re-add with: cswap --add-account --slot {account_num}")`.
3. **Refresh-token check + one proactive refresh.** `_has_refresh_token(creds)` parses `json.loads(creds)["claudeAiOauth"]["refreshToken"]`; treats an unparsable/unknown shape as `True` (let the refresh attempt decide) rather than `False`. If truthy:
   - `refreshed = refresh_oauth_credentials(creds)` (see external OAuth contract below). On success, `creds = refreshed` and `switcher.write_account_credentials(account_num, email, creds)` — persists the possibly-rotated refresh token back to backup so future switches/runs see the latest generation.
   - On failure (`None`), warn: `f"Could not refresh the token for Account-{account_num}; continuing with the stored credentials."` and proceed with the original `creds`.
   - Setup-token accounts (`--add-token`) have no refresh token by design — `_has_refresh_token` returns `False` for them and the refresh is skipped **silently** (no warning printed).
4. `config_text = switcher.read_account_config(account_num, email)`; parse JSON (empty dict on decode failure or empty text). `oauth_account = config_data.get("oauthAccount")`; if falsy, raise `SessionError(f"Account-{account_num} has no stored config backup. Re-add with: cswap --add-account --slot {account_num}")`.
5. `session_dir.mkdir(parents=True, exist_ok=True)`; on POSIX, `os.chmod(session_dir, 0o700)`.
6. Write `.credentials.json` = `creds` verbatim; POSIX chmod `0o600`.
7. **Merge** the identity seed into any existing `.claude.json` at the profile (so a re-bootstrap after invalidation preserves the profile's own `projects`/history — never overwrite the whole file):
   - Load existing JSON (empty dict on missing/corrupt).
   - `existing["oauthAccount"] = oauth_account`
   - `existing["hasCompletedOnboarding"] = True` — **load-bearing**: Claude shows onboarding whenever `!config.theme || !config.hasCompletedOnboarding` (external Claude Code contract, quoted from the source comment).
   - `existing.setdefault("theme", config_data.get("theme") or "dark")` — only fills in if absent, so a re-bootstrap doesn't clobber a theme the profile changed in-session.
   - Write with `json.dumps(existing, indent=2)`; POSIX chmod `0o600`.
8. Log info: `f"Bootstrapped session profile for account {account_num} at {session_dir}"`.

**External OAuth refresh contract** (`oauth.py`, invoked from bootstrap):
- `OAUTH_TOKEN_URL = "https://platform.claude.com/v1/oauth/token"`
- `OAUTH_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"`
- `OAUTH_BETA_HEADER = "oauth-2025-04-20"` (defined; not sent as a header in the observed refresh POST — request headers are just `Content-Type: application/json` and `User-Agent: claude-swap/1.0`)
- POST body: `{"grant_type": "refresh_token", "refresh_token": <refreshToken>, "client_id": OAUTH_CLIENT_ID}`, `urllib.request` timeout **10 seconds**.
- Success: response JSON has `access_token`, `expires_in` (seconds), optional `refresh_token`, optional `scope` (space-separated). New `expiresAt` = `now_ms + expires_in * 1000`. `refreshToken` is overwritten only if the response carries a new one; `scopes` set only if `scope` present (split on whitespace).
- `credential_fingerprint`/`OAUTH_EXPIRY_BUFFER_MS = 5*60*1000` exist for other call sites, not directly in the session bootstrap path.
- Failure classification: HTTP 400/401/403 **and** body contains `"invalid_grant"` or `"invalid_client"` → permanent (`"invalid_grant"`); anything else (other HTTP errors, network errors, timeouts) → `"transient"`. Session bootstrap treats *any* failure the same (warn + keep stored creds) — it does not act on the distinction.

### 1.7 Local validation (`_is_session_valid`)

Local-only check — `claude auth status` makes **no network call**, so a
revoked-but-unexpired token still passes and only fails on first real use.

```python
claude_bin = shutil.which("claude") or "claude"
result = subprocess.run(
    [claude_bin, "auth", "status", "--json"],
    env=_probe_env(session_dir),
    capture_output=True, text=True,
    timeout=_AUTH_STATUS_TIMEOUT,   # 10.0 seconds
)
```
Uses `shutil.which` (not a bare `"claude"` string) specifically because on
Windows `claude` is a `.cmd` shim and `subprocess` does not apply `PATHEXT`
resolution to a bare name — that would raise `FileNotFoundError`, which the
handler otherwise turns into a false "failed validation".

Rejection conditions (any one fails validation → `False`):
- `session_dir` is not a directory.
- `subprocess.run` raises `OSError` or `subprocess.TimeoutExpired`.
- `result.returncode != 0`.
- stdout isn't valid JSON.
- `status.get("loggedIn") is not True`.
- `status.get("authMethod") != "claude.ai"` (verified against claude
  2.1.175; an env API key reports a different method — moot anyway since the
  probe env already drops the override vars).
- `status.get("email") != email`.
- **Lenient org check**: only rejects when *both* `status.get("orgId")` and
  `org_uuid` are truthy and they differ. Missing org on either side degrades
  to email-only validation (never a false negative from schema drift).

`_probe_env(session_dir)`:
```python
env = {k: v for k, v in os.environ.items() if k not in AUTH_OVERRIDE_ENV_VARS}
env["CLAUDE_CONFIG_DIR"] = str(session_dir)
```

### 1.8 `_exec` — terminal handoff

```python
def _exec(self, claude_bin: str, claude_args: list[str], env: dict[str, str]) -> NoReturn:
    argv = [claude_bin, *claude_args]
    if sys.platform == "win32":
        try:
            rc = subprocess.run(argv, env=env).returncode
        except KeyboardInterrupt:
            rc = 130  # Ctrl+C went to claude; just mirror the exit
        sys.exit(rc)
    os.execvpe(claude_bin, argv, env)
    raise AssertionError("unreachable")  # pragma: no cover
```
POSIX: `execvpe` replaces the cswap process image entirely — by this point
`switcher.lock_file`'s `FileLock` is already released (never held across an
exec). Windows: `os.exec*` detaches from the console confusingly, so cswap
stays resident as a thin wrapper subprocess and exits with `claude`'s own
return code (Ctrl+C while waiting → exit code 130).

### 1.9 Identity helpers (used for drift detection, not directly by `run`)

```python
def read_session_identity(session_dir: Path) -> tuple[str, str] | None
```
Reads `<session_dir>/.claude.json`'s `oauthAccount.emailAddress` /
`organizationUuid` (Claude rewrites this key on every login). Returns
`(email, org_or_empty_string)`, or `None` when the dir/file/field is
missing, the JSON is invalid, or the bytes are undecodable (`OSError` or
`ValueError`, which also covers `JSONDecodeError`/`UnicodeDecodeError`).

```python
def session_identity_drifted(session_dir: Path, email: str, org_uuid: str) -> bool
```
Compares the profile's *current* logged-in identity to the slot it was
created for (an in-session `/login` can re-point the profile without moving
its directory). Mirrors `_is_session_valid`'s comparison: email must match
exactly; org only compared when both sides are non-empty. **Unreadable
identity is NOT drift** — it degrades to trusting the profile rather than
abandoning it over a broken `.claude.json`.

---

## 2. Sharing mechanics (`_sync_sharing`)

Runs on **every** launch (both the fast-reuse path and post-bootstrap),
idempotently, and is lock-free on the reuse path — concurrent runs with
different flags are last-writer-wins and self-heal on the next launch. Only
the MCP mirror (§2.3) ever takes a lock, and only when it needs to write.
Sourced **always** from `Path.home() / ".claude"` — the literal default
profile — never from `get_claude_config_home()`, so sharing behaves
identically even when invoked from inside another session
(`CLAUDE_CONFIG_DIR` already set).

### 2.1 Constants

```python
SHARED_ITEMS = ("settings.json", "keybindings.json", "CLAUDE.md", "skills", "commands", "agents")
HISTORY_ITEMS = ("projects", "history.jsonl")
SHARE_MANIFEST = ".cswap-shared.json"
```
`SHARED_ITEMS` deliberately excludes anything account- or instance-scoped:
`plugins/`, `sessions/`, `ide/`, `.claude.json`, `.credentials.json`,
`statsig/` and other telemetry. `.claude.json` itself stays excluded as a
file, but its one user-scoped key (`mcpServers`) is mirrored separately
(§2.3).

### 2.2 Algorithm

```
active_items = (SHARED_ITEMS if share else ()) + (HISTORY_ITEMS if share_history else ())
```
(with `share_history` forced to `False` when `switcher.platform == Platform.WINDOWS`, both to keep Windows copy-mode from forking history and to auto-drop links left behind by a profile that moved from POSIX → Windows).

1. `managed = _read_manifest(manifest_path)` — the list of item names cswap
   itself created last time (manifest schema below); silently `[]` on
   missing/corrupt/non-dict.
2. **Prune deactivated items**: for every `name in managed` that is *not* in
   `active_items` this launch, remove it — **except** for a `HISTORY_ITEMS`
   name whose `dest` exists and is *not* a symlink (real accumulated
   history): that is left alone even though the manifest claims it, because
   a stale manifest from a lock-free race must never be able to delete real
   conversation history — only ever unlink actual symlinks.
3. If `active_items` is now empty, unlink the manifest file and return.
4. `use_symlinks = switcher.platform != Platform.WINDOWS`.
5. For each `name in active_items`:
   - If it's a `HISTORY_ITEMS` name, call `_prepare_history_share` (§2.4);
     if it returns `False`, skip this item for this launch (still linked
     next time).
   - If `src = source_root / name` doesn't exist: prune any previously-managed entry for it and skip (source vanished or never existed).
   - If `dest` is already a symlink:
     - Adopt it into `managed` if not already listed (cswap only ever
       manages symlinks it created — any symlink found here is presumed
       cswap's).
     - On POSIX (`use_symlinks`): if `dest.readlink() != src`, unlink and
       re-`symlink_to(src)` (repoint a stale link, e.g. after a profile
       moved between machines); append to `new_managed`; continue to next
       item. On any `OSError`, just `continue` (skip, don't crash the
       launch).
     - On Windows (platform moved POSIX → Windows since the link was made): unlink the symlink and fall through to the copy branch below.
   - Else if `dest.exists()` and `name not in managed`: this is pre-existing user data the profile accumulated on its own — **never touch it**. Print `dimmed(f"Not sharing {name}: the session profile already has its own copy.")` and skip.
   - Otherwise create the share: on POSIX, remove any existing `dest` (via `_remove_managed`) then `dest.symlink_to(src)`; on Windows, remove any existing `dest` then `shutil.copytree(src, dest)` for a directory or `shutil.copy2(src, dest)` for a file. On `OSError`, log a warning (`f"Failed to share {name} into session: {e}"`) and skip — never raises.
   - On success, append `name` to `new_managed`.
6. `_write_manifest(manifest_path, new_managed)` (atomic; see below) —
   replaces the whole manifest with exactly what this launch actually
   created/kept, so anything managed-but-no-longer-active was already
   removed in step 2.

### 2.3 Share manifest — format & location

Path: `<session_dir>/.cswap-shared.json`.
```json
{
  "items": ["settings.json", "CLAUDE.md", "skills"],
  "mode": "symlink"
}
```
`mode` is `"symlink"` on macOS/Linux/WSL, `"copy"` on Windows (recorded from
`switcher.platform`, not per-item). Written atomically: `tempfile.mkstemp(dir=parent, prefix=".cswap-shared-", suffix=".tmp")` → write → `os.replace(tmp, manifest_path)`; on any `OSError`, best-effort `os.unlink(tmp)`, no exception propagated. `_read_manifest` filters the loaded `items` list to only names that are in `SHARED_ITEMS + HISTORY_ITEMS` (defense against a hand-edited or foreign-written file naming something cswap never manages).

`_remove_managed(dest)`: unlinks if `dest` is a symlink or regular file
(`missing_ok=True`), or `shutil.rmtree(dest, ignore_errors=True)` if it's a
directory; swallows `OSError`. Callers guarantee `dest` is either
manifest-listed or itself a symlink before calling this — it is never used
to delete arbitrary user directories.

### 2.4 `--share-history` mechanics

Enabled by `--share-history`; rejected outright on Windows by `run()`
(§1.3) with an explicit error, since Windows sharing is copy-based and would
fork history rather than unify it (`_sync_sharing` also independently forces
`share_history=False` on Windows as a second line of defense for direct
callers of `_sync_sharing`).

`_prepare_history_share(src, dest, session_dir) -> bool` (called once per
history item — `projects` and `history.jsonl` — before the generic
link/skip logic runs):

1. **Merge real profile history first.** If `dest.exists()` and is *not*
   already a symlink (i.e., the profile accumulated its own history before
   the flag was ever used, or from a still-linked prior run):
   - If `live_sessions_for(session_dir)` is non-empty, **defer**: print
     `dimmed(f"Not sharing {dest.name} yet: another session is using this profile — retrying on the next launch.")` and return `False` — merging would move files out from under a running `claude`.
   - Otherwise call `_merge_history_into_source(src, dest)` (below). On
     `OSError`, log `f"Could not merge {dest.name} into {src}: {e}"` and
     print `dimmed(f"Not sharing {dest.name}: merging the profile's existing history failed (see log).")`, return `False`.
   - On success, print `dimmed(f"Merged the profile's existing {dest.name} into {src} — conversation history is now shared.")`.
2. **Seed a missing source.** If `src` still doesn't exist (fresh
   `~/.claude`, or first-ever history share): create it so the generic loop
   has something to link.
   - For `history.jsonl` (name ends `.jsonl`): `src.parent.mkdir(parents=True, exist_ok=True)`; `src.touch(mode=0o600)`.
   - For `projects` (a directory): `_mkdir_private(src)` — `mkdir -p` applying `0o700` to **every** level created, not just the leaf (`Path.mkdir(mode=...)` only applies mode to the leaf; Claude Code's own history dirs are `0o700` at every level, so cswap must match that exactly).
   - On `OSError`, log `f"Could not create {src}: {e}"` and return `False`.
3. Return `True` (item is now linkable this launch).

`_merge_history_into_source(src, dest)` (static; moves `dest`'s content into
`src`, removing `dest` when empty; any raised `OSError` leaves whatever
remains in place for the next attempt):
- **Directory** (`projects/`): `_mkdir_private(src)`, then walk
  `sorted(dest.rglob("*"), reverse=True)` (deepest-first) — transcript
  filenames are UUIDs, so a same-name collision means an identical session:
  the file already present in `src` wins, the profile's duplicate is
  dropped (`path.unlink()`); non-colliding files are `shutil.move`d after
  `_mkdir_private(target.parent)`; empty directories are `rmdir()`'d as the
  reverse walk clears their children. Finally `dest.rmdir()`.
- **File** (`history.jsonl`): merge by **line**. `existing = set(src.read_text().splitlines())` if `src` exists else empty set; append (in order) any line from `dest` that is non-empty and not already in `existing`, via `src.open("a").write("\n".join(lines) + "\n")` (creating `src` with mode `0o600` first if it didn't exist). Then `dest.unlink()`.

### 2.5 Cross-cutting sharing behaviors

- `test_toggle_off_removes_links_keeps_data`: turning `share`/`share_history`
  off removes only the cswap-created links, never the shared source data in
  `~/.claude`.
- `share_history` is fully independent of `share` — `share=False,
  share_history=True` still links history while leaving `settings.json`
  etc. unshared.
- A manifest claiming a history item as managed, when the profile actually
  holds a *real* (non-symlink) directory/file there, is never trusted for
  deletion — always routed through the merge path instead
  (`test_stale_manifest_never_deletes_real_history`,
  `test_toggle_off_with_stale_manifest_keeps_real_history`).

---

## 3. User-scope MCP server mirroring (`_sync_mcp_servers`, issue #139)

Runs unconditionally as the first step of `_sync_sharing` (before the
generic `SHARED_ITEMS`/`HISTORY_ITEMS` loop), on **every** launch. Pure
one-way mirror: the *default* profile's `~/.claude.json` (or legacy
`~/.claude/.config.json`) top-level `mcpServers` key is the single source of
truth. Adds, edits, and deletions in the default profile all propagate; any
edit made to `mcpServers` *inside* a session gets silently overwritten the
next time cswap prepares that profile. Nothing ever flows back to the
default config. Per-project entries (`projects[…].mcpServers`) on both sides
are never touched.

### 3.1 Constants
```python
MCP_KEY = "mcpServers"
MCP_MIRROR_MARKER = ".cswap-mcp-mirror-v1"        # empty marker file
MCP_DISPLACED_STASH = ".cswap-mcp-displaced.json"  # write-once stash
```

### 3.2 Adoption gating (backward compatibility)

`share=False` (i.e. `cswap run --no-share`) removes the mirrored key —
**but only** from profiles that have already adopted mirroring
(`MCP_MIRROR_MARKER` exists). An unadopted profile is left completely
untouched by `--no-share`, so pre-feature session-local MCP definitions can
never be silently destroyed by the flag.

The **first** mirror onto an unadopted profile stashes whatever definitions
it is about to displace into `MCP_DISPLACED_STASH` (write-once — an existing
valid stash from an earlier interrupted adoption is never overwritten).

### 3.3 Full algorithm

```python
config_path = session_dir / ".claude.json"
marker = session_dir / MCP_MIRROR_MARKER

if share:
    source = self._read_mcp_source()      # None ⇒ unusable, bail
    if source is None: return
elif marker.exists():
    source = {}                            # share=False: mirror "nothing"
else:
    return                                 # never adopted: no-op
```
`_read_mcp_source()`: loads `get_default_global_config_path()` (legacy
`~/.claude/.config.json` if it exists, else `~/.claude.json` — **always**
the real default path, ignoring any `CLAUDE_CONFIG_DIR` in the calling
environment, so a nested `cswap run` from inside a session never mirrors
from another session). Returns `None` if the file is missing/unreadable/not
valid JSON/not a dict; `config.get(MCP_KEY, {})` otherwise, further coerced
to `None` if that value isn't a dict. `{}` and `None` are semantically
distinct: `{}` means "genuinely no user MCP servers" (propagates as a
removal); `None` means "unusable, leave the target alone."

**Pre-lock fast-out** (no lock, no I/O beyond one read+parse of the session
config):
- If `config_path` doesn't exist → return (bootstrap/validation owns a
  missing config).
- If `config_path.is_symlink()` or is not a regular file → warn
  `f"Not syncing MCP servers: {config_path} is not a regular file."` and
  return. (A symlinked target must never be written through or replaced; a
  FIFO must never be opened for read, which would hang the launch.)
- Load `existing = _load_json_object(config_path)`; `None` (unreadable/
  corrupt/non-dict) → return.
- `target = existing.get(MCP_KEY, {})`; if not a dict, warn
  `f"Not syncing MCP servers: the profile's {MCP_KEY} is not an object."`
  and return.
- If `target == source` **and** (`not share` or `marker.exists()`) → already
  in sync (and, for a `share` run, already adopted) → return with **no
  lock taken at all**. This is the steady-state fast path.

**Locked splice** (only reached when a write might be needed):
```python
lock_dir = config_path.parent / (config_path.name + ".lock")   # <profile>/.claude.json.lock
with proper_lockfile(lock_dir):
    ...
```
This is the **same lock** a `claude` process running inside this exact
profile takes for its own `.claude.json` writes (`CLAUDE_CONFIG_DIR` is the
session dir) — see `claude_locks.py`'s external-protocol notes below. Inside
the lock:
1. Re-read `source` (if `share`) and re-load `existing`/`target` — a writer
   that waited for the lock must not clobber a newer state with its
   stale pre-lock snapshot. Any of the same fail conditions as above (symlink,
   non-file, unreadable, non-dict target) aborts with no write.
2. If `target == source`: already in sync — if `share`, call
   `_ensure_mcp_marker(marker)` (touch it if absent); return. No config
   write in this branch either.
3. If `share` and `not marker.exists()` (first-ever adoption): compute
   `displaced = {name: value for name, value in target.items() if name not in source or source[name] != value}`
   (membership test, so a JSON-`null`-valued entry not present upstream is
   still counted as displaced). If `displaced` is non-empty, call
   `_stash_displaced_mcp(session_dir, displaced)`; if it returns `False`
   (stash failed or was blocked), **abort the whole reset** — return without
   writing `target` at all, so pre-feature data is never destroyed.
4. Write: if `source` is truthy, `existing[MCP_KEY] = source`; else
   `existing.pop(MCP_KEY, None)` — deliberately mirrors Claude Code's own
   behavior of stripping default-valued (empty) keys rather than persisting
   `{}`. `atomic_write_json(config_path, existing)`; on `OSError`, warn
   `f"Could not sync MCP servers: {e}"` and return (no marker write).
5. Only **after** a successful write, if `share`: `_ensure_mcp_marker(marker)`.
   (An unadopted profile whose marker write itself fails simply retries
   next launch — by then already in sync, so nothing can be mis-stashed on
   the retry.)

Outer exception handling: `except (ClaudeCodeLockTimeout, OSError) as e:` →
warn `f"Could not sync MCP servers ({e}) — skipping this launch."` and
**never blocks the launch**. `OSError` here covers lock-machinery failures
(e.g. `mkdir` on a read-only/full filesystem); everything else inside the
`with` block already handles its own errors.

### 3.4 Stash format & validity

`_stash_displaced_mcp(session_dir, displaced) -> bool`:
- If `<session_dir>/.cswap-mcp-displaced.json` already exists (file, dir, or
  symlink): only counts as "already saved" if `_is_valid_stash` — a
  **regular file**, not a symlink, whose parsed JSON has a dict at key
  `mcpServers`. If it exists but is *not* a valid stash (e.g. a directory
  squatting on the name), warn
  `f"{stash.name} exists but is not a valid stash; leaving the profile's MCP servers in place."`
  and return `False` — this **blocks** the reset entirely (the caller must
  not silently drop the profile's real definitions just because it can't
  save a backup).
- Otherwise write:
  ```json
  {"schemaVersion": 1, "mcpServers": {<displaced entries>}}
  ```
  via `atomic_write_json`. On `OSError`, warn
  `f"Could not stash the profile's MCP servers ({e}); leaving them in place."`
  and return `False`.
- On success, print:
  `dimmed("Session MCP servers now mirror your default profile; the profile's previous definitions were saved to {stash.name}.")`
  and return `True`.

`_is_valid_stash(stash)`: `not stash.is_symlink() and stash.is_file()` and
`_load_json_object(stash)` is a dict whose `"mcpServers"` value is a dict.

### 3.5 External locking protocol quoted (`claude_locks.py`)

> Claude Code guards its OAuth token refresh with the npm `proper-lockfile`
> package on the config home directory, and its `~/.claude.json` writes with
> the same mechanism on the config file. The protocol:
> - The lock artifact is a **directory** at `<target>.lock`
>   (`~/.claude.lock`, `~/.claude.json.lock`); `mkdir` atomicity is the mutex.
> - A lock is considered stale when its mtime is older than 10s; live
>   holders touch the mtime every 5s to prove liveness, and a stale lock may
>   be removed and taken over.
> - Claude Code retries a held credentials lock 5 times with 1-2s jittered
>   sleeps before giving up.
>
> References (claude-code source): `utils/auth.ts
> checkAndRefreshOAuthTokenIfNeededImpl`, `utils/config.ts
> saveConfigWithLock`, `utils/lockfile.ts`.

cswap's own `proper_lockfile(lock_dir, timeout=None, staleness=STALENESS_S)`
implements the identical directory-`mkdir` protocol:
- `STALENESS_S = 10.0` (matches Claude's own staleness threshold).
- `TOUCH_INTERVAL_S = 3.0` (cswap touches a bit faster than Claude's 5s for
  margin) — a daemon thread calls `os.utime(lock_dir)` every 3s while held.
- `DEFAULT_TIMEOUT_S = 9.0` — the default acquire timeout when the caller
  passes `timeout=None` (as `_sync_mcp_servers` does); resolved at call time
  so tests can shorten it. On timeout, raises
  `ClaudeCodeLockTimeout(f"Could not acquire {lock_dir.name} — Claude Code appears to be refreshing credentials. Retry in a few seconds.")`.
- Acquire loop: `os.mkdir(lock_dir)`; on `FileExistsError`, check elapsed
  time against `timeout` (raise if exceeded); else stat the lock's mtime —
  if `time.time() - held_mtime > staleness`, treat as dead, `rmdir` and
  retry immediately (losing the race to another waiter just means looping
  again); otherwise sleep `0.25 + random.random() * 0.25` seconds and retry.
  A `FileNotFoundError` on the stat (holder released between `mkdir` and
  `stat`) retries immediately with no sleep.
- On exit: stop the toucher thread (`.join(timeout=1.0)`), then
  `os.rmdir(lock_dir)` — `FileNotFoundError` (lock vanished/stolen while
  held) logs a warning but doesn't raise.

---

## 4. `process_detection.py`

Reads Claude Code's own state files — **the same mechanism Claude Code uses
internally** — to detect currently-running instances. Nothing here writes
anything; it is a pure read-only probe.

### 4.1 Data model

```python
@dataclass
class ClaudeSession:
    pid: int
    session_id: str
    cwd: str
    started_at: int          # epoch milliseconds
    kind: str                 # "interactive", "bg", "daemon", "daemon-worker"
    entrypoint: str            # "cli", "claude-vscode", "claude-desktop", "sdk-cli", "mcp" (also seen: "sdk-ts", "sdk-py", "local-agent", "remote")
    status: str | None = None  # "busy", "idle", "waiting"

@dataclass
class IdeInstance:
    port: int                          # parsed from the lockfile's filename stem
    pid: int
    ide_name: str                       # "Visual Studio Code", "Cursor", "Windsurf"
    workspace_folders: list[str] = field(default_factory=list)
```

### 4.2 Source paths (external Claude Code file formats)

> Reads session PID files (`~/.claude/sessions/{pid}.json`) and IDE
> lockfiles (`~/.claude/ide/{port}.lock`) to determine which Claude Code
> instances are currently running.

```python
def get_claude_dir() -> Path:
    return get_claude_config_home()   # CLAUDE_CONFIG_DIR if set, else ~/.claude
```

`list_sessions(claude_dir=None)`: globs `<claude_dir>/sessions/*.json`
(returns `[]` if the `sessions` subdir isn't a directory). For each file,
`json.loads` the text, require an integer-valued `pid` key (`KeyError`/
`TypeError` on a malformed/missing field skips that file — logged at
`debug`, never raised), then keep it only if `is_pid_alive(pid)`. Fields
default via `.get(...)`: `sessionId` → `""`, `cwd` → `""`, `startedAt` → `0`,
`kind` → `""`, `entrypoint` → `""`, `status` → `None`. A corrupt/unparsable
JSON file (`json.JSONDecodeError`) or any `OSError` reading it is likewise
skipped, not raised.

`list_ide_instances(claude_dir=None)`: globs `<claude_dir>/ide/*.lock`.
`port` comes from `int(path.stem)` (the filename, not the JSON body).
Requires a non-`None` `pid` in the JSON, filtered through `is_pid_alive`.
`ideName` defaults to `"Unknown IDE"`; `workspaceFolders` defaults to `[]`.
Same fail-silent-and-skip handling for `JSONDecodeError`/`KeyError`/
`TypeError`/`ValueError` (the `ValueError` also covers a non-integer `port`
filename stem)/`OSError`.

`get_running_instances(claude_dir=None) -> tuple[list[ClaudeSession], list[IdeInstance]]`:
resolves `claude_dir` once (`claude_dir or get_claude_dir()`) and returns
`(list_sessions(resolved), list_ide_instances(resolved))`.

### 4.3 `is_pid_alive` — per-platform liveness check

```python
def is_pid_alive(pid: int) -> bool:
    if pid <= 1:
        return False
    if sys.platform == "win32":
        return _is_pid_alive_windows(pid)
    try:
        os.kill(pid, 0)
        return True
    except PermissionError:
        return True   # EPERM means the process exists but we lack permission
    except OSError:
        return False
```
`pid <= 1` (covers `0` and negative PIDs, and PID 1 / `init`) is always
`False` regardless of platform — guards against treating a malformed/absent
`pid` field's default as "alive". macOS/Linux/WSL: `os.kill(pid, 0)` (the
POSIX no-signal existence probe) — `OSError` (`ProcessLookupError` etc.)
means dead; `PermissionError` (`EPERM`, process exists but owned by another
user) counts as **alive**.

```python
def _is_pid_alive_windows(pid: int) -> bool:
    try:
        import ctypes
        PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
        kernel32 = ctypes.windll.kernel32
        handle = kernel32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, pid)
        if handle:
            kernel32.CloseHandle(handle)
            return True
        return False
    except Exception:
        return False
```
Windows: `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION=0x1000, bInheritHandle=False, pid)`; a non-null handle means alive (closed immediately after the check); any exception (including on non-Windows hosts where `ctypes.windll` doesn't exist) is swallowed to `False`.

### 4.4 What it's used for

- **`session.py`**: `live_sessions_for(session_dir) -> list[ClaudeSession]`
  is `list_sessions(claude_dir=session_dir)` guarded by `session_dir.exists()`
  (returns `[]` immediately if not). This is the input to:
  - the deferred stale-credential-invalidation check in `setup_session` (§1.4
    step 3) — a session profile is only re-bootstrapped from a stale marker
    when nothing is live against it;
  - `_prepare_history_share`'s live-session deferral (§2.4 step 1) — history
    is never merged out from under a running `claude`.
- **`switcher.py`** (guard chokepoint `_ensure_no_live_session`, built on
  `_live_session_pids` → `live_sessions_for`): refuses these destructive/
  mutating operations while any PID is live against the account's session
  profile, raising `SessionError`:
  - `_delete_account_files` (the single chokepoint for `remove_account`,
    and `add_account`/`add_token` slot overwrite/migration) — message:
    `f"Account-{account_num} ({email}) has a live session-mode Claude instance (PID {pids}). Exit it first, then retry {action}."`
    with `action` filled in per call site (`"the operation"`,
    `"--swap-accounts"`, `"--move-account"`, `"--remove-account"`).
  - `purge()` sweeps *every* session dir under `<backup_dir>/sessions/`
    (not just one account) and, if any has live PIDs, raises
    `SessionError(f"Live session-mode Claude instance(s) found: {details}. Exit them first, then retry --purge.")`
    where `details` lists `"{profile-dirname} (PID {pids}); ..."` for each
    live profile.
  - `_perform_switch` (switching the *default* login) does **not** refuse —
    it only *warns* (`"live session-mode"` appears in the printed output)
    and completes the switch, because switching the default login doesn't
    touch the session profile's own credential copy.
  - `list_accounts()` skips a **proactive token refresh** for any account
    with a live session PID (treating it "like active": no refresh) — so
    `cswap --list` never rotates a session's backup-store token copy out
    from under a running `claude`.
- **TUI/CLI display** (`printer.py`, exercised alongside process-detection
  tests): `entrypoint_label(entrypoint)` maps raw entrypoint strings to
  human labels via
  ```python
  {"cli": "CLI", "claude-vscode": "VS Code", "claude-desktop": "Desktop",
   "sdk-cli": "SDK", "sdk-ts": "SDK", "sdk-py": "SDK", "mcp": "MCP",
   "local-agent": "Agent", "remote": "Remote"}
  ```
  falling back to the raw string for anything unrecognized.
  `ide_short_name(ide_name)` maps `{"Visual Studio Code": "VS Code"}`,
  else passes through. `abbreviate_path(path)` replaces a leading
  `str(Path.home())` prefix with `"~"` (exact-prefix string match, not path-
  component aware) — `home` itself becomes exactly `"~"`. `format_age(started_at_ms)`:
  `elapsed = int(time.time()) - (started_at_ms // 1000)`; `< 60s` →
  `"just now"`; `< 3600s` → `f"{elapsed // 60}m ago"`; `< 86400s` →
  `f"{elapsed // 3600}h ago"`; else `f"{elapsed // 86400}d ago"`.

---

## 5. Directory → account mappings (`mappings.py`)

### 5.1 Storage location & schema

Path: `<backup_dir>/mappings.json`. `SCHEMA_VERSION = 1`.
```json
{
  "schemaVersion": 1,
  "mappings": {
    "<normalized-absolute-path>": {
      "email": "work@co.com",
      "organizationUuid": "org-1",
      "added": "2026-07-17T12:00:00Z"
    }
  }
}
```
`organizationUuid` is always a string (`org_uuid or ""` — never `null`).
`added` is `models.get_timestamp()` — `datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")`.

Identity is stored as the **stable `(email, organizationUuid)` composite**,
not the slot number — slot numbers are reused when accounts are removed and
re-added, so a mapping keyed on a number could silently point at a different
account later. `mappings.py` is deliberately decoupled from `switcher.py`
(never imports it); callers resolve `(email, org)` → a live slot themselves
(`switcher.slot_for_directory`, `switcher._find_account_slot`).

### 5.2 Path normalization

```python
def normalize_path(p: str | Path) -> str:
    resolved = Path(p).expanduser().resolve()
    return os.path.normcase(str(resolved))
```
Expands `~`, makes absolute, **resolves symlinks** (`Path.resolve()`), then
applies `os.path.normcase` — case-folding (and `/`→`\`) on Windows, a no-op
on POSIX. Guarantees `path`, `path/`, and `path/.` all normalize to the
identical key (verified by test). This is the sole comparability contract:
every store method funnels paths through it before use.

### 5.3 `MappingStore` API

```python
class MappingStore:
    def __init__(self, backup_dir: Path): self.path = Path(backup_dir) / "mappings.json"

    def load(self) -> dict[str, dict]          # {} on missing file, corrupt JSON, non-dict root, or non-dict "mappings" value
    def all(self) -> dict[str, dict]            # alias for load()
    def get(self, path) -> dict | None           # exact normalized-key lookup, no ancestor walk
    def set(self, path, email, org_uuid) -> None  # upsert; always rewrites "added" to now
    def remove(self, path) -> bool               # True iff a mapping existed and was deleted
    def prune_account(self, email, org_uuid) -> int  # delete every mapping for (email, org_uuid); returns count removed
    def resolve(self, cwd) -> tuple[str, dict] | None  # nearest-ancestor lookup, see 5.4
```
`load()` swallows `json.JSONDecodeError`, `UnicodeDecodeError`, `OSError` →
`{}`. `prune_account` normalizes `org_uuid` the same way as `set`
(`org_uuid or ""`) before comparing, and only rewrites the file
(`self._write`) if at least one mapping was actually removed.

### 5.4 Nearest-ancestor resolution

```python
def resolve(self, cwd: str | Path) -> tuple[str, dict] | None:
    target = Path(normalize_path(cwd))
    best = None; best_len = -1
    for key, entry in self.load().items():
        candidate = Path(key)
        if candidate == target or candidate in target.parents:
            if len(key) > best_len:
                best = (key, entry); best_len = len(key)
    return best
```
A mapping matches when its directory **equals** `cwd` or is an **ancestor**
of it (`Path.parents`). Because every candidate that matches lies on the
single root→cwd chain, the *longest key string* is unambiguously the
*deepest* (most specific) match — no need to count path segments. Ties are
impossible (distinct ancestors of one chain have distinct lengths). A
sibling whose name is a string-prefix of the mapped directory does **not**
match — `Path.parents` is component-aware, not a string-prefix test (e.g.
`/foo/bar` mapped does not match cwd `/foo/barbaz`). Unmapped or missing
store → `None`.

### 5.5 `cswap run` (bare) resolution — `switcher.slot_for_directory`

```python
def slot_for_directory(self, directory) -> tuple[str | None, str | None]:
    match = MappingStore(self.backup_dir).resolve(directory)
    if match is None:
        return None, None
    _, entry = match
    email = entry.get("email", "")
    seq = self._get_sequence_data_migrated() or {}
    slot = self._find_account_slot(seq, email, entry.get("organizationUuid", "") or "")
    return slot, email
```
Three-way result consumed by `cli.py`'s `_run_command` (§1.2):
`(None, None)` = no mapping at all; `(None, email)` = mapping exists but the
account behind it no longer has a slot (removed); `(slot, email)` = live
resolution.

### 5.6 CLI: `cswap map` / `cswap unmap`

```
cswap map [NUM|EMAIL] [PATH]     # no NUM|EMAIL → list all mappings
cswap unmap [PATH]               # default PATH: current directory
```
`map` with an account argument: `target = args.path or os.getcwd()`; if
`not os.path.isdir(target)`, warns
`f"Warning: {target} is not an existing directory (mapping it anyway)"` but
proceeds (a mapping for a not-yet-created directory is allowed). Resolves
the account via `switcher.resolve_account(args.account)` (same resolver as
`run` — raises `AccountNotFoundError`/`ConfigError` on ambiguity). Reads any
`previous = store.get(target)` **before** overwriting, so it can report a
re-map:
- No previous mapping, or same email: `f"{accent('Mapped')} {shown} → Account-{account_num} ({email})"`.
- Previous mapping pointed at a different email:
  `f"{accent('Mapped')} {shown} → Account-{account_num} ({email}) {muted(f'(was {prev_email})')}"`.

`map` with no account argument → `switcher.list_mappings()`:
- No mappings at all: `dimmed("No directory mappings yet.")` then
  `muted("Map one with: cswap map <NUM|EMAIL> [PATH]")`.
- Otherwise, `bolded("Directory mappings:")` then, for each path in sorted
  key order:
  - If the mapped `(email, org)` still resolves to a slot: `f"  {path} {dimmed('→')} {slot}: {email} {muted(f'[{tag}]')}"` (`tag` from `_get_display_tag`, e.g. an org name or `"personal"`).
  - Else: `f"  {path} {dimmed('→')} {email} {muted('(account removed)')}"`.

`unmap`: `target = args.path or os.getcwd()`; `store.remove(target)` — `True`
→ `f"{accent('Unmapped')} {shown}"`; `False` → `dimmed(f"No mapping for {shown}")`.

Both `map` and `unmap` run the same root guard as `run` (`_guard_root`,
§1.1) and the same top-level error handling (`ClaudeSwitchError` → `Error:
{e}`, exit 1; `KeyboardInterrupt` → cancelled message, exit 130).

### 5.7 Cleanup on account removal

`switcher._prune_mappings(email, org_uuid)` — called from every code path
where an `(email, org)` identity permanently leaves the account table
(`remove_account`, and `add_account`/`add_token`'s slot-overwrite path):
`MappingStore(backup_dir).prune_account(email, org_uuid or "")`; if any were
removed, prints `dimmed(f"Removed {pruned} directory mapping(s) for this account")`.
**Not** called on slot *migration* (`--move-account`) or `--import --force`
— those operations keep the same `(email, org)` identity, just relocate
which slot number it occupies, so existing mappings (keyed on identity, not
slot) remain correct without any pruning.

### 5.8 Per-machine, non-exported

`mappings.json` is never referenced by `transfer.py` (the `--export`/
`--import` account-transfer module) — directory mappings are inherently
local to the machine they were created on (they encode filesystem paths)
and are **not** part of any exported/imported account bundle. A fresh
machine, or an imported account set, starts with an empty (or
independently-managed) `mappings.json`.

### 5.9 Persistence details

```python
def _write(self, mappings: dict[str, dict]) -> None:
    self.path.parent.mkdir(parents=True, exist_ok=True)
    if sys.platform != "win32":
        os.chmod(self.path.parent, 0o700)
    payload = json.dumps({"schemaVersion": SCHEMA_VERSION, "mappings": mappings}, indent=2)
    fd, tmp = tempfile.mkstemp(dir=str(self.path.parent), prefix=".mappings-", suffix=".tmp")
    try:
        if sys.platform != "win32":
            os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(payload)
        os.replace(tmp, self.path)
    except OSError:
        try: os.unlink(tmp)
        except OSError: pass
        raise
```
Atomic write via tempfile + `os.replace`; on POSIX the backup dir is forced
to `0o700` and the temp file (hence the final file, since `chmod` isn't
re-applied post-`replace` — the mode travels with the renamed inode) to
`0o600` **before** any content is written (`os.fchmod` on the open fd, not a
post-hoc `os.chmod` on the path). Unlike `session.py`'s manifest/`
atomic_write_json`, a failure here **re-raises** (`set`/`remove`/
`prune_account` do not silently swallow a write failure the way share-sync
does).

---

## 6. Edge cases & subtleties (from tests)

- **Fast path only fires without a preset `CLAUDE_CONFIG_DIR`, even when the
  identity matches.** `test_preset_config_dir_disables_fast_path`: if
  `CLAUDE_CONFIG_DIR` is already set in the environment, `cswap run` for the
  currently-active account still builds a full session profile rather than
  exec'ing plain `claude` — "current default account" is undefined/
  ambiguous from inside another session.
- **Auth-override scrubbing is asymmetric.** The fast path
  (`test_fast_path_keeps_env_untouched`) and `exec_default`
  (`test_exec_default_uses_plain_env`) never scrub `AUTH_OVERRIDE_ENV_VARS`
  — only the actual session-profile launch path does.
  `test_auth_override_vars_scrubbed_from_session_env` confirms unrelated env
  vars pass through unmodified alongside the scrub.
- **Refresh-token detection defaults to attempting refresh on ambiguity.**
  `_has_refresh_token` returns `True` (not `False`) for a credentials blob
  it can't parse the shape of — "unknown shape, let the refresh attempt
  decide" rather than silently skipping a refresh that should have
  happened.
- **Setup-token (`--add-token`) accounts skip refresh with zero visible
  side effect** — no warning is printed (`test_setup_token_account_skips_refresh_silently`),
  distinguishing "no refresh token by design" from "refresh attempted and
  failed."
- **Refresh failure is non-fatal and silently falls back to stored
  creds** (`test_refresh_failure_uses_stored_creds`) — only a printed
  warning, bootstrap still succeeds.
- **A stale macOS keychain entry from an earlier profile at the identical
  path is deleted before every seed attempt**, not just once
  (`test_stale_keychain_entry_deleted_before_seed`), and again on validation
  failure (`test_validation_failure_cleans_up` — the keychain entry does not
  survive a failed bootstrap either).
- **Reuse genuinely skips both the refresh call and any credential
  rewrite** (`test_reuse_skips_refresh_and_writes`) — the reuse path must
  not touch `.credentials.json` at all when the profile is already valid.
- **The stale marker only forces re-bootstrap once the profile is
  quiescent.** `test_stale_marker_preserved_while_session_still_live`: with
  a live PID present, `setup_session` leaves stale credentials in place
  *and* leaves the marker file in place (deferred, not dropped) for the
  next launch after the session actually exits
  (`test_stale_marker_forces_rebootstrap_after_session_exits`).
- **Re-bootstrap always merges into the existing `.claude.json`, never
  overwrites it** (`test_rebootstrap_preserves_profile_history`) — a
  profile's own `projects` key (in-session-accumulated conversation state)
  survives a full credential re-seed.
- **`_is_session_valid` resolves `claude` via `shutil.which`, not a bare
  string**, specifically so the probe works when `claude` is a Windows
  `.cmd` shim (`test_invokes_pathext_resolved_launcher`) — passing bare
  `"claude"` to `subprocess.run` does not consult `PATHEXT` and would raise
  `FileNotFoundError`, silently manifesting as "failed validation" instead
  of a clear PATH problem.
- **`_sync_sharing`'s "never touch user data" rule is airtight even for a
  file the profile only *just* wrote before this launch**
  (`test_never_touches_user_data`): a plain (non-symlink) `CLAUDE.md` the
  profile holds is left completely alone, with an explicit
  `"Not sharing CLAUDE.md: ..."` message, and is never added to the
  manifest (so it's still recognized as "not ours" on the next launch too).
- **A previously-cswap-managed symlink pointing somewhere unexpected gets
  silently repointed** to the correct source
  (`test_repoints_stale_link`) — sharing self-heals a manually-edited or
  stale symlink without user intervention or warning.
- **`--no-share` removes only manifest-tracked entries, never arbitrary
  files the profile accumulated** (`test_no_share_removes_only_managed`) —
  a `private.txt` dropped directly in the profile survives `--no-share`.
- **History merge is deferred, not skipped, when the profile is live** —
  the deferral message explicitly promises a retry
  (`"retrying on the next launch"`), and the item stays unmanaged (absent
  from the manifest) in the meantime so the generic sync loop doesn't try
  to touch it either.
- **History-file merge dedupes by exact line content, not by any semantic
  key** — `test_merges_existing_profile_history` confirms
  `'{"p": "main"}'` appears exactly once after merge even though it existed
  verbatim on both sides, while a differently-valued line
  (`'{"p": "profile"}'`) is kept.
- **Directory history merge is first-writer-wins on filename collision** —
  `test_merge_collision_keeps_target`: when both sides have a
  `-home-user-app/aaa.jsonl`, the pre-existing `~/.claude` copy (`"main-a\n"`)
  survives; the profile's colliding copy is discarded, not appended or
  renamed.
- **Freshly-seeded (created-because-missing) history sources get exact
  Claude Code permissions**, not merely "some private mode" —
  `0o700` for every directory level of a freshly created `projects/` tree
  and `0o600` for a freshly created `history.jsonl`
  (`test_seeded_source_has_claude_code_modes`,
  `test_merge_creates_dirs_and_files_with_claude_code_modes`) — this
  matters because `Path.mkdir(mode=...)` only applies to the leaf directory,
  so a naive recursive mkdir would leave intermediate dirs world-readable
  forever.
- **MCP mirror: a `null`-valued key not present upstream is still "displaced"
  and gets stashed** (`test_null_valued_entry_is_stashed`) — the membership
  check is `name not in source or source[name] != value`, which correctly
  treats an explicit `null` as a real (differing) value rather than treating
  it as equivalent to "absent."
- **MCP mirror: editing an already-mirrored profile's `mcpServers` locally
  is silently reset on the next `share=True` sync, with no stash** — only
  the very *first* adoption stashes; every subsequent divergence is treated
  as expected drift to be corrected, not data to preserve
  (`test_session_local_change_reset_without_stash`).
- **MCP mirror: a squatter (directory) on the stash filename blocks the
  reset entirely** rather than silently overwriting it or proceeding
  without a backup (`test_invalid_stash_blocks_reset`) — the marker is
  never written in this case either, so the next launch retries the same
  adoption attempt.
- **MCP mirror: the steady-state adopted-and-in-sync path takes zero
  locks** (`test_adopted_in_sync_run_takes_no_lock` — patches
  `proper_lockfile` to raise `AssertionError` if called at all, and the sync
  must not raise). Only first adoption, and any subsequent actual change,
  goes through the lock.
- **MCP mirror fails open on every conceivable malformed input** — missing
  source config, corrupt JSON, non-dict JSON root, non-dict `mcpServers`
  value, binary/non-UTF-8 bytes on the source side
  (`test_fail_open_on_bad_source`, parametrized); `null`/`[]`/a bare string
  as the *profile's* `mcpServers` value (`test_fail_open_on_bad_target_mcp`);
  a corrupt session `.claude.json` (`test_corrupt_session_config_skipped`);
  a symlinked session `.claude.json` (`test_symlinked_session_config_skipped`,
  POSIX-only); and a lock held by a live (fresh-mtime) directory that never
  clears within the timeout (`test_held_lock_fails_open` — shortens
  `claude_locks.DEFAULT_TIMEOUT_S` to `0.3` for the test). In every case the
  session config is byte-for-byte unchanged and the launch proceeds.
- **The legacy `.config.json` is honored as the MCP mirror source** ahead of
  `.claude.json` when both could apply (`test_legacy_config_json_source`),
  matching `get_default_global_config_path`'s own legacy-first resolution.
- **`--no-share` is a true no-op before adoption**
  (`test_no_share_before_adoption_untouched`) and a full remove-then-restore
  cycle after (`test_no_share_after_adoption_removes_then_restores` — the
  marker itself survives a `--no-share` run, since "adoption is history"
  that a temporary opt-out must not erase).
- **`slugify_email` filters per *character* (post-NFC), not per byte.**
  `"bø@x.com"` → `"b__x.com"`: `ø` is a single NFC codepoint but fails
  `isascii()` → one `_`; `@` fails the alnum/`._-` test → another `_`; the
  two underscores in the expected output come from these two distinct
  characters, not from `ø` being multi-byte. `.` is kept as-is (it's in the
  allowed `._-` set). Implementers must iterate the NFC-normalized string
  rune-by-rune (not byte-by-byte — a naive UTF-8 byte loop would emit one
  `_` per byte of `ø`, over-escaping multi-byte characters).
- **Keychain service-name hashing is exactly reproducible and NFC-order-
  insensitive**: `keychain_service_name` NFC-normalizes the *raw, unresolved*
  `str(session_dir)` before hashing — `test_keychain_service_name_nfc_nfd_equal`
  confirms an NFC- vs NFD-composed identical-looking path string yields the
  *same* service name (both normalize to the same NFC form first), even
  though the raw strings compare unequal.
- **`read_session_credentials` prefers the macOS keychain over the plaintext
  file when both exist**, because Claude migrates the plaintext seed into
  its hashed keychain entry on first write and only ever updates it there
  afterward (`test_keychain_shadows_plaintext_on_macos`) — the plaintext
  file is a stale first-boot snapshot the instant the keychain entry
  appears. Off macOS, or with no keychain entry present, it falls back to
  the plaintext file (`test_macos_falls_back_to_file_without_keychain_entry`).
  A byte-corrupt plaintext file (undecodable UTF-8) returns `None`, not an
  exception (`test_byte_corrupt_file_returns_none`).
- **`is_pid_alive` treats a live PID owned by another user (`EPERM`) as
  alive**, not as "can't tell" — this matters for the live-session guard,
  which must refuse a destructive operation even when cswap cannot signal
  the process directly.
- **`slot_for_directory`'s three-state return (`(None,None)` /
  `(None,email)` / `(slot,email)`) is exactly mirrored by the CLI's
  three-branch message dispatch** in `_run_command` — porting only the two-
  state "found or not" simplification would lose the "was mapped but the
  account is gone" distinct message.
- **`mappings.resolve` is a linear scan over every stored mapping per
  call** — no prefix trie or sorted-path optimization; correctness (longest
  ancestor via string length on filtered candidates) does not depend on
  iteration order, but a Go port doing the naive equivalent will match
  behavior exactly.
- **A mapping to a directory that doesn't exist yet is allowed** (with a
  warning), not rejected — supports mapping a repo path before it's been
  cloned.

---

## 7. Go port notes

### 7.1 Concurrency & threading in the Python code

- **`proper_lockfile`'s liveness-touch thread** (`claude_locks.py`): a
  daemon thread wakes every `TOUCH_INTERVAL_S=3.0`s and calls
  `os.utime(lock_dir)` while the lock is held, stopped via a
  `threading.Event` and joined with a `1.0`s timeout on release. A Go port
  needs an equivalent background goroutine + cancellation (context or a done
  channel) around every `proper_lockfile`-guarded critical section — this is
  the mechanism that keeps cswap's own MCP-sync lock hold from being
  mistaken for a dead/stale lock by a concurrently-running real `claude`
  process (or another `cswap` instance) waiting on the same directory.
- **`FileLock`** (`locking.py`) is a straightforward blocking-poll wrapper
  over `fcntl.flock` (POSIX) / `msvcrt.locking` (Windows) with a
  `time.sleep(0.1)` retry loop up to a timeout — no background thread. Go:
  `syscall.Flock` (POSIX) / `LockFileEx` (Windows), or a well-tested
  cross-platform library; the polling interval (100ms) and default timeout
  (10.0s, though the bootstrap path in `session.py` passes `30.0`
  explicitly) should be preserved as named constants.
- **No other threading** appears in the mandate's three modules —
  `_sync_sharing`, `_bootstrap`, and mapping I/O are all synchronous,
  single-threaded, called directly on the CLI's main goroutine equivalent.
  Concurrency-safety instead comes from cooperative file-locking protocols
  (advisory `flock`/`mkdir`-lock, atomic rename) rather than in-process
  synchronization — a Go port should resist the temptation to add mutexes
  for state that's actually protected (or deliberately left racy/self-
  healing, e.g. the lock-free share-sync path) by the file-level protocol.

### 7.2 Platform-conditional logic to preserve exactly

- **`Platform.detect()`** dispatches on `sys.platform`/`WSL_DISTRO_NAME` env
  var, *not* `platform.system()` — the code comment explains this is
  because `platform.system()` on Windows calls `platform.uname()`, which
  runs a WMI query that can hang indefinitely on a slow/unresponsive WMI
  service. A Go port should use `runtime.GOOS` plus an explicit
  `WSL_DISTRO_NAME` env check, never shell out to anything WMI-adjacent.
- **Symlink vs. copy sharing mode** is gated on `Platform != WINDOWS`, not
  on filesystem capability detection — even a Windows host with Developer
  Mode symlinks enabled still gets copy mode. Preserve this as a hard
  platform switch, not a runtime symlink-support probe.
- **`--share-history` is unconditionally rejected on Windows** at the CLI
  layer (`run()`) *and* independently forced off inside `_sync_sharing` —
  both checks should be ported; the second is defense-in-depth for any
  direct (non-CLI) caller of the sync function.
- **`_exec`'s POSIX/Windows split** (`execvpe` vs. subprocess+exit-code-
  mirror) is fundamental: Go has no direct `execve`-and-never-return
  equivalent that's portable, but POSIX Go can use `syscall.Exec` (which
  *does* replace the process image, same as `execvpe`) while Windows Go
  should spawn+wait+`os.Exit(cmd.ProcessState.ExitCode())`, mirroring
  Ctrl+C (`SIGINT`) as exit code 130 to match the Python `KeyboardInterrupt`
  handling.
- **`chmod`/`fchmod` calls are all wrapped in `sys.platform != "win32"`**
  guards throughout (`session.py` bootstrap, `mappings.py` `_write`) — Go's
  `os.Chmod` on Windows is a partial no-op for most bits anyway, but the
  explicit skip should be preserved for clarity and to avoid spurious
  errors on exotic filesystems.
- **Windows PID liveness** uses `ctypes.windll.kernel32.OpenProcess` with
  `PROCESS_QUERY_LIMITED_INFORMATION = 0x1000` — Go should use
  `golang.org/x/sys/windows.OpenProcess` with the same access-right
  constant (`windows.PROCESS_QUERY_LIMITED_INFORMATION`), not
  `PROCESS_QUERY_INFORMATION` (a different, more-privileged constant) or a
  toolhelp-snapshot enumeration (behaviorally different: this call fails
  softly to "not alive" on a handle it can't open, matching the POSIX
  `EPERM`-is-alive asymmetry only approximately — port the exact semantics:
  non-null handle ⇒ alive, any failure/exception ⇒ not alive, no special
  permission-denied-means-alive case on Windows, unlike POSIX).
- **`os.path.normcase`** (`mappings.normalize_path`) is a no-op on POSIX and
  lowercases (plus slash-normalizes) on Windows. Go has no direct
  equivalent; a port should implement
  `filepath.Clean` + (on Windows only) `strings.ToLower` explicitly, and
  must resolve symlinks (`filepath.EvalSymlinks` or equivalent to match
  `Path.resolve()`) before that — the test `test_normalize_path_applies_normcase`
  exists specifically to pin this call site, so the Go equivalent should be
  a single injectable/mockable function for the same reason.

### 7.3 Python-isms needing a deliberate Go equivalent

- **`str.isascii()`/`str.isalnum()` per-character filtering** in
  `slugify_email` operates on Python's Unicode-aware `str`, post
  `unicodedata.normalize("NFC", ...)`. Go's `range` over a `string` yields
  runes; the port should NFC-normalize first (`golang.org/x/text/unicode/norm`),
  then per-rune test `rune < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))`
  or `strings.ContainsRune("._-", r)`, else emit `_` — matching Python's
  `ch.isascii() and (ch.isalnum() or ch in "._-")` exactly (note:
  `isalnum()` is broader than `IsLetter||IsDigit` in general Unicode, but
  since ASCII-only alnum is what survives the `isascii()` guard anyway, a
  simple `('a'<=r&&r<='z')||('A'<=r&&r<='Z')||('0'<=r&&r<='9')` ASCII-range
  check is both simpler and exactly equivalent here).
- **`hashlib.sha256(...).hexdigest()[:8]`** — Go's `crypto/sha256` +
  `hex.EncodeToString(sum[:])[:8]` is a direct equivalent; the critical
  detail to preserve is hashing the **NFC-normalized, unresolved** (not
  `filepath.EvalSymlinks`'d / `Abs`'d) string representation of the session
  directory path — resolving it first would produce a different hash than
  Claude Code's own `envUtils.ts` computation and break keychain lookups.
- **JSON `None`/absent-key ambiguity** is load-bearing in several places
  (`_read_mcp_source` distinguishing `{}` from `None`; `read_session_identity`
  treating a present-but-empty `organizationUuid` field the same as an
  absent one via `oauth_account.get("organizationUuid") or ""`). Go's
  `encoding/json` into `map[string]any` (not a typed struct with
  `omitempty`) is the closest match for the MCP source/target values, since
  the port needs to detect "key present but not a dict" as a distinct
  failure mode from "key absent" in a few places (e.g. the malformed-
  `mcpServers` fail-open branch) — a typed struct would silently coerce or
  error differently than Python's duck-typed `isinstance` checks.
- **`Path.resolve()` failure modes**: Python's `pathlib.Path.resolve()`
  does not raise on a nonexistent path (it resolves as far as it can and
  leaves the rest lexically joined); Go's `filepath.EvalSymlinks` **does**
  return an error for a nonexistent path. `mappings.normalize_path` is
  called on paths that may not exist yet (mapping an as-yet-uncloned repo,
  per §6). A Go port must special-case "path doesn't exist" to fall back to
  a lexical `filepath.Abs` + `filepath.Clean` rather than erroring, to
  preserve the "map a not-yet-existing directory" behavior.
- **Exception-as-control-flow for "fail open"** is pervasive in
  `_sync_mcp_servers`/`_sync_sharing` (broad `except OSError` / `except
  (json.JSONDecodeError, OSError)` blocks that log-and-continue rather than
  propagate). A Go port must thread explicit `error` returns through every
  one of these helper functions and have each call site consciously decide
  to log-and-continue vs. propagate — there is no free equivalent of
  Python's blanket `except` swallowing here, and getting the swallow scope
  wrong (too broad, hiding a real bug; too narrow, breaking the fail-open
  contract tested extensively in §6) is the single highest-risk area of
  this module to port faithfully. Recommend porting the exact exception
  tuples as sentinel checks (`errors.Is(err, os.ErrNotExist)`, a custom
  `ErrCorruptJSON`, etc.) rather than a blanket `if err != nil { log; return }`.
- **Dataclass `field(default_factory=list)`** (`IdeInstance.workspace_folders`)
  — straightforward as a Go slice defaulting to `nil`/`[]string{}`; just
  ensure JSON-unmarshal of an absent `workspaceFolders` key produces the
  Python-equivalent empty slice rather than `nil` if any downstream code
  distinguishes them (none currently does, per the read source).
- **argparse's pre-dispatch verb sniffing** (`argv[0] == "run"` / `"map"` /
  `"unmap"` checked *before* the main flag-based parser runs, with the
  explicit limitation that `run` must be argv's first token) is a
  hand-rolled shortcut, not something a Go flag/cobra framework does for
  free — a Go port using e.g. `cobra` subcommands would naturally support
  `cswap --debug run 2`, which is a **behavior change** from the Python CLI
  (arguably an improvement, but worth flagging as an intentional deviation
  rather than an accidental one if adopted).
