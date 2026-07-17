# claude-swap — CLI Surface & Infrastructure Spec

## Overview

`claude-swap` (binary `cswap`, also `claude-swap`) is a Python CLI that switches
between multiple Claude Code login accounts. This spec covers the **command
grammar** (every subcommand and flag, legacy `--flag` spellings and their
aliasing, argument-form resolution, and help text), plus the **infrastructure
modules**: `settings.py` (`settings.json` config store, `cswap config`),
`json_output.py` (the `--json` schema-v1 envelope), `printer.py` (human/color
output), `exceptions.py` (the exception→exit-code table), `logging_config.py`
(log destinations), and `update_check.py` (PyPI update check + self-upgrade).
The CLI has an unusual two-layer dispatcher: a handful of subcommands are
**pre-dispatched** by inspecting the first argv token before argparse is built,
and the rest are **rewritten** from memorable verbs (`list`, `switch`, …) into a
hidden legacy `--flag` interface that argparse actually parses. The Go port must
reproduce both layers, all the exact validation strings, and the exact JSON
shapes; anything omitted becomes a port bug.

Files covered:
`src/claude_swap/cli.py`, `settings.py`, `json_output.py`, `printer.py`,
`exceptions.py`, `logging_config.py`, `update_check.py` (and the `cache.py` /
`paths.py` constants they depend on).

---

## 1. Entry point & dispatch order (`cli.py::main`)

`main()` runs, in this exact order:

1. `force_utf8_output()` — reconfigure stdout/stderr to UTF-8 (see §7).
2. `_use_native_tls()` — best-effort `truststore.inject_into_ssl()` (see §11 Go notes).
3. `argv = sys.argv[1:]`.
4. **Pre-dispatch** on the first token (each returns immediately after handling):
   - `run`   → `_run_command(argv[1:])`
   - `auto`  → `_auto_command(argv[1:])`
   - `config`→ `_config_command(sys.argv[2:])`  *(note: gated on `sys.argv[1] == "config"`, uses `sys.argv[2:]` — behaviorally identical to `argv[1:]`)*
   - `map`   → `_map_command(argv[1:])`
   - `unmap` → `_unmap_command(argv[1:])`
   - `alias` → `_alias_command(argv[1:])`
   - `swap`  → `_swap_command(argv[1:])`
   - `move`  → `_move_command(argv[1:])`
5. **Bare-`cswap` TUI gate**: if `argv` is empty AND `sys.stdout.isatty()` AND
   `sys.stdin.isatty()`, set `argv = ["--tui"]`. (Non-TTY bare invocation falls
   through to the "no command" error — scripts/pipes keep getting the usage error.)
6. `argv = _translate_subcommand(argv)` — rewrite memorable verbs → legacy flags (§2).
7. Build the main argparse parser (§3), parse, run cross-flag validation (§4),
   dispatch (§5), serialize any JSON payload, then run the passive update check (§6).

The pre-dispatched subcommands (`run`, `auto`, `config`, `map`, `unmap`, `alias`,
`swap`, `move`) **must be the first argument** — e.g. `cswap --debug run 2` is
NOT supported; use `cswap run 2 --debug`. Each of these builds its own
`argparse.ArgumentParser` and accepts its own `--debug`.

### `_prog_name()`

Computes the program name shown in usage/help:
```
name = basename(sys.argv[0] or "")
strip a trailing ".exe" / ".pyw" / ".py" (case-insensitive, first match)
if not name or name in {"__main__","python","python3","py"} → return "cswap"
else return name
```
Go equivalent: use the invoked binary basename, stripping `.exe`; fall back to `"cswap"`.

---

## 2. Memorable-subcommand translation (`_translate_subcommand`)

Rewrites `argv` (args after the program name) so bare verbs expand to the legacy
`--flag` interface. Only fires when the **first** token is a recognized verb
(verbs never start with `-`); tokens after the verb pass through verbatim so
`--json`, `--strategy`, `--slot`, `--force`, etc. keep combining.

Rules:
- Empty argv → returned unchanged.
- `switch`:
  - `switch <X>` where `X` does **not** start with `-` → `["--switch-to", X, ...]`
  - `switch` alone, or `switch -<flag> …` (next token starts with `-`) → `["--switch", ...]`
- Any verb found in `_SUBCOMMAND_FLAGS` → `[flag, *rest]`.
- Otherwise argv returned unchanged (argparse later rejects unknown tokens).

`_SUBCOMMAND_FLAGS` (verb → flag):

| verb | flag | | verb | flag |
|------|------|-|------|------|
| `help` | `--help` | | `disable` | `--disable-account` |
| `list` | `--list` | | `enable` | `--enable-account` |
| `ls` | `--list` | | `export` | `--export` |
| `status` | `--status` | | `import` | `--import` |
| `add` | `--add-account` | | `purge` | `--purge` |
| `add-token` | `--add-token` | | `upgrade` | `--upgrade` |
| `remove` | `--remove-account` | | `update` | `--upgrade` |
| `rm` | `--remove-account` | | `tui` | `--tui` |
| | | | `watch` | `--watch` |
| | | | `menubar` | `--menubar` |

`run`, `auto`, `config`, `map`, `unmap`, `alias`, `swap`, `move` are NOT in this
table — they were already pre-dispatched (§1) and never reach translation.

---

## 3. Main parser — full flag grammar

`prog = _prog_name()`, `usage = "%(prog)s <command> [args] [options]"`,
`formatter_class = argparse.RawDescriptionHelpFormatter`. The description is the
canonical command list (rendered in `--help`); reproduce it verbatim:

```
Multi-Account Switcher for Claude Code

Commands:
  cswap help                       show this help
  cswap list                       list managed accounts
  cswap status                     show current account
  cswap switch                     rotate to the next account
  cswap switch <num|email>         switch to a specific account
  cswap add                        add the current account
  cswap add-token [TOKEN|-]        register a setup-token or API key
  cswap remove <num|email>         remove an account
  cswap disable <num|email>        hold an account out of auto-rotation
  cswap enable <num|email>         return a disabled account to rotation
  cswap run <num|email> [-- ...]   run as an account, this terminal only
  cswap run                        run the current dir's mapped account
  cswap map <num|email> [path]     map a directory to an account
  cswap map                        list directory mappings
  cswap unmap [path]               remove a directory mapping
  cswap alias <num|email> <name>   set a short alias for an account
  cswap alias <num|email> --unset  remove an account's alias
  cswap alias                      list all aliases
  cswap swap <a> <b>               exchange two accounts' slot numbers
  cswap move <a> <slot>            assign an account to a slot (swaps if taken)
  cswap auto                       auto-switch when nearing rate limits
  cswap config [set KEY VALUE]     show or change settings (settings.json)
  cswap export <path>              export accounts
  cswap import <path>              import accounts
  cswap tui                        interactive dashboard (also: bare cswap)
  cswap watch                      dashboard, opened on the live watch page
  cswap menubar                    macOS menu bar app
  cswap upgrade                    self-upgrade to latest
  cswap purge                      remove all claude-swap data

Aliases: ls=list  rm=remove  update=upgrade
```
(Each `cswap` above is literally `%(prog)s`.) The epilog is:
```
Flags combine with subcommands:
  cswap switch --strategy best           # pick the account with most quota left
  cswap switch --strategy next-available # rotate, skipping rate-limited accounts
  cswap switch user@example.com
  cswap list --json
  cswap add --slot 3                      # add to a specific slot
  cswap add-token sk-ant-oat01-... --email me@example.com
  cswap run 2 -- --resume                 # forward args after '--' to claude
  cswap auto --once                       # single auto-switch tick (cron-friendly)
  cswap config set autoswitch.threshold 80

The original flag spellings (cswap --switch, cswap --list, ...) keep working.
```

### 3.1 Visible flags (outside the mutually-exclusive group)

