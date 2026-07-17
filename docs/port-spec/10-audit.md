# claude-swap — Completeness Audit of the Go-Port Spec Set

Auditor pass over the 8 partitioned specs (`01`–`08`) plus the TUI/menubar
spec (`09-tui-menubar.md`) against the actual source in
`src/claude_swap/`. Bottom line: **coverage is near-complete**. All 36 `.py`
modules (28 top-level + 8 under `tui/`) are covered by at least one spec, and
every load-bearing constant/string I spot-checked matches the source
verbatim. Only two trivial entry-point files (`__init__.py`, `__main__.py`)
were left implicit; their missing content is filled in below, along with a
handful of small incompletenesses (not contradictions) worth folding into the
port.

## File-by-file coverage map (all 36 modules)

| File | Covered by | Status |
|---|---|---|
| `switcher.py` | 01 (account store/lifecycle), 02 (switch/report) | full |
| `credentials.py` | 03 §5 | full |
| `macos_keychain.py` | 03 §4 | full |
| `claude_locks.py` | 03 §6 (also 06 §3.5) | full |
| `locking.py` | 03 §7 | full |
| `paths.py` | 03 §2 (also 01 §1) | full |
| `oauth.py` | 04 §1 | full |
| `usage_store.py` | 04 §2 | full |
| `poll_policy.py` | 04 §3 | full |
| `cache.py` | 04 §4 | full |
| `models.py` | 04 §5 (Platform/AccountInfo/Snapshots/get_timestamp), 01 §8.1 (normalize_alias), 02 §8.2 (SwitchTransaction) | full |
| `autoswitch.py` | 05 | full |
| `settings.py` | 08 §8 (also 05 §2) | full |
| `session.py` | 06 §1–3 | full |
| `mappings.py` | 06 §5 | full |
| `process_detection.py` | 06 §4 | full |
| `transfer.py` | 07 §1–4 | full |
| `migrations.py` | 07 §5 | full |
| `cli.py` | 08 §1–7 | full |
| `json_output.py` | 08 §9 | full |
| `printer.py` | 08 §10 (also 06 §4.4) | full |
| `exceptions.py` | 08 §11 | full |
| `logging_config.py` | 08 §12 | full |
| `update_check.py` | 08 §13 (also 02 §15) | full |
| `snapshot_source.py` | 09 §6.1 | full |
| `menubar.py` | 09 §10 | full (flagged out-of-scope) |
| `tui/__init__.py` | 09 §1 | full |
| `tui/app.py` | 09 §2 | full |
| `tui/dashboard.py` | 09 §3 | full |
| `tui/autoview.py` | 09 §4 | full |
| `tui/widgets.py` | 09 §5 | full |
| `tui/data.py` | 09 §6 | full |
| `tui/modals.py` | 09 §7 | full |
| `tui/theme.py` | 09 §8 | full |
| `__init__.py` | — (implicit only) | **thin — see Gap 1** |
| `__main__.py` | — (not specced) | **thin — see Gap 2** |

Non-`.py` shipped asset: `tui/cswap.tcss` — covered behaviorally by 09 §8.2
(the port uses lipgloss, so the exact CSS is not a hard contract; the
visually load-bearing facts are captured).

---

## Gaps found (with the missing spec content filled in)

### Gap 1 — Package version derivation (`__init__.py`) is unspecified

The *uses* of `__version__` are specced (08 §11 `--version`; 07 §1.1
`swapVersion`; 08 §13 update check), but its **derivation** is not. Source:

```python
# src/claude_swap/__init__.py
from importlib.metadata import version
__version__ = version("claude-swap")          # read from INSTALLED package metadata
from claude_swap.switcher import ClaudeAccountSwitcher
__all__ = ["ClaudeAccountSwitcher", "__version__"]
```

Missing spec content:

- `__version__` is **not hardcoded** — it is read at import time from the
  installed distribution's metadata (`importlib.metadata.version("claude-swap")`).
  The canonical source of the string is `pyproject.toml`'s
  `[project] version` (currently **`0.22.0b1`** — a PEP 440 beta).
