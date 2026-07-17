# Judgment — cswap Go-port architecture: Faithful vs. Idiomatic

Both proposals are strong, spec-grounded, and clearly written by authors who
read the specs closely. They agree on almost every leaf-package decision
(`keychain` argv/rc-44, `cclock` proper-lockfile protocol, `filelock`
non-reentrancy, `map[string]any` for `sequence.json`, the atomic-write
unification, the update/menubar/truststore deviations). The divergence that
matters is entirely at the top of the graph: **Faithful keeps `switcher.py`'s
4,840-line god object as one `internal/switcher` package; Idiomatic decomposes
it into `store` (substrate) + `lifecycle`/`switching`/`reporting` (free
functions over `*store.Store`) + a thin `core.Switcher` façade.** Almost every
scoring difference flows from that one choice.

Scores are 1–10.

---

## Criterion 1 — Data-compatibility & lock-interop safety (highest priority)

**Faithful 8.5 · Idiomatic 7.5**

Both defend the hard contracts explicitly and both dedicate a "Top-5 risks"
section (Faithful §7, Idiomatic §7) covering the same five: keychain
byte-compat, `proper-lockfile` interop, `sequence.json` optional-key omission,
usage-cache schema-v2 identity guard, `.enc` fail-closed/best-effort split.
Both model `sequence.json` account records as `map[string]any`/`json.RawMessage`
and both commit Python-produced golden fixtures under `testdata/` — the single
most important defense, and they tie on it.

Two concrete places tip Criterion 1 to **Faithful**, both in data-compat:

1. **Usage round-trip fidelity.** Faithful carries usage dicts as
   `map[string]any` end-to-end "for round-trip fidelity (04 §7.7)" and stores
   `lastGood` verbatim. Idiomatic introduces a typed `oauth.Usage` struct
   (`FiveHour/SevenDay/Spend/Scoped`) and its Deviation #8 deliberately coerces
   `five_hour/seven_day.pct` to `float64` — but spec 04 §1.18 is explicit:
   `pct` is stored **uncoerced** ("no coercion — stored as whatever type
   `utilization` is, int or float"). If `usage.UsageEntry.LastGood` is a typed
   `*oauth.Usage`, a Python-produced `usage.json` with `"pct": 22` round-trips
   to `22.0`, and any future field the Python tool adds is dropped on rewrite.
   `usage.json` is cswap's own cache (not a Claude-Code interop file), so the
   blast radius is small and Idiomatic *flags* the deviation — but it is the one
   place Idiomatic is measurably looser on the top-priority axis, and it is a
   flagged deviation where the spec is unusually emphatic that no deviation
   exists.

2. **Switch-history log-line format is an external contract, and only Faithful
   caught it.** Spec 09 §12 (verified: `_SWITCH_LOG_RE`,
   `2026-06-27 00:57:50,178 - INFO - Switched from account 1 to 3`) pins the
   Python `logging` `"%(asctime)s - %(levelname)s - %(message)s"` format with
   comma-milliseconds **and** the exact message `"Switched from account X to Y"`
   as a byte-level dependency of the menubar's history parser — a cross-tool
   interop point that survives even though menubar itself is out of scope
   (Python menubar + Go cswap on one machine). Faithful's `logging` section
   calls this out explicitly ("must emit lines byte-compatible … including the
   exact 'Switched from account X to Y' message … 09 §12 pins that format").
   Idiomatic's §3.6 mentions only the logger name, lazy-dir, rotation, and the
   paste-safe email invariant — the format contract is absent.

Idiomatic's offsetting strengths on this criterion are real but don't fully
close the gap: its injected `clock.Clock` seam into `cclock.Acquire(dir,
timeout, clk)` makes the staleness/stale-takeover interop tests deterministic
(Faithful's `Acquire(dir, timeout, staleness)` uses ambient wall time), and its
credstore `Config{Platform, CredentialsDir}` value + injected `KeychainClient`
interface is a cleaner test seam than Faithful's `credstore.Host` callback. Both
help *verify* the contracts; neither changes the contracts themselves.

## Criterion 2 — Parallel implementability (6–10 concurrent agents)

**Faithful 6.5 · Idiomatic 9**

This is Idiomatic's decisive win and the reason it takes the judgment.

Below the aggregate, the two graphs are nearly identical in granularity — both
break out `oauth`, `usage`, `keychain`, `credstore`, `settings`, `mappings`,
`procdetect`, `jsonout`, `session`, `migrations`, `transfer`, `autoswitch`,
`cli`, `tui` as separate packages/WPs. The difference is the **critical-path
package**: Faithful's `internal/switcher` is specs 01+02 in one Go package,
which Faithful itself must split across two "tightly coordinated" agents (WP8
store/report + WP9 switch/session) editing the **same package** — sharing one
`Switcher` struct definition, package-level declarations, and a 40-method
surface. Faithful even writes "Coordinate on the shared `Switcher` struct +
method set defined in WP0." That is precisely the merge-conflict hotspot
Criterion 2 penalizes, and it sits on the heart of the system.

Idiomatic splits that same surface into **separate packages** — `store` (WP6,
blocking), then `lifecycle` (WP7), `switching`+`reporting` (WP8), `core` (WP10)
— each its own files, each independently compilable and testable against
`*store.Store`. The `store` method surface (§2.13) is the one pinned interface
all three behavior packages share, and it is spelled out. Idiomatic's WP tiering
(Tier 0→5, 16 WPs) is more explicit about what can start early: WP13
`autoswitch` can begin against the fakeable consumer interface
`autoswitch.Switcher` (§2.18, fully enumerated) before `core` lands. The
consumer-defined `autoswitch.Switcher` and `tui.Facade` interfaces are written
out method-by-method — genuinely pinned seams.

Idiomatic's one interface-pinning wrinkle: §2.19 declares
`type Manager struct{ sw *core.Switcher }` (session depends on `core`
concretely), yet §5 schedules session as WP9 in **Tier 3**, before `core`
(WP10, Tier 4). As written, session cannot compile in its own tier. This is a
real but narrow flaw — and the fix is to adopt Faithful's `session.Accounts`
consumer interface (see Synthesis graft #2). It doesn't overturn the structural
advantage.

## Criterion 3 — Correct concurrency mapping

**Faithful 8.5 · Idiomatic 8.5**

Near-tie; both are excellent. Both map: `cclock` toucher = `time.Ticker(3s)` +
`stop`/`done` channels stopping on first `Chtimes` error; autoswitch loop =
`stopCh` (closed-once, latching) + `wakeCh` (buffered-1) with clear-at-top;
usage fetch = bounded goroutines with `idx*250ms` stagger, results merged under
mutex, **no lock held across the network**; persist callback re-acquires
`FileLock→cclock.Credentials→cclock.Config` from the unlocked fetch (the reason
`filelock` must be non-reentrant); SIGTERM→`Stop()` installed **only** around
the auto loop; two-clock discipline (wall for persisted/staleness-vs-mtime,
monotonic for acquire/backoff). Both correctly honor the audit's finding that
SIGTERM→stop is the sole handler.

Faithful edges the Ctrl-C story: its dedicated "Ctrl-C → exit 130 story
(precise)" paragraph articulates root-context cancellation, the prompt
`select{ctx.Done()}→"Cancelled"/return nil` local catch vs. top-level
`cserr.ErrInterrupted→130`, and the JSON-vs-plain stderr routing of the cancel
note. Idiomatic edges the wake race: its §4 note that the loop drains/clears
`wakeCh` at the top "so a wake racing a timeout is never lost" is the more
precise statement of that specific hazard, and its `clock.Sleeper` interface
makes toucher/loop timing deterministically testable. These offset.

## Criterion 4 — Idiomatic quality & testability

**Faithful 6.5 · Idiomatic 9**

Idiomatic wins by design intent — which is the point of the exercise. Seams are
**values + injected interfaces**: `credstore.New(cfg Config, kc
keychain.KeychainClient, clk clock.Clock, log)`, `oauth.Client` interface with
HTTP + fake impls, `clock.Clock`/`Sleeper`/`Fake`, `keychain.Fake`,
consumer-defined `autoswitch.Switcher`/`tui.Facade`. Behavior-as-free-functions
over `*store.Store` is idiomatic Go and keeps each behavior independently
unit-testable. The shared test scaffolding (§5: `keychain.Fake`, `clock.Fake`,
`oauth` fake, fixture-`$HOME` builder, Python-produced golden files) is defined
once in WP0 and reused.

Faithful's `credstore.Host`/`session.Accounts`/`migrations.Host` **callback
interfaces satisfied by the god-object `switcher`** are a less idiomatic seam:
`credstore` reads its own `Platform`/`CredentialsDir` back through a live Host
rather than owning config as a value, so testing `credstore` in isolation means
standing up a Host fake for data it should just hold. The 40-method `Switcher`
aggregate is intrinsically harder to test than five focused packages. Faithful
is trading idiomaticity for mechanical spec-review — a legitimate trade, but
this criterion scores the thing traded away.

## Criterion 5 — Scope discipline

**Faithful 8.5 · Idiomatic 8.5**

Tie. Both drop menubar, redesign update off PyPI/uv/pipx (keeping the
`{"timestamp","data"}` 24h cache format for interop), drop `truststore` with an
explicit Windows-verification action item, unify the four atomic-write helpers,
and hand-roll the CLI front controller (no cobra) to preserve the two-layer
pre-dispatch grammar. Both flag the `import`-takes-`FileLock` hardening and the
TUI structured-result-vs-stdout-capture change as deliberate, justified
deviations rather than silent ones. Faithful lists D1–D8; Idiomatic lists 10,
including the explicit "no cobra" *non*-change. Both caught audit Gap 3
(`mountinfo` `docker|overlay` vs cgroup `docker|lxc|containerd|kubepods`) and
Gap 1 (build-time version embedding). Neither invents features. Even scores.

---

## Winner: **Idiomatic**

Priority order is data-compat (1) → parallel implementability (2) → concurrency
(3) → idiomatic quality (4) → scope (5). Faithful wins #1 by ~1 point;
Idiomatic wins #2 by ~2.5 and #4 by ~2.5; #3 and #5 tie.

The decision turns on **the asymmetric graftability of each proposal's
weaknesses.** Faithful's Criterion-1 edge is entirely point-fixes: adopt
`map[string]any` for `usage.UsageEntry.LastGood` and add the switch-log-format
note — two drop-in changes that don't touch Idiomatic's skeleton. Idiomatic's
Criterion-2/4 edge is **structural**: you cannot recover the parallel-agent and
testability benefits inside Faithful without adopting Idiomatic's
store+behaviors decomposition wholesale. When the loser's advantages graft in
cleanly and the winner's do not, you build on the winner. The winning skeleton
is therefore Idiomatic's decomposition, hardened with Faithful's data-compat
conservatism.

---

## Synthesis — Idiomatic skeleton + grafts from Faithful

Base: Idiomatic's `platform`/`clock`/`cerr`/`atomicfile`/`logging`/`printer`/
`jsonout`/`paths`/`keychain`/`ccfile`/`cclock`/`filelock` leaves; `credstore`
(Config-value + injected `KeychainClient`); `oauth`/`usage`/`settings`;
`sessprofile` leaf breaking the store↔session cycle; `store` substrate;
`lifecycle`/`switching`/`reporting` free-function behavior packages; `core`
façade; `session`/`transfer`/`autoswitch`/`migrations`/`update`/`cli`/`tui`
consumers; the consumer-defined `autoswitch.Switcher` and `tui.Facade`
interfaces; the Tier 0→5 WP plan and shared test scaffolding.

Graft the following, precisely:

1. **Usage round-trip fidelity (from Faithful `internal/oauth`/`usagestore`).**
   Reject Idiomatic Deviation #8. Store `usage.UsageEntry.LastGood` as
   `map[string]any` / `json.RawMessage`, and have `oauth.BuildUsageResult`
   return the normalized dict as `map[string]any` (no `float64` coercion of
   `five_hour/seven_day.pct`, per 04 §1.18's "no coercion"). Keep a typed
   `oauth.Usage` view *only* if it's a read-only projection that never becomes
   the persisted form. `RelevantWindows`/`AccountHeadroom`/`jsonout.UsageToJSON`
   operate on the map. This preserves byte-level round-trip against a
   Python-produced `usage.json`.

2. **`session.Accounts` consumer interface (from Faithful `internal/session`).**
   Replace Idiomatic §2.19's `Manager struct{ sw *core.Switcher }` with a
   narrow consumer-defined interface declared *in* `session`
   (`ResolveAccount`/`ReadAccountCredentials`/`WriteAccountCredentials`/
   `ReadAccountConfig`/`AccountKindFor`/`CurrentAccount`/`BackupDir`/`LockFile`/
   `Platform`), satisfied by `*core.Switcher`. This fixes the Tier-3-vs-Tier-4
   scheduling contradiction: `session` (WP9) then compiles and tests against a
   fake in its own tier, before `core` (WP10). Apply the same treatment to
   `transfer` if it holds `*core.Switcher` concretely — pin a narrow consumer
   interface so WP11 can start against a fake.

3. **Switch-history log-line format contract (from Faithful `internal/logging`
   + `switcher`).** Add to Idiomatic's `logging` package the explicit
   requirement that the file handler emit
   `"YYYY-MM-DD HH:MM:SS,mmm - LEVEL - message"` (Python `asctime` with
   **comma**-milliseconds) and that the switch-INFO line read exactly
   `"Switched from account X to Y"` (09 §12), because the Python menubar's
   `parse_switch_history` regex/slicing is a live cross-tool contract. This
   requires a **custom `slog.Handler`** — `slog`'s default `TextHandler` emits
   `key=value` with RFC3339 time and will silently break the parser (see Shared
   Mistake #1). Emit the switch INFO line from the `switching` package.

4. **Explicit non-atomic `cache` write note (from Faithful).** Idiomatic folds
   `cache` into `usage`; carry over Faithful's explicit callout that
   `cache.WriteCache` is the one **deliberately non-atomic, non-chmod'd** writer
   (04 §4), so no agent "improves" it by routing it through `atomicfile`.

5. **`internal/wincred` Windows legacy-read package (from Faithful).**
   Idiomatic's `migrations` says "windows keyring migration" without a
   mechanism; Go cannot read the legacy Windows Credential Manager without
   `x/sys/windows` `CredRead`/`CredDelete`. Add Faithful's Windows-build-tagged
   `wincred` leaf (no-op stub elsewhere) and have `migrations` use it for
   `migrateWindowsKeyringToFiles`; macOS legacy read stays
   `keychain.Get("claude-code", …)`.

6. **File→spec-section tagging discipline (from Faithful).** Idiomatic tags
   packages with spec §; extend to the file level inside the decomposed
   `store`/`lifecycle`/`switching`/`reporting` packages (as Faithful does for
   `internal/switcher`'s files) so spec-fidelity review stays mechanical even
   after decomposition.

7. **Faithful's precise Ctrl-C→130 prose (from Faithful §4) into `cli`.** Adopt
   Faithful's articulation of root-context cancellation + prompt-local
   `"Cancelled"` catch vs. top-level `cserr.ErrInterrupted→130` + JSON-vs-plain
   stderr routing as the WP15 `cli` spec; it is sharper than Idiomatic's table
   row.

Optionally, keep Faithful's `snapshotsrc` as its own small package (rather than
folding `SnapshotSource`+`RunAction` into `tui`) to isolate the
structured-result deviation behind one seam; low stakes either way.

---

## Shared mistakes / gaps (both proposals, verified against specs)

1. **Neither pins the concrete log formatter, and `slog` defaults cannot
   produce it.** Both name `slog` (Faithful returns `*slog.Logger`; Idiomatic
   wraps one). Even Faithful — which correctly identifies the byte-format
   *contract* (09 §12) — proposes `*slog.Logger` without noting that `slog`'s
   `TextHandler` emits `time=… level=… msg=…` key=value pairs with RFC3339 time,
   which does **not** match `"%(asctime)s - %(levelname)s - %(message)s"` with
   comma-milliseconds. Both need an explicit custom `slog.Handler` (or a
   hand-rolled logger) that formats `YYYY-MM-DD HH:MM:SS,mmm - LEVEL - msg`.
   Faithful states the contract; neither states the mechanism.

2. **Semver comparator vs. the actual PEP-440 version string.** Both "fix" the
   `0.22.0b1` update-notice quirk with a "real semver comparator," but
   `0.22.0b1` is not valid semver (`x/mod/semver` requires a `v` prefix and a
   semver-form pre-release like `v0.22.0-beta.1`). Neither reconciles the
   embedded build-version string format with the comparator's expected input.
   Self-resolving once the Go port owns its own `vMAJOR.MINOR.PATCH[-pre]`
   tagging via ldflags, but unaddressed by both and worth an explicit line so
   the embedded version and the remote release-tag comparison agree on format.

3. **SIGINT handler vs. the audit's "do not invent a SIGINT trap."** Both
   install `signal.Notify(SIGINT)`; the audit (§Signal handling) explicitly
   warns the port should "let interrupt propagate … and install a SIGTERM→stop()
   handler only around the auto loop … so it does not invent a SIGINT trap."
   The handler is arguably a Go *necessity* (Go's default SIGINT kills the
   process without printing the cancel note or running JSON-vs-plain routing),
   and both keep its behavior to a faithful KeyboardInterrupt→130 reproduction —
   so this is defensible, not clearly wrong. But neither reconciles its SIGINT
   handler with the audit's caution in writing, and neither states plainly that
   the handler must (a) reproduce, not extend, Python semantics and (b) be
   irrelevant after `syscall.Exec` on the `cswap run` POSIX path. Worth an
   explicit justification so a downstream agent doesn't grow it into new
   behavior.