| flag | type / action | metavar | default | notes |
|------|---------------|---------|---------|-------|
| `--version` | `action="version"` | — | — | prints `%(prog)s {__version__}` then exits 0 |
| `--debug` | store_true | — | False | enable debug logging |
| `--token-status` | store_true | — | False | only valid with `list` |
| `--json` | store_true | — | False | only with `list`/`status`/`switch`/`switch-to` |
| `--strategy` | choice | `{best,next-available}` | None | only with bare `switch` |
| `--model` | str | `NAMES` | None | only with `switch --strategy`; comma-list or `all` |
| `--slot` | int | `NUM` | None | only with `add`/`add-token` |
| `--email` | str | `EMAIL` | None | only with `add-token` |
| `--account` | str | `NUM\|EMAIL` | None | only with `export` |
| `--alias` | str | `NAME` | None | only with `add` |
| `--force` | store_true | — | False | only with `import` or `switch <num\|email>` |
| `--full` | store_true | — | False | only with `export` |

### 3.2 Legacy `--flag` interface (mutually-exclusive group, all `help=SUPPRESS`)

The group is `required=False`. All members are hidden from `--help` (the "no
command" case is handled explicitly — a required group with every member
suppressed would print a broken empty-list error).

| flag | action / dest | metavar |
|------|---------------|---------|
| `--add-account` | store_true | — |
| `--remove-account` | value | `NUM\|EMAIL` |
| `--disable-account` | value | `NUM\|EMAIL` |
| `--enable-account` | value | `NUM\|EMAIL` |
| `--list` | store_true | — |
| `--switch` | store_true | — |
| `--switch-to` | value | `NUM\|EMAIL` |
| `--status` | store_true | — |
| `--purge` | store_true | — |
| `--export` | value | `PATH` |
| `--import` | value (`dest=import_`) | `PATH` |
| `--tui` | store_true | — |
| `--watch` | store_true | — |
| `--menubar` | store_true | — |
| `--upgrade` | store_true | — |
| `--add-token` | value, `nargs="?"`, `const=""` | `TOKEN\|-` |

`--add-token` with no value stores `""` (empty string, distinct from `None`). All
downstream checks use `args.add_token is not None` to mean "add-token selected".

Two members combined → argparse's own error `argument X: not allowed with
argument Y` (exit 2). `--export /p --import /q` → "not allowed".

---

## 4. Cross-flag validation (all exit code 2 via `parser.error`)

Checked in this order after parse:

1. **No command selected** (none of add_account/list/switch/status/purge/tui/
   watch/menubar/upgrade set, and remove_account/disable_account/enable_account/
   switch_to/export/import_/add_token all `None`):
   `parser.error("no command given — try 'cswap help'")`
   *(the `'cswap'` is `_prog_name()` substituted). Must NOT leak legacy flag
   names or argparse's "one of the arguments … is required".*
2. `--token-status` without `--list` → `--token-status can only be used with 'list'`
3. `--json` without (list|status|switch|switch_to) → `--json can only be used with 'list', 'status', or 'switch'`
4. `--json` with `--token-status` → `--token-status cannot be combined with --json`
5. `--strategy` without `--switch` → `--strategy can only be used with bare 'switch'`
6. `--model` with `strategy is None` → `--model can only be used with 'switch --strategy best' or 'switch --strategy next-available'`
7. `--slot` without (add_account|add_token) → `--slot can only be used with 'add' or 'add-token'`
8. `--email` without add_token → `--email can only be used with 'add-token'`
9. `--account` without export → `--account can only be used with 'export'`
10. `--alias` without add_account → `--alias can only be used with 'add'`
11. `--force` without (import_|switch_to) → `--force can only be used with 'import' or 'switch <num|email>'`
12. `--full` without export → `--full can only be used with 'export'`

Note: `--json` is accepted with `switch_to` even though the message names only
'switch'. Bare `--json` alone hits check #1 first (no command). `--purge --json`
hits check #3.

---

## 5. Dispatch (main parser path)

**`--upgrade` runs first, before the switcher is constructed** (so upgrading the
tool never touches config/keychain): `sys.exit(run_self_upgrade())`; on
`KeyboardInterrupt` print `"\nUpgrade cancelled"` (dimmed) and `sys.exit(130)`.
`test_upgrade_dispatches_without_constructing_switcher` asserts the switcher
class is never instantiated.

Otherwise: `switcher = ClaudeAccountSwitcher(debug=args.debug)`, then the **root
guard** (POSIX only): if `os.geteuid() == 0` and NOT
`switcher._is_running_in_container()` →
`error("Error: Do not run this script as root (unless running in a container)")`
and `sys.exit(1)`. Then dispatch:

| selected | call |
|----------|------|
| `--add-account` | `switcher.add_account(slot=args.slot, alias=args.alias)` |
| `--add-token` (not None) | `switcher.add_account_from_token(token=args.add_token, email=args.email, slot=args.slot)` |
| `--remove-account` | `switcher.remove_account(args.remove_account)` |
| `--disable-account` (not None) | `switcher.set_account_disabled(x, True)` |
| `--enable-account` (not None) | `switcher.set_account_disabled(x, False)` |
| `--list` | `payload = switcher.list_accounts(show_token_status=args.token_status, json_output=args.json)` |
| `--switch` | model resolution (below), then `payload = switcher.switch(strategy=…, json_output=…, models=…, model_source=…)` |
| `--switch-to` | `payload = switcher.switch_to(x, json_output=args.json, force=args.force)` |
| `--status` | `payload = switcher.status(json_output=args.json)` |
| `--purge` | `switcher.purge()` |
| `--export` | `transfer.export_accounts(switcher, path, account=args.account, full=args.full)` |
| `--import` | `transfer.import_accounts(switcher, path, force=args.force)` |
| `--tui` | `sys.exit(tui.run(switcher))` |
| `--watch` | `sys.exit(tui.run(switcher, start="watch"))` |
| `--menubar` | macOS-only (below) |

**Model resolution for `--switch`:**
```
if args.strategy is None:            models, model_source = (), None
elif args.model is not None:         models, model_source = parse_model_names(args.model), "cli"
else:                                 models = parse_model_names(load_settings(backup_dir).model)
                                      model_source = "autoswitch.model" if models else None
payload = switcher.switch(strategy=args.strategy, json_output=args.json,
                          models=models, model_source=model_source)
if payload is not None and models:
    payload["models"] = list(models)
    payload["modelSource"] = model_source