- **Go port**: there is no installed-package-metadata concept. Embed the
  version at build time (linker `-ldflags "-X …=<version>"` or a generated
  `version.go`) and expose it wherever the spec references `__version__`:
  the `--version` command (`"<prog> <version>"`, exit 0), the export
  envelope's `swapVersion`, and the update-check comparison.
- The package also re-exports `ClaudeAccountSwitcher` at the package root
  (`claude_swap.ClaudeAccountSwitcher`) — a public API surface, not port-critical.

### Gap 2 — `python -m claude_swap` entry point (`__main__.py`) is unspecified

Neither spec mentions the module-execution entry point. Source:

```python
# src/claude_swap/__main__.py
from claude_swap.cli import main
if __name__ == "__main__":
    main()
```

Missing spec content: in addition to the two console scripts
`claude-swap` and `cswap` (both → `claude_swap.cli:main`, specced in 08),
the tool is runnable as `python -m claude_swap`, which dispatches to the
same `main()`. Note this interacts with `_prog_name()` (08 §1): when invoked
this way `sys.argv[0]` basename is `__main__`, which `_prog_name()` maps to
the literal `"cswap"`. A Go port has no `-m` equivalent; only the two binary
names matter, but the `__main__`→`"cswap"` fallback in `_prog_name()` is
already covered by 08 §1 and needs no change.

### Gap 3 (minor) — `_is_running_in_container` mountinfo substrings not enumerated

Specs 08 §15 and 02 §18 list the four probes
(`CONTAINER`/`container` env, `/.dockerenv`, `/proc/1/cgroup`,
`/proc/self/mountinfo`) but only give the **cgroup** substring set. The two
probes use *different* substring sets. Full source behavior
(`switcher.py:_is_running_in_container`):

- env `CONTAINER` or `container` truthy → True.
- `platform == WINDOWS` → False (skips the file probes).
- `/.dockerenv` exists → True.
- `/proc/1/cgroup` contains any of **`docker`, `lxc`, `containerd`,
  `kubepods`** → True (`PermissionError` swallowed).
- `/proc/self/mountinfo` contains any of **`docker`, `overlay`** → True
  (`PermissionError` swallowed). ← the `docker`/`overlay` set is the missing
  detail.
- else False.

A Go port replicating the root-guard bypass must use the `docker`/`overlay`
substrings for mountinfo, not reuse the cgroup set.

---

## Cross-cutting concerns audit

**Entry points & version** — Console scripts `claude-swap`/`cswap` →
`cli:main` (08, correct). `python -m claude_swap` and version derivation:
Gaps 1–2 above. `pyproject.toml` `requires-python = ">=3.12"`; the 3.12/3.13
`Path.exists()` divergence is already called out in 03 §5.7/§9.3, but the
3.12 floor itself is only implicit — worth stating as the minimum-behavior
baseline for the port's file-probe semantics.

**Signal handling** — The **only** signal handler in the entire codebase is
`signal.signal(signal.SIGTERM, lambda *_: engine.stop())` in `cli.py`
(auto-loop mode). Fully covered by 05 §15/§19/§22 and 08 §7.7. There is **no
SIGINT handler** anywhere — `Ctrl-C` is handled purely as a caught
`KeyboardInterrupt` (→ exit 130, per 08 §11) in `cli.py`, `session.py`
(Windows `_exec` path, 06 §1.8), and `switcher.py` (add/overwrite prompts,
01 §5.2). No gap; noting for the port so it does not invent a SIGINT trap —
the Go equivalent should let interrupt propagate as an error/exit-130 path,
and install a SIGTERM→stop() handler only around the auto loop.

