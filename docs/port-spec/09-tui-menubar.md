# claude-swap TUI & Menu Bar — Behavioral Spec

## Overview

`claude-swap` ships a Textual-based terminal UI (`cswap tui` / bare `cswap` / `cswap watch`) and an optional macOS-only menu-bar app (`cswap --menubar`, built on `rumps`). Both are thin display/interaction shells over the same core engine used by the CLI: `ClaudeAccountSwitcher` (account CRUD, credential I/O, structured `accounts_snapshot()`), `SnapshotSource` (paced, store-governed usage reads shared by every dashboard), and `AutoSwitchEngine` (the exact threshold-based auto-switcher `cswap auto` runs, driven live from the TUI's "Auto view" and, on macOS, from the menu bar). Neither surface re-implements account/usage/switch logic or scrapes CLI text output — the TUI consumes typed dataclasses (`AccountsSnapshot`, `AccountSnapshot`, `UsageEntry`, `AutoSwitchEvent`) and renders them itself; the one place raw text crosses the boundary is captured, ANSI-colored stdout/stderr from mutating CLI-style calls (add/remove/disable), shown verbatim in an "output modal." All blocking work (file locks, Keychain subprocesses, network) runs in background thread workers; the UI event loop only ever touches in-memory state and dispatches via `call_from_thread`.

---

## 1. Entry points & process wiring

| Invocation | Behavior |
|---|---|
| `cswap tui` | Explicit dashboard launch. |
| bare `cswap` (both stdout and stdin are TTYs) | Rewritten internally to `argv = ["--tui"]` before parsing — see `cli.py`: `if not argv and sys.stdout.isatty() and sys.stdin.isatty(): argv = ["--tui"]`. Non-interactive/piped invocation with no args keeps the normal argparse usage error (exit code `2`). |
| `cswap watch` | `tui_run(switcher, start="watch")` — dashboard is pushed first, then `WatchScreen` is pushed **on top of it** (`app.push_screen(WatchScreen())` inside `on_mount` when `self._start == "watch"`), so `Esc` from the watch page lands back on the dashboard, not on process exit. |
| `cswap --menubar` | macOS only. Non-macOS: prints `"The menu bar is only available on macOS."` and exits `1`. If the `rumps` extra isn't installed: `ImportError` is caught and it prints `"Menu bar mode requires 'rumps'. Install with: pip install 'claude-swap[menubar]'"`, exits `1`. |

Entry function (`src/claude_swap/tui/__init__.py`):

```python
def run(switcher: "ClaudeAccountSwitcher", start: str = "dashboard") -> int:
    from claude_swap.tui.app import CswapApp
    app = CswapApp(switcher, start=start)
    app.run()
    return app.return_code or 0
```

Heavy imports (`textual`, `rich`) are deferred inside `run()` so plain CLI paths (`cswap list`, cron's `cswap auto --once`) never pay the import cost. **Go port note**: bubbletea/lipgloss imports have no comparable cost concern, but keep the TUI package import-isolated from the CLI's hot paths regardless, for build-graph hygiene.

Exit code: `app.return_code or 0` — Textual's `App.return_code` is `None` on a normal `action_quit`, so the process exits `0`. There is no distinct non-zero exit path from inside the TUI itself (errors are shown as notifications/modals, never propagated to the exit code).

---

## 2. Application shell (`tui/app.py`)

### 2.1 State

```python
class CswapApp(App):
    TITLE = "claude-swap"
    CSS_PATH = "cswap.tcss"
    ENABLE_COMMAND_PALETTE = False   # see §2.5
    POLL_INTERVAL_S = 3.0            # matches the old watch view's recapture cadence

    snapshot: reactive[AccountsSnapshot | None] = reactive(None)
    busy: reactive[bool] = reactive(False)
```

Constructor takes `(switcher, *, start="dashboard")`. It:
- builds `self.source = SnapshotSource(switcher)`;
- sets `self._store_only = False`, `self._full_next = False`, `self._refreshing = False`, `self._last_refresh_error = ""`;
- loads `self.threshold_pct: float | None = load_settings(switcher.backup_dir).threshold` — **any exception during load falls back to `None`** (bare `except Exception: self.threshold_pct = None`). `threshold_pct` is the value drawn as the tick mark on every usage bar everywhere in the app (dashboard, switch, watch, auto screens) — it is loaded once at construction and only otherwise mutated by the Auto screen's session-only threshold adjustment (§4.4).

### 2.2 Mount sequence

```python
def on_mount(self) -> None:
    self.register_theme(CSWAP_DARK)
    self.theme = "cswap-dark"
    self.push_screen(DashboardScreen())
    if self._start == "watch":
        self.push_screen(WatchScreen())   # stacked, so Esc → dashboard not exit
    self.set_interval(self.POLL_INTERVAL_S, self._tick)
    self._tick()   # fire immediately, don't wait for the first interval
```

Only one theme is ever registered (`cswap-dark`); see §5.

### 2.3 Snapshot poll loop

Single-flight, driven by a repeating 3-second timer plus one immediate call at mount:

```python
def _tick(self) -> None:
    if self._refreshing:
        return                      # a pass is already in flight — skip this tick
    self._refreshing = True
    full, self._full_next = self._full_next, False
    self.run_worker(
        partial(self._refresh_blocking, full, self._store_only),
        thread=True, group="refresh", exit_on_error=False, name="snapshot-refresh",
    )

def _refresh_blocking(self, full: bool, store_only: bool) -> None:
    snap = self.source.take(full=full, store_only=store_only)
    self.call_from_thread(self._apply_snapshot, snap)

def _apply_snapshot(self, snap: AccountsSnapshot) -> None:
    self._refreshing = False
    self._last_refresh_error = ""
    self.snapshot = snap            # reactive assignment fans out to every watcher
```

- `request_refresh(*, full=False)`: if `full`, arms `self._full_next = True`, then immediately calls `_tick()` (bypassing the timer, but still respecting the single-flight guard — a refresh already in flight silently absorbs the request; the "full" flag is queued for the *next* tick if one is currently running, since `_full_next` is set before the in-flight check would matter on the following invocation... in practice: if a refresh is already running, `_tick()` no-ops this call, but `_full_next` remains `True` and is picked up + reset to `False` on the *next* natural or explicit tick).
- `set_store_only(value)`: sets `self._store_only` and immediately calls `request_refresh()`. Used exclusively by the Auto screen (§4) to switch the poller from "may fetch the network" to "read the persisted usage store only," because while the Auto screen is open the `AutoSwitchEngine` itself is the sole fetcher.
- **Important**: `full=True` (`action_refresh_full`, bound to hidden key `f`) is **not** a faster path than normal polling — see §6.1: `SnapshotSource.take(full=...)` is accepted for API stability only; the underlying usage store's serve-TTL/poll-plan cadence caps every pass identically, full or not. `_full_next`/`full` exist only to route the pass through `fetch=None` (an on-demand pass, same as a plain `cswap list`) vs `fetch=set()` (store-only, no network) — see §6.1's exact fetch-set semantics. There is no "force re-fetch now" capability from the TUI.

`_refreshing`/`busy` are only ever *written* from the main/UI thread (workers only read blocking I/O results and hand them back via `call_from_thread`) — a clean single-writer pattern. **Go port note**: replicate by never mutating shared UI state directly from a goroutine; always post a message back to the update loop.

### 2.4 Worker error handling

`on_worker_state_changed` only reacts to `WorkerState.ERROR`, branching on `event.worker.group`:

| group | on error |
|---|---|
| `"refresh"` | `self._refreshing = False`; if the error message differs from `self._last_refresh_error` (de-dupe so a persistently-failing poll doesn't spam), notify: `f"Refresh failed: {msg}"`, `severity="warning"`, `timeout=6`. |
| `"action"` | `self.busy = False`; notify `f"Action failed: {event.worker.error}"`, `severity="error"` (no de-dupe, no title, default timeout). |
| `"engine"` | notify `f"Auto-switch engine stopped: {event.worker.error}"`, `severity="error"`. (The engine's `run_loop` never raises under normal operation — see autoswitch.py's own guard — so this is a last-resort surface.) |

### 2.5 Command palette is explicitly disabled

```python
ENABLE_COMMAND_PALETTE = False
```
Reasoning documented in-source: "actions live in the dashboard's nested menu, in their own context — not in a global searchable list. This also drops Textual's system commands (theme picker included; there is one theme)." Test: `CswapApp.ENABLE_COMMAND_PALETTE is False`. **Go port**: do not build a global fuzzy-command-palette; the menu is the sole discoverable action surface, contextual submenus are the sole per-account action surface.

### 2.6 Mutating actions — single-flight, captured, off-thread

All mutating operations funnel through `_start_action`:

```python
def _start_action(self, label, fn, *, show_output=False) -> None:
    if self.busy:
        self.notify("Another action is still running", severity="warning")
        return
    self.busy = True
    self.run_worker(partial(self._action_blocking, label, fn, show_output),
                     thread=True, group="action", exit_on_error=False, name=label)
```

Only one mutating action may be in flight at a time app-wide (`busy` reactive gates all of them, shared across every screen). `_action_blocking` calls `run_action(fn)` (§6.2 — captures stdout/stderr, forces color) then hands the `ActionResult` back via `call_from_thread`.

`_action_done(label, result, show_output)` — **exact dispatch order, load-bearing for the port**:

1. `self.busy = False`
2. `self.request_refresh()` — **every action, success or failure, triggers a non-full refresh immediately after.**
3. If `not result.ok`: `push_screen(OutputModal(f"{label} — failed", result.output))`, return.
4. `payload = result.payload or {}`
5. **If `"switched"` is a key in `payload`** (i.e. the action was `switch_to`/`switch`, which always return a dict with that key) — this branch fires **regardless of `show_output`**:
   - truthy → `notify(f"Switched to {target}", title="Switch")` where `target = to.get("email") or f"account {to.get('number')}"`.
   - falsy → `notify(str(reason or "no switch performed"), title="No switch", severity="warning")`.
   - `return` — an output modal is never shown for switch actions even if `show_output=True` was passed (none of the switch call sites pass it, but the branch order means it wouldn't matter).
6. Else, if `show_output and result.output.strip()`: `push_screen(OutputModal(label, result.output))`.
7. Else, if `result.first_line`: `notify(result.first_line)` (plain toast, no title).

### 2.7 Account operations (exact call sites)

| App method | Label | Switcher call | `show_output` |
|---|---|---|---|
| `do_switch(number)` | `f"Switch to account {number}"` | `switcher.switch_to(number, json_output=True)` | No |
| `action_switch_best()` | `"Switch (best)"` | `switcher.switch(strategy="best", json_output=True)` | No |
| `do_toggle_disabled(number)` | `f"{verb} account {number}"` (`verb` = `"Disable"` if currently enabled else `"Enable"`) | `switcher.set_account_disabled(number, target)` where `target = not acc.disabled` read from the **live in-memory `self.snapshot`** (not re-fetched) | No |
| `confirm_remove(number, email)` → `_on_remove_confirm` | `f"Remove account {number}"` | `switcher.remove_account(number, assume_yes=True)` | No |
| `action_add_current()` → `_on_add_confirm` | `"Add current login"` | `switcher.add_account()` | **Yes** |
| `action_add_token()` → `_on_token_form` | `"Add account from token"` | `switcher.add_account_from_token(token=, email=, slot=, assume_yes=True)` | **Yes** |

`do_toggle_disabled`: if the account number isn't found in the current snapshot (`acc is None`), the call is silently dropped — no notification, no action started.

`confirm_remove(number, email)` pushes:
```
ConfirmModal(
    f"Remove account {number} ({email})?\n\n"
    "Its stored credentials and config backup are deleted.",
    title="Remove account", yes_label="Remove",
)
```
Only on explicit `True` dismissal does `_on_remove_confirm` fire the action.

`action_add_current()` pushes:
```
ConfirmModal(
    "Back up the current Claude Code login as a managed account?\n\n"
    "If this account is already managed, its stored credentials are refreshed in place.",
    title="Add account", yes_label="Add",
)
```

`action_add_token()` pushes `AddTokenModal()` (§4.5 for its own field validation), then in `_on_token_form(form)`:
- `form is None` (cancelled) → no-op.
- Else, look up `_slot_occupant(form.slot)`: scans `self.snapshot.accounts` for `acc.number == str(slot)`, returns its email or `None`. `slot=None` (unspecified) always returns `None` (no occupant check).
  - If occupied: push `ConfirmModal(f"Slot {form.slot} is occupied by {occupant}. Overwrite?", title="Overwrite slot", yes_label="Overwrite")`; only proceeds on confirm.
  - Else: runs immediately.

### 2.8 Navigation actions

- `action_refresh_full()` — `request_refresh(full=True)` + `notify("Refreshing usage…", timeout=2)`. Bound to hidden key `f` (see §2 in dashboard/watch bindings). Despite the name and message, this is **not** a bypass of the store's pacing (§2.3).
- `action_open_auto()` — `push_screen(AutoScreen())` unless the current screen is already an `AutoScreen` (idempotent re-entry guard).
- `action_open_watch()` — `push_screen(WatchScreen())` unless already a `WatchScreen`.

---

## 3. Dashboard screen (`tui/dashboard.py`)

Layout (`compose`): `AccountsPanel(id="accounts-panel")` (the always-visible monitor, §5.4) → `Static(id="menu-title")` (breadcrumb) → `ListView(id="menu")` → `Footer`.

### 3.1 Bindings

| Key | Action | Visible in footer |
|---|---|---|
| `s` | `open_switch` (label "Switch accounts") | yes |
| `w` | `app.open_watch` (label "Watch") | yes |
| `escape`, `left` | `menu_back` (label "Back") | **hidden** |
| `q` | `app.quit` | yes |
| `g` | `app.open_auto` (label "Auto view") | **hidden** — power shortcut, menu is the discoverable path |
| `f` | `app.refresh_full` (label "Refresh usage") | **hidden** |
| `j` | `cursor_down` | **hidden** |
| `k` | `cursor_up` | **hidden** |

### 3.2 Menu structure

The menu is a **stack** of `(title, entries)` frames; `_menu_stack: list[tuple[str, MenuEntries]]`, `MenuEntries = list[tuple[str, str]]` = `(label, action_id)`. Depth 1 = root. `_BACK = ("← back", "back")`.

`on_mount` focuses `#menu` and pushes the root frame: `await self._push_menu("menu", self._root_entries())` — note the root frame's *title string is literally `"menu"`* (this is what renders in the breadcrumb at depth 1, not an empty string).

**Root entries** (exact labels and ids, in order — test-asserted):
```
[
    ("Switch account…",              "switch"),
    ("Watch accounts",                "watch"),
    ("Auto-switch view",              "auto"),
    ("Add account…",                  "add-menu"),
    ("Disable / enable account…",     "disable-menu"),
    ("Remove account…",               "remove-menu"),
    ("Quit",                          "quit"),
]
```
No "Refresh" entry — deliberate: "every view auto-refreshes, so a menu item would wrongly imply the user has to." `f` stays as the hidden escape hatch.

**Add submenu** (`_add_entries`):
```
[
    ("From current Claude Code login",     "add-login"),
    ("From a setup-token / API key…",      "add-token"),
    ("← back",                             "back"),
]
```

**Remove submenu** (`_remove_entries`) — one row per account in snapshot order:
```
label = f"{acc.number}  {f'{acc.alias} ({acc.email})' if acc.alias else acc.email}  [{acc.display_tag}]"
action_id = f"remove:{acc.number}"
```
plus trailing `_BACK`. (Alias, when set, is shown *before* the parenthesized email; a plain account shows only the bare email — never `(email)` with no alias.)

**Disable/enable submenu** (`_disable_entries`) — one row per account:
```
name = f"{acc.alias} ({acc.email})" if acc.alias else acc.email
action = "→ enable" if acc.disabled else "→ disable"
state = "  (disabled)" if acc.disabled else ""
label = f"{acc.number}  {name}{state}   {action}"
action_id = f"disable:{acc.number}"
```
plus trailing `_BACK`. (So a disabled account's row visibly ends in `"→ enable"` and carries the `(disabled)` tag; an active/enabled account's row ends in `"→ disable"` with no tag.)

`_render_menu`: breadcrumb = `" › ".join(t for t, _ in self._menu_stack)` written into `#menu-title`; the `ListView` is cleared and repopulated with `MenuItem(label, action_id, muted=(action_id == "back"))` — the back row is rendered in the muted color, everything else in foreground. `menu.index = 0` after every rebuild (cursor always resets to the top on nav in/out).

### 3.3 Dispatch table

```python
actions = {
    "switch":    self.action_open_switch,
    "watch":     app.action_open_watch,
    "auto":      app.action_open_auto,
    "add-login": app.action_add_current,
    "add-token": app.action_add_token,
    "quit":      app.exit,
}
```
Plus special-cased prefixes handled outside the table:
- `"back"` → `_pop_menu()` (no-op if already at root: `if len(self._menu_stack) > 1`).
- `"add-menu"` → push `("add account", add_entries)`.
- `"remove-menu"` → push `("remove account", remove_entries)`.
- `"remove:{number}"` → looks up the email for that number from the live snapshot (fallback `"?"` if not found), calls `app.confirm_remove(number, email)`.
- `"disable-menu"` → push `("disable / enable", disable_entries)`.
- `"disable:{number}"` → `app.do_toggle_disabled(number)` **then immediately `await self._pop_menu()`** — the submenu pops back to root right after firing the toggle, with **no confirmation modal** (unlike remove).
- anything else → `actions[action_id]()`.

### 3.4 `AccountListScreen` — shared base for Switch/Watch

```python
def on_mount(self) -> None:
    self.watch(self.app, "snapshot", self._on_snapshot)
```

`_on_snapshot(snap)`:
- `snap is None` → no-op (initial state before the first poll lands).
- Compares the new account **number list** to the previously rendered one (`self._numbers`).
  - **If the set of numbers/order changed** (accounts added/removed/reordered): full rebuild — `listview.clear()`, `listview.extend(AccountItem(acc) for acc in snap.accounts)`, `self._numbers = numbers`, then set the cursor via `_index_after_build` (or `None` if the account list is now empty).
  - **Else** (same numbers, same order — the common case, a routine usage refresh): in-place update — `zip(listview.query(AccountItem), snap.accounts)` and `item.set_account(acc)` per row. **The cursor position is left untouched** on this path.
- Always calls `_flash_updated(snap, listview)` afterward (§3.5).

`_index_after_build(snap, first_build, previous)`:
- `first_build` (very first population) → `_active_index(snap)`: index of the account whose `number == snap.active_number`, or `0` if not found.
- Else → `min(previous or 0, len(snap.accounts) - 1)` (clamp the prior cursor position into the new, possibly-shorter list).

### 3.5 Flash-on-update

```python
FLASH_S = 1.5   # how long a just-refreshed row stays highlighted
```
`_flash_updated`: builds `new_stamps = {number: acc.usage.fetched_at for acc in snap.accounts}`. If `self._stamps` was already populated (i.e. this isn't the very first snapshot), any account whose `fetched_at` **changed and is not None** gets its `AccountItem` given the `"flash"` CSS class (if not already flashing), with a one-shot timer removing the class after `FLASH_S` seconds. This is how a live "watch" screen visually signals "this row's usage measurement just advanced" without a full repaint flicker. `self._stamps` is then replaced with `new_stamps` for the next comparison.

### 3.6 `SwitchScreen`

Full-size, selection-first: every account rendered full (no minis), Enter switches and pops back to whatever screen opened it.

Bindings:
| Key | Action | Notes |
|---|---|---|
| `enter` | `select_highlighted` — label **"Switch"**, `priority=True` | Priority is needed to outrank the focused `ListView`'s own hidden Enter binding so "Switch" (not the default) shows in the footer; the action just delegates to the list's own cursor-select. |
| `b` | `app.switch_best` — label "Best pick" | |
| `escape`, `q`, `s` | `back` | |
| `j` / `k` | cursor down/up | hidden |

`on_mount`: sets `#list-title` to `"switch to which account?"`, focuses `#accounts`, then calls the base `AccountListScreen.on_mount()`.

`on_list_view_selected`: if the selected item is an `AccountItem`, `app.do_switch(item.number)` then `app.pop_screen()` — **immediately pops regardless of whether the switch action later succeeds or fails** (the failure surfaces later via the action's own `OutputModal`/notify, on the screen that was underneath).

Cursor starts on the active account on first build (inherited `_active_index` logic).

### 3.7 `WatchScreen`

Live, read-only monitor by default; `s` "arms" selection.

```python
_WATCH_TITLE  = "watching all accounts"
_SELECT_TITLE = "switch to which account? · enter confirm · esc cancel"
```

Bindings:
| Key | Action | Notes |
|---|---|---|
| `s` | `toggle_select` — label "Switch" | |
| `enter` | `select_highlighted` — label "Confirm", `priority=True` | **hidden and inert** (`check_action` returns `False`) while not selecting |
| `f` | `app.refresh_full` — label "Refresh" | hidden |
| `escape`, `q` | `back` | |
| `down`, `j` | `nav_down` | hidden |
| `up`, `k` | `nav_up` | hidden |

`check_action(action, params)`: `action == "select_highlighted" and not self._selecting` → return `False` (Textual convention: hides the binding from the footer and makes it inert). All other actions return `None`/truthy (allowed).

`_index_after_build`: overrides the base — **while `not self._selecting`, always returns `None`** (monitor mode literally has no list cursor at all, even across rebuilds); once selecting, defers to the base logic.

`_set_selecting(on)`:
- `on=True`: if the snapshot has accounts, `listview.index = self._active_index(snap)` (cursor jumps straight to the active account, not wherever it happened to be); focuses the list; title → `_SELECT_TITLE`.
- `on=False`: `listview.index = None`; `self.set_focus(None)`; title → `_WATCH_TITLE`.
- Always ends with `self.refresh_bindings()` (so the footer/`check_action` gating for `enter` updates immediately).

`action_toggle_select()` — flips `_selecting`.

`on_list_view_selected`: if not `_selecting`, ignored (guards against "a stray click while just watching"). Else: `app.do_switch(item.number)` then `self._set_selecting(False)` — **disarms but stays on the Watch screen**; this is the defining difference from `SwitchScreen` (which pops away entirely).

`action_back()`: **two-stage escape** — if currently selecting, disarm only (`_set_selecting(False)`); a *second* `Esc`/`q` press (now not selecting) actually pops the screen.

`action_nav_down`/`action_nav_up`: while selecting, drive the list cursor (`action_cursor_down`/`up`); while just watching, **scroll the viewport instead** (`listview.scroll_down(animate=False)` / `scroll_up`) — so arrow keys in pure-monitor mode pan a long account list rather than doing nothing.

---

## 4. Auto-switch view (`tui/autoview.py`)

Purpose (from the module docstring): "Runs `AutoSwitchEngine` in a thread worker and renders its typed events... Opens in **dry-run** — opening a view must never start switching accounts on its own; going live is an explicit, confirmed action." The engine's own state-file semantics (shared cooldown/quarantine/state lock in `<backup_root>/autoswitch_state.json`) make it safe to run alongside an external `cswap auto` process.

Layout (`compose`):
```
AccountsPanel(show_minis=False, id="auto-active-panel")   # only the active account's full card
Vertical(id="auto-top"):
    Horizontal(id="auto-title-row"):
        Static(" DRY-RUN ", id="mode-badge", classes="dry")
        Static("", id="auto-summary")
    Static("", id="candidates")
RichLog(id="event-log", highlight=False, markup=False, wrap=True)
Footer
```

### 4.1 Bindings

| Key | Action |
|---|---|
| `l` | `toggle_live` — "Go live / dry-run" |
| `t` | `adjust_threshold` — "Threshold" |
| `left` | `threshold_step(-1)` — "-1%" |
| `right` | `threshold_step(1)` — "+1%" |
| `enter` | `adjust_done` — "Done" |
| `escape`, `q` | `back` |

`check_action`: `threshold_step`/`adjust_done` are hidden/inert while `not self._adjusting`.

### 4.2 Lifecycle

`on_mount`:
1. `self.app.set_store_only(True)` — the app's own 3s poll loop switches to store-only reads for the duration; the engine is the sole network fetcher while this screen is up.
2. `self._settings = load_settings(self.app.switcher.backup_dir)` — a **fresh** load from `settings.json`, independent of `app.threshold_pct`.
3. `self._configured_threshold = self._settings.threshold` (remembered so unmount can restore it).
4. `self.app.threshold_pct = self._settings.threshold` — syncs the app-wide bar tick to the freshly-loaded file value (bars everywhere, and the engine, now agree).
5. `_update_summary()`.
6. `self.watch(self.app, "snapshot", self._on_snapshot)`.
7. `_start_engine(dry_run=True)`.

`on_unmount`:
1. `self._engine.stop()` if present.
2. `self.app.switcher.clear_poll_policy_inputs()` — un-pins the poll planner (see §4.6 for what pinning means) so it falls back to the settings file rather than continuing to be steered by an engine that's now gone.
3. If `self._configured_threshold is not None`: `self.app.threshold_pct = self._configured_threshold` — restores the pre-screen tick value (only the *session* adjustment reverts; the mount-time file-value correction from step 4 above is **not** undone, i.e. if the file changed underneath the screen, the mount-time sync sticks).
4. `self.app.set_store_only(False)`.

`action_back()`: if currently in threshold-adjust mode, `_end_adjust()` only (stay on screen); else `app.pop_screen()`.

### 4.3 Engine hosting

```python
def _start_engine(self, *, dry_run: bool) -> None:
    engine = AutoSwitchEngine(self.app.switcher, self._settings, self._emit_from_thread, dry_run=dry_run)
    self._engine = engine
    self.run_worker(engine.run_loop, thread=True, group="engine", exit_on_error=False,
                     name=f"auto-engine-{'dry' if dry_run else 'live'}")
    self._update_badge()
    log.write(Text(f"— engine started: {mode} —", style=MUTED))
    # mode = "DRY-RUN (watching only)" if dry_run else "LIVE (will switch accounts)"
```

`_emit_from_thread(event)` — the engine's `on_event` callback, called **on the worker thread**:
```python
def _emit_from_thread(self, event):
    try:
        self.app.call_from_thread(self._on_engine_event, event)
    except Exception:
        pass   # app/screen tearing down mid-tick; the event has nowhere to go
```
`_on_engine_event(event)` (runs on UI thread): guarded by `if not self.is_attached: return` (screen may have been popped between the callback firing and the UI thread processing it). Writes `event_text(event)` to the log; if `event.kind == "switch"`, also calls `app.request_refresh()` (non-full).

**Go port note**: this two-guard pattern (try/except around the cross-thread hop, plus an `is_attached`/liveness check on arrival) is the concurrency-safety idiom to replicate — in Go, prefer a `select` against a screen-lifetime `done`/context channel around the send, plus a liveness check on receipt before touching UI state.

`action_toggle_live()`:
- If `self._engine.dry_run`: pushes a confirm modal:
  ```
  "Go live? claude-swap will switch your active account automatically when the "
  "threshold is reached.\n\n(Same behavior as running `cswap auto` in a terminal.)"
  title="Go live", yes_label="Go live"
  ```
  Only on confirm → `_restart_engine(dry_run=False)`.
- Else (currently live): `_restart_engine(dry_run=True)` **with no confirmation** — going back to dry-run is unguarded.

`_restart_engine(*, dry_run)`: stops the current engine (if any) then calls `_start_engine(dry_run=dry_run)` — this **constructs a brand-new `AutoSwitchEngine`** from `self._settings` (which carries any in-flight session threshold override — see §4.4), so a dry↔live toggle after adjusting the threshold preserves the adjustment in the new engine instance.

`_update_badge()`: LIVE → `" LIVE "` text, CSS class `"live"`; DRY-RUN → `" DRY-RUN "` text, CSS class `"dry"`.

### 4.4 Event log rendering

```python
_EVENT_STYLES = {"switch": ACCENT, "error": SEV_WARN, "account-quarantined": SEV_WARN, "all-exhausted": SEV_CRIT}
_QUIET_KINDS  = {"poll", "no-switch", "sleep", "account-unquarantined"}

def event_text(event: AutoSwitchEvent) -> Text:
    style = _EVENT_STYLES.get(event.kind)
    if style is None:
        style = MUTED if event.kind in _QUIET_KINDS else FOREGROUND
    text = Text()
    text.append(f"{data.clock_stamp()}  ", style=MUTED)   # "HH:MM:SS  "
    text.append(event.human(), style=style)
    return text
```
So every log line is `HH:MM:SS  <event.human()>` (local clock, `time.strftime("%H:%M:%S")`), with `event.human()` (defined in `autoswitch.py`, styled the same as the CLI's human renderer — see event kinds/messages in §"External systems knowledge" below) colored by kind: switch = accent, error/quarantine = warn, all-exhausted = crit, quiet kinds (poll/no-switch/sleep/unquarantine) = muted, anything else (e.g. `config-warning`) = plain foreground.

### 4.5 Threshold-adjust mode (session-only, never persisted)

State: `_adjusting: bool`, `_configured_threshold` (mount-time file value — restored on unmount), `_entry_threshold` (value when adjust mode was *entered* — used to detect "no net change" on exit).

`action_adjust_threshold()` (`t`): toggles — if already adjusting, calls `_end_adjust()`; else sets `_adjusting = True`, `_entry_threshold = self._settings.threshold`, updates the summary line, `refresh_bindings()`.

`action_threshold_step(delta)` (`←`/`→`, only live while adjusting): 
```python
spec = SETTING_SPECS["autoswitch.threshold"]   # lo=50.0, hi=99.9
value = min(spec.hi, max(spec.lo, self._settings.threshold + delta))
self._set_threshold(value)
```
`_set_threshold(value)`: no-ops if unchanged; else `self._settings = replace(self._settings, threshold=value)`, `self._engine.apply_threshold(value)` (retargets the running engine's decision *and* poll-planning threshold immediately — see §4.6), `self.app.threshold_pct = value` (bar ticks everywhere move live), `#auto-active-panel.refresh()`, `_update_summary()`.

`action_adjust_done()` (`Enter`, only live while adjusting) and pressing `t` again both call `_end_adjust()`:
```python
def _end_adjust(self):
    self._adjusting = False
    self._update_summary()
    self.refresh_bindings()
    if self._settings.threshold == self._entry_threshold:
        return   # no net change: nothing to announce, no tick to force
    if self._engine is not None:
        self._engine.wake()   # show a decision at the new value now
    log.write(Text(f"— threshold set to {pct_label(self._settings.threshold)}% for this session —", style=MUTED))
```
**Escape while adjusting** exits adjust mode only (via `action_back`'s check), it does **not** leave the screen — a second `Esc` is needed to actually leave.

`_update_summary()` — builds the `#auto-summary` Text exactly:
```
"auto-switch · " + "threshold {pct_label}%"[accented if adjusting] + (" (session)"[muted] if threshold != _configured_threshold) + f" · poll every {interval_seconds:.0f}s" + ("   ← → adjust · enter done"[muted] if adjusting)
```
`pct_label(value)` (`autoswitch.py`) is `f"{value:.10g}"` — 10-significant-digit `%g` formatting, chosen so `99.9` never renders as a lying `"100"` (as `.0f` would) and a computed value like `85.555555` isn't rounded to `"85.5556"`. **Any place displaying a threshold percentage must use this exact formatter**; mixing it with a differently-rounded formatter elsewhere risks impossible-looking comparisons like "85.5556% < 85.555555%".

The session override is **never written to `settings.json`** — confirmed by test: after adjusting, `not (tmp_path / "settings.json").exists()`. Unmounting the screen reverts `app.threshold_pct` to the file value and calls `clear_poll_policy_inputs()` on the switcher.

### 4.6 Poll-policy pinning

`AutoSwitchEngine.__init__` calls `switcher.set_poll_policy_inputs(settings.threshold, self._models)` at construction, and `apply_threshold(value)` re-pins on every session adjustment: `self.switcher.set_poll_policy_inputs(threshold, self._models)`. This overrides the switcher's poll-plan threshold/model inputs (which normally come from the settings file) with whatever the **hosted engine's effective, possibly-CLI/session-merged settings** actually are — so the store's adaptive fetch cadence (escalate near the threshold, etc.) agrees with what the on-screen engine is actually deciding against, not with a stale file value. `clear_poll_policy_inputs()` (called on Auto-screen unmount) drops the pin, falling back to the settings file.

### 4.7 "Next best" candidates panel

```python
def _candidates_text(self, snap, active_number) -> Text:
    models = parse_model_names(self._settings.model) if self._settings else ()
    ...
    for acc in snap.accounts:
        if acc.number == active_number or not acc.switchable:
            continue
        pct = binding_pct(acc.usage.last_good, models)
        ...
```
Rules:
- Excludes the active account and any account with `switchable == False`.
- **Uses the exact same model axis (`parse_model_names(settings.model)`) as the engine** — deliberate, per in-source comment: "so the displayed ranking can never disagree with the account it picks." (Test `test_candidates_ranking_honors_configured_model` proves a Fable-bound account with roomy 5h ranks by its Fable pct, not its 5h pct, once `autoswitch.model = "Fable"` is set.)
- Per-candidate line: `f"\n  {number:>2}  {email}"` then:
  - sentinel present → `f"  {sentinel_label}"` (muted), sort key `998.0`.
  - `pct is None` (usage unknown, no sentinel) → `"  usage unknown"` (muted), sort key `999.0`.
  - else → `f"  {pct:3.0f}% used"` colored by `severity_color(pct)`, sort key = `pct`.
- Sorted ascending by sort key (best headroom first); ties broken by account number (Python tuple sort on `(key, number)`).
- Header: `"Next best"` (muted). If no ranked candidates exist: appends `"\n  no other switchable accounts"` (muted).

---

## 5. Widgets & rendering (`tui/widgets.py`)

Custom renderers rather than Textual's stock `ProgressBar`, because three things are needed the stock widget doesn't do: "a severity color ramp, an optional threshold tick mark (the auto-switch trigger line), and stale-measurement dimming."

### 5.1 Bar glyphs

```python
_BAR_FILLED = "━"
_BAR_HALF   = "╸"
_BAR_EMPTY  = "─"
_BAR_TICK   = "┃"
```

`bar_cells(pct, width, *, stale=False, threshold=None) -> Text`:
```python
if pct is None:
    return Text(_BAR_EMPTY * width, style=TRACK)
frac = min(max(pct, 0.0), 100.0) / 100.0
cells = frac * width
full = int(cells)
half = (cells - full) >= 0.5 and full < width
tick_at = min(width - 1, max(0, round(threshold / 100.0 * width))) if threshold is not None else None
color = severity_color(pct)
fill_style = f"{color} dim" if stale else color
for i in range(width):
    if tick_at is not None and i == tick_at:
        append(_BAR_TICK, style=SEV_WARN)          # tick always warn-colored, overrides fill/track at that index
    elif i < full:
        append(_BAR_FILLED, style=fill_style)
    elif i == full and half:
        append(_BAR_HALF, style=fill_style)
    else:
        append(_BAR_EMPTY, style=TRACK)
```
Note: the threshold tick is drawn **unconditionally** at its computed cell — even inside the filled region — and always colored `SEV_WARN`, independent of the bar's own severity color. `stale` appends a Rich `"dim"` style modifier to the fill color (not to the track or the tick).

`usage_bar(label, pct, suffix, width, *, stale=False, threshold=None) -> Text`: one full line —
```
"{label} " + bar_cells(...) + ("  usage unknown" if pct is None else f" {pct:3.0f}%"[dimmed if stale]) + ("  {suffix}" if suffix)
```
Example from the docstring: `` 5h ━━━━╸────┃──  47%  resets 2h 13m · 20:39 ``.

### 5.2 Reset-time suffixes

`_reset_parts(window, now) -> (reset_text_or_None, reset_text_with_clock_or_None)` — `reset_text` (`data.py`, §6.3) computed first; if present, `reset_clock` (`data.py`, §6.4) is appended as `f"{reset} · {clock}"` when a clock is available, else the two are identical strings.

### 5.3 `usage_rows(last_good, now) -> list[(label, pct, suffix, suffix_full)]`

Mirrors the CLI's `_format_usage_lines` exactly (out of this module's mandate but the contract is load-bearing here). Order, exactly:
1. `spend` (if present) — label `"$$"`. `amounts = f"${used:,.2f} / ${limit:,.2f}"`. `suffix = f"{reset}  {amounts}"` if a reset exists, else just `amounts`. (Note: two spaces between reset and amounts, not one.)
2. `five_hour` (if present) — label `"5h"`.
3. `seven_day` (if present) — label `"7d"`.
4. Each entry in `scoped` (a list, order as given by the API/store) — label = the window's own `"name"` field (e.g. `"Fable"`). If `pct >= 100`: `"(!)"` is appended to **both** `suffix` and `suffix_full`, joined with two spaces if a reset string already exists (`f"{suffix}  (!)"`), else just `"(!)"`.
- **A window key entirely absent from `last_good` produces no row at all** — e.g. an annual plan with no `seven_day` key never gets an invented "7d" row; this is asserted directly by tests (`test_absent_window_produces_no_row`, `test_active_card_skips_absent_window_and_shows_scoped`).
- `usage_rows(None, ...)` and `usage_rows({}, ...)` both return `[]`.

### 5.4 `account_card_text(acc, width, *, threshold=None, now=None) -> Text` — the full account card

Header line:
```
f"{number:>2}  "  [bold foreground]
+ (alias [bold accent] + f" ({email})" [foreground]   if acc.alias else   email [foreground])
+ f"  [{display_tag}]"  [muted]           # display_tag = org_name or "personal"
+ ("   ● active" [bold accent]            if acc.is_active)
+ ("   (disabled)" [muted]                if acc.disabled)
+ (f"   {age}" [muted]                    if data.format_age(acc.usage.age_s) is truthy)
```

**Sentinel branch** — if `acc.usage.sentinel is not None`, the bars are entirely replaced:
```
"\n    " + marker + " " + sentinel_label
```
where `marker = "·"` (style muted) if the sentinel is `USAGE_API_KEY`, else `"⚠"` (style `SEV_WARN`). If the sentinel is **not** `USAGE_API_KEY`, a "last seen" line is appended below (same wording `cswap list` prints, via `switcher.last_seen_note`):
```
"\n    " + f"└ {last_seen}"   [muted]
```
— only if `last_seen_note(acc.usage)` returns non-`None` (requires both `last_good` and `fetched_at` to be present). API-key accounts (`USAGE_API_KEY` sentinel) never get a "last seen" line even if `last_good`/`fetched_at` happen to be populated — "API-key accounts have no quota to have 'seen'." The card **returns early** here — no bar rows are rendered when a sentinel is active.

**No-data branch** — if `usage_rows(...)` is empty (no sentinel, but also no windows at all): `"\n    " + "usage unavailable"` [muted], plus `f" · {last_error}"` [muted] appended if `acc.usage.last_error` is set.

**Normal branch** — bar rows:
```python
stale = acc.usage.age_s is not None and acc.usage.age_s > STALE_OK_S   # STALE_OK_S = 300.0
label_width = max(len(label) for label, ... in rows)
bar_width = max(12, min(30, width - 42 - label_width))
row_overhead = 4 + label_width + 1 + bar_width + 5 + 2
for label, pct, suffix, suffix_full in rows:
    if suffix_full != suffix and row_overhead + len(suffix_full) <= width:
        suffix = suffix_full          # show the clock-extended variant only where it fits
    append "\n    " + usage_bar(f"{label:<{label_width}}", pct, suffix or None, bar_width, stale=stale, threshold=threshold)
```
Per-row degradation is deliberate and independently decided per row (not per card): a wide card shows every row's absolute reset clock; a mid-width card keeps the 5h/7d clocks but drops the (longer) spend row's clock back to a bare countdown once it no longer fits; a narrow card is the old countdown-only look with no clocks at all. (Tests exercise widths 40/78/100 explicitly.) `bar_width` clamps to `[12, 30]` and shrinks as the terminal narrows or the label column widens.

### 5.5 `mini_account_text(acc, now) -> Text` — one-line form for inactive accounts

```
f"{number:>2}  "  [bold muted]
+ (alias [bold accent] + f" ({email})" [foreground]   if alias else   email [foreground])
+ f"  [{display_tag}]"  [muted]
+ ("  (disabled)" [muted]   if disabled)
+ "   "
```
Then, if a sentinel is present: just `sentinel_label` (muted, or `SEV_WARN` if not `USAGE_API_KEY`) and **return** — no percentages shown at all for a sentinel'd mini row.

Else, for each of `five_hour`/`seven_day` present in `last_good` (in that order), joined by `" · "` [track color]: `f"{label} " [muted] + f"{pct:.0f}%"` colored by severity (dimmed if stale, same `STALE_OK_S` threshold as the full card) — and if `pct >= 100`, `f" ({reset})"` [muted] is appended when a reset text is available. Then, for every `scoped` window at/over 100%: `f"{name} (!)"` in `SEV_CRIT` (not the normal severity ramp — maxed scoped windows are hard-coded crit-red in the mini form). If no parts were added at all (`five_hour`/`seven_day` both absent, no maxed scoped): `"usage unknown"` [muted].

Constructed with `Text(no_wrap=True, overflow="ellipsis")` — a mini line never wraps; it truncates with `…` if the terminal is too narrow.

### 5.6 `AccountsPanel` — the always-visible monitor

```python
class AccountsPanel(Static):
    def __init__(self, *, show_minis=True, id=None): ...
```
`on_mount`: `self.watch(self.app, "snapshot", lambda _snap: self.refresh(layout=True))` — repaints on every snapshot update.

`render()`:
- `snap is None` → `"loading…"` [muted].
- `not snap.accounts` → `"No managed accounts yet.\nUse the menu below: Add account — from your current Claude Code login, or from a setup-token / API key."` [muted]. (Exact wording, two lines.)
- Else: `width = (self.size.width or 80) - 2`; for each account in snapshot order: if `is_active` → full `account_card_text(acc, width, threshold=app.threshold_pct, now=now)`; else if `show_minis` (default `True`, `False` on the Auto screen's active-only panel) → `mini_account_text(acc, now)`; else skipped entirely.
- If no blocks resulted at all (e.g. no active account and minis suppressed) → `"no active managed login"` [muted].
- Blocks are joined with a **blank line** (`"\n\n"`) whenever either the current or the previous block was itself multi-line (i.e. around the expanded active card), and a single `"\n"` otherwise (between two consecutive one-line minis) — "breathe around the expanded active card."

### 5.7 `AccountCard`, `AccountItem`, `MenuItem`

- `AccountCard(Static)`: wraps one `AccountSnapshot`; `set_account(acc)` swaps it and triggers `refresh(layout=True)`; `render()` calls `account_card_text(self._acc, self.size.width or 80, threshold=self._threshold)` (note: **no `now=` passed** here, unlike `AccountsPanel` — this only matters if reset countdowns are computed at a slightly different instant than the panel's, since `now` defaults to `time.time()` inside `account_card_text` when omitted).
- `AccountItem(ListItem)`: wraps an `AccountCard`; carries `.number`/`.email` as plain attributes (used for lookups by the screens, e.g. `[item.number for item in items]`).
- `MenuItem(ListItem)`: wraps a `Static(Text(label, style=...))`; carries `.action_id`; `muted=True` (used for the "back" row) renders in the muted color instead of foreground.

---

## 6. Data service (`tui/data.py`)

Module docstring: "The TUI never parses printed CLI output — it consumes `ClaudeAccountSwitcher.accounts_snapshot` (one collect pass) and renders structured data." Everything in this module is blocking and must be called from a thread worker.

### 6.1 `SnapshotSource` (defined in `snapshot_source.py`, re-exported here)

```python
class SnapshotSource:
    def __init__(self, switcher): self.switcher = switcher; self._last = None
    def take(self, *, full=False, store_only=False) -> AccountsSnapshot:
        fetch = set() if store_only else None
        snap = self.switcher.accounts_snapshot(fetch=fetch)
        self._last = snap
        return snap
```
- `fetch=None` → every stale account is *eligible* to be fetched this pass (the usage store's own poll-plan/freshness/backoff logic in `UsageStore.reserve` decides which, if any, actually are — this is the same on-demand pass `cswap list` runs).
- `fetch=set()` (empty set, `store_only=True`) → **no network eligibility at all**; a pure read of whatever the store already has cached.
- `full=True` does **not** change the `fetch` argument or bypass pacing — test-asserted: `source.take(); source.take(); source.take(full=True)` all produce identical `fetch_sets == [None, None, None]` against the fake switcher.
- Shared by any future GUI shell, not TUI-specific — the module comment: "Pacing is store-governed... this class therefore just runs the same on-demand pass as `cswap list`... and offers `store_only` for shells that host an auto engine."

### 6.2 `ActionResult` / `run_action`

```python
@dataclass
class ActionResult:
    ok: bool
    output: str                 # captured stdout+stderr, ANSI-colored
    payload: dict | None = None # structured result for json-capable actions

    @property
    def first_line(self) -> str:
        for line in self.output.splitlines():
            plain = Text.from_ansi(line).plain.strip()
            if plain:
                return plain
        return ""
```

`run_action(fn) -> ActionResult`:
```python
def run_action(fn):
    buf = io.StringIO()
    payload = None
    saved_stdin = sys.stdin
    sys.stdin = io.StringIO()          # empty stream: input() raises EOFError instead of hanging
    try:
        with printer.force_color(), redirect_stdout(buf), redirect_stderr(buf):
            try:
                payload = fn()
            except ClaudeSwitchError as e:
                print(f"Error: {e}")
                return ActionResult(False, buf.getvalue())
            except EOFError:
                print("Error: interactive input is not available here.")
                return ActionResult(False, buf.getvalue())
    finally:
        sys.stdin = saved_stdin
    return ActionResult(True, buf.getvalue(), payload if isinstance(payload, dict) else None)
```
Notes load-bearing for the port:
- `printer.force_color()` (context manager, `printer.py`) temporarily forces the CLI's internal color-enabled flag on even though stdout is redirected to a non-tty `StringIO` — "the TUI re-renders the ANSI itself, so it wants the codes emitted." The CLI's accent color is xterm-256 code 173, ANSI escape `\033[38;5;173m` (`printer._ACCENT`) — same terracotta as the TUI's own `ACCENT = "#d7875f"` (theme.py), "a deliberate nod to Claude Code's orange."
- The `sys.stdin`/`sys.stdout`/`sys.stderr` redirection is **process-global** for the duration of the call — safe here only because "the TUI owns the terminal and nothing else prints while it runs." **Go port note**: Go has no direct analog to `contextlib.redirect_stdout`; if the ported switcher-equivalent functions print human text as a side effect (rather than returning structured results + a log), the port will need either (a) a genuine `os.Pipe()`-based fd-redirect (process-wide, same caveat), or (b) — strongly preferred — refactor the switcher-equivalent to return `(payload, []LogLine)` directly so the Go TUI never needs stdout capture at all. This is the single biggest "Python-ism" in the data layer.
- `ClaudeSwitchError` → captured as a failed `ActionResult`, message printed as `f"Error: {e}"` into the captured buffer (so it shows up as the modal body / first line).
- Any unexpected `input()`/`EOFError` → captured as failed, `"Error: interactive input is not available here."` — "in-scope actions never prompt once `assume_yes`/explicit identifiers are used; this is defensive."
- `payload` is kept only if it's a `dict` (some switcher calls return `None`).

### 6.3 Display helpers

```python
def sentinel_label(sentinel: str) -> str:
    return SENTINEL_NOTES.get(sentinel, sentinel)   # exact wording `cswap list` prints
```

```python
def window_pct(last_good, key) -> float | None: ...   # last_good[key]["pct"] if numeric, else None

def reset_text(window, now) -> str | None:
    """"resets 2h 13m", computed live from resets_at at render time (the API's
    own countdown string is correct only at fetch time and drifts as the
    measurement ages)."""
    resets_at = window.get("resets_at")
    if not resets_at: return None
    ts = datetime.fromisoformat(str(resets_at).replace("Z", "+00:00")).timestamp()   # ValueError → None
    remaining = ts - now
    if remaining <= 0: return "resets now"
    return f"resets {format_duration(remaining)}"

def reset_clock(window, now) -> str | None:
    """Absolute local reset time ("20:39" / "Jul 14 09:00"). None once the
    reset has already elapsed — "resets now" needs no clock."""
    ...
    if reset_utc.timestamp() - now <= 0: return None
    return oauth.reset_clock_string(reset_utc, datetime.fromtimestamp(now, tz=timezone.utc))
```

```python
def format_duration(seconds: float) -> str:
    s = int(seconds)
    if s < 60:    return f"{s}s"
    if s < 3600:  return f"{s // 60}m"
    if s < 86400: h, m = divmod(s // 60, 60); return f"{h}h {m}m" if m else f"{h}h"
    d, h = divmod(s // 3600, 24);              return f"{d}d {h}h" if h else f"{d}d"
```
Exact examples (test-asserted): `42 → "42s"`, `180 → "3m"`, `7980 → "2h 13m"`, `93600 (26h) → "1d 2h"`.

```python
def format_age(age_s: float | None) -> str | None:
    if age_s is None or age_s < usage_store.SERVE_TTL_S:   # SERVE_TTL_S = 180.0
        return None
    return f"· {format_duration(age_s)} ago"
```
Test-asserted: `format_age(3.0) is None`, `format_age(120) is None` (120 < 180), `format_age(None) is None`, `format_age(400) == "· 6m ago"`.

```python
def clock_stamp() -> str:
    return time.strftime("%H:%M:%S")   # local time, used for the auto-view event log
```

### 6.4 `oauth.reset_clock_string(reset_utc, now_utc) -> str`

(defined in `oauth.py`, referenced heavily by the widgets/data layers): `reset_local.strftime("%H:%M")` if `reset_local.date() == now_local.date()`, else `reset_local.strftime(f"%b {day} %H:%M")` with a non-zero-padded day (`str(reset_local.day)`) — e.g. `"Jul 5 08:59"`, not `"Jul 05 08:59"`.

---

## 7. Modal dialogs (`tui/modals.py`)

All are `ModalScreen[T]` pushed via `app.push_screen(modal, callback)`; the callback receives the dismissal value.

### 7.1 `ConfirmModal(message, *, title="Confirm", yes_label="Yes") -> bool`

Bindings: `y` → `action_confirm` (dismiss `True`); `n`, `escape` → `action_cancel` (dismiss `False`); `left`/`right` → `app.focus_previous`/`app.focus_next` (moves focus between the two buttons; hidden bindings). Clicking a `Button` also dismisses via `on_button_pressed`: `dismiss(event.button.id == "yes")`.

Compose: a centered `.modal-box` containing `Label(title)`, `Static(message)`, a right-aligned button row (`Button(yes_label, id="yes")`, `Button("Cancel", id="no")`), and a hint line: `f"← → · enter  ·  y {yes_label.lower()}  ·  n / esc cancel"`.

**Only an explicit `True` result runs the follow-up action** — dismissing any other way (Escape, clicking Cancel, pressing `n`) is always safe/no-op.

### 7.2 `AddTokenModal() -> TokenForm | None`

```python
@dataclass
class TokenForm:
    token: str
    email: str | None
    slot: int | None
```
Fields (in order): `Input(password=True, placeholder="token (required)", id="token")`, `Input(placeholder="email label (optional)", id="email")`, `Input(placeholder="slot number (optional)", id="slot", type="integer")`. Body text: `"OAuth setup-token (sk-ant-oat…) or managed API key (sk-ant-api…); the type is auto-detected."` Title: `"Add account from token"`.

Bindings: `escape` → `cancel` (dismiss `None`); `left`/`right` → `app.focus_previous`/`app.focus_next`, hidden — "only reach the screen when a Button is focused (a focused Input consumes them for cursor movement), so they safely double as button navigation." Buttons: `Add` (id `add`) submits, `Cancel` (id `cancel`) dismisses `None`. `Input.Submitted` on any field also submits.

`_submit()` validation, in order:
1. `token = #token.value.strip()`; if empty → `#form-error` set to `"Token is required."`, no dismissal.
2. `email = #email.value.strip() or None`.
3. `slot_raw = #slot.value.strip()`; if non-empty:
   - `int(slot_raw)` — `ValueError` → `#form-error` = `"Slot must be a number."`, no dismissal.
   - `slot < 1` → `#form-error` = `"Slot must be >= 1."`, no dismissal.
4. Else dismiss `TokenForm(token=token, email=email, slot=slot)`.

Hint line: `"enter add  ·  tab next field  ·  esc cancel"`.

### 7.3 `OutputModal(title, output) -> None`

Bindings: `escape`, `q`, `enter` → `dismiss_modal` (dismiss `None`); the same close button does the same.

Compose: a wide `.modal-box.modal-box-wide` containing `Label(title)`, then inside a `VerticalScroll(.modal-output)`: `Static(Text.from_ansi(output.rstrip() or "(no output)"))` — the captured, color-forced CLI output is rendered as real ANSI-styled Rich text (bold/color codes preserved), not stripped plain text. Hint line: `"esc close"`.

---

## 8. Theme & visual language (`tui/theme.py`, `cswap.tcss`)

### 8.1 Color constants (single source of truth — widgets import these directly for Rich renderables; the `Theme` object below maps the same values onto Textual's design tokens)

```python
ACCENT      = "#d7875f"   # warm terracotta, xterm 173 — matches printer._ACCENT (CLI)
FOREGROUND  = "#e8e4de"   # soft, slightly warm off-white
MUTED       = "#8a8a8a"   # secondary text
BACKGROUND  = "#141414"
SURFACE     = "#1e1e1e"
PANEL       = "#262626"

SEV_OK      = "#87af87"   # calm green: plenty of headroom
SEV_WARN    = "#d7af5f"   # amber: climbing (>= 70%)
SEV_CRIT    = "#d75f5f"   # soft red: near the limit (>= 90%)
TRACK       = "#3a3a3a"   # unfilled bar track

WARN_PCT = 70.0
CRIT_PCT = 90.0

def severity_color(pct):
    if pct is None: return MUTED
    if pct >= CRIT_PCT: return SEV_CRIT
    if pct >= WARN_PCT: return SEV_WARN
    return SEV_OK
```
Note: `CRIT_PCT` (90.0) intentionally mirrors the auto-switch default threshold (`AutoSwitchSettings.threshold = 90.0`) — "bar color and switch behavior agree," per the module docstring — so out-of-the-box the bar turns red right around where auto-switch would fire.

`CSWAP_DARK = Theme(name="cswap-dark", primary=ACCENT, secondary=MUTED, accent=ACCENT, foreground=FOREGROUND, background=BACKGROUND, surface=SURFACE, panel=PANEL, success=SEV_OK, warning=SEV_WARN, error=SEV_CRIT, dark=True, variables={"footer-key-foreground": ACCENT, "block-cursor-background": PANEL, "block-cursor-foreground": FOREGROUND, "block-cursor-text-style": "none"})` — this is the **only** theme registered; footer keybinding hints render in the accent color instead of Textual's default blue.

### 8.2 Layout (`cswap.tcss`) — key structural facts for the Go port's visual parity

- Screen background = `$background`.
- `#accounts-panel` (dashboard monitor): `height: auto`, `padding: 1 3`, `border-bottom: solid $panel`.
- `#auto-active-panel` (Auto screen's active-only panel): `padding: 1 3 0 3` (no bottom padding — the summary/candidates row sits right under it).
- Menu/account list items: `border-left: thick $background` at rest, becomes `border-left: thick $primary` (i.e. the accent) plus `background: $surface` on the highlighted (`-highlight`) row — this accent-colored left-border-as-cursor is the primary "what row is selected" affordance throughout the app, in both the menu and every account `ListView`.
- `#accounts ListItem` gets `margin-bottom: 1` (vertical breathing room between full account cards in Switch/Watch lists) — the dashboard's own menu (`#menu ListItem`) does not.
- `#accounts ListItem.flash` → `background: $panel` (the flash-on-update highlight, §3.5).
- Auto screen's mode badge: `.dry` → `background: $panel; color: $warning`; `.live` → `background: $primary; color: $background` (i.e. a solid accent-colored block once live — the single loudest visual signal that switching is armed).
- Modals: centered (`align: center middle`), scrim `background: $background 60%`; `.modal-box` is `width: 64` (`max-width: 90%`), `.modal-box-wide` (Output modal only) is `width: 90`; buttons are flat/borderless chips (`border: none`), quiet (`background: $panel`) at rest, solid accent block (`background: $primary; color: $background; text-style: bold`) when focused — "the accent shows where Enter lands."
- `.modal-output` caps at `max-height: 20` rows inside a `VerticalScroll`.

---

## 9. Edge cases & subtleties (from tests)

- **Switch actions always take the notify path, never an output modal** — even though `add-login`/`add-token` pass `show_output=True`, `do_switch`/`action_switch_best` don't need to (and structurally can't, since payload `"switched"` short-circuits before the `show_output` check is ever reached).
- **`SwitchScreen` pops immediately on selection**, before the switch action has resolved — the switch may still fail; failure surfaces later as an `OutputModal` (or nothing, if it's a `"switched": false` no-op) on whatever screen is now visible, not on `SwitchScreen` itself.
- **`WatchScreen` never pops on switch** — it disarms selection and stays; this is the entire reason it exists as a separate screen from `SwitchScreen` rather than a mode flag on one screen.
- **A same-account-set snapshot update never touches list cursor position** — only a changed *set/order* of account numbers triggers a rebuild (and only a rebuild resets/repositions the cursor). A routine usage-only refresh (numbers unchanged) is purely an in-place field update via `AccountItem.set_account`.
- **The disable/enable submenu pops back to the dashboard root immediately after toggling**, with zero confirmation — contrast with Remove, which always confirms first. This asymmetry is deliberate and must be preserved (disabling is reversible/low-stakes; removing deletes stored credentials).
- **Menu breadcrumb literally reads `"menu"` at the root** (not blank) — `_push_menu("menu", root_entries)` on mount; nested crumbs read e.g. `"menu › add account"`.
- **`f` (full refresh) is not actually faster** — `SnapshotSource.take(full=True)` produces the identical `fetch=None` call as any other on-demand pass; the notification text ("Refreshing usage…") is arguably misleading about what "full" buys you, but it's the documented, tested behavior (`test_every_pass_is_store_governed`).
- **The occupied-slot overwrite check in `add_account_from_token` only fires when a slot was explicitly typed** — `slot=None` never triggers the confirm, even if some other logic downstream would auto-assign into an occupied slot.
- **A `_slot_occupant` lookup silently returns `None` (no confirmation) if `self.snapshot is None`** — i.e. if the very first snapshot poll hasn't landed yet when the user races through Add Token, the occupied-slot guard is a no-op for that one submission.
- **Threshold session-adjust: `wake()`/log-line only fire on a *net* change** — entering adjust mode and immediately leaving without touching arrows (or nudging it back to exactly the entry value) produces neither a forced engine tick nor a log line. Test: `test_threshold_adjust_escape_exits_mode_not_screen` explicitly asserts `wakes == 0` in that case.
- **Threshold clamp is `[50.0, 99.9]` inclusive, never `100.0`** — the spec deliberately excludes 100 so the display never lies ("never a lying 100%"); `pct_label` uses `%.10g` specifically so `99.9` doesn't get rounded up to `"100"` by a naive `.0f`.
- **A dry↔live engine restart carries forward the session threshold** — `_restart_engine` rebuilds the engine from `self._settings` (which holds the adjusted value in memory), not from a fresh `load_settings()` re-read, so toggling live after adjusting doesn't silently drop the adjustment.
- **`AccountCard.render()` omits `now=`** (unlike `AccountsPanel.render()`, which passes an explicit `now=time.time()`) — both ultimately default to `time.time()` internally, but they call it at slightly different instants; this only matters for sub-second countdown-string skew and is presumably harmless, but note it if the Go port centralizes "now" into a single per-frame value (which would actually be a slight behavior *improvement*/simplification, not a regression, if done deliberately).
- **`sentinel != USAGE_API_KEY` gates the "last seen" line** on both the full card and (implicitly, since the mini form shows no percentages at all under a sentinel) the mini row — API-key accounts are the one sentinel kind that never shows historical usage, because they structurally have none to show.
- **A per-row clock-vs-countdown decision is made independently per row**, not per card — `test_card_shows_clock_only_where_it_fits` proves a mid-width card can show clocks on 5h/7d while the (longer) spend row falls back to a bare countdown on the very same render.
- **`usage_rows`/`mini_account_text` never invent a row for a window key the account doesn't have** — an annual-plan account (5h only, no 7d key in `last_good`) shows no "7d" anywhere, full card or mini, ever; this must not regress to "0%" or "—" placeholders in the port.
- **The empty-accounts hint text is exact and two-line**: `"No managed accounts yet.\nUse the menu below: Add account — from your current Claude Code login, or from a setup-token / API key."`

---

## 10. macOS menu bar app (`menubar.py`) — **candidate for exclusion in the Go port**

**Platform**: hard macOS-only. Built on `rumps` (a thin Objective-C/PyObjC wrapper around `NSStatusItem`/`NSMenu`), an optional install extra (`pip install 'claude-swap[menubar]'`). One code path (`on_add_token`) additionally imports `AppKit` directly to force-activate the app (`NSApplication.sharedApplication().activateIgnoringOtherApps_(True)`) before showing a modal `rumps.Window`, because "a menu-bar (accessory) app isn't the active app, so a modal `rumps.Window` can render black/blank until we bring the app forward."

**Recommendation**: mark this entire surface out-of-scope for a first cross-platform Go port. A native macOS status-bar app is a substantial, platform-specific undertaking (Cocoa bindings, code-signing/notarization for distribution, a fully separate event-loop model from a terminal TUI) with no Linux/Windows equivalent implied by the rest of the codebase. If menu-bar parity is wanted later, it should be its own scoped effort (e.g. `github.com/getlantern/systray` or a native Cocoa/Swift shim called from Go) — not bundled into the primary bubbletea TUI port. The rest of this section documents its behavior in full in case that decision is revisited.

### 10.1 Settings — `<backup_dir>/menubar_settings.json`

```python
@dataclass
class MenuBarSettings:
    show_account_name: bool = True
    title_pct: str = "both"          # one of TITLE_PCT_CHOICES = ("off","5h","7d","both")
    title_scoped: bool = False       # append per-model weekly limits (e.g. Fable) to the title
    refresh_interval: int = 60       # one of REFRESH_CHOICES = (30, 60, 300)
    auto_switch_enabled: bool = False
```
JSON shape (pretty-printed, `indent=2`, field names as written — **snake_case, not camelCase**, unlike `settings.json`):
```json
{
  "show_account_name": true,
  "title_pct": "both",
  "title_scoped": false,
  "refresh_interval": 60,
  "auto_switch_enabled": false
}
```
`load(path)`: any `OSError`/`ValueError` (including malformed JSON) → all-defaults, silently. Non-dict JSON → all-defaults. Per-field: kept only if the key is present **and** `isinstance(raw[key], type(default_for_that_field))` — a wrong-typed value for one field is dropped (that field reverts to default) without invalidating the rest of the file (test: `refresh_interval: "fast"` (string, not int) is dropped and defaults to `60`, while a sibling valid `show_account_name: false` is kept).
`save(path)`: `path.parent.mkdir(parents=True, exist_ok=True)` then `path.write_text(json.dumps(asdict(self), indent=2))`.

**Auto-switch *policy* (threshold/cooldown/hysteresis/model/etc.) is explicitly NOT stored here** — it lives in the shared `settings.json` (`claude_swap.settings`, `autoswitch.*` keys) "so the CLI and the menu bar share one source of truth." Only display prefs + the on/off toggle live in the menu-bar-local file.

### 10.2 Pure display/formatting helpers (unit-testable without `rumps`)

```python
ICON = "⇄"
SWITCH_HISTORY_LIMIT = 10
```

`tightest_pct(usage) -> float | None` — max of `five_hour`/`seven_day` pcts (spend excluded — "it isn't a rate-limit window").

`_window_pct(usage, key) -> float | None`.

`_resets_at_ts(window) -> float` — POSIX timestamp of `resets_at`, or `float("inf")` if missing/unparseable/not-a-dict.

`_live_countdown(window, now) -> str | None` — `"Xd Yh"` / `"Xh Ym"` / `"Xm"` from `resets_at`, `None` if already past or missing. **Deliberately re-derived from the absolute timestamp at call time, never from a cached "countdown" string**, because a cached string frozen at fetch time drifts stale.

```
_WEEKLY_PERIOD_S = 7 * 86400
```
`_rolled_weekly_window(window, now) -> dict | window` — **critical external-knowledge behavior**: weekly (`seven_day` and `scoped`) usage windows reset on a **fixed 7-day cadence from the API's own schedule**, so once the stored `resets_at` is in the past, the menu bar knows (without any new fetch) that the window has rolled over. It computes:
```python
missed = int((now - ts) // _WEEKLY_PERIOD_S) + 1
new_ts = ts + missed * _WEEKLY_PERIOD_S
rolled = dict(window); rolled["pct"] = 0.0
rolled["resets_at"] = datetime.fromtimestamp(new_ts, tz=timezone.utc).isoformat()
rolled.pop("countdown", None); rolled.pop("clock", None)   # stale cached strings dropped
```
— i.e. it advances to the *next future* weekly boundary (correctly handling multiple missed weeks, e.g. a machine asleep for 10 days rolls forward by 2 boundaries, landing 4 days in the future from `now`, not showing a still-past date), zeroes the displayed percentage, and strips any stale cached `countdown`/`clock` strings so they get recomputed live. Future or unparseable/missing windows pass through **unchanged** (identity, not a copy — `is` comparison holds in tests). **This roll-forward logic is applied only in the menu bar** (`usage_summary`, `format_title`) for `seven_day` and `scoped` windows — `five_hour` is explicitly left untouched ("dynamic session window," not on a fixed schedule) even inside the same call.

`usage_summary(usage, now=None) -> str` — one-line summary for a menu row:
- `usage` is a bare string (a sentinel note) → returned verbatim.
- `usage is None` → `"usage unavailable"`.
- Else, parts joined by `" · "`: `"5h {pct:.0f}%"` (+ `" ({countdown})"` if available), `"7d {pct:.0f}%"` (7d rolled-forward first per above; countdown appended same way), then each `scoped` window as `"{name} {pct:.0f}%"` (+ `" (!)"` if `>= 100`, + countdown), then finally `"$ {pct:.0f}%"` for `spend` if present. No numeric window available at all → `"usage unavailable"`.
  - Exact test example: `{"five_hour":{"pct":42},"seven_day":{"pct":18},"spend":{"pct":30,...}} → "5h 42% · 7d 18% · $ 30%"`.

`format_account_label(num, email, usage, now=None, alias=None, disabled=False) -> str`:
```python
label = f"{alias}  ({email})" if alias else email
marker = "  (disabled)" if disabled else ""
return f"{num}  {label}{marker}  {usage_summary(usage, now)}"
```

`_local_part(email, limit=12) -> str` — the part before `@`, truncated to `limit` with a trailing `*` if longer: kept length is `limit - 1` chars + `*` (so a 12-char budget yields **11 real characters plus the marker**, never 12 real chars silently truncated with no indicator). Test: `"averylonglocalpart"` (18 chars) truncates to `"averylonglo*"` (11 chars + `*`).

`format_title(active_email, active_usage, settings, now=None, alias=None) -> str` — the literal macOS menu-bar title string:
- `active_email is None` → just `ICON` (`"⇄"`).
- Else, segments (in order, each conditionally included):
  1. account name: `alias` if set, else `_local_part(active_email)` — only if `settings.show_account_name`.
  2. 5h pct (`f"{p:.0f}%"`) — only if `settings.title_pct in ("5h","both")` and the window is numeric.
  3. 7d pct — only if `settings.title_pct in ("7d","both")`, **rolled forward first** via `_rolled_weekly_window`.
  4. Each `scoped` window's `f"{name} {pct}%"` — only if `settings.title_scoped`, each rolled forward.
- If no segments at all (e.g. `title_pct="off"` and `show_account_name=False`) → just `ICON`.
- Else → `f"{ICON} " + " · ".join(segments)`.
- A non-dict `active_usage` (e.g. `"no credentials"`, a sentinel string) silently drops all pct/scoped segments (only the name segment, if enabled, survives) — `format_title("loc@x.com", "no credentials", both-settings) == "⇄"` when `show_account_name=False`.

`format_usage_log(email, usage) -> str | None` — a log-file line using each window's **absolute `clock` string** (not a live countdown — log lines are already timestamped, so a frozen clock is fine): `f"usage {email}: 5h {pct}% (resets {clock}) · 7d {pct}% (resets {clock})"`, only the windows with a numeric pct included, `clock` segment omitted if absent. Returns `None` (log nothing) when neither window has a numeric pct — "callers can skip logging nothing."

`_usage_log_key(usage) -> (float|None, float|None)` — de-dupe key = `(5h pct, 7d pct)` only, deliberately ignoring `clock` (which changes every refresh) so an idle account isn't re-logged every tick.

`parse_switch_history(log_text, limit=10) -> list[str]` — **reads the switcher's own rotating log file** (`<backup_dir>/claude-swap.log`) for lines matching:
```python
_SWITCH_LOG_RE = re.compile(r"Switched from account (\d+) to (\d+)")
```
Each match paired with its timestamp trimmed to the minute (`line.split(" - ", 1)[0].strip()[:16]`, i.e. the first field of a `"YYYY-MM-DD HH:MM:SS,mmm - LEVEL - message"` log line, sliced to `"YYYY-MM-DD HH:MM"`), formatted as `f"{from} → {to}   {timestamp}"`. Returns the **most recent `limit` entries, most-recent-first** (`out[-limit:][::-1]`). Unparseable lines are silently skipped. **This is a real external-format dependency**: the exact Python `logging` line format (`"%(asctime)s - %(levelname)s - %(message)s"`, comma-millisecond `asctime`) and the exact logged message text `"Switched from account X to Y"` (emitted elsewhere in `switcher.py`) must both be preserved byte-for-byte if a Go port ever re-implements this feature, or the regex/slicing breaks silently (no error, just empty/wrong history).

`_account_display_usage(entry) -> dict | str | None` — sentinel note (`SENTINEL_NOTES[entry.sentinel]`) if sentinel set, else `entry.last_good`, else `None`.

`_adapt_snapshot(snap) -> dict` — pure transform of an `AccountsSnapshot` into the menu bar's render shape:
```python
EMPTY_SNAPSHOT = {"accounts": [], "active_email": None, "active_usage": None, "active_alias": None}
```
```json
{
  "accounts": [["<num>", "<email>", "<is_active bool>", "<display usage: dict|str|None>", "<last_good: dict|None>", "<alias str>", "<disabled bool>"], ...],
  "active_email": "...|null",
  "active_usage": "...|null",
  "active_alias": "...|null"
}
```
(Each account entry is a 7-tuple, in this exact field order.) `active_email`/`active_usage`/`active_alias` are pulled from whichever account has `is_active == True` (undefined/last-wins if the snapshot were ever inconsistent, though `accounts_snapshot()` guarantees at most one active account).

### 10.3 App glue (`run(switcher)`, `rumps.App` subclass) — behavior only, not literal Python

- Two independent timers on the `rumps` main run-loop:
  - `refresh_timer` at `settings.refresh_interval` seconds (default 60; user-selectable 30/60/300 via the Settings submenu) — triggers `refresh_async()`, a background-thread fetch.
  - `sync_timer` at a **fixed 1 second** — runs `on_sync_tick` on the **main thread**: if `self._dirty` (a background fetch completed since the last tick), rebuild the menu; always calls `_detect_active_change()` and `_drain_engine_events()`.
- **Concurrency model**: exactly one background fetch worker at a time (`self._refreshing` guard in `refresh_async`), started via a bare `threading.Thread(daemon=True)` (not a pool). The worker rebinds plain attributes (`self.snapshot`, `self._snapshot_at`, `self._dirty = True`) with no lock — comment: "Lock-free handoff: worker only rebinds plain attributes (atomic in CPython); the main-thread sync tick reads them." **Go port note**: this relies on CPython's GIL making single-attribute assignment atomic; Go has no such guarantee — the equivalent must use a channel send (worker → main-loop message) or an explicit mutex/atomic pointer swap, not a bare struct-field write from a goroutine.
- `_worker(full)`: calls `SnapshotSource.take(full=full, store_only=(self._engine is not None))` — **while the auto-switch engine is running, the menu bar's own display refresh is store-only too** (same pattern as the TUI's Auto screen), because the engine is already the sole fetcher. On any exception, the *previous* snapshot is kept ("keep the last good snapshot rather than blanking the menu") and the exception is logged at `debug` level only.
- `_log_usage(snap)`: on every refresh, for each account whose `(5h,7d)` pct pair changed since last logged for that account number, calls `switcher._logger.info(format_usage_log(...))` — de-duped per-account via `self._last_usage_log: dict[num, key]` so an idle machine's log doesn't churn identical lines every refresh cycle.
- `_detect_active_change()` (runs every 1s on the sync tick, main thread): cheap `stat()` on `~/.claude.json` (`self._config_path`); if the mtime hasn't changed since last check, does nothing (avoids parsing a possibly-large config file every second). If it *has* changed, re-reads the active account locally (`switcher._get_current_account()` — no Keychain, no network) and, only if the resolved active email differs from the currently-displayed one, kicks a full `refresh_async()`. Skipped entirely while a refresh worker is already in flight. Comment: "Reflect account switches from any source (menu, CLI, auto engine) within ~1s... Claude Code rewrites this file often for unrelated reasons" (hence the mtime gate rather than reading unconditionally).
- **Auto-switch engine hosting**: `_start_engine()` constructs a **live** (`dry_run=False`) `AutoSwitchEngine` from `load_settings(backup_dir)` (the shared, non-menubar-local settings) and runs `engine.run_loop()` in its own daemon thread. A construction failure is caught, logged at `warning`, and surfaced via `rumps.notification("claude-swap", "Auto-switch failed to start", str(e))` — it does **not** crash the menu bar. `_stop_engine()` calls `engine.stop()`. `_restart_engine()` (used when the threshold is changed from the menu) stop+starts to pick up the new setting. Engine events are queued cross-thread under `self._event_lock` (a real `threading.Lock`, unlike the lock-free snapshot handoff) and drained on the 1s sync tick:
  - `kind == "switch"` and not `dry_run` → `rumps.notification("claude-swap", "Auto-switched account", ev.human())` + immediate `refresh_async()`.
  - `"account-quarantined"` → `rumps.notification("claude-swap", "Account quarantined", ev.human())`.
  - `"all-exhausted"` → `rumps.notification("claude-swap", "All accounts exhausted", ev.human())`.
  - `"config-warning"` → `rumps.notification("claude-swap", "Configuration warning", ev.human())` — "the engine emits it once per run; dropping it would leave a menu-bar user with a silently inert filter."
- The engine is auto-started at app launch if `settings.auto_switch_enabled` was persisted `True`.

### 10.4 Menu structure (exact items, in order)

```
[account rows]                                   # one rumps.MenuItem per managed account, or
                                                   # "No managed accounts" (disabled) if none
None                                              # separator
"Rotate to next"          → switch(strategy=None)
"Switch to best"          → switch(strategy="best")
"Next available"          → switch(strategy="next-available")
None
"Add account" ▸
    "From current login"
    "From setup-token…"                           # only if switcher has add_account_from_token
"Remove account" ▸  [one row per account, or "No managed accounts"]
"Disable / enable account" ▸  [one row per account, check-marked if disabled, or "No managed accounts"]
"Refresh current credentials"
"Switch history" ▸  [up to 10 most-recent switches, or "No switches logged yet"]
    None
    "Open full log…"
None
"Settings" ▸
    "Show account name in menu bar"   [checkmark]
    "Title percentage" ▸  None / Session (5h) / Weekly (7d) / Both (5h · 7d)   [radio via checkmark]
    "Show model limits in title"      [checkmark]
    "Refresh interval" ▸  30 seconds / 60 seconds / 5 minutes   [radio]
    "Auto-switch accounts"            [checkmark]
    "Auto-switch threshold" ▸  80% / 90% / 95% / 98%   [radio, reflects live settings.json value]
"Refresh now"
"Quit"
```
Each account row's `format_account_label(...)`; the **active** account's row gets `item.state = 1` (rumps' checkmark convention doubles as "this is the current account," not a selectable toggle). The disable/enable submenu reuses the same checkmark glyph to mean "held out of rotation" instead — explicitly noted in-source as a semantic overload of the same visual affordance.

`_make_switch_to(num)` / `_switch(strategy)` / `_make_remove(num)` / `_make_toggle_disabled(num, disabled)` all wrap the call in `_guard(fn)`:
```python
def _guard(self, fn):
    try:
        fn(); return True
    except ClaudeSwitchError as e:
        rumps.alert(title="claude-swap", message=str(e)); return False
```
— any core error surfaces as a native macOS alert dialog, never a crash.

On a successful switch (any of the three strategies, or a direct account pick), `_notify_switched()` fires a native notification: title `"Account switched"`, body `"Switch takes effect within ~30s — restart Claude Code to apply immediately."` — **this ~30s figure is a real external-system fact**: macOS Keychain read caching means a running Claude Code process doesn't see a just-swapped credential instantly.

`_make_remove(num)`: uses `rumps.alert(title="Remove account", message=f"Remove account {num}?", ok="Remove", cancel="Cancel")`; proceeds only if the return value is `1` (rumps' "OK pressed" sentinel).

`on_add_token`: two sequential native `rumps.Window` text-entry dialogs (email, then token) — `AppKit` app-activation workaround noted above; cancel or empty text at either step aborts.

`on_open_log`: reveals `<backup_dir>/claude-swap.log` in Finder via `subprocess.run(["open", "-R", str(target)])` (or the parent dir if the log doesn't exist yet) — **macOS `open` CLI dependency**, no Linux/Windows equivalent implied.

`on_refresh_creds`: re-runs `switcher.add_account(slot=None)` to refresh the *currently active* account's stored credential in place. Two special error paths:
- No active login detected (`switcher._get_current_account() is None`) → alert `"No active Claude Code login detected. Log in first."`
- `CredentialReadError` (typically a locked/inaccessible Keychain) → alert: `"Couldn't read the active credential. If the menu bar is running as a background/login agent, macOS blocks its Keychain access — quit and relaunch it from a Terminal with: cswap --menubar"` — **documents a real macOS platform quirk**: a `launchd` background/login-agent process cannot prompt for Keychain access the way a Terminal-foreground process can; the `security` CLI call simply times out/fails silently in that context.

`on_refresh_now`: `refresh_async(full=True)` — the user's one explicit "go fetch" action (unlike the TUI's `f`, this **does** actually matter here in the sense that it kicks a refresh outside the timer cadence, though the underlying store pacing still applies).

`on_quit`: stops the engine, then `rumps.quit_application()`.

`_make_interval(secs)`: rumps 0.4.0's `Timer.interval` setter is a documented no-op while the timer is already running unless a full interval has already elapsed — worked around by explicit `stop(); interval = secs; start()` to force the new cadence to take effect immediately rather than waiting out the stale interval once.

`_make_threshold(pct)`: calls the **shared** `set_setting(backup_dir, "autoswitch.threshold", str(pct))` (writes to `settings.json`, the same file/keys the CLI's `cswap config set` uses), then `_restart_engine()` to apply immediately if running.

---

## 11. Go port notes

### 11.1 Concurrency model — replace Textual's worker system

Textual's `run_worker(fn, thread=True, group=..., exit_on_error=False, name=...)` gives: (a) run `fn` on a background OS thread, (b) marshal its result back to the single UI-loop thread via `call_from_thread`, (c) on an uncaught exception in `fn`, emit a `WorkerState.ERROR` event to the app instead of crashing the process, tagged with `group` for routing. The three groups in this codebase (`"refresh"`, `"action"`, `"engine"`) are functionally three independent single-flight background-task classes with distinct error-handling policies (§2.4).

**Go/bubbletea equivalent**: use `tea.Cmd` returning a typed result message (`refreshDoneMsg`, `refreshErrMsg`, `actionDoneMsg`, `actionErrMsg`, `engineEventMsg`, `engineStoppedMsg`) processed in `Update`. Single-flight guards (`_refreshing`, `busy`) become plain `bool` fields on the bubbletea model — safe because `Update` runs on a single goroutine by construction; the actual I/O goroutines must **never** write model fields directly, only send messages through the `tea.Program`'s channel (via the `tea.Cmd` return contract) — this is the direct Go-idiomatic analog of "only ever mutated on the main/UI thread" called out in §2.3.

The Auto screen's engine worker is long-lived (blocks in `run_loop` until `stop()`), unlike the one-shot refresh/action workers — model it as a goroutine holding a `context.Context`/cancel func (or a `chan struct{}` "done" signal matching Python's `threading.Event`-based `stop()`/`wake()`), emitting events onto a channel the bubbletea program drains via a `tea.Cmd` that does a blocking channel receive and re-arms itself (the standard bubbletea "long-running background producer" pattern).

### 11.2 `stop()`/`wake()` semantics to preserve exactly

`AutoSwitchEngine.stop()` sets both an internal "stop" and "wake" signal, is idempotent, and is safe to call **before** the loop has even started ("the stop is never cleared, so the loop exits immediately — engines are single-use"). `wake()` only cuts short the current inter-tick sleep; it does not stop the loop. In Go, model `stop`/`wake` as two separate broadcastable signals (e.g. `stopCh chan struct{}` closed once, `wakeCh chan struct{}` a buffered-1 channel drained-and-refilled) rather than one flag, since the two are semantically and independently observable (`wake()` after `stop()` should be a harmless no-op, not a panic-on-closed-channel).

### 11.3 Reactive/observer pattern → explicit fan-out

Textual's `reactive` + `self.watch(app, "snapshot", callback)` is a pub-sub mechanism: assigning `app.snapshot = snap` synchronously notifies every screen/widget that registered a watcher, each re-rendering its own view of the same data independently (dashboard's `AccountsPanel`, the Auto screen's own panel + candidates list, `AccountListScreen`'s list diffing, etc.). There is no single "the app renders" call — every interested component reacts on its own. **Go/bubbletea has no built-in equivalent** (a bubbletea `Model` is one tree, updated by one `Update` dispatch per message) — the natural port is: on receipt of a `snapshotMsg`, the top-level `Update` stores the new snapshot in the model and then calls each active sub-view's own "recompute from snapshot" step before the next `View()` render, rather than trying to replicate independent per-widget subscriptions. The key behavioral contract to preserve is **not** the subscription mechanism but its *effects*: (a) every visible surface reflects the same coherent `AccountsSnapshot` instance after an update — never a torn mix of old/new; (b) list-cursor position is preserved across an update unless the account *set* actually changed (§3.4/§9); (c) the "flash on advanced measurement" diffing (§3.5) needs its own explicit prior-vs-current comparison step, since it isn't free the way Python's `watch` callback machinery makes it.

### 11.4 ANSI-captured output

`run_action`'s `redirect_stdout`/`redirect_stderr` + `Text.from_ansi` round-trip (§6.2) is the most Python-specific mechanism in the whole surface: it exploits the fact that the CLI-equivalent functions already print human-readable, ANSI-colored progress/result text, and the TUI just captures and re-renders it verbatim in a modal. Go has no equivalent to `contextlib.redirect_stdout` at the language level without an `os.Pipe()`-based fd-level redirect (process-global, same single-writer caveat the Python code already accepts). **Recommended departure for the Go port** (not just a mechanical translation): have the Go equivalent of the switcher's mutating operations (add/remove/disable/switch) return a structured result type (`{OK bool; Message string; Payload any}` or similar) directly, with no printing at all, and let the bubbletea layer format its own modal/toast text from that struct. This sidesteps ANSI capture and an fd-redirect entirely and is very likely the right design regardless of porting fidelity — but flag it explicitly as a **deliberate behavior change** from the Python original if adopted, since the exact rendered modal text (order of printed lines, embedded formatting) will no longer be byte-identical to what `run_action` currently captures.

### 11.5 `check_action`/hidden-binding pattern

Textual's per-screen `check_action(action, params) -> bool | None` (returning `False` hides the binding from the footer *and* makes the key inert; `None`/`True` allow it) is used for exactly two state-gated interaction modes: `WatchScreen`'s `enter`/select-confirm (only live while `_selecting`), and `AutoScreen`'s `threshold_step`/`adjust_done` (only live while `_adjusting`). **Go port**: bubbletea has no declarative binding-registry equivalent; replicate by branching in `Update`'s key-message handler on the same mode flags, and by conditionally including/excluding the corresponding help text in the status/footer line rendered from `View()`. The `priority=True` binding on `SwitchScreen`/`WatchScreen`'s `enter` (to outrank the focused `ListView`'s own built-in Enter-select binding, purely so the *footer label* reads "Switch"/"Confirm" instead of the list's default) has no real analog needed in Go — a hand-rolled list widget's Enter handling is whatever you write it to be; just make sure the equivalent footer/help text says the intended verb.

### 11.6 Session-only overrides that must never be persisted

Two pieces of state are explicitly documented as memory-only, reverted on screen-exit, and must **never** be written to `settings.json`: the Auto screen's threshold adjustment (§4.5) and — by the same "same memory-only precedent" comment in the source — the dry-run/live toggle itself is also not persisted anywhere (it's re-derived fresh, always starting dry-run, every time the Auto screen is opened; only `MenuBarSettings.auto_switch_enabled` persists an on/off flag, and that's a *different* surface with a *different* persistence contract). A Go port must be careful not to accidentally round-trip either of these through a settings file "for convenience" — that would be an observable, undocumented behavior change (e.g. re-opening the Auto screen a week later would silently resume live mode instead of always requiring the explicit re-confirmation).

### 11.7 Platform-conditional logic inventory

| Concern | Where | Go treatment |
|---|---|---|
| `cswap --menubar` gate (`sys.platform != "darwin"`) | `cli.py` | If ported: `runtime.GOOS != "darwin"` equivalent gate, or omit the flag entirely per §10's recommendation. |
| `rumps`/`AppKit` import | `menubar.py` | No cross-platform Go equivalent without a Cocoa binding; see §10 recommendation to exclude. |
| macOS Keychain background-agent access failure (`CredentialReadError` → specific alert text) | `menubar.py` `on_refresh_creds` | Only relevant if the menu bar is ported; the underlying Keychain-access constraint itself belongs to the credential-storage module's spec, not this one. |
| `open -R` (Finder reveal) | `menubar.py` `on_open_log` | macOS-only `open` CLI; no attempt needed unless the whole menu bar is ported. |
| `Platform.detect()` (`sys.platform`/`WSL_DISTRO_NAME` env var) | `models.py`, used indirectly | Not TUI-specific; note only that the TUI itself has **no** platform-conditional rendering logic of its own — every screen/widget/keybinding in `tui/*.py` is platform-agnostic. Only `menubar.py` and (at the entry-point level) the `--menubar` CLI gate are platform-conditional. |

### 11.8 Numeric constants to carry over exactly

| Constant | Value | Source |
|---|---|---|
| `POLL_INTERVAL_S` | `3.0` s | `app.py` — main snapshot poll cadence |
| `FLASH_S` | `1.5` s | `dashboard.py` — row highlight duration after a usage update |
| `STALE_OK_S` | `300.0` s | `usage_store.py` — bar-dimming staleness threshold |
| `SERVE_TTL_S` | `180.0` s | `poll_policy.py` (re-exported via `usage_store`) — `format_age` silence threshold |
| `WARN_PCT` / `CRIT_PCT` | `70.0` / `90.0` | `theme.py` — severity color bands |
| autoswitch threshold clamp | `[50.0, 99.9]` | `settings.py` `SETTING_SPECS["autoswitch.threshold"]` |
| `AutoSwitchSettings` defaults | `threshold=90.0, interval_seconds=60.0, cooldown_seconds=300.0, hysteresis_pct=10.0, unhealthy_ticks=3` | `settings.py` |
| menu bar `REFRESH_CHOICES` | `(30, 60, 300)` s | `menubar.py` |
| menu bar `AUTO_THRESHOLD_CHOICES` | `(80, 90, 95, 98)` % | `menubar.py` |
| menu bar `TITLE_PCT_CHOICES` | `("off", "5h", "7d", "both")` | `menubar.py` |
| menu bar `SWITCH_HISTORY_LIMIT` | `10` | `menubar.py` |
| menu bar sync tick | `1` s (fixed, not user-configurable) | `menubar.py` `run()` |
| menu bar `_WEEKLY_PERIOD_S` | `7 * 86400` s | `menubar.py` — weekly-window roll-forward cadence |
| `_local_part` truncation | `12` chars total (11 + `*`) | `menubar.py` |
| bar width clamp | `[12, 30]` cells | `widgets.py` `account_card_text` |

---

## 12. External systems knowledge embedded in this code (quoted exactly)

These are facts about Claude Code's own files, the Anthropic usage API's data shape, and this project's own on-disk/log formats that the TUI and menu bar depend on and must be preserved precisely by any port.

**Usage window dict shape** (as produced by `oauth.build_usage_result`, stored in `UsageEntry.last_good`, and consumed throughout `widgets.py`/`data.py`/`menubar.py`):
```json
{
  "five_hour":  {"pct": 47.0, "resets_at": "2026-07-17T20:39:00Z", "countdown": "...", "clock": "..."},
  "seven_day":  {"pct": 63.0, "resets_at": "2026-07-20T09:00:00Z"},
  "spend":      {"used": 12.5, "limit": 50.0, "pct": 25.0, "currency": "USD", "resets_at": "..."},
  "scoped": [
    {"name": "Fable", "pct": 62.0, "resets_at": "2026-07-19T00:00:00Z"}
  ]
}
```
`resets_at` is an ISO-8601 timestamp; both consuming code paths handle a trailing literal `"Z"` by rewriting it to `"+00:00"` before `datetime.fromisoformat` (`str(resets_at).replace("Z", "+00:00")` in `data.py`'s `reset_text`/`reset_clock`; `menubar.py`'s `_resets_at_ts` uses `datetime.fromisoformat` directly and tolerates either form — Python 3.11+ `fromisoformat` accepts `Z` natively but the code doesn't rely on that for the `data.py` path, defensively normalizing first). A window's `"countdown"`/`"clock"` fields, when present, are **fetch-time snapshots that go stale** — every live render recomputes both from `resets_at` instead (documented rationale in both `data.py` and `menubar.py`: "the API's own countdown string is correct only at fetch time and drifts as the measurement ages").

**Sentinel usage states** — string constants standing in for a usage dict when normal fetching isn't applicable, and their exact user-facing wording (must be byte-identical to what `cswap list` prints, per multiple explicit test assertions):
```python
USAGE_NO_CREDENTIALS      = "no credentials"      # (json_output.py — not surfaced via SENTINEL_NOTES)
USAGE_TOKEN_EXPIRED       = "token expired"
USAGE_API_KEY             = "api key"
USAGE_KEYCHAIN_UNAVAILABLE = "keychain unavailable"
USAGE_RELOGIN_REQUIRED    = "re-login needed"

SENTINEL_NOTES = {
    USAGE_TOKEN_EXPIRED:        "token expired — Claude Code refreshes the active account",
    USAGE_API_KEY:               "API key (no quota)",
    USAGE_KEYCHAIN_UNAVAILABLE:  "keychain unavailable — locked or in use; try again",
    USAGE_RELOGIN_REQUIRED:      "re-login needed — refresh token dead; log in with Claude Code, then run: cswap add",
}
```
`USAGE_TOKEN_EXPIRED` specifically means "Claude Code refreshes the active account" (i.e. this is *not* asking the user to re-login) — a distinction the UI text must preserve precisely, since it's semantically different from `USAGE_RELOGIN_REQUIRED` ("only the user can fix it").

**Rotating log-line format** the menu bar's switch-history feature parses (produced elsewhere, by `switcher.py`'s logger, using Python's standard `logging` `"%(asctime)s - %(levelname)s - %(message)s"` formatter):
```
2026-06-27 00:57:50,178 - INFO - Switched from account 1 to 3
```
Parsed via `re.compile(r"Switched from account (\d+) to (\d+)")` against the message portion, with the timestamp taken as the first 16 characters of the line before the first `" - "` (i.e. `"YYYY-MM-DD HH:MM"`, seconds/milliseconds dropped). **A Go port that re-implements switch-history parsing must reproduce this exact log-line shape** (including the comma-separated milliseconds Python's default `asctime` produces) or write its own log and re-derive the parser from that instead — do not assume the two can silently diverge.

**Settings file** (`<backup_root>/settings.json`, shared — not menu-bar-local) — camelCase JSON keys, `schemaVersion` envelope, referenced here only for the keys the TUI reads/writes:
```json
{
  "schemaVersion": 1,
  "autoswitch": {
    "threshold": 90.0,
    "intervalSeconds": 60.0,
    "cooldownSeconds": 300.0,
    "hysteresisPct": 10.0,
    "strategy": "best",
    "includeApiKeyAccounts": false,
    "unhealthyTicks": 3,
    "model": null
  }
}
```
The Auto screen reads this at mount (`load_settings`) and, via the menu bar's threshold quick-picks, writes back through `set_setting(backup_dir, "autoswitch.threshold", str(pct))` — the **only** write path from either UI surface into this file (everything else the TUI adjusts is session-memory-only, §11.6).

**Switch action JSON payload** shape returned by `switcher.switch_to`/`switcher.switch` (consumed directly by `app._action_done`, §2.6):
```json
{
  "switched": true,
  "from": {"number": 1, "email": "old@example.com"},
  "to":   {"number": 2, "email": "new@example.com"},
  "reason": "requested"
}
```
`"switched": false` variant (e.g. no better target found) carries `"from": null, "to": null, "reason": "no-better-target"` (or similar machine-readable reason strings) — the TUI shows `reason` verbatim as the notification body.

**macOS Keychain ~30s propagation latency** — a real hardware/OS fact baked into UI copy in two places: the menu bar's post-switch notification body (`"Switch takes effect within ~30s — restart Claude Code to apply immediately."`) and the auto-switch engine's own design rationale (`autoswitch.py` module docstring: switching is done *proactively*, "so the old account is still valid while a running Claude Code picks the new one up — this is what makes the macOS ~30s Keychain cache latency harmless"). Any Go port surfacing switch-completion messaging should preserve this caveat's substance even if the exact wording changes.