```

**`--menubar`:** if `sys.platform != "darwin"` →
`error("The menu bar is only available on macOS.")`, exit 1. Else
`try: from claude_swap.menubar import run` — on `ImportError`:
`error("Menu bar mode requires 'rumps'. Install with: pip install 'claude-swap[menubar]'")`,
exit 1. Else `sys.exit(menubar_run(switcher))`.

**Error handling around dispatch:**
- `except ClaudeSwitchError as e`:
  - JSON mode: `print(json.dumps(error_envelope(e), indent=2))` to **stdout**, exit 1.
  - else: `error(f"Error: {e}")` to **stderr**, exit 1.
- `except KeyboardInterrupt`: `print("\nOperation cancelled"` (dimmed) to
  **stderr if json else stdout**, `sys.exit(130)`.

**JSON serialization is centralized:** no dispatch branch prints JSON itself.
After the try/except: `if args.json and payload is not None: print(json.dumps(payload, indent=2))`.
So exactly one JSON object reaches stdout, no extra text.

---

## 6. Passive update notification

After successful dispatch, and only if `not args.purge and not args.upgrade and
not args.json`:
```
from claude_swap.update_check import check_for_update
msg = check_for_update(__version__)
if msg:
    print(f"\n{muted(msg)}", file=sys.stderr)
```
Skipped after `--purge` (would recreate `<backup>/cache/update_check.json` inside
the just-deleted dir), after `--upgrade` (safety), and in JSON mode (keeps stdout
pure). Prints to **stderr**, muted.

---

## 7. Pre-dispatched subcommands (detailed)

Each: builds a dedicated parser, constructs
`ClaudeAccountSwitcher(debug=args.debug)`, calls `_guard_root(switcher)`, does its
work, and wraps in `except ClaudeSwitchError → error("Error: …"); exit 1` /
`except KeyboardInterrupt → print("\nOperation cancelled" dimmed); exit 130`
(unless noted otherwise).

`_guard_root(switcher)`: POSIX only — if `os.geteuid() == 0` and not
`switcher._is_running_in_container()` →
`error("Error: Do not run this script as root (unless running in a container)")`,
`sys.exit(1)`.

### 7.1 `cswap run [NUM|EMAIL] [--no-share] [--share-history/--no-share-history] [--debug] [-- <claude args>]`

- `prog = f"{_prog_name()} run"`. `RawDescriptionHelpFormatter`.
- description: `"[EXPERIMENTAL] Launch Claude Code as a stored account in this terminal only (the default login and other terminals are unaffected)."`
- **`--` split**: everything after the *first* `--` is `tail`, forwarded to claude
  verbatim and never parsed (`cswap run 2 -- --no-share` forwards `["--no-share"]`).
- args:
  - `account` (`nargs="?"`, `metavar="NUM|EMAIL"`) — omit to use the cwd's mapping.
  - `--no-share` (store_true) — don't share settings/keybindings/CLAUDE.md/skills/commands/agents.
  - `--share-history` (`argparse.BooleanOptionalAction`, default `False`) — also gives `--no-share-history`.
  - `--debug` (store_true).
- Dispatch: `manager = session.SessionManager(switcher)`.
  - If `account is not None`: `manager.run(account, tail, share=not no_share, share_history=share_history)`, return.
  - Else resolve mapping: `slot, email = switcher.slot_for_directory(os.getcwd())`.
    - `slot is not None` → `manager.run(slot, tail, share=…, share_history=…)`, return.
    - `email is not None` (mapped account removed) → `warning(f"Mapped account {email} no longer exists — launching the default account.")`
    - else → `print(dimmed(f"No account mapped for {os.getcwd()} — launching the default account."))`
    - then `manager.exec_default(tail)`.
- On POSIX `manager.run`/`exec_default` exec into claude and never return; on
  Windows they exit with claude's return code. The post-dispatch update check is
  thus unreachable (intended). Test doubles mock exec so `run()` returns.
- Epilog examples: `cswap run 2`, `cswap run user@example.com`, `cswap run 2 --no-share`, `cswap run 2 --share-history`, `cswap run 2 -- --resume`.
- `SessionError("boom")` → stderr contains "boom", exit 1.

### 7.2 `cswap map [NUM|EMAIL] [PATH]`

- `prog = "cswap map"` (hardcoded). args: `account` (`nargs="?"`), `path`
  (`nargs="?"`, default cwd), `--debug`.
- No `account` → `switcher.list_mappings()`, return. (Human strings originate in
  switcher: empty → `"No directory mappings yet"`; populated → `"Directory
  mappings"` header + `"<slot>:"` + email; a mapping whose account is gone shows
  `"account removed"`.)
- With account: `store = MappingStore(switcher.backup_dir)`;
  `account_num, email, org_uuid = switcher.resolve_account(account)`;
  `target = path or os.getcwd()`.
  - if `not os.path.isdir(target)` → `warning(f"Warning: {target} is not an existing directory (mapping it anyway)")` (to **stdout**).
  - `previous = store.get(target)`; `store.set(target, email, org_uuid)`; `shown = normalize_path(target)`.
  - if `previous` existed and its `email` differs →
    `print(f"{accent('Mapped')} {shown} → Account-{account_num} ({email}) {muted(f'(was {prev_email})')}")`
  - else → `print(f"{accent('Mapped')} {shown} → Account-{account_num} ({email})")`
- Unknown account (`resolve_account` raises) → exit 1, "Error" on stderr.

### 7.3 `cswap unmap [PATH]`

- `prog = "cswap unmap"`. args: `path` (`nargs="?"`, default cwd), `--debug`.
- `store = MappingStore(switcher.backup_dir)`; `target = path or cwd`; `shown = normalize_path(target)`.
  - `store.remove(target)` truthy → `print(f"{accent('Unmapped')} {shown}")`
  - else → `print(dimmed(f"No mapping for {shown}"))`

### 7.4 `cswap alias [NUM|EMAIL] [NAME] [--unset]`

- `prog = "cswap alias"`. args: `account` (`nargs="?"`), `alias_name`
  (`nargs="?"`, `metavar="NAME"`), `--unset` (store_true), `--debug`.
- Argument validation (via `parser.error`, exit 2):
  - `--unset` with a NAME → `--unset does not take a NAME argument`
  - `--unset` with `account is None` → `NUM|EMAIL is required with --unset`
  - account given, not `--unset`, no NAME → `NAME is required (or pass --unset to remove the alias)`
- No account → `rows = switcher.list_aliases()`; empty → `print(dimmed("No aliases set"))`;
  else `print(bolded("Aliases:"))` then per row `print(f"  {num}: {alias_name} {muted(f'({email})')}")`.
- `--unset` → `account_num = switcher.unset_alias(account)`; `print(f"{accent('Removed alias')} for Account {account_num}")`.
- else → `account_num, normalized = switcher.set_alias(account, alias_name)`; `print(f"{accent('Set alias')} '{normalized}' for Account {account_num}")`.
- NAME validity (letters, digits, `.`, `-`, `_`; **not purely numeric**) is
  enforced in `switcher.set_alias`; e.g. `alias 2 123` raises → exit 1.

### 7.5 `cswap swap NUM|EMAIL|ALIAS NUM|EMAIL|ALIAS`

- `prog = f"{_prog_name()} swap"`. Positionals `first`, `second` (both required,
  `metavar="NUM|EMAIL|ALIAS"`), `--debug`.
- `num_a, num_b = switcher.swap_accounts(first, second)`;
  `print(f"{accent('Swapped')} Account {num_a} and Account {num_b}:")`; then for
  each num in `sorted((num_a,num_b), key=int)` print `f"  {num}: {email}"`.

### 7.6 `cswap move NUM|EMAIL|ALIAS SLOT`

- `prog = f"{_prog_name()} move"`. Positionals `account`, `slot` (required),
  `--debug`.
- `num_src, num_target, swapped = switcher.move_account(account, slot)`.
  - `num_src == num_target` → `print(f"{dimmed('Already in')} slot {num_target}: {email}")`
  - `swapped` → `print(f"{accent('Swapped')} Account {num_src} and Account {num_target}:")` + numbered list
  - else → `print(f"{accent('Moved')} {email} to slot {num_target}")`

### 7.7 `cswap auto` (CLI surface; engine is a separate module)

- `prog = "cswap auto"`. Flags:
  - `--once` (store_true) — single tick; exit code = outcome.
  - `--json` (store_true) — one JSON event per line on stdout.
  - `--interval` (`type=float`, `metavar="SECONDS"`) — poll interval (min 15; default 60).
  - `--threshold` (`type=float`, `metavar="PCT"`) — 50–99.9; default 90.
  - `--cooldown` (`type=float`, `metavar="SECONDS"`) — default 300.
  - `--model` (`metavar="NAMES"`) — one name or comma list (Fable, Opus, Sonnet, Haiku) or `all`.
  - `--include-api-key-accounts` (`argparse.BooleanOptionalAction`, `default=None`).
  - `--dry-run` (store_true).
  - `--debug` (store_true).
- `settings = merged_with_cli(load_settings(switcher.backup_dir), args)` (CLI flags override settings.json).
- `engine = AutoSwitchEngine(switcher, settings, jsonl_emit if args.json else human_emit, dry_run=args.dry_run)`.
- `--once`: `sys.exit(engine.tick().value)`.
- loop mode: `signal.signal(signal.SIGTERM, lambda *_: engine.stop())`; if not
  JSON print the dimmed banner
  `f"Auto-switch running: threshold {settings.threshold:.0f}%, every {settings.interval_seconds:.0f}s{' (dry-run)' if args.dry_run else ''} — Ctrl-C to stop"`;
  `sys.exit(engine.run_loop())`.
- Root guard here is inlined (not `_guard_root`) but identical.
- `jsonl_emit(event)`: `print(json.dumps(event.to_json()), flush=True)`.
- `human_emit(event)`: `stamp = time.strftime("%H:%M:%S")`; `line = event.human()`;
  color by kind — `switch`→accent; `error`/`account-quarantined`→yellowed;
  `poll`/`no-switch`/`sleep`→dimmed; `print(f"{stamp}  {line}", flush=True)`.