**Environment variables** — grep of `os.environ`/`getenv` yields 9 distinct
vars, **all specced**: `USER` (03 §4.2), `WSL_DISTRO_NAME` (03 §1 / 04 §5.1),
`CLAUDE_CONFIG_DIR` (03 §2), `XDG_DATA_HOME` (03 §2.7), `NO_COLOR` /
`FORCE_COLOR` / `TERM` (08 §10.2), the 5-tuple `AUTH_OVERRIDE_ENV_VARS`
(`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`,
`CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`, `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`
— 06 §1.3), `CONTAINER`/`container` (Gap 3 / 08 §15), and `UV_TOOL_DIR` /
`PIPX_HOME` (08 §13.3). **No unmentioned env var.**

**Network endpoints** — grep of `https://` in code yields exactly 4 live
endpoints, **all specced**: `platform.claude.com/v1/oauth/token` (04 §1.1),
`api.anthropic.com/api/oauth/profile` (04 §1.12),
`api.anthropic.com/api/oauth/usage` (04 §1.15), `pypi.org/pypi/claude-swap/json`
(08 §13.1). The GitHub URLs in `pyproject.toml` are metadata, not code.
**No unmentioned endpoint.**

**pyproject.toml / workflows** — Nothing here changes a port contract that
isn't already captured, with these notes:
- Runtime deps: `keyring>=25` (legacy backup backend — migrations/purge only,
  07 §5 / 01 §11), `textual>=8.2.8,<9` (TUI, 09), `truststore>=0.10.4`
  (`_use_native_tls`, 08 §1/§15). Optional extra `menubar = rumps>=0.4.0`
  (09 §10). All accounted for.
- `.github/workflows/ci.yml`: tests run on **ubuntu + windows + macos**,
  Python **3.12**, via `uv sync` + `pytest`; a dedicated macOS job runs the
  Keychain **contract + real `security` wrapper** tests (faulthandler at 60s,
  step timeout 5m). This confirms the macOS-Keychain interop in 03 §4/§4.8 is
  CI-enforced against the real `/usr/bin/security` — the port's Keychain
  behavior must stay byte-compatible with Claude-Code-seeded items, not just
  self-consistent.
- `.github/workflows/publish.yml`: release-triggered PyPI publish via OIDC
  trusted publishing (`id-token: write`). Python-packaging-specific; the Go
  port's release/update path is a fresh design (already flagged in 08 §15's
  "GitHub-release equivalent" note). No spec change needed, but the port must
  **not** carry over PyPI/uv/pipx assumptions in its own updater.

---

## Spot-check results

Nine load-bearing constants/strings checked against source; **all match the
specs exactly** (0 contradictions).

1. **Lock timings (`locking.py`)** — spec 03 §7.1 claims `FileLock(timeout=10.0)`,
   0.1s poll, `time.monotonic` for elapsed. Source: `def __init__(..., timeout: float = 10.0)`,
   `time.sleep(0.1)`, `time.monotonic() - start > timeout`. ✅
2. **proper-lockfile timings (`claude_locks.py`)** — spec 03 §6.2 claims
   `STALENESS_S=10.0`, `TOUCH_INTERVAL_S=3.0`, `DEFAULT_TIMEOUT_S=9.0`.
   Source lines 42/43/47 verbatim. ✅
3. **Keychain constants (`macos_keychain.py`)** — spec 03 §4.1 claims
   `SECURITY_STDIN_LINE_LIMIT = 4096-64 (=4032)`, `_NOT_FOUND_RC = 44`,
   `_TIMEOUT = 5.0`, `_SECURITY = "/usr/bin/security"`. Source lines
   45/47/54/60 verbatim; `_TIMEOUT` used in all three subprocess calls, rc-44
   handled in find/delete. ✅
4. **Schema versions** — spec claims usage cache `schemaVersion: 2`
   (02 §18 / 04 §2.1), and `SCHEMA_VERSION = 1` for switcher/JSON payloads,
   mappings, session manifests (02 §10 / 06 §5.1 / 08 §9). Source:
   `usage_store.py: SCHEMA_VERSION = 2`; `mappings.py: SCHEMA_VERSION = 1`;
   `switcher.py` imports and emits `SCHEMA_VERSION` (=1) in all payloads. ✅