- **Exit codes with `--once`** (from `TickOutcome.value`): `0` switched · `1`
  error · `2` no action needed · `3` blocked (no viable target / all exhausted).
- `except ClaudeSwitchError`: JSON → `print(json.dumps(error_envelope(e)))`
  (**compact, no indent**); else `error(f"Error: {e}")`; exit 1.
- `except KeyboardInterrupt`: `print("\nAuto-switch stopped"` dimmed,
  `file=sys.stderr if args.json else sys.stdout)`; exit 130.
- Epilog documents the exit codes and examples verbatim.

### 7.8 `cswap config [list | get KEY | set KEY VALUE | unset KEY | path]`

- `prog = "cswap config"`. Top-level flags `--json`, `--debug`. Subparsers
  `dest="action"`, `metavar="{list,get,set,unset,path}"`:
  - `list` (default when no action) — also re-adds `--json` (`default=argparse.SUPPRESS`).
  - `get KEY` — `KEY` metavar `KEY`; also re-adds `--json` (SUPPRESS).
  - `set KEY VALUE`.
  - `unset KEY`.
  - `path`.
- `--json` on `list`/`get` uses `default=argparse.SUPPRESS` so a pre-verb
  `cswap config --json get X` isn't clobbered by the subparser's default. Both
  `config get X --json` and `config --json get X` work identically.
- `json_mode = bool(getattr(args, "json", False))`; `action = args.action or "list"`.
- If `json_mode and action not in ("list","get")` → `parser.error("--json can only be used with list or get")` (exit 2).
- Root guard (inlined, POSIX). `root = switcher.backup_dir`.
- `path` → `print(settings_path(root))`.
- `list`:
  - JSON payload:
    ```json
    {
      "schemaVersion": 1,
      "path": "<abs path to settings.json>",
      "settings": [ {"key": "autoswitch.threshold", "value": 90.0, "isSet": false}, ... ]
    }
    ```
    printed with `json.dumps(payload, indent=2)`.
  - Human: right-pad columns to the max key width and max formatted-value width;
    append `"  (default)"` (dimmed) to rows not present in the file.
- `get`:
  - `spec = setting_spec(args.key)` (unknown key raises `ConfigError`).
  - JSON: `{"schemaVersion": 1, "key": spec.dotted, "value": value, "isSet": is_set}` (indent 2).
  - Human: `print(format_setting_value(value))`.
- `set` → `value = set_setting(root, key, value)`; `print(f"{args.key} = {format_setting_value(value)}")`.
- `unset` → if `unset_setting(root, key)`:
  `print(f"{args.key} unset (default: {format_setting_value(default)})")`; else
  `print(muted(f"{args.key} is not set; nothing to do"), file=sys.stderr)` (exit 0).
- `except ClaudeSwitchError`: JSON → `print(json.dumps(error_envelope(e), indent=2))`; else `error`; exit 1.
- `except KeyboardInterrupt`: exit 130.
- Epilog dynamically lists every key: for each spec,
  `f"  {spec.dotted:<34}{spec.help} (default {format_setting_value(spec.default)})"`.
- `config frobnicate` (bad action) / `config set KEY` (missing VALUE) →
  argparse usage error, exit 2. `config --json set …` → exit 2.

---

## 8. `settings.py` — `settings.json` config store

### 8.1 Location & format

- File: `<backup_root>/settings.json` (`settings_path(root) = root / "settings.json"`).
- `SETTINGS_FILENAME = "settings.json"`, `SETTINGS_SCHEMA_VERSION = 1`.
- Backup root (from `paths.get_backup_root()`):
  - Linux/WSL: `$XDG_DATA_HOME/claude-swap` if `$XDG_DATA_HOME` is set and
    absolute (`~` is expanded); else `~/.local/share/claude-swap`.
  - macOS / Windows / unknown: `~/.claude-swap-backup` (legacy layout).
- Keys are **camelCase**; the in-memory dataclass fields are snake_case.
- Written atomically with 0600 file / 0700 dir modes (POSIX). Unknown keys and
  unknown top-level sections **survive a round trip**.
- Reading is **forgiving** (missing/corrupt/wrong-type → defaults, logged
  warning, never a crash). Writing via `cswap config set` is **strict**.

### 8.2 `AutoSwitchSettings` dataclass (frozen) & the setting registry

Every key (single source of truth is `SETTING_SPECS`, keyed by dotted key):

| dotted key | field (snake) | kind | lo | hi | choices | default | help |
|-----------|---------------|------|----|----|---------|---------|------|
| `autoswitch.threshold` | `threshold` | float | 50.0 | 99.9 | — | `90.0` | Switch when the binding 5h/7d window reaches this pct |
| `autoswitch.intervalSeconds` | `interval_seconds` | float | 15.0 | 3600.0 | — | `60.0` | Poll interval for the cswap auto loop, in seconds |
| `autoswitch.cooldownSeconds` | `cooldown_seconds` | float | 0.0 | 86400.0 | — | `300.0` | Minimum seconds between proactive switches |
| `autoswitch.hysteresisPct` | `hysteresis_pct` | float | 0.0 | 50.0 | — | `10.0` | A target must beat the active account by this many pct |
| `autoswitch.strategy` | `strategy` | choice | — | — | `("best",)` | `"best"` | How auto-switch picks the target account |
| `autoswitch.includeApiKeyAccounts` | `include_api_key_accounts` | bool | — | — | — | `False` | Allow rotating onto managed API-key accounts (bill per token) |
| `autoswitch.unhealthyTicks` | `unhealthy_ticks` | int | 1 | 100 | — | `3` | Consecutive failed polls before an account is unhealthy |
| `autoswitch.model` | `model` | string | — | — | — | `None` | Also switch on these models' weekly limits (e.g. Fable, Fable,Opus, or all) |

`SETTING_SPECS` must cover every dataclass field, and each spec's `default` must
equal the dataclass default (both enforced by tests). `spec.dotted =
f"{section}.{json_key}"`; `spec.default = getattr(AutoSwitchSettings(), field)`.

### 8.3 Reading

`_read_raw(path)`:
- `FileNotFoundError` → `{}` (silent).
- `OSError`/`json.JSONDecodeError`/`UnicodeDecodeError` → warn
  `"Could not read %s (%s); using defaults"`, return `{}`.
- Result not a dict → warn `"%s is not a JSON object; using defaults"`, return `{}`.

`load_settings(root)`: read raw; if `raw["autoswitch"]` is not a dict → all
defaults. Else copy present json_keys into kwargs, build
`AutoSwitchSettings(**kwargs)` (any `TypeError` → defaults), then `_clamped(...)`.

`_clamped(settings)`:
- float/int: value that is a `bool` or not `int|float` → the spec default;
  otherwise `float(min(max(value, lo), hi))`; int kind additionally `int(...)`.
- bool: `bool(value)`.
- string: a **non-empty `str`** kept as-is; anything else (null/number/empty) → default (`None`).
- choice: value not in `choices` → warn
  `"settings.json: unsupported %s %r; using %r"` and use default.

Clamp examples (tests): `threshold 200 → 99.9`; `intervalSeconds 1 → 15.0`;
`hysteresisPct -5 → 0.0`; `unhealthyTicks 0 → 1`; `threshold "high" → 90.0`
default; `includeApiKeyAccounts 1 → True`; `strategy "chaos" → "best"`;
`model 123 → None`.

### 8.4 Writing

`save_settings(root, settings)` — writes **every** known key + `schemaVersion`,
preserving unknown keys/sections. Used by non-config callers (TUI etc.), NOT by
`config set` (which would freeze current defaults into the file).

`set_setting(root, dotted_key, raw_value)` — for `config set`:
- `spec = setting_spec(dotted_key)` (unknown → ConfigError).
- `value = parse_setting_value(spec, raw_value)` (strict — see below).
- Read via `_read_raw_for_write` (corrupt file **errors**, does not degrade to `{}`).
- Stamp `schemaVersion` (only if absent), write **only that one key** into its
  section. Unknown keys/sections survive. Returns the parsed value.
- Result file for a single set is minimal, e.g. `{"schemaVersion": 1, "autoswitch": {"threshold": 80.0}}`.

`unset_setting(root, dotted_key)` → removes the key; if its section becomes
empty the whole section is deleted; stamps `schemaVersion`; returns `False`
(and does NOT write) when the key wasn't present.

`_read_raw_for_write(path)`:
- `FileNotFoundError` → `{}`.
- `OSError`/`UnicodeDecodeError` → `ConfigError(f"could not read {path}: {e}")`.
- `json.JSONDecodeError` → `ConfigError(f"{path} is not valid JSON ({e}); fix or delete it before changing settings")`.
- not a dict → `ConfigError(f"{path} is not a JSON object; fix or delete it before changing settings")`.
- A corrupt file is left byte-for-byte untouched on error.

`parse_setting_value(spec, raw_value)` (strict) — error messages:
- `setting_spec` unknown key: `f"unknown setting '{dotted_key}'\nValid keys: {comma-joined dotted keys}"`.
- bool via `_BOOL_WORDS = {true:1:yes → True, false:0:no → False}` (lowercased,
  stripped); miss → `f"{dotted} expects true or false (or 1/0, yes/no), got '{raw_value}'"`.
- choice not in choices → `f"{dotted} must be one of: {comma-joined choices}"`.
- string stripped empty → `f"{dotted} expects a non-empty value; use 'cswap config unset {dotted}' to clear it"`.
- int (`int(raw)`) / float (`float(raw)`) parse fail → `f"{dotted} expects an integer, got '{raw}'"` / `f"{dotted} expects a number, got '{raw}'"`.
- out of range → `f"{dotted} must be between {format_setting_value(lo)} and {format_setting_value(hi)}"` (e.g. `between 50 and 99.9`).

`format_setting_value(value)`: `None → "(none)"`; `bool → "true"/"false"`;
float that `is_integer()` → `str(int(value))` (so `90.0 → "90"`); else `str(value)`.

`effective_settings(root)` → list of `(spec, effective_value, is_set)` in registry
order. `is_set` == the json_key is **present in the raw file** — an explicit
value equal to the default still counts as set (presence, not value equality).

`merged_with_cli(settings, args)` — overlays non-`None` CLI overrides
(argparse Namespace attr → field): `threshold→threshold`,
`interval→interval_seconds`, `cooldown→cooldown_seconds`,
`include_api_key_accounts→include_api_key_accounts`, `model→model`. No overrides
→ returns the **same object** (identity). Else `_clamped(dataclasses.replace(...))`
(so CLI values are clamped too — `interval 1 → 15.0`).

`parse_model_names(value)` — comma-split, strip each; case-insensitively dedupe
(first spelling wins); returns a tuple. `"Opus, opus,Fable" → ("Opus","Fable")`;
`None`/`""` → `()`.

`atomic_write_json(path, data)`:
- `path.parent.mkdir(parents=True, exist_ok=True)`; POSIX `os.chmod(parent, 0o700)`.
- `tempfile.mkstemp(dir=parent, suffix=".tmp")`, write `json.dumps(data, indent=2)`
  UTF-8, `os.replace(tmp, path)`, POSIX `os.chmod(path, 0o600)`.
- On any exception the temp file is unlinked.

---

## 9. `json_output.py` — the `--json` schema-v1 envelope

`SCHEMA_VERSION = 1`. Field naming is **camelCase** (matching the export
envelope). The CLI does the single `json.dumps`; helpers only build dicts.

**stdout/stderr discipline** (guaranteed for completion and handled errors, NOT
for Ctrl-C): in JSON mode stdout carries exactly one JSON document; handled
`ClaudeSwitchError`s emit the error envelope on **stdout** (exit 1) with nothing
on stderr; the KeyboardInterrupt note is routed to stderr.

### 9.1 Usage sentinels & `usageStatus`

Internal sentinel strings (produced by the collectors) → `usageStatus`:

| sentinel constant | string value | `usage_fields` → (status, usage) |
|---|---|---|
| `USAGE_NO_CREDENTIALS` | `"no credentials"` | `("no_credentials", None)` |
| `USAGE_TOKEN_EXPIRED` | `"token expired"` | `("token_expired", None)` |
| `USAGE_API_KEY` | `"api key"` | `("api_key", None)` |
| `USAGE_KEYCHAIN_UNAVAILABLE` | `"keychain unavailable"` | `("keychain_unavailable", None)` |
| `USAGE_RELOGIN_REQUIRED` | `"re-login needed"` | `("relogin_required", None)` |
| (a usage `dict`) | — | `("ok", usage_to_json(entry))` |
| (any other `str`) | — | `("no_credentials", None)` |
| `None` (fetch failed / anything else) | — | `("unavailable", None)` |

### 9.2 `usage_to_json(usage)` → camelCase projection

- `five_hour` → `fiveHour` (via `_window_to_json`).
- `seven_day` → `sevenDay` (via `_window_to_json`).
- `spend` → `spend`: `{"used","limit","pct","currency"}`, plus `resetsAt` if
  `resets_at` present, plus `countdown`/`clock` from `oauth.fresh_reset_strings(spend)`.
- `scoped` → `scoped`: list of `_scoped_window_to_json` (a window plus `"name"`).

`_window_to_json(entry)`:
```
out = {"pct": entry["pct"]}
if "resets_at" in entry: out["resetsAt"] = entry["resets_at"]     # raw preserved
cell = oauth.fresh_reset_strings(entry)                            # recomputed live
if cell: out["countdown"], out["clock"] = cell
```
`countdown`/`clock` are **recomputed from `resets_at` at serialization time** (the
store may serve a measurement hours after fetch). Entries without `resets_at`, or
with an unparseable one, fall back to the fetch-time `countdown`/`clock` strings.
Sub-keys are emitted only when present in the source.

### 9.3 Row / reference / freshness / error builders

`account_ref(number, email)` → `{"number": number, "email": email}` (used for
switch `from`/`to`; `number` may be `null`).

`usage_freshness_fields(fetched_at, age_s)`:
- `fetched_at is None` → `{}`.
- else `{"usageFetchedAt": <ISO8601 UTC>}` where the timestamp is
  `datetime.fromtimestamp(fetched_at, tz=UTC).isoformat(timespec="seconds")` with
  trailing `+00:00` replaced by `Z` (e.g. `2026-01-01T00:00:00Z`).
- plus `"usageAgeSeconds": round(age_s, 1)` when `age_s is not None`.

`account_row(number, email, org_name, org_uuid, active, usage_entry, *,
usage_fetched_at=None, usage_age_s=None, alias="", disabled=False)`:
```json
{
  "number": <int>,
  "email": "<str>",
  "organizationName": "<str>",
  "organizationUuid": "<str>",
  "isOrganization": <bool(org_uuid)>,
  "active": <bool>,
  "usageStatus": "<status>",
  "usage": <object|null>
}
```
Additive fields: `"alias": <str>` **only when non-empty**; `"disabled": true`
**only when disabled**; the freshness fields (`usageFetchedAt`,
`usageAgeSeconds`) **only when `usage` is not null**.

`error_envelope(exc)`:
```json
{ "schemaVersion": 1, "error": { "type": "<type(exc).__name__>", "message": "<str(exc)>" } }
```
e.g. `SwitchError("boom") → {"schemaVersion":1,"error":{"type":"SwitchError","message":"boom"}}`.

### 9.4 Payload shapes emitted by switcher methods (observed from tests)

These are built in `switcher.py` (outside this mandate) but consumed by the CLI;
the port must keep them stable:

- **`list_accounts(json_output=True)`**:
  ```json
  { "schemaVersion": 1, "activeAccountNumber": <int|null>, "accounts": [ <account_row>, ... ] }
  ```
  Empty account set → `{"schemaVersion":1,"activeAccountNumber":null,"accounts":[]}`
  and **never prompts** (no first-run setup, no `input()`).