5. **poll-policy cadence (`poll_policy.py`)** — spec 04 §3.2 claims
   `SERVE_TTL_S=180`, `MIN_INTERVAL_S=180`, `URGENT_INTERVAL_S=60`,
   `EDGE_BACKOFF_S=300`, `POST_429_MIN_INTERVAL_S=360`,
   `ESCALATION_MARGIN_PCT=15`, `RESET_SLACK_S=60`. Source lines
   44/48/57/77/82/88/92 verbatim. ✅
6. **OAuth constants (`oauth.py`)** — spec 04 §1.1 / 05 §21 claim
   `OAUTH_BETA_HEADER="oauth-2025-04-20"`, `OAUTH_EXPIRY_BUFFER_MS=5*60*1000`,
   `OAUTH_CLIENT_ID="9d1c250a-e61b-44d9-88ed-5944d1962f5e"`, and
   `SETUP_TOKEN_SCOPES=("user:inference",)`. Source: oauth.py 16/17/19 and
   switcher.py:96 verbatim. ✅
7. **AUTH_OVERRIDE_ENV_VARS (`session.py`)** — spec 06 §1.3 lists the exact
   5-tuple. Source lines 124–130 match in order and spelling. ✅
8. **Update check (`update_check.py`)** — spec 08 §13.1/§13.2 claim
   `CACHE_TTL = 24*3600`, `PYPI_URL="https://pypi.org/pypi/claude-swap/json"`,
   `_parse_version(v) = tuple(int(x) for x in v.split("."))`. Source lines
   15/16/19–20 verbatim. ✅
9. **Container detection paths (`switcher.py`)** — spec 08 §15 lists env +
   `/.dockerenv` + `/proc/1/cgroup` + `/proc/self/mountinfo`; source matches
   (cgroup set `docker|lxc|containerd|kubepods`), with the **mountinfo
   substring set `docker|overlay` under-documented** — see Gap 3. Paths ✅,
   one substring detail incomplete.

---

## Corrections to existing specs

No **contradictions** were found — the specs are unusually faithful to the
source. The items below are incompletenesses / clarifications rather than
errors:

1. **08 §15 & 02 §18 (`_is_running_in_container`)** — add the
   `/proc/self/mountinfo` substring set: it matches **`docker`** or
   **`overlay`**, which is *different* from the `/proc/1/cgroup` set
   (`docker`/`lxc`/`containerd`/`kubepods`). A port that reuses the cgroup
   set for mountinfo would subtly mis-detect. (Detail, not a contradiction.)

2. **08 §13.2 (`_parse_version`) — version-scheme fragility** — the spec
   correctly transcribes `tuple(int(x) for x in v.split("."))`, but should
   note the consequence for the **current package version `0.22.0b1`**
   (PEP 440 beta): `int("0b1")` raises `ValueError`, which
   `check_for_update`'s blanket `except → None` swallows, so **any
   pre-release build silently never shows an update notice**. The spec's
   "any exception returns None" (08 §13.2) already covers the mechanism, but
   the interaction with the tool's own beta versioning is worth an explicit
   line. A Go port using a real semver comparator will *not* reproduce this
   quirk — flag it as an intentional behavior difference rather than a bug to
   replicate. (The `swapVersion: "1.2.3"` in 07 §1.1 is illustrative and
   fine; the real default is the installed `__version__`, e.g. `0.22.0b1`.)

3. **`requires-python = ">=3.12"` (pyproject)** — the Python 3.12 floor is
   only implicit in the specs (surfacing via the 3.12-vs-3.13 `Path.exists()`
   note in 03 §5.7/§9.3). Worth stating once as the baseline the file-probe
   error-normalization behavior is written against, so a Go port knows the
   "treat unsearchable-dir stat error as missing" rule (03 §9.3) is the
   intended cross-version-normalized behavior, not a 3.12-only artifact.

4. **`__init__.py` / `__main__.py`** — see Gaps 1–2; fold the version-derivation
   note and the `python -m claude_swap` entry point into the CLI/infra spec
   (08) so the port has a single home for "how the binary is named and
   versioned."