- **`status(json_output=True)`**:
  - no active → `{"schemaVersion":1,"active":null}`.
  - unmanaged active → `active = {"email": "...", "managed": false}`.
  - managed active → `active` has `number`, `managed:true`, `usageStatus`,
    `usage`, optional `alias`; payload also has `"totalManagedAccounts": <int>`.
- **`switch(...)` / `switch_to(...)`** result dict keys: `schemaVersion`,
  `switched` (bool), `strategy` (`"direct"` for switch_to), `reason`
  (`"switched"`,`"already-active"`,`"activated"`,`"only-one-account"`,
  `"unmanaged-account"`), `from`/`to` (each an `account_ref`; equal on any
  `switched:false`), `warnings` (list), optional `message`. The CLI adds
  `models`/`modelSource` when a model-steered strategy was in effect.

### 9.5 JSON staleness gating (observed)

For `--list --json`, a last-good usage measurement is served only while
"decision-grade": below a `STALE_OK_S` threshold it's fresh; between that and
`TRUST_MAX_AGE_S` a failed refetch still yields `usageStatus:"ok"` with
`usageAgeSeconds >= age`; past `TRUST_MAX_AGE_S` the row reports
`usageStatus:"unavailable"`, `usage:null`, and omits `usageFetchedAt` (even
though the human view still shows the aged numbers). Thresholds live in
`usage_store.py`; the CLI/JSON layer just surfaces the resulting status.

---

## 10. `printer.py` — human output, color & TTY detection

### 10.1 ANSI codes

```
RESET  = "\033[0m"     BOLD   = "\033[1m"     DIM    = "\033[2m"
RED    = "\033[31m"    YELLOW = "\033[33m"
ACCENT = "\033[38;5;173m"   # warm salmon/terracotta
MUTED  = "\033[38;5;250m"   # soft gray
```

### 10.2 Color detection (`_detect_color_support`, cached by `colors_enabled`)

Evaluated in order:
1. `os.environ.get("NO_COLOR") is not None` (present, even empty string) → **False**.
2. `os.environ.get("FORCE_COLOR") is not None` → **True**.
3. `sys.stdout` has no `isatty` or `isatty()` is False → **False**.
4. Windows (`sys.platform == "win32"`) → `_enable_windows_vt()` (True on success).
5. `os.environ.get("TERM","") == "dumb"` → **False**.
6. otherwise → **True**.

`colors_enabled()` memoizes the first result in module global `_colors_enabled`
(so later env changes don't take effect). `force_color()` is a context manager
that temporarily forces `_colors_enabled = True` and restores the prior value
(used by the TUI when capturing CLI output into a non-TTY buffer).

`_enable_windows_vt()`: non-Windows → True. On Windows uses ctypes
`kernel32.GetStdHandle(-11)` (STD_OUTPUT_HANDLE), `GetConsoleMode`,
`SetConsoleMode(handle, mode | 0x0004)` (ENABLE_VIRTUAL_TERMINAL_PROCESSING);
any exception → False.

### 10.3 Stylers & line printers

`_style(text, *codes)`: if colors disabled return text unchanged; else
`"".join(codes) + text + RESET`.

- Inline stylers (return strings): `accent`(ACCENT), `muted`(MUTED),
  `dimmed`(DIM), `bolded`(BOLD), `bold_accent`(BOLD+ACCENT), `yellowed`(YELLOW).
- Line printers (call `print`): `error(msg)` → **stderr**, red;
  `warning(msg)` → **stdout**, yellow.

### 10.4 `force_utf8_output()`

For each of stdout/stderr: if it has `reconfigure`, call
`reconfigure(encoding="utf-8", errors="replace")`, swallowing `ValueError`/`OSError`.
Streams without `reconfigure` (captured StringIO in tests) are skipped silently.
Purpose: keep glyphs like `● → ├ ─ └` from crashing a cp1252 Windows console or
ASCII/C-locale terminal.

### 10.5 Display helpers

- `_ENTRYPOINT_LABELS`: `cli→"CLI"`, `claude-vscode→"VS Code"`,
  `claude-desktop→"Desktop"`, `sdk-cli/sdk-ts/sdk-py→"SDK"`, `mcp→"MCP"`,
  `local-agent→"Agent"`, `remote→"Remote"`. `entrypoint_label(x)` returns the
  map value or `x` itself.
- `_IDE_SHORT_NAMES`: `"Visual Studio Code"→"VS Code"`. `ide_short_name(x)` maps or passthrough.
- `abbreviate_path(path)`: if path starts with `str(Path.home())`, replace that
  prefix with `~`; else unchanged.
- `format_age(started_at_ms)`: `elapsed = int(time.time()) - started_at_ms//1000`;
  `<60 → "just now"`; `<3600 → "{m}m ago"`; `<86400 → "{h}h ago"`; else `"{d}d ago"`.

---

## 11. `exceptions.py` — exception hierarchy & exit-code table

All exceptions derive from `ClaudeSwitchError(Exception)`:

```
ClaudeSwitchError
├── CredentialError
│   ├── CredentialReadError
│   └── CredentialWriteError
├── ConfigError
├── SwitchError
├── SessionError
├── LockError
│   └── ClaudeCodeLockTimeout
├── AccountNotFoundError
├── ValidationError
├── TransferError
├── MigrationError
└── MigrationIncomplete
```

Notable docstring semantics the port should preserve:
- `ClaudeCodeLockTimeout` — timed out on Claude Code's own advisory locks
  (`~/.claude.lock` / `~/.claude.json.lock`); **nothing was mutated**, safe to retry.
- `MigrationIncomplete` — a run-once migration couldn't finish for every record;
  the runner treats it as "not applied" and retries next run.

### Exit-code table (the CLI contract)

| exit | when |
|------|------|
| `0` | success; `--version`/`--help`; TUI/menubar/`config`/`run` normal returns; `auto --once` **switched**; `run_self_upgrade` success |
| `1` | any handled `ClaudeSwitchError` (init or dispatch) → `error("Error: …")` on stderr (or JSON error envelope on stdout in JSON mode); root guard; `--menubar` on non-macOS or without `rumps`; `run_self_upgrade` failure (unknown method / not on PATH / Windows print-only); `auto --once` **error** |
| `2` | argparse usage errors and every cross-flag validation `parser.error` (§4); unknown subcommand/action; missing required argument; mutually-exclusive violation; "no command given" |
| `3` | `auto --once` **blocked** (wanted to switch but no viable target / all exhausted) |
| `130` | `KeyboardInterrupt` (Ctrl-C) — every command's handler prints a cancelled note and exits 130; `auto --once` **no action** returns `2` (not a signal) |

`auto --once` maps `TickOutcome.value` directly: `0` switched, `1` error, `2` no
action, `3` blocked.

---

## 12. `logging_config.py` — log destinations & levels

`setup_logging(log_dir, debug=False) -> Logger`:
- Logger name: **`"claude-swap"`** (shared across modules, e.g. `settings.py`).
- Level: `DEBUG` if `debug` else `INFO`.
- Clears existing handlers first (`logger.handlers.clear()`).
- **File handler**: `_LazyDirRotatingFileHandler(log_dir / "claude-swap.log",
  maxBytes=1024*1024 (1 MB), backupCount=3, delay=True)`, level `DEBUG`,
  formatter `"%(asctime)s - %(levelname)s - %(message)s"`.
- **Console handler** (only if `debug`): `logging.StreamHandler()` (stderr), level
  `DEBUG`, formatter `"%(levelname)s: %(message)s"`.

`_LazyDirRotatingFileHandler` overrides `_open()` to
`Path(self.baseFilename).parent.mkdir(parents=True, exist_ok=True)` before opening.
Combined with `delay=True`, **the log dir is created lazily on the first record
actually written**, not when the logger is configured. Critical: a no-op run
(e.g. `cswap --status` with no accounts) must not materialize `cache/` or log
files under the XDG path, which would later trip the legacy→XDG migration
collision check.

Call site: `ClaudeAccountSwitcher.__init__` does
`self._logger = setup_logging(self.backup_dir, debug=debug)`, so the log file is
`<backup_root>/claude-swap.log` (with rotated `.1`/`.2`/`.3` siblings).

---

## 13. `update_check.py` — PyPI update check & self-upgrade

### 13.1 Constants

```
CACHE_PATH = CACHE_DIR / "update_check.json"        # CACHE_DIR = get_backup_root()/"cache"
CACHE_TTL  = 24 * 3600                               # 86400 seconds
PYPI_URL   = "https://pypi.org/pypi/claude-swap/json"
urlopen timeout = 2 seconds
```

Cache file format (from `cache.py`): `{"timestamp": <epoch float>, "data": <value>}`.
`read_cache(path, ttl)` returns the stored `data` if `time.time() - timestamp <
ttl`, else the `MISSING` sentinel (so a cached `None` is distinguishable from "no
cache"). `write_cache(path, data)` writes `{"timestamp": now, "data": data}`
(creating the parent dir).

### 13.2 `check_for_update(current_version) -> str | None`

- Wrapped so **any exception returns `None`** (never fails the CLI).
- Read `read_cache(CACHE_PATH, CACHE_TTL)`:
  - not `MISSING` → use cached value (which may be `None` from a previous failed fetch — this skips the network entirely while fresh).
  - `MISSING` → fetch PyPI: `urllib.request.urlopen(Request(PYPI_URL), timeout=2)`,
    parse `data["info"]["version"]`; any exception → `latest = None`. **Always**
    `write_cache(CACHE_PATH, latest)` afterward (caches failures as `None`, so a
    down PyPI is retried at most once per TTL).
- Version comparison: `_parse_version(v) = tuple(int(x) for x in v.split("."))`,
  numeric tuple compare. Notify only when `latest and parse(latest) > parse(current)`.
- Message: `f"A newer version of claude-swap is available ({latest}). You are
  using {current}. {hint}"` where `hint` depends on install method and platform:
  - method uv/pipx (a `direct` command exists) AND not Windows → `"Run \`cswap upgrade\` to update."`
  - method uv/pipx AND Windows → `f"Run \`{direct}\` to update."` (`direct` = `"uv tool upgrade claude-swap"` or `"pipx upgrade claude-swap"`)
  - unknown method → `"Run \`cswap upgrade\` for upgrade instructions."`

### 13.3 `_detect_install_method() -> "uv" | "pipx" | None`

Reads `sys.prefix`, lowercases its path parts, forms adjacent pairs
`zip(parts, parts[1:])`:
- `("uv","tools")` adjacent in the path → `"uv"` (e.g. `~/.local/share/uv/tools/claude-swap`).
- `("pipx","venvs")` adjacent → `"pipx"` (e.g. `~/.local/pipx/venvs/claude-swap`).
- Non-adjacent occurrences must NOT match (e.g. `.../uv/some-tools/.venv` → None).
- Matching is **case-insensitive** (Windows).
- Env-var override, only trusted if `sys.prefix` is actually under it: `UV_TOOL_DIR`
  (→uv), `PIPX_HOME` (→pipx) — `prefix.is_relative_to(Path(root))` (guards
  `ValueError`/`OSError`). The env var alone, with the prefix elsewhere, does NOT trigger.
- Otherwise `None` (e.g. a `pip install -e .` source checkout `.../claude-swap/.venv`).

### 13.4 `run_self_upgrade() -> int` (invoked by `cswap upgrade`)

`commands = {"uv": ["uv","tool","upgrade","claude-swap"], "pipx": ["pipx","upgrade","claude-swap"]}`.
- Unknown method → `error(...)` (to stderr) with the full manual-instruction block:
  ```
  Could not detect install method (looked for uv tool / pipx).
    sys.prefix:     <sys.prefix>
    sys.executable: <sys.executable>
  To upgrade manually, run one of:
    uv tool upgrade claude-swap
    pipx upgrade claude-swap
    <sys.executable> -m pip install --upgrade claude-swap
  If you installed with `pip install -e .`, use `git pull` instead.
  ```
  return `1`.
- Windows (`sys.platform == "win32"`): the running `cswap.exe` is locked, so it
  never upgrades in place — `print(f"To upgrade claude-swap on Windows, run:\n  {accent(' '.join(cmd))}")`
  (stdout), return `1`.
- Else `subprocess.run(cmd, check=False)`, return its `returncode` (propagates
  non-zero). `FileNotFoundError` (package manager not on PATH) →
  `error(f"Detected {method} install but \`{cmd[0]}\` is not on PATH. Run the
  upgrade manually from a shell where it is available.")`, return `1`.

---

## 14. Edge cases & subtleties (from tests)

- **`--version` output** contains `__version__` (`test_version_flag`); format is
  `"<prog> <version>"`.
- **`--help` structure** (`test_help_flag`): contains "Multi-Account Switcher";
  bare subcommands (`switch <num|email>`, `list `, `status `, `add`) lead; the
  legacy `--flag` spellings are **absent from the options section** (checked in
  the substring before "Flags combine with subcommands:") but the note
  containing "keep working" is present. `--slot`, `--email`, `add-token
  [TOKEN|-]`, `export <path>`, `import <path>`, `upgrade `, `run 2`,
  `alias <num|email>`, `auto`, `config` all appear in `--help`.
- **No-args non-TTY** exits `2`, stderr contains "no command given", and must NOT
  contain "--add-account" or "one of the arguments".
- **`--list --token-status`** forwards `show_token_status=True`. **`--token-status
  --status`** errors (only `list`). **`--token-status --json`** errors.
- **`--strategy bogus`** with `--switch` → argparse choice error, exit 2.
- **Bare `--switch`** forwards `strategy=None, models=(), model_source=None`.
- **`--switch --strategy best`** with `autoswitch.model` unset → `models=()`,
  `model_source=None`; with `model="Fable"` set → `models=("Fable",)`,
  `model_source="autoswitch.model"`.
- **`--model "Opus, opus,Fable"`** beats the setting, dedupes case-insensitively
  to `("Opus","Fable")`, `model_source="cli"`.
- **`--switch --model Fable`** without `--strategy` → error exit 2.
- **`--switch-to 2`** forwards `force=False`; with `--force` → `force=True`.
- **`--export /tmp/x --account 2`** → `export_accounts(sw, "/tmp/x", account="2", full=False)`.
  **`--export --full`** → `full=True`. `--export /p --import /q` mutually exclusive.
- **`--add-token X`** without `--email` → `add_account_from_token(token=X,
  email=None, slot=None)` (switcher defaults the email). With `--slot 3` →
  `slot=3` forwarded.
- **`--upgrade`** never constructs the switcher; `run_self_upgrade` called with no args.
- **Bare `cswap menubar`** and `cswap --menubar` route identically to `menubar.run`.
- **`_translate_subcommand`** unit behaviors: `["--list"]` unchanged; `[]`
  unchanged; `["switch"]→["--switch"]`; `["switch","--strategy","best"]→["--switch","--strategy","best"]`;
  `["switch","2"]→["--switch-to","2"]`; `["switch","u@x.com","--json"]→["--switch-to","u@x.com","--json"]`;
  `["ls"]→["--list"]`; `["rm","2"]→["--remove-account","2"]`;
  `["update"]→["--upgrade"]`; `["export","b.cswap","--full"]→["--export","b.cswap","--full"]`;
  `["bogus"]` unchanged.
- **`cswap run 2 -- --no-share`**: the tail after `--` is NOT parsed by cswap
  (forwards `["--no-share"]`). `cswap run 2 --bogus` (before `--`) → exit 2.
- **`cswap run`** with no account resolves the cwd mapping; a mapped subdir
  inherits the parent's mapping; unmapped → `exec_default([])` and prints "No
  account mapped"; a removed mapped account → `exec_default` and prints "no
  longer exists". `--share-history` and the `--`-tail survive the resolve path.
- **map**: nonexistent path warns "is not an existing directory" but still maps;
  by-email defaults to cwd; unknown account → exit 1 "Error"; empty list prints
  "No directory mappings yet"; a mapping to a removed account shows "account
  removed"; root guard → exit 1 "root" (POSIX).
- **alias**: `alias 2` (no NAME) → error; `alias --unset` (no target) → error;
  `alias 2 dev --unset` → error; invalid name `123` → exit 1; unknown account →
  exit 1; `add --alias dev` sets the alias on the newly added slot.
- **auto**: `--once` exit 0/2/3 for SWITCHED/NO_ACTION/BLOCKED; loop returns the
  loop exit; `--threshold 60` overrides a settings.json threshold while
  `cooldownSeconds` from the file is kept; `--dry-run` forwarded; `--json --once`
  stdout is pure JSONL (one object/line, each `event`+`schemaVersion:1`);
  switcher init error → exit 1, "nope" on stderr; a value set via `config set
  autoswitch.threshold 77` is picked up by `auto` (end-to-end).
- **config**: `config` with no args lists all 8 keys, `(default)` appears 8×;
  after `set` a key is not marked default (even setting it equal to the default);
  `--json` list has 8 settings with correct `value`/`isSet`; `set` writes only
  that one key (`{schemaVersion, autoswitch:{threshold}}`); bool words accepted
  (`no → false`); unknown keys/sections preserved on set; `get --json` works both
  pre- and post-verb; out-of-range → exit 1 "between 50 and 99.9"; unknown key →
  exit 1 "unknown setting" + valid-keys list; bad bool → "true or false"; bad
  number → "expects a number"; int key rejects float → "expects an integer"; bad
  strategy → "must be one of: best"; JSON error envelope on `--json get bogus`
  (exit 1, `schemaVersion:1`, message contains "unknown setting"); corrupt file +
  `set` → exit 1 "not valid JSON", file byte-preserved; missing VALUE → exit 2;
  unknown action → exit 2; `--json set …` → exit 2; `unset` restores default and
  removes the emptied section; `unset` when not set → exit 0 "not set" (on stderr).
- **JSON list** `capsys.out` from the switcher method is empty — the method
  returns the dict, the CLI serializes it; the whole stdout parses as exactly the
  payload.
- **printer**: `NO_COLOR=""` (empty) still disables; `FORCE_COLOR` enables;
  `colors_enabled()` caches (later env removal has no effect);
  `force_utf8_output` converts a cp1252 `TextIOWrapper` to utf-8 and is a no-op
  on `StringIO`; `force_color()` restores the prior cache value.
- **logging**: `setup_logging` does not create the dir; the dir + `claude-swap.log`
  appear only after the first emit; `debug=True` adds a non-file `StreamHandler`
  and sets level DEBUG.
- **update_check**: fresh cache (even a cached `None`) skips the network; a
  network error returns `None` and caches `data:null`; stale cache re-fetches;
  method/platform combinations produce the three distinct hint strings.

---

## 15. Go port notes

- **Two-layer dispatch**: reproduce the pre-dispatch (first-token match for
  `run`/`auto`/`config`/`map`/`unmap`/`alias`/`swap`/`move`) *before* the flag
  parser runs, then the verb→`--flag` translation, then the main flag parser. Go's
  `flag`/`cobra`/`kong` won't naturally model the "mutually-exclusive hidden legacy
  flags + memorable verbs" duality; a hand-rolled front controller mirroring
  `main()` is simplest and safest for fidelity.
- **`BooleanOptionalAction`**: two flags accept `--share-history/--no-share-history`
  and `--include-api-key-accounts/--no-include-api-key-accounts`. The latter is
  **tri-state** (`default=None`) so `merged_with_cli` can tell "unset" from
  "explicitly false". Model this with a `*bool` (nil = unset).
- **`--` verbatim tail**: in `cswap run`, split on the first `--`; everything
  after is forwarded to `claude` unparsed. Do not let a Go flag library consume it.
- **`nargs="?" const=""`** on `--add-token`: an empty-string sentinel distinct
  from unset; downstream logic keys on `is not None`. Represent with `*string`.
- **Root guard**: `os.geteuid() == 0` is POSIX-only (`os.Geteuid()` returns `-1`
  on Windows — guard the check to non-Windows). Container detection
  (`_is_running_in_container`) checks env `CONTAINER`/`container`, `/.dockerenv`,
  `docker|lxc|containerd|kubepods` in `/proc/1/cgroup`, and `/proc/self/mountinfo`.
- **JSON serialization**: main + config error envelopes and payloads use
  `indent=2`; `auto` uses **compact** (no indent) for both the error envelope and
  each JSONL event. Match `encoding/json` `MarshalIndent(…, "", "  ")` vs
  `Marshal`. **Field order in the tables is illustrative — tests compare parsed
  objects, not byte strings, so key ordering is free**, but presence/absence of
  optional keys (`alias`, `disabled`, freshness fields, `resetsAt`,
  `countdown`/`clock`) is load-bearing.
- **Number formatting**: `format_setting_value` renders integral floats without a
  decimal (`90.0 → "90"`, but the config JSON `value` stays the float `90.0`). In
  Go, keep the stored value a float64 but format the human string specially.
- **`check_for_update`/`run_self_upgrade` should become a GitHub-release
  equivalent.** PyPI + uv/pipx are Python-packaging concepts. A Go port should:
  query the GitHub Releases API (or an equivalent version endpoint) for the latest
  tag, compare with the embedded build version, cache the result with a 24h TTL in
  `<backup_root>/cache/update_check.json` (keep the `{"timestamp","data"}` cache
  format for interop), and self-upgrade by detecting how the binary was installed
  (e.g. Homebrew, `go install`, a downloaded release, a package manager) — the
  uv/pipx/`sys.prefix` heuristics do not apply. Keep the "any error → no
  notification" and "cache failures too" behaviors. The Windows "print the command
  instead of upgrading in place" concern (a running `.exe` is locked) still applies
  to a Go binary replacing itself on Windows.
- **`truststore` / `_use_native_tls`**: Python routes TLS trust through the OS
  verifier (SChannel/SecureTransport) to dodge a Windows OpenSSL bug where a stale
  expired duplicate intermediate (e.g. old `ISRG Root X2`) shadows the valid Let's
  Encrypt chain for `platform.claude.com`, failing verification. Go's `crypto/tls`
  already uses the platform trust store natively (SChannel on Windows), so this
  workaround is likely **unnecessary** in Go — but verify inactive-account token
  refresh against `platform.claude.com` on Windows before dropping it.
- **`force_utf8_output` / Windows VT**: Go writes bytes; UTF-8 is native. To match
  the cp1252-console fix, set the console output code page to UTF-8
  (`SetConsoleOutputCP(CP_UTF8)`) and enable VT processing
  (`SetConsoleMode(..., ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004)` on
  `GetStdHandle(-11)`) on Windows. Color-detection precedence (`NO_COLOR` present
  incl. empty → off; `FORCE_COLOR` present → on; non-TTY → off; Windows → try VT;
  `TERM=dumb` → off; else on) and the **first-call caching** must be preserved.
- **Logger name & lazy dir**: use `"claude-swap"` as the logger name; the log dir
  (`<backup_root>`) must be created lazily on first write, not at startup — a no-op
  run must not materialize `cache/` or `claude-swap.log`, or the legacy→XDG
  migration collision check breaks. Rotating file: 1 MB max, 3 backups.
- **Concurrency**: minimal in this mandate. `auto` loop installs a `SIGTERM`
  handler that calls `engine.stop()` (Go: `signal.Notify` on `SIGTERM`, cancel a
  context). Python's `RotatingFileHandler` is lock-protected; Go's `log` package
  and file writes should be similarly synchronized if multiple goroutines log.
- **`str(exc)` and `type(exc).__name__`** in `error_envelope`: the JSON `error.type`
  is the Python class name (`ConfigError`, `SwitchError`, …). The Go port must
  emit the **same type strings** for script compatibility — map each Go error type
  to its Python-class-name string (they are part of the external contract, §11).
- **Backup root resolution** (Linux/WSL XDG vs. macOS/Windows legacy) and the
  `settings.json`/`cache/update_check.json` paths under it are shared with other
  modules; keep one `getBackupRoot()` used everywhere. `$XDG_DATA_HOME` is honored
  only when set and absolute, with `~` expansion.
- **`parse_model_names` dedupe**: case-insensitive, first-spelling-wins, order
  preserved — replicate exactly (used by both `auto` and manual `switch`).
