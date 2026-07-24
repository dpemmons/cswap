# cswap — Idiomatic Go Architecture

A full-fidelity Go port of `claude-swap`, grounded in the behavioral specs in
`docs/port-spec/01–10` (10-audit.md corrections override the rest).

- Module: `git.dpemmons.com/dpemmons/cswap` · Go 1.25 · single binary `cswap`
  (also installed as `claude-swap`).
- No cgo. macOS Keychain via the `security` subprocess exactly as Python.
- TUI: bubbletea v1.3 + lipgloss v1.1 + bubbles v1.0. HTTP: stdlib `net/http`.
  Extra deps limited to `golang.org/x/sys` (flock/`LockFileEx`, Windows PID
  probe, console VT) and `golang.org/x/text/unicode/norm` (NFC for
  `slugify_email` / keychain service hashing). No other third-party deps.
- Out of scope: the rumps macOS menu bar (`menubar.py`, spec 09 §10). The
  update-check/upgrade subsystem is redesigned for Go (§6, §7 below).

The 4,840-line `switcher.py` is **decomposed**, not transliterated. The
"switcher" becomes a thin façade (`core.Switcher`) over a shared substrate
(`store`) and three behavior packages (`lifecycle`, `switching`, `reporting`)
that operate on it as free functions. Seams are interfaces: credential store,
keychain client, OAuth/usage client, clock/sleeper, and consumer-defined
façade interfaces for the auto engine and TUI.

---

## 1. Package dependency graph (`internal/...`)

No import cycles. Arrows point "depends on". Layers are strictly downward.

```
cmd/cswap ─────────────────────────────────────────────► internal/cli

                                       ┌──────────────► internal/tui
internal/cli ──────────────────────────┼──────────────► internal/autoswitch
   │  (front controller, dispatch,     ├──────────────► internal/transfer
   │   config cmd, exit codes)         ├──────────────► internal/session
   │                                   ├──────────────► internal/update
   │                                   └──────────────► internal/core
   │                                                        │
   ▼                                                        ▼
internal/core  (Switcher façade) ──► lifecycle ─┐   switching ─┐   reporting ─┐
   │                                             │              │              │
   └───────────────► internal/migrations         └──────┬───────┴──────┬───────┘
                                                         ▼              ▼
                                                   internal/store  (substrate)
   ┌───────────────────────────────────────────────────┼───────────────────────┐
   ▼                 ▼            ▼            ▼          ▼           ▼           ▼
credstore        usage        oauth      sessprofile  procdetect  mappings   filelock
   │  ┌──────────────┘            │          │            │          │           │
   ▼  ▼                          ┌┘          │            │          │           │
 ccfile   cclock                 │           │            │          │           │
   │        │                    │           │            │          │           │
   ▼        ▼                    ▼           ▼            ▼          ▼           ▼
 keychain  ── paths ── atomicfile ── platform ── clock ── cerr ── logging ── printer ── jsonout
                                  (leaves — no internal deps beyond each other)
```

Settings sits beside `usage` (depends `paths`, `atomicfile`, `cerr`,
`logging`); `cache` (TTL JSON) is a small file inside `usage`.

### What each package owns

| Package | Owns (spec §) |
|---|---|
| `platform` | `Platform` enum + `Detect()` via `runtime.GOOS`+`WSL_DISTRO_NAME`; `IsWindows()` chmod gate; container detection (env `CONTAINER`/`container`, `/.dockerenv`, cgroup `docker\|lxc\|containerd\|kubepods`, mountinfo `docker\|overlay`). (03§1, 08§15, 10 Gap3) |
| `clock` | `Clock` (wall `Now`), `Sleeper`, `Timer`; `System` + `Fake` impls. Two clocks: wall for persisted state/staleness, monotonic (via `time.Since`) for acquire timeouts/cooldown. (03§9.2, 05§22.5) |
| `cerr` | Typed error set mirroring the Python hierarchy; `Kind` string == Python class name for the JSON `error.type`; `TypeName`, `IsClaudeSwitchError`, constructors. (08§11) |
| `atomicfile` | The single unified atomic-write helper (temp-in-dir → chmod → rename), `WriteJSON`/`WriteJSONValidated` (round-trip re-parse for `sequence.json`), `Write`; parent mkdir + 0700/0600 gated on non-Windows. (01§2.3, 07§9) |
| `logging` | Named `"claude-swap"` logger; **lazy** dir creation on first write; rotating 1 MB × 3. (08§12) |
| `printer` | ANSI styles, color detection precedence (`NO_COLOR`/`FORCE_COLOR`/TTY/Windows-VT/`TERM=dumb`) with first-call caching; `force_color` ctx; Windows console UTF-8 + VT; entrypoint/IDE labels, `abbreviate_path`, `format_age`. (02§11, 08§10) |
| `jsonout` | schema-v1 envelope builders: `account_row`, `usage_to_json`, `error_envelope`, freshness fields, `usageStatus` mapping. camelCase, optional-key omission. (02§10, 08§9) |
| `paths` | All path resolution: backup root (XDG/legacy), Claude config home/global/default-global/credentials, legacy backup dir + `MigrateLegacyBackupDir` protocol. (03§2) |
| `keychain` | `/usr/bin/security` wrapper (get/set/delete/item_exists), exact argv, hex `-X`, single-`\n` strip, rc 44, 5s timeout; `KeychainClient` interface + real + in-memory fake; `IsUnusable(err)` = `KEYCHAIN_ERRORS`. (03§4) |
| `ccfile` | Claude Code file I/O: read `~/.claude.json` as `map[string]any`, key-scoped atomic RMW preserving unknown keys, `oauthAccount` splice, raw `.credentials.json` read/write. (03§3, 03§5.5) |
| `cclock` | proper-lockfile interop: directory `mkdir` mutex, 10s staleness (wall mtime), 3s touch goroutine, 9s acquire (monotonic), `[0.25,0.50)` jitter; `ClaudeCodeLockTimeout`. (03§6) |
| `filelock` | cswap's own advisory lock (`flock`/`LockFileEx`, 0.1s poll, 10s default), non-reentrant. `LockError`. (03§7) |
| `credstore` | `Store` interface + `FileKeychainStore`: active OAuth/managed-key read/write, backup `.enc`/Keychain routing, usability cache (None/T/F + 60s monotonic cooldown + sticky pin), `.prev`, unclaimed stash, `looks_like_api_key`, `approved_form`. (03§5, 01§3) |
| `oauth` | Network client (`Client` interface + HTTP impl + fake): refresh/profile/usage, normalization (`build_usage_result`), classification tokens, `relevant_windows`, `account_headroom`, fingerprint, reset formatting. Owns the normalized `Usage` type. (04§1) |
| `usage` | `UsageStore` (schema v2, identity-guarded, lock protocol), `UsageEntry`/`FetchRecord`, poll policy (`plan_after_fetch`, backoff, due-candidate), TTL `cache`. (04§2–4) |
| `settings` | `settings.json` store, `SETTING_SPECS`, `AutoSwitchSettings` (frozen value), clamp, `config set/get/unset`, `merged_with_cli`, `parse_model_names`. (08§8, 05§2) |
| `sessprofile` | Session-profile leaf: dir naming (`slugify_email`, `session_dir_for`), NFC keychain service hashing, invalidation/stale-marker/delete. (06§1.5, breaks store↔session cycle) |
| `procdetect` | Read Claude Code `sessions/*.json` + `ide/*.lock`, `is_pid_alive` (POSIX `kill 0`, Windows `OpenProcess`). (06§4) |
| `mappings` | `MappingStore`: `mappings.json`, `normalize_path`, nearest-ancestor `resolve`, prune. (06§5) |
| `store` | The substrate: `sequence.json` model + read/write, path wiring, credential proxies (+`_post_backup_write`), identity resolution, org-field backfill, live-session guards, `FileLock` handle, `UsageStore` handle, migrations-at-construct. (01, 03 wiring) |
| `lifecycle` | Free funcs over `*store.Store`: add/add-token/remove/move/swap/alias/disable/enable/purge. (01§5–11) |
| `switching` | switch/switch-to/best/next-available, `performSwitch`, `classifyOutgoing`, replan, self-switch provenance. (02§4–9) |
| `reporting` | list/status renderers + JSON payloads, `collectUsageEntries` (paced, identity-guarded), snapshots, duplicate/lockstep warnings. (02§10–13) |
| `core` | `Switcher` façade: composes `store` + `lifecycle`/`switching`/`reporting`; satisfies the façade interfaces `autoswitch`/`tui`/`session`/`transfer` consume. |
| `session` | `SessionManager`: run/exec-default, bootstrap/reuse, `_sync_sharing`, MCP mirror, share-history. (06§1–3) |
| `transfer` | `.cswap` export/import (reads Python files). (07§1–4) |
| `migrations` | Registry migrations (windows keyring, macos keyring) + `.migrations.json` state; org backfill is in `store`. (07§5–6) |
| `autoswitch` | `AutoSwitchEngine`, events, tick, `run_loop`, quarantine, cooldown, freshen. (05) |
| `update` | Go-redesigned update check + upgrade guidance (§6). |
| `cli` | Two-layer front controller, verb→flag translation, dispatch, cross-flag validation, exit codes, config command. (08§1–7) |
| `tui` | bubbletea app/screens/widgets/modals/theme + `SnapshotSource`. (09§1–9) |

---

## 2. Per-package plan (files, responsibilities, key signatures)

Signatures are sketch-level. All fallible ops return `error` (a `cerr` value at
the domain boundary). "spec" tags the governing section(s).

### 2.1 `platform` — 03§1, 08§15, 10 Gap 3
`platform.go`
```go
type Platform int
const ( MacOS Platform = iota; Linux; WSL; Windows; Unknown )
func Detect() Platform                 // runtime.GOOS + WSL_DISTRO_NAME (non-empty ⇒ WSL)
func (p Platform) String() string      // "macos"/"linux"/"wsl"/"windows"/"unknown" (export tag)
func IsWindows() bool                   // chmod-gate helper
func RunningInContainer() bool          // env + /.dockerenv + cgroup + mountinfo (distinct substr sets)
```
Never calls `platform.system()`-equivalent (no WMI risk in Go; noted for parity).

### 2.2 `clock` — 03§9.2, 04§7.3, 05§22.5
```go
type Clock interface { Now() time.Time }                 // wall clock
type Sleeper interface { Sleep(time.Duration); After(time.Duration) <-chan time.Time }
func Seconds(c Clock) float64                             // Now().UnixNano()/1e9 (usage store / autoswitch clock() seam)
type System struct{}                                     // real
type Fake struct{ t time.Time }                          // tests; advanceable
```
Monotonic elapsed uses `time.Since(start)` (Go embeds the monotonic reading).

### 2.3 `cerr` — 08§11, 02§18
```go
type Error struct { Kind Kind; Msg string; wrapped error }
type Kind string   // "ConfigError","SwitchError","CredentialReadError",... == Python class names
func (e *Error) Error() string; func (e *Error) Unwrap() error
func Config(format string, a ...any) *Error            // + Switch, Session, Validation, AccountNotFound,
                                                       //   CredentialRead, CredentialWrite, Credential, Lock,
                                                       //   ClaudeCodeLockTimeout, Transfer, Migration, MigrationIncomplete
func IsClaudeSwitchError(err error) bool               // any *Error
func TypeName(err error) string                        // Kind string for JSON error.type; "" if not a *Error
```
`ClaudeCodeLockTimeout` and `LockError` are distinct Kinds; `TypeName` round-trips
the external script contract.

### 2.4 `atomicfile` — 01§2.3, 03§9.3, 07§9
```go
type Opts struct { FileMode, DirMode os.FileMode }      // default 0600/0700; skipped on Windows
func Write(path string, data []byte, o Opts) error      // CreateTemp(dir) → chmod → Rename; unlink temp on err
func WriteJSON(path string, v any, o Opts) error        // MarshalIndent(v,"","  ")
func WriteJSONValidated(path string, v any, o Opts) error // re-parse temp before Rename → cerr.Config("Generated invalid JSON")
```
Unifies `_write_json`, `atomic_write_json`, `_atomic_b64_write`, `_atomic_write_file`,
`_mark_applied` (07§9). `WriteJSONValidated` used for `sequence.json`.

### 2.5 `keychain` — 03§4, 03§9
```go
type KeychainClient interface {
    Get(service, account string) (string, bool, error)  // value, found(rc0), err; rc44 ⇒ (_,false,nil)
    Set(service, account, password string) error
    Delete(service, account string) error               // rc0|44 ⇒ nil
    Exists(service, account string) bool                // never errors; rc0 ⇒ true
}
func AccountName() string                               // $USER → os/user → "claude-code-user"
func IsUnusable(err error) bool                         // KeychainError | ctx-deadline | missing-binary (KEYCHAIN_ERRORS)
type Security struct{ Path string; Timeout time.Duration } // real: exec /usr/bin/security
type Fake struct{ m map[[2]string]string }              // conftest block_real_keychain parity
```
`Set` hex-encodes via `-X`, stdin `-i` under 4032-byte limit else argv fallback;
`Get` uses `find-generic-password -a … -w -s …`, `strings.TrimSuffix(out,"\n")`
(not TrimSpace). 5s `exec.CommandContext` timeout → `IsUnusable`-classified.

### 2.6 `ccfile` — 03§3, 03§5.5
```go
func ReadGlobalConfig() (map[string]any, error)         // nil if absent; json→map preserving unknown keys
func UpdateGlobalConfig(mutate func(map[string]any)) error // key-scoped atomic RMW, preserves unknown keys
func ReadCredentialsFile() (string, bool, error)        // raw text, exists
func WriteCredentialsFile(raw string) error             // raw verbatim, atomic, 0600
func SpliceOAuthAccount(configText string, oauth map[string]any) (string, error) // preserve local keys
func ReadOAuthIdentity() (email, orgUUID string, ok bool) // ~/.claude.json oauthAccount
```

### 2.7 `cclock` — 03§6
```go
const ( StalenessS = 10*time.Second; TouchIntervalS = 3*time.Second; DefaultTimeoutS = 9*time.Second )
type Handle struct{ /* dir, stop chan, done chan */ }
func Acquire(lockDir string, timeout time.Duration, clk clock.Clock) (*Handle, error) // ClaudeCodeLockTimeout
func (h *Handle) Release()                               // stop toucher (join ≤1s) then rmdir; tolerate stolen
func CredentialsLockDir() string; func ConfigLockDir() string
```
`Acquire` spawns the touch goroutine (`time.Ticker(3s)`, `stop chan`, stops on first
`Chtimes` error). Staleness = `time.Now().Sub(fi.ModTime()) > 10s`; acquire timeout
monotonic. Jitter `0.25 + rand*0.25`.

### 2.8 `filelock` — 03§7
```go
type FileLock struct{ path string; timeout time.Duration; clk clock.Clock }
func New(path string, timeout time.Duration) *FileLock   // default 10s
func (l *FileLock) Acquire(timeout time.Duration) (bool, error) // false on timeout (no raise)
func (l *FileLock) Release() error                       // safe twice
func (l *FileLock) With(fn func() error) error           // ctx-manager form → cerr.Lock on false
```
POSIX `unix.Flock(fd, LOCK_EX|LOCK_NB)` 0.1s poll; Windows `LockFileEx` 1 byte.
**Non-reentrant** — documented; callers never nest.

### 2.9 `credstore` — 03§5, 01§3
`store.go` (interface), `filekeychain.go`, `active.go`, `backup.go`, `unclaimed.go`, `classify.go`
```go
type Store interface {
    ReadActive() (value string, keychainUnavailable bool, err error) // "" none, nil-string-as-"" per contract
    WriteActive(creds string) error                       // single-axis; OAuth clears managed & vice-versa
    ReadBackup(num, email string) (string, error)         // .enc-wins; "" missing
    WriteBackup(num, email, creds string) error           // retain .prev; routing; reconcile .enc
    DeleteBackup(num, email string) error                 // best-effort sweep incl account-None
    DeleteBackupStrict(num, email string) error           // fail-closed; cerr.Credential "aborting before commit"
    ReadPrev(num, email string) (string, error); DeletePrev(num, email string) error
    KCReadBackup(num, email string) (string, error)       // security-service-only (migrations)
    KCWriteBackup(num, email, creds string) error
    WriteUnclaimed(creds string, ctx map[string]any) (id string, err error)
    ListUnclaimed() (map[string]map[string]any, error)
    LastActiveBackend() string                            // "keychain"|"file"|"" (switch follow-up)
}
func LooksLikeAPIKey(s string) bool; func ApprovedForm(k string) string
func New(cfg Config, kc keychain.KeychainClient, clk clock.Clock, log *logging.Logger) *FileKeychainStore
```
`Config{ Platform, CredentialsDir }`. Usability cache (`*bool`/nil, monotonic
`disabledUntil`, `pinFileMode`) guarded by a mutex if shared. macOS-only Keychain
branch gated on `cfg.Platform==MacOS`.

### 2.10 `oauth` — 04§1
`client.go` (interface+HTTP+fake), `normalize.go`, `windows.go`, `classify.go`, `format.go`, `fingerprint.go`
```go
type Client interface {
    Refresh(ctx context.Context, creds string) RefreshOutcome            // 10s
    Profile(ctx context.Context, accessToken string) *Identity           // 5s; 401 fail-open
    Usage(ctx context.Context, accessToken string) (raw map[string]any, err error) // 5s
}
type RefreshOutcome struct { Credentials string; Error string; TokenAccount *Identity } // "invalid_grant"/"no_refresh_token"/"transient"
type Usage struct { FiveHour, SevenDay *Window; Spend *Spend; Scoped []ScopedWindow }    // normalized; float64 pcts
func BuildUsageResult(raw map[string]any) *Usage
func RelevantWindows(u *Usage, models []string) []Window                 // (label,pct,resetsAt); spend excluded
func AccountHeadroom(u *Usage, models []string) *float64                 // nil = unknown
func CredentialFingerprint(creds string) *string                        // sha256: / sha256-full:
func FormatReset(resetsAt string, now time.Time) (countdown, clock string, ok bool)
func FreshResetStrings(w *Window, now time.Time) (countdown, clock string, ok bool)
func TryFetchUsageForAccount(ctx, c Client, num, email, creds string, isActive bool, persist PersistFn) UsageOutcome
```
Error tokens (`"http-429"`, `"timeout"`, `"network"`, `"bad-response"`,
`"invalid_grant"`, …) are exact literals (04§7.5). `Retry-After` parse: seconds
only, negatives→0, HTTP-date→nil. HTTPError-before-URLError-before-Timeout ordering
via `errors.As` + status inspection. Refresh permanent iff status∈{400,401,403} AND
body contains `invalid_grant`/`invalid_client` (case-sensitive substring).

### 2.11 `usage` — 04§2–4
`store.go`, `entry.go`, `pollpolicy.go`, `cache.go`, `consts.go`
```go
const SchemaVersion = 2
type Identity struct{ Email, OrgUUID string }
type FetchRecord struct { Usage *oauth.Usage; Error, Sentinel string; RetryAfterS *float64 }
type UsageEntry struct { /* Sentinel, LastGood, FetchedAt, AgeS, ... TrustExtended */ }
func (e UsageEntry) DecisionValue() any                  // *oauth.Usage | string sentinel | nil
type Store struct{ path, lockPath string; clk clock.Clock }
func NewStore(cacheDir string, clk clock.Clock) *Store
func (s *Store) Entries(ids map[string]Identity) map[string]UsageEntry     // lock-free read
func (s *Store) Reserve(nums []string, ids map[string]Identity, respectPlans bool) []string
func (s *Store) Record(outcomes map[string]FetchRecord, ids map[string]Identity)
func (s *Store) Claim(...); func (s *Store) SetPollPlan(...); func (s *Store) ClearDeadToken(...)
func DueCandidate(cands []string, entries map[string]UsageEntry, now float64) string
func PlanAfterFetch(in PlanInput) (nextPollAt, intervalS float64)          // rng injectable
```
Legacy/foreign file (missing/corrupt/`schemaVersion!=2`) → empty (reads Python
version-less cache as empty; **reading a v2 Python cache works**). Identity guard
`_matches`. Lock protocol (a) lock-read-reserve (b) fetch unlocked (c) lock-merge-write.

### 2.12 `settings` — 08§8, 05§2
```go
type AutoSwitchSettings struct { /* Threshold, IntervalSeconds, CooldownSeconds, HysteresisPct, Strategy, IncludeAPIKeyAccounts, UnhealthyTicks, Model *string */ }
var SettingSpecs []Spec                                  // single source of truth; dotted key ↔ field
func Load(root string) AutoSwitchSettings                // forgiving clamp
func SetSetting(root, dotted, raw string) (any, error)   // strict; cerr.Config
func UnsetSetting(root, dotted string) (bool, error); func EffectiveSettings(root string) []Effective
func MergedWithCLI(s AutoSwitchSettings, o CLIOverrides) AutoSwitchSettings   // identity when no override
func ParseModelNames(v *string) []string                 // case-insensitive dedupe, first-spelling-wins
```

### 2.13 `store` — 01, 03 wiring
`store.go` (construct + fields), `sequence.go` (read/write/model), `identity.go`
(resolve, org backfill), `credproxy.go` (`_post_backup_write`), `guards.go`
(live-session), `dirs.go`.
```go
type Store struct {
    Home, BackupDir, SequenceFile, ConfigsDir, CredentialsDir, LockFile string
    Platform platform.Platform
    Creds credstore.Store; Usage *usage.Store; Lock *filelock.FileLock
    OAuth oauth.Client; Log *logging.Logger; Clk clock.Clock
}
func New(opts Options) (*Store, error)                   // reproduces __init__ order: home/platform → backup root
                                                         // → MigrateLegacyBackupDir (fallible) → derive paths →
                                                         // logging (lazy) → UsageStore → credstore → migrations.Run
type SequenceData struct { ActiveAccountNumber *int; LastUpdated string; Sequence []int; Accounts map[string]json.RawMessage }
func (s *Store) ReadSequence() (*SequenceData, error); func (s *Store) WriteSequence(*SequenceData) error
func (s *Store) SequenceMigrated() (*SequenceData, error)          // org backfill on read
func (s *Store) ResolveAccount(id string) (num, email, org string, err error)
func (s *Store) FindAccountSlot(d *SequenceData, email, org string) string
func (s *Store) WriteAccountCredentials(num, email, creds string) error   // credstore + _post_backup_write once
func (s *Store) EnsureNoLiveSession(num, email, action string) error      // procdetect via sessprofile
```
Account records kept as `map[string]any`/`json.RawMessage` so `alias`/`kind`/
`disabled` **absence** survives (never zero-filled). `sequence` ints, `accounts`
keys strings, `activeAccountNumber` int|null. Timestamp `2006-01-02T15:04:05Z` UTC.

### 2.14 `lifecycle` — 01§5–11
`add.go`, `addtoken.go`, `remove.go`, `moveswap.go`, `alias.go`, `disable.go`, `purge.go`
```go
func AddAccount(s *store.Store, slot *int, assumeYes bool, alias *string) error
func AddAccountFromToken(s *store.Store, token string, email, slotArg *string, assumeYes bool) error
func RemoveAccount(s *store.Store, id string, assumeYes bool) error
func MoveAccount(s *store.Store, account, target string) (srcNum, tgtNum string, swapped bool, err error)
func SwapAccounts(s *store.Store, a, b string) (numA, numB string, err error)
func SetAlias(s *store.Store, id, alias string) (num, normalized string, err error)
func UnsetAlias(s *store.Store, id string) (num string, err error); func ListAliases(...) 
func SetAccountDisabled(s *store.Store, id string, disabled bool) error
func Purge(s *store.Store) error
```
Interactive prompts (`Prompter` seam injected via `store.Options`) so TUI/tests
supply `assume_yes`/EOF behavior. Move/swap run the whole resolve-validate-mutate
span under one `FileLock` acquisition; best-effort vs fail-closed preserved per call
site (01§14, 02§18) — strict clears return errors that abort the commit; post-commit
cleanup logs and continues.

### 2.15 `switching` — 02§4–9
`switch.go`, `switchto.go`, `strategies.go`, `perform.go`, `classify.go`, `provenance.go`, `replan.go`
```go
func Switch(s *store.Store, strategy *string, jsonOut bool, models []string, modelSrc *string) (any, error)
func SwitchTo(s *store.Store, id string, jsonOut, force bool) (any, error)
func performSwitch(s *store.Store, target string, emit, forceActivate bool, prov *Provenance) (SwitchOp, error)
func classifyOutgoing(...) (kind string, foreignSlot string)     // 02§9 identity oracle
```
Triple lock `FileLock → cclock.Credentials → cclock.Config` in that order (03§7.4);
network refresh runs **outside** the FileLock (non-reentrant), persist callback
re-locks. `SwitchTransaction` rollback (reverse-order restore).

### 2.16 `reporting` — 02§10–13
`list.go`, `status.go`, `collect.go`, `snapshot.go`, `warnings.go`, `render.go`
```go
func ListAccounts(s *store.Store, showTokenStatus, jsonOut bool, fetch map[string]bool) (any, error)
func Status(s *store.Store, jsonOut bool) (any, error)
func CollectUsageEntries(s *store.Store, infos []AccountInfo, fetch map[string]bool) map[string]usage.UsageEntry
func AccountsSnapshot(s *store.Store, fetch map[string]bool) *AccountsSnapshot   // TUI/menubar consumer
func UsageByAccount(s *store.Store) map[string]any                                // strategies (DecisionValue)
```
`CollectUsageEntries` implements the static-sentinel → quarantine → reserve →
staggered fetch → record → replan pipeline (02§13), sentinels never persisted.
Fetch stagger `idx*250ms` via bounded goroutines (§4).

### 2.17 `core` — façade
```go
type Switcher struct{ *store.Store }
func New(opts store.Options) (*Switcher, error)
// thin delegations, e.g.:
func (sw *Switcher) AddAccount(slot *int, yes bool, alias *string) error { return lifecycle.AddAccount(sw.Store, slot, yes, alias) }
func (sw *Switcher) SwitchTo(id string, j, f bool) (any, error)          { return switching.SwitchTo(sw.Store, id, j, f) }
func (sw *Switcher) AccountsSnapshot(fetch map[string]bool) *reporting.AccountsSnapshot { ... }
// ...plus the methods the façade interfaces (§2.18/§2.20) require.
```

### 2.18 `autoswitch` — 05
`engine.go`, `tick.go`, `events.go`, `state.go`, `collect.go`, `select.go`, `freshen.go`, `loop.go`
```go
// Consumer-defined façade interface (idiomatic Go — defined HERE, implemented by *core.Switcher):
type Switcher interface {
    CurrentAccountNumber() *string; HasLiveLogin() bool; AccountEmail(num string) string
    SwitchableAccountNumbers() []string; AccountKindFor(num string) string
    AccountIdentity(num string) map[string]string; ReadAccountCredentials(num, email string) string
    PersistBackupCredentials(num, email, creds string) error; BackfillAccountUUID(num, uuid string)
    UsageEntriesByAccount(fetch map[string]bool) map[string]usage.UsageEntry
    SwitchTo(num string, jsonOut bool) (map[string]any, error)
    LiveSessionPidsFor(num, email string) []int
    SetPollPolicyInputs(threshold float64, models []string); ClearPollPolicyInputs()
    BackupDir() string
}
type Engine struct{ /* sw, settings atomic.Pointer, clk, onEvent, dryRun, stopCh, wakeCh, state file */ }
func NewEngine(sw Switcher, s settings.AutoSwitchSettings, onEvent func(Event), dryRun bool) *Engine
func (e *Engine) Tick() TickOutcome; func (e *Engine) RunLoop() int
func (e *Engine) Stop(); func (e *Engine) Wake(); func (e *Engine) ApplyThreshold(v float64)
type TickOutcome int   // SWITCHED=0 ERROR=1 NO_ACTION=2 BLOCKED=3 (value == --once exit code)
```
State file `autoswitch_state.json` under its own `.autoswitch_state.lock`
(`filelock`), `_mutate_state` never nested under another lock. `Tick()` never panics
out (recover→ErrorEvent, transient).

### 2.19 `session` — 06§1–3
`manager.go`, `bootstrap.go`, `sharing.go`, `mcp.go`, `history.go`, `exec_posix.go`, `exec_windows.go`
```go
type Manager struct{ sw *core.Switcher }
func (m *Manager) Run(id string, tail []string, share, shareHistory bool) error   // execs; never returns (POSIX)
func (m *Manager) ExecDefault(tail []string) error
func (m *Manager) SetupSession(id string, share, shareHistory bool) (dir, num, email string, err error)
```
`sessprofile` leaf holds dir naming/invalidation (used by both `store` and here,
breaking the cycle). MCP mirror + share manifest + history merge as spec §2–3. `_exec`
POSIX = `syscall.Exec` (replaces image; `FileLock` already released); Windows =
spawn+wait, Ctrl+C→130.

### 2.20 `tui` — 09
`app.go`, `dashboard.go`, `switchscreen.go`, `watchscreen.go`, `autoview.go`,
`widgets.go`, `snapshot.go`, `modals.go`, `theme.go`
```go
type Facade interface {   // consumer-defined; *core.Switcher satisfies it
    AccountsSnapshot(fetch map[string]bool) *reporting.AccountsSnapshot
    SwitchTo(id string, jsonOut bool) (map[string]any, error); Switch(...) (map[string]any, error)
    SetAccountDisabled(id string, disabled bool) error; RemoveAccount(id string, yes bool) error
    AddAccount(...) error; AddAccountFromToken(...) error; BackupDir() string
    SetPollPolicyInputs(float64, []string); ClearPollPolicyInputs()
}
func Run(f Facade, start string) int
```
bubbletea model holds single-flight bools; workers are `tea.Cmd`s returning typed
msgs; 3s poll via `tea.Tick`; engine hosted as a long-lived goroutine draining onto
a channel re-armed by a `tea.Cmd`. **Deliberate departure**: mutating ops return
structured results (no stdout capture) — see §6.

### 2.21 `cli` — 08§1–7
`main.go` (front controller), `translate.go`, `parse.go`, `dispatch.go`,
`config.go`, `run.go`, `map.go`, `alias.go`, `swapmove.go`, `auto.go`, `exit.go`
```go
func Main() int
```
Hand-rolled front controller mirroring Python `main()`: pre-dispatch first token
(`run/auto/config/map/unmap/alias/swap/move`) → bare-TTU gate → verb→flag translate
→ main flag parser → cross-flag validation (exit 2) → dispatch → single
`json.MarshalIndent` of the one payload → passive update notice. `--upgrade` runs
before constructing the switcher. Root guard (POSIX geteuid==0 & not container).

---

## 3. Cross-cutting conventions

### 3.1 Error taxonomy → exit-code mapping (mirrors the Python table, 08§11)

`cerr.Kind` strings are the external contract (JSON `error.type`):

| Go `cerr` Kind | Python class | Typical exit |
|---|---|---|
| `ConfigError`, `SwitchError`, `SessionError`, `ValidationError`, `AccountNotFoundError`, `CredentialError`, `CredentialReadError`, `CredentialWriteError`, `LockError`, `ClaudeCodeLockTimeout`, `TransferError`, `MigrationError`, `MigrationIncomplete` | same names | **1** (handled `ClaudeSwitchError`) |

Exit codes (single source in `cli/exit.go`):

| exit | when |
|---|---|
| 0 | success; `--version`/`--help`; TUI/config/run normal return; `auto --once` SWITCHED; upgrade success |
| 1 | any `cerr.IsClaudeSwitchError` (init or dispatch); root guard; `--menubar`; upgrade failure; `auto --once` ERROR |
| 2 | flag parse / cross-flag validation / "no command given" / unknown subcommand-action / mutually-exclusive |
| 3 | `auto --once` BLOCKED |
| 130 | SIGINT (Ctrl-C) — cancelled note, then exit |

`cli` maps: `cerr.IsClaudeSwitchError(err)` → JSON envelope on **stdout** (json mode)
or `printer.Error("Error: "+msg)` on **stderr** → exit 1. `auto --once` uses
`os.Exit(int(engine.Tick()))`.

### 3.2 JSON output envelope discipline (stdout/stderr split, 08§9, 02§17)

- Commands **return** `(payload any, err error)`; they never print JSON. `cli`
  performs exactly one `json.MarshalIndent(payload,"","  ")` to **stdout** after a
  clean dispatch — so stdout carries exactly one JSON document in `--json` mode.
- Handled errors in JSON mode → `jsonout.ErrorEnvelope` on **stdout**, exit 1,
  nothing on stderr.
- `auto` emits **compact** JSONL (`json.Marshal` + `\n`, flushed) per event; its
  error envelope is also compact.
- Human errors → stderr (red). Warnings → stdout (yellow). Passive update notice →
  stderr (muted). Skip-slot export warnings → stderr. Ctrl-C note → stderr if json
  else stdout.
- Optional keys (`alias`, `disabled`, `kind`, freshness fields, `resetsAt`,
  `countdown`/`clock`) are **omitted when unset** (pointers / `omitempty` /
  build-the-map), never zero-valued (load-bearing, 02§18, 08§15).

### 3.3 Atomic-write helper — see `atomicfile` (§2.4)
One helper, temp-in-same-dir → chmod (skip Windows) → rename. `WriteJSONValidated`
adds the re-parse guard for `sequence.json`. Cleanup temp on any error.

### 3.4 Platform detection incl. WSL — see `platform` (§2.1)
`runtime.GOOS` + `WSL_DISTRO_NAME` (non-empty ⇒ WSL). No `/proc/version` sniff.
Container detection uses the two **distinct** substring sets for cgroup vs mountinfo
(10 Gap 3). `platform.IsWindows()` gates every chmod.

### 3.5 Path resolution — see `paths` (§2.7)
One `GetBackupRoot()` used everywhere (XDG on Linux/WSL with absolute-and-`~`-expanded
guard; legacy `~/.claude-swap-backup` on macOS/Windows/unknown). The `.claude.json`
home-root asymmetry and legacy `.config.json` precedence are external Claude Code
contracts (03§2.3).

### 3.6 Logging — see `logging` (§2.6)
Named `"claude-swap"`; **lazy** dir creation on first write (a no-op run must not
materialize `cache/` or the log under the XDG path — would trip the migration
collision check). Rotating 1 MB × 3. Console handler (stderr) only with `--debug`.
Paste-safe invariant preserved (never log email in usage-failure WARNING; 04§1.17).

---

## 4. Concurrency mapping

Every Python thread/loop → Go construct, with lifecycle/shutdown:

| Python | Go | Lifecycle / shutdown |
|---|---|---|
| `claude_locks` daemon toucher thread (`os.utime` every 3s, `threading.Event` stop, join ≤1s) | `cclock.Handle` per-lock goroutine: `time.Ticker(3s)` + `stop chan`; stops on first `os.Chtimes` error (lock stolen) | `Release()` closes `stop`, waits ≤1s, then `os.Remove(dir)`; tolerates stolen/vanished lock (log only). |
| autoswitch `run_loop` (`_stop`/`_wake` `threading.Event`s, `_wake.wait(delay)`, clear-at-top) | goroutine; `stopCh chan struct{}` (closed once, latching), `wakeCh chan struct{}` (buffered-1); inter-tick `select { <-time.After(delay); <-wakeCh; <-stopCh }`. Drain/clear wake at top of loop before checking stop (so a wake racing a timeout is never lost) | `Stop()` closes `stopCh` + non-blocking send on `wakeCh` (idempotent, safe before start); **CLI installs `signal.Notify(SIGTERM)` → `engine.Stop()`** for the loop. `Wake()` after `Stop()` is a harmless no-op. |
| autoswitch `--once` | synchronous `engine.Tick()`; `os.Exit(int(outcome))` | no goroutine. |
| usage fetch `ThreadPoolExecutor().map` with `idx*0.25s` stagger | bounded goroutines (an errgroup or a `sync.WaitGroup` + worker cap); each goroutine `clk.Sleeper.Sleep(idx*250ms)` before its `oauth.Usage` call; results collected into `map[string]FetchRecord` under a `sync.Mutex` (or via a results channel). Context carries per-request 5s deadline | joined before `store.Record`; never holds the usage-store lock across the network calls (lock protocol a/b/c). |
| active-refresh persist callback re-acquiring locks from inside a fetch | network refresh runs with **no** `FileLock` held; the persist callback re-acquires `FileLock → cclock.Credentials → cclock.Config` and re-checks owner/lineage before writing (non-reentrant lock preserved) | callback returns `USAGE_TOKEN_EXPIRED` on mid-refresh owner-appears/lineage change. |
| TUI Textual workers (`run_worker(thread=True, group=…)`) + 3s poll timer | bubbletea: `tea.Tick(3s)` poll; workers are `tea.Cmd`s returning typed msgs (`refreshDoneMsg`/`refreshErrMsg`/`actionDoneMsg`/`engineEventMsg`/`engineStoppedMsg`) handled in `Update`; single-flight = plain `bool` model fields (Update is single-goroutine) | goroutines never touch model state directly — only send messages. Engine hosted as long-lived goroutine draining onto a channel, re-armed by a `tea.Cmd` blocking-receive. |
| `FileLock` (flock/msvcrt, 0.1s poll, 10s) | `filelock.FileLock` `unix.Flock`/`LockFileEx`, monotonic timeout | released on `Release()`/fd-close/process death; non-reentrant. |
| SIGINT / Ctrl-C (Python: no handler, `KeyboardInterrupt` → 130) | `cli` installs `signal.Notify(sigint)`; on receipt prints the cancelled note and `os.Exit(130)`. For `cswap run` POSIX the process is already replaced by `syscall.Exec` (no cswap signal handler after exec); Windows run wrapper mirrors child exit / Ctrl-C→130. The auto loop installs **only** SIGTERM→Stop (matching the single Python signal handler); its Ctrl-C path prints "Auto-switch stopped" → 130. |

Data-race discipline: usability cache (`credstore`) guarded by a mutex if a store
is shared across goroutines (TUI). Store CRUD is single-threaded under `FileLock`.
Two clocks kept distinct: wall (persisted state, lock staleness vs mtime) vs
monotonic (acquire timeouts, keychain cooldown).

---

## 5. Implementation plan for parallel agents

Work packages with explicit files, dependency order, and per-package test strategy.
"Blocking" WPs gate downstream work; same-tier WPs run concurrently.

### Tier 0 — Foundation (1 agent, blocking; ~6 leaf packages)
**WP0** `platform`, `clock`, `cerr`, `atomicfile`, `logging`, `keychain`,
`printer`, `jsonout`, `paths`, `filelock`.
- Files per §2.1–2.8, 2.4, 2.6, 2.5.
- Tests: table tests for platform/container detection (env-var matrices),
  color-detection precedence + caching, atomic-write success/failure/cleanup,
  `keychain.Fake` round-trip incl. `"quotes"`/`\`/`é` and the 4032 argv/stdin split
  (mock `exec`), path resolution (XDG absolute/relative/empty/`~`, `.config.json`
  precedence, migration state machine from 03§2.8 table), `cerr.TypeName`
  round-trip, filelock acquire/contention/reacquire (subprocess holds lock).

### Tier 1 — Core leaves (parallel; 3–4 agents)
- **WP1** `oauth` — depends WP0. Files §2.10. Tests: **httptest** for refresh/profile/
  usage; the full refresh classification matrix (04§1.9), `build_usage_result`
  transformation rules (cents/spend gating/scoped surfacing/null resets_at),
  `relevant_windows`/`account_headroom` unknown-vs-at-limit, fingerprint edge cases,
  `format_reset` local-time/day-no-pad, Retry-After parsing.
- **WP2** `usage` (+ `cache`) — depends WP0, oauth types, filelock. Files §2.11.
  Tests: reserve race semantics, backoff sequences (04§2.6 worked values),
  `plan_after_fetch` worked test values (04§3.4, rng=0.5), trust/age computation,
  legacy/foreign file → empty, **read a Python-produced v2 `usage.json` fixture**.
- **WP3** `ccfile`, `cclock` — depends WP0. Tests: RMW preserves unknown keys;
  oauthAccount splice; cclock acquire/stale-takeover/toucher-keeps-fresh/release-
  tolerates-stolen (03§8 test list); honors `CLAUDE_CONFIG_DIR`.
- **WP4** `procdetect`, `sessprofile`, `mappings`, `settings` — depends WP0. Tests:
  `is_pid_alive` (EPERM-is-alive), session PID/IDE parsing skip-on-malformed,
  `slugify_email` rune-by-rune NFC (the `bø@x.com`→`b__x.com` case), keychain
  service NFC-order-insensitivity, `normalize_path`+nearest-ancestor resolve
  (component-aware, longest-key), settings clamp table (08§8.3).

### Tier 2 — Credential store + substrate
- **WP5** `credstore` — depends WP0, ccfile. Files §2.9. Tests: `.enc`-wins reads
  (corrupt/empty/whitespace fall-through), usability cache flips + 60s cooldown +
  sticky pin, `.prev` one-generation retention (+ Keychain-not-a-file on Mac),
  strict-clear fail-closed, unclaimed stash entry-before-manifest,
  `looks_like_api_key`/`approved_form`. macOS contract tests behind a build tag,
  run on macOS CI against real `security` (03§4.8).
- **WP6** `store` — depends credstore, usage, sessprofile, procdetect, oauth,
  paths, atomicfile, filelock, mappings. **Blocking** for Tier 3. Files §2.13.
  Tests: sequence.json round-trip **byte-golden against a Python-produced file**,
  optional-key omission, identity resolution precedence (number→alias→email,
  ambiguity), org backfill, live-session guard, construction order (migration
  before dir setup). Integration: fixture fake `~/.claude`.

### Tier 3 — Behaviors (parallel; 3 agents) — all depend WP6
- **WP7** `lifecycle` — Files §2.14. Tests: the entire 01§13 edge-case list as
  table tests (sparse slots, move cap, same-email swap staging/rollback, alias
  travel, cross-kind collision, dead-token clear, `.prev` lifecycle, prune-mappings
  on remove/overwrite not migrate/swap).
- **WP8** `switching` + `reporting` (may split into two agents) — Files §2.15/§2.16.
  Tests: strategy scoring (`best` never moves onto worse; ties→earliest;
  next-available anchor drift), `_perform_switch` rollback, classify-outgoing oracle
  (02§9), JSON payloads golden, list/status rendering column alignment, usage-collect
  gating (02§17), snapshot coherence.
- **WP9** `session` — depends WP6 + sessprofile. Files §2.19. Tests: bootstrap/reuse,
  stale-marker deferral while live, sharing symlink/copy + repoint + never-touch-user-
  data, MCP mirror fail-open matrix + adopted-in-sync-takes-no-lock, history merge
  dedupe/collision, `_exec` split (mock exec).

### Tier 4 — Composition & consumers (parallel; 3–4 agents)
- **WP10** `core` façade — depends WP7/8. Thin; implements `autoswitch.Switcher` and
  `tui.Facade`. Tests: interface-satisfaction compile checks + delegation smoke.
- **WP11** `transfer` — depends core/store, credstore, oauth. Files. Tests: **import a
  Python-produced `.cswap` file**, path-traversal defense, `--full` privacy boundary,
  bool-not-int number check, alias self-collision, active-account slim/full, broken-
  slot tolerance, dead-token clear on import.
- **WP12** `migrations` — depends store, credstore, keychain. Tests: windows/macos
  keyring relocation write-verify-delete, `account-None` disambiguation, idempotent
  runner short-circuit, **honor an existing Python `.migrations.json`** (§7 keep list).
- **WP13** `autoswitch` — depends `autoswitch.Switcher` interface (fakeable) → can
  start against a fake before WP10. Files §2.18. Tests: the entire 05§20 edge list
  (hysteresis, at-limit/failover escapes, idle-hold, all-exhausted reset math,
  quarantine lifecycle, run-loop timing/jitter, `pct_label`), FakeClock, fake engine
  Switcher.
- **WP14** `update` — depends paths, cache, printer, platform. Small (§6). Tests:
  semver compare (incl. pre-release notifies), cache TTL/failure-caching, install-
  method detection, Windows print-only.

### Tier 5 — Top level (2 agents)
- **WP15** `cli` — depends everything. Files §2.21. Tests: `_translate_subcommand`
  unit table (08§14), cross-flag validation (each exit-2 message), dispatch routing,
  no-command non-TTY exit 2, `--upgrade` doesn't construct switcher, `run`/`map`/
  `alias`/`swap`/`move`/`auto`/`config` sub-parsers, end-to-end golden CLI output via
  fixture home.
- **WP16** `tui` — depends core façade, autoswitch, settings, oauth, usage, printer.
  Files §2.20. Tests: bubbletea `Update` message handling, single-flight guards,
  cursor-preservation-on-same-set, flash-on-advance, menu structure, threshold
  session-override never persisted, dry↔live carry-forward. `teatest` harness.

**Dependency order summary:** WP0 → {WP1,WP2,WP3,WP4} → WP5 → WP6 → {WP7,WP8,WP9} →
{WP10,WP11,WP12,WP13,WP14} → {WP15,WP16}. WP13 can begin against the fake
`autoswitch.Switcher` before WP10 lands. 6–10 agents fit: one on WP0 then fanning to
the tiers.

**Shared test scaffolding (built in WP0, used everywhere):** `keychain.Fake`,
`clock.Fake`, an `oauth.Client` fake, a fixture-`$HOME` builder (fake `~/.claude`,
`~/.claude.json`, `.credentials.json`), and a set of **Python-produced golden
files** (`sequence.json`, `usage.json` v2, a `.cswap` export, `.migrations.json`)
committed under `testdata/` to enforce data compatibility.

---

## 6. Deliberate deviations from Python behavior

1. **Version comparator uses real semver** — Python's `_parse_version =
   tuple(int(x) for x in v.split("."))` throws on `0.22.0b1` (`int("0b1")`), and
   `check_for_update`'s blanket `except→None` swallows it, so **pre-release builds
   never show an update notice** (spec 10 §Corrections.2). The Go `update` package
   uses a real semver comparator, so a pre-release build **does** get notified. This
   is an intentional bug-fix, not a behavior to replicate.
2. **Update/upgrade subsystem redesigned for Go** (task-mandated). No PyPI/uv/pipx.
   `update.CheckForUpdate` queries a GitHub Releases (or configured version) endpoint
   for the latest tag, compares to the embedded build version, caches with the **same
   `{"timestamp","data"}` 24h-TTL cache format** at `<backup_root>/cache/
   update_check.json` (interop preserved). `update.SelfUpgrade` detects install shape
   (Homebrew / `go install` GOBIN / downloaded release / package manager) and prints
   guidance; on Windows it prints the command instead of self-replacing a locked
   `.exe`. "Any error → no notification" and "cache failures too" behaviors kept.
3. **Version embedded at build time** (`-ldflags "-X …=<version>"` / generated
   `version.go`) — there is no `importlib.metadata` (10 Gap 1). Surfaced at
   `--version`, export `swapVersion`, and the update check.
4. **`truststore`/`_use_native_tls` dropped** — Go `crypto/tls` uses the platform
   verifier (SChannel on Windows) natively, so the Windows OpenSSL stale-intermediate
   workaround is unnecessary (08§15). Action item: verify inactive-account refresh
   against `platform.claude.com` on Windows before finalizing.
5. **macOS menu bar excluded** (task-mandated; spec 09 §10). `cswap --menubar` on any
   platform prints a "not available in this build" message and exits 1 (macOS text
   preserved for parity where reasonable).
6. **Four atomic-write helpers unified into one** `atomicfile` (07§9), keeping the
   JSON round-trip validation only where Python had it (`sequence.json`, via
   `WriteJSONValidated`). Same observable outcome; less code.
7. **TUI mutating ops return structured results instead of capturing ANSI stdout**
   (spec 09 §11.4). Python's `run_action` does a process-global
   `redirect_stdout`+`Text.from_ansi`; the Go lifecycle/switching functions return
   `(payload, []LogLine)` and the bubbletea layer formats its own modal/toast. The
   rendered modal text is therefore **not byte-identical** to Python's captured
   output — a deliberate, documented change (avoids fd-level redirect + its
   single-writer caveat).
8. **`build_usage_result` pct stored as `float64` consistently** — Python leaves
   `five_hour`/`seven_day` `pct` as the raw (possibly int) `utilization` while
   coercing `spend`/`scoped` (04§6). Not observable in tests; Go normalizes to
   `float64` throughout.
9. **Import takes the `FileLock` around its write pass** — Python's `import_accounts`
   does an unlocked per-account RMW (07§9), unsafe against a concurrent `switch`. The
   Go port acquires `FileLock` around the write pass. Single-process observable
   behavior is unchanged; concurrent safety improves. Flagged as an intentional,
   low-risk hardening rather than a silent fix.
10. **CLI front controller preserved as-is (no cobra)** — `run`/`map`/etc. must be the
    first argv token (`cswap --debug run 2` unsupported). A cobra port would silently
    change this (spec 06 §7.3). We keep the exact Python two-layer dispatch for
    fidelity — an explicit decision *not* to "improve" it.
11. **TUI "Next best" candidates panel excludes disabled accounts** — Python's
    `_candidates_text` filter admits any `switchable` account regardless of its
    `disabled` flag, so a disabled account can top the displayed ranking even
    though the auto engine's own candidate set, `SwitchableAccountNumbers`,
    excludes disabled slots and could never pick it — breaking the panel's
    documented contract that the display can never disagree with the pick
    (spec 09 §4.7). The Go filter (`internal/tui/autoview.go`
    `candidatesText`) also excludes `acc.Disabled`; the account card and
    mini-line `(disabled)` marker renders in the warning color rather than
    muted. See A18.

---

## 7. Top 5 data-compatibility / lock-interop risks & defenses

A user points the Go binary at an existing Python `claude-swap` backup directory
and everything must work. The five things most likely to break:

1. **macOS Keychain byte-compatibility with Claude-Code-seeded items.** A divergent
   account name (`$USER` → OS user → literal `"claude-code-user"`), service string
   (`Claude Code-credentials` / `Claude Code` / `claude-swap`), hex `-X` encoding, or
   the single-trailing-`\n` strip keys a *different* item or misreads a value —
   breaking interop on headless hosts and corrupting the active login.
   *Defense:* `keychain` package pins exact argv + `/usr/bin/security` absolute path;
   `AccountName()` replicates `getUsername()`; `Get` uses `TrimSuffix(out,"\n")` (not
   `TrimSpace`); rc 44 handled everywhere; macOS **contract tests run against the real
   `security` binary on CI** (03§4.8), plus an in-memory `Fake` mirroring rc-44/rc-51
   semantics for other platforms.
2. **proper-lockfile interop with a live Claude Code.** If the directory-`mkdir`
   mutex, 10s staleness (wall-clock vs filesystem mtime), 3s touch, 9s acquire, or
   `[0.25,0.50)` jitter diverge, cswap and a refreshing `claude` can stomp each
   other's `~/.claude.json` / credentials during the token-refresh window.
   *Defense:* `cclock` mirrors the protocol exactly — staleness from
   `fi.ModTime()` (wall), acquire timeout monotonic, a touch goroutine on a 3s ticker
   stopped on first `Chtimes` error, `rmdir`+retry stale takeover; integration test
   simulating a held-fresh lock (times out) and a back-dated lock (taken over,
   new mtime fresh), plus a stolen-lock-on-release tolerance test.
3. **`sequence.json` shape fidelity / optional-key omission.** `sequence` as ints,
   `accounts` keys as strings, `activeAccountNumber` int|null, and — critically —
   `alias`/`kind`/`disabled` **absent** (not `""`/`false`/`"oauth"`) drive duplicate
   detection, back-compat identity matching, and rotation. Zero-filling any of them,
   or writing the add-token config's `null` org fields as `""` (or vice-versa),
   silently corrupts behavior.
   *Defense:* records modeled as `map[string]any`/`json.RawMessage`, never a fixed
   struct with `omitempty` guesses; a **byte-golden round-trip test against a
   Python-produced `sequence.json`** (modulo 2-space indent); explicit tests that
   omitted keys stay omitted after a mutate-rewrite; the add-token config-`null` vs
   record-`""` asymmetry pinned by a test.
4. **Usage cache (schema v2) migrate-in-place.** The Python tool's existing
   `cache/usage.json` should keep working. A wrong `schemaVersion` gate, a broken
   identity guard (`email`,`organizationUuid`), or persisting the never-persisted
   `sentinel`/computing `age_s`/`trust_extended` at write time would either discard a
   valid cache or serve another account's usage after slot reuse.
   *Defense:* `usage.Store` treats missing/corrupt/`!=2` as empty (never crashes),
   keeps `_matches` identity-guard on every read/write, computes trust/age at read
   time only; a test **reads a Python-produced v2 `usage.json` fixture** and asserts
   identity-mismatch rows are invisible and replaced on write.
5. **`.enc` backups + fail-closed/best-effort semantics.** base64 with strict
   `validate=True` decode + the non-empty guard, `.enc`-wins reads, and — most
   dangerous — the per-call-site split between **fail-closed** transactional clears
   (`DeleteBackupStrict`, move/swap required-clears that *abort the commit*) and
   **best-effort** post-commit cleanup/`.prev`/session-profile/Keychain deletes.
   Getting this backwards either bricks switching on a flaky Keychain or leaks a
   stale credential onto a reused slot (01§14, 03§9.4).
   *Defense:* Go threads explicit `error` returns through every helper (no blanket
   swallow); strict clears return errors that propagate to abort; best-effort sites
   log-and-continue by explicit choice; base64 via `base64.StdEncoding.DecodeString`
   (errors on junk) + non-empty guard; the 01§13 / 02§17 edge-case lists become table
   tests (unbacked-relocation clears stale target key, metadata-write-is-commit-point,
   same-email swap staging durability, `.enc`-wins fall-through). Legacy migrations
   (windows/macos keyring, legacy→XDG dir) are **kept** and honor an existing Python
   `.migrations.json` (07§9) so a long-gap upgrader's data is rescued exactly once.

---

## Appendix — assembly & construction order (parity-critical)

`core.New` / `store.New` reproduces `ClaudeAccountSwitcher.__init__` order exactly
(07§5.6): (1) resolve `home`/`platform`; (2) `GetBackupRoot()`; (3)
`MigrateLegacyBackupDir` — the one construction step that may return a hard
`MigrationError` aborting startup; (4) derive `sequenceFile`/`configsDir`/
`credentialsDir`/`lockFile`; (5) lazy logging + `usage.NewStore`; (6) `credstore.New`;
(7) `migrations.Run` — must **never** abort construction (every error caught, logged,
left for retry). Swapping (3) and (7), or making (7) fallible, is an observable
regression.


---

# Amendments (final design decisions — these OVERRIDE anything above)

These amendments state final design decisions. Where an amendment contradicts
the body above, the amendment wins.

## A1. Usage round-trip fidelity (REJECTS Deviation #8)

`usage.UsageEntry.LastGood` is persisted as `map[string]any` (or
`json.RawMessage`), NOT as a typed struct. `oauth.BuildUsageResult` returns the
normalized dict as `map[string]any` with **no numeric coercion** of
`five_hour`/`seven_day` `pct` (04§1.18: the raw `utilization` value passes
through as-is — int stays int in JSON round-trip). Any typed `oauth.Usage`
struct is a **read-only projection** for decision logic (strategies,
autoswitch, TUI) and is never the persisted form. What Python writes into
`usage.json`, Go must re-write byte-compatibly (modulo key order/indent).

## A2. Consumer-defined interfaces for session and transfer

`internal/session` does NOT hold `*core.Switcher`. It declares:

```go
// session package
type Accounts interface {
    ResolveAccount(id string) (num, email, org string, err error)
    ReadAccountCredentials(num, email string) (string, error)
    WriteAccountCredentials(num, email, creds string) error
    ReadAccountConfig(num, email string) (map[string]any, error)
    AccountKindFor(num string) string
    CurrentAccountNumber() *string
    BackupDir() string
    Platform() platform.Platform
    SlotForDirectory(dir string) (slot *string, email *string, err error)
}
```

satisfied structurally by `*core.Switcher` (compile-asserted in `cli`).
`internal/transfer` gets the same treatment with its own narrow interface
(resolve/read/write accounts + sequence access + backup dir). Neither package
imports `core` or `store`. This lets WP9/WP11 build and test against fakes
before the substrate is final.

## A3. migrations.Host consumer interface (breaks store→migrations→store cycle)

`internal/migrations` declares a `Host` interface exposing exactly what the
registry migrations need (backup dir, credentials dir, sequence read,
`credstore` handles, keychain client, platform, logger, migration-state file
path). `store.New` constructs an adapter and calls `migrations.Run(host)` as
construction step 7. `migrations` never imports `store`.

## A4. Log-format contract is a hard interop requirement (custom handler)

Spec 09§12: the TUI switch-history reader parses the log file produced by the
logging package, format `YYYY-MM-DD HH:MM:SS,mmm - LEVEL - message` (Python
`%(asctime)s`, **comma** milliseconds) and the switch INFO line is exactly
`Switched from account X to Y`. Neither `slog.TextHandler` nor any stock Go
logger emits this. `internal/logging` therefore implements a small custom
formatter (hand-rolled; slog optional underneath but the on-disk bytes are the
contract). The `Switched from account X to Y` line is emitted by the
`switching` package. Golden test: write lines, re-parse with the TUI history
reader, and cross-check against a Python-produced log-line fixture.

## A5. Version scheme + comparator (resolves the 0.22.0b1 confusion)

The Go port's own versions are **semver with a leading `v`** (`v0.1.0`,
`v0.2.0-beta.1`), embedded at build time via
`-ldflags "-X git.dpemmons.com/dpemmons/cswap/internal/version.Version=v0.1.0"`
(package `internal/version`, default value `v0.0.0-dev`). `--version` prints
`<prog> <version>` with the `v` stripped for display parity. The update
comparator is `golang.org/x/mod/semver` (approved dep) on the `v`-prefixed
strings; pre-releases DO get update notices (intentional fix of the Python
quirk, Deviation #1). The export envelope `swapVersion` carries the display
form (no `v`). Never feed a PEP-440 string to the comparator; the Python
tool's versions are irrelevant to the Go update check.

## A6. Update endpoint

`update.CheckForUpdate` queries the Forgejo releases API of the canonical
repo: `https://git.dpemmons.com/api/v1/repos/dpemmons/cswap/releases/latest`
(response field `tag_name`, same as GitHub's schema), 3s timeout, any error →
no notice, negative results cached too. Endpoint string is an `internal/update`
variable overridable via ldflags. Cache file keeps the Python
`{"timestamp": <unix>, "data": ...}` 24h-TTL format at
`<backup_root>/cache/update_check.json`. `SelfUpgrade`: if the running binary
resolves inside `$GOBIN`/`$GOPATH/bin`/`$HOME/go/bin`, run
`go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest`; otherwise print
download/upgrade guidance (Windows always print-only).

## A7. SIGINT handling rationale (audit 10 §Signal handling)

The audit warns "do not invent a SIGINT trap" — in Python, no handler exists
and `KeyboardInterrupt` unwinds to the exit-130 paths. In Go, default SIGINT
delivery kills the process before the cancel note / JSON-vs-plain routing can
run, so `cli` MUST install a SIGINT notifier to **reproduce** (never extend)
Python's semantics: print the exact cancelled note, route stderr-vs-stdout by
JSON mode, exit 130. Interactive prompts convert cancellation into the
prompt-local "Cancelled" behavior. On the POSIX `cswap run` path the handler
is irrelevant after `syscall.Exec` (process image replaced). The auto loop
additionally installs SIGTERM→`engine.Stop()` — the only signal handler the
Python code has.

## A8. Explicitly non-atomic cache writer

`usage`'s TTL `cache` file writer (04§4) is the ONE deliberately non-atomic,
non-chmod'd writer in the codebase (plain create/truncate write). Do not route
it through `atomicfile`; a comment in the code marks it deliberate.

## A9. `internal/wincred` build-tagged leaf

The Windows legacy-keyring migration (`migrateWindowsKeyringToFiles`) uses a
tiny `internal/wincred` package (build tag `windows`) wrapping
`golang.org/x/sys/windows` CredRead/CredDelete for generic credentials. On
macOS the legacy read stays `keychain.Get("claude-code", ...)`. Non-Windows
builds compile a stub returning not-found.

## A10. File-level spec tagging

Every file in `store`, `lifecycle`, `switching`, `reporting` carries a header
comment naming the spec sections it implements (e.g. `// Implements spec
01§5.2–5.4 (add --slot displacement).`) so fidelity review stays mechanical
after the decomposition.

## A11. snapshotsrc seam

`reporting.AccountsSnapshot` + a small `RunAction` structured-result seam live
in their own file group inside `reporting` (not a separate package), but the
TUI consumes them ONLY through `tui.Facade` — keeping Deviation #7 isolated
behind one seam.

## A12. Dependency freeze (go.mod is frozen)

Allowed third-party deps, already in `go.mod`:
`github.com/charmbracelet/bubbletea` v1.3.x, `charmbracelet/lipgloss` v1.1.x,
`charmbracelet/bubbles` v1.0.x, `golang.org/x/sys`, `golang.org/x/text`,
`golang.org/x/mod` (+ transitive). Implementation agents MUST NOT add
dependencies or edit `go.mod`/`go.sum`. `internal/deps/deps.go` blank-imports
the charm stack until the TUI lands (keeps `go mod tidy` from pruning); it is
deleted in the final integration pass.

## A13. Pinned cross-package interfaces

The `autoswitch.Switcher` (§2.18) and `tui.Facade` (§2.20) interface method
sets as written in this document are FROZEN. `core.Switcher` implements them;
`cli` carries the compile assertions
(`var _ autoswitch.Switcher = (*core.Switcher)(nil)`, etc.). An agent needing
a signature change must change it in BOTH the consumer interface and core in
the same work package, and note it in the commit/summary.

## A14. Genuine Python-produced fixtures

`testdata/python-fixtures/` contains files generated by actually running the
Python claude-swap (add-token accounts — no network needed —, aliases,
disable, mappings, settings, an export file, `.migrations.json`, and a
hand-written spec-conformant `usage.json` v2 where network would be required).
Golden tests read these to enforce data compatibility. Do not regenerate or
hand-edit them casually; a README in that directory records how they were made.

## A15. At-limit surfacing (Go-side additive extension)

`cswap list` / `cswap status` (human and `--json`) and the TUI dashboard/watch
views surface when an account sits at a rate limit, folding in the per-model
weekly windows configured via the existing `autoswitch.model` setting. An
account whose named "Fable 5" weekly window is exhausted reads as at-limit even
when the account-wide 7d window has room ("expired when EITHER is expired").

**Source & semantics.** Models come from `settings.Load(backupDir).Model`
through `settings.ParseModelNames` (comma lists and the `all` sentinel) — no new
CLI flag and no new settings key; `list`/`status` stay flag-free (CLI-surface
parity). The decision reuses the same projection the switch strategies and auto
engine decide with: `oauth.RelevantWindows(usage, models)` /
`oauth.AccountHeadroom(usage, models)`, computed only for accounts whose
decision-grade usage is a usage map (`usageStatus` "ok"). `atLimit` ⇔
`AccountHeadroom != nil && <= 0`; `limitingWindows` is every relevant window
with `pct >= 100`, in `RelevantWindows` order (e.g. `["7d","Fable 5"]`). A nil
headroom (no window data) is *unknown*, never at-limit. With the setting unset
the decision runs on 5h/7d alone — uniform semantics, no special case.

**Fields.** `reporting.AccountSnapshot` carries `AtLimit bool` /
`LimitingWindows []string`. The `--json` list rows and the status active-account
object gain `"atLimit": true` and `"limitingWindows": [...]` — but only when
at-limit; both keys are OMITTED when false. Human surfaces append an
`at limit: <labels>` marker beside the existing row markers; the TUI shows the
same compact marker on the account card.

**Additive-contract argument.** This is additive under schemaVersion 1: no
existing field changes and `usageStatus` values are untouched (an at-limit "ok"
account is still `usageStatus: "ok"` plus the new optional keys), following the
same optional-key omission discipline as `alias`/`disabled`/the freshness
fields. It is a deliberate Go-side extension with no Python counterpart: the
Python original shows per-model pct rows but its `usageStatus` and list markers
ignore them, so this adds signal without breaking any documented shape. The
Python-fidelity contract in `docs/port-spec/` is unchanged.

## A16. `cswap env` — pin a shell to an account (Go-side additive extension)

`cswap env [NUM|EMAIL|ALIAS] [--no-share] [--share-history] [--shell sh|fish|pwsh]
[--unset] [--debug]` gives the CURRENT shell a per-account Claude Code identity —
`eval "$(cswap env 2)"` — by preparing the same persistent session profile
`cswap run` uses and then PRINTING shell-evalable env lines instead of exec'ing
claude. It is a deliberate Go-side extension with no Python counterpart, added to
the `--help` command list (one line) and epilog as a documented deviation from
the verbatim Python help text; `docs/port-spec/` is untouched.

**Surface & dispatch.** Pre-dispatched on the first argv token exactly like
`run`/`map`/`alias` (front-controller pre-dispatch list + its own flag parser +
`_guard_root` + the `ClaudeSwitchError`→exit 1 / Ctrl-C→130 wrapping). `--shell`
defaults to `sh`; an invalid value is a usage error (exit 2) listing the choices.
`--unset` skips account resolution and bootstrap entirely and prints only the
`CLAUDE_CONFIG_DIR` unset line for the chosen shell; `--unset` with an account
argument is a usage error (exit 2).

**Account resolution** is identical to `run`: explicit `NUM|EMAIL|ALIAS`, else the
cwd's directory mapping, else error (exit 1). Unlike `run` there is NO
default-login fallback — an unset `CLAUDE_CONFIG_DIR` IS the default, so the
error points the user at passing an account, mapping the directory, or
`cswap env --unset`.

**Profile preparation** reuses the existing session machinery verbatim:
`session.Manager.SetupEnv` calls the SAME exported `SetupSession`
(bootstrap/validate/mirror/share, honoring `--no-share`/`--share-history`) that
`Run` calls — no duplicated bootstrap logic. The one intentional divergence from
`Run` is the same-account case: `Run` has an exec fast path when the requested
account is already the active default login, but a shell pinned to a profile is
still pinned even when the default currently matches, so `env` does NOT
short-circuit — it prints an informational stderr note and still emits the
export of the session profile.

**Output discipline (critical).** stdout carries ONLY the eval-able lines; every
notice/warning goes to stderr. The `SessionManager`'s message sink is wired to
stderr, so its bootstrap/scrub/"Prepared" notices never pollute the eval stream.
Emitted lines: for each `AUTH_OVERRIDE_ENV_VARS` currently set, a shell-specific
unset line (`unset VAR` / `set -e VAR` / `Remove-Item Env:VAR …`) plus ONE
stderr warning naming them (the `run` scrub wording adapted for env); then the
export line (`export CLAUDE_CONFIG_DIR='<dir>'` with POSIX single-quote escaping
/ `set -gx CLAUDE_CONFIG_DIR '<dir>'` / `$env:CLAUDE_CONFIG_DIR = '<dir>'`), each
quoted for its shell so a profile path containing a single quote survives.

**Staleness.** The shell keeps the pinned profile until the user re-evals; after
switching accounts or a credential change they re-run the `eval` or use
`cswap run` (which re-prepares on every launch). Claude Code's live-session
guards keep working because it writes its session files inside the profile
directory the export points at.

## A17. `autoswitch.strategy: soonest-reset` — renewal-ordered candidates (Go-side additive extension)

`autoswitch.strategy` (`internal/settings/settings.go` `SettingSpecs`, kind
`KindChoice`) gains a second choice, `soonest-reset`, alongside the existing
`best`. Default stays `best`; an invalid persisted value still falls back to
the default via the existing `KindChoice` clamp — no new fallback path. It is
a deliberate Go-side extension with no Python counterpart: Python's auto-switch
has no ranking axis beyond most-headroom, so this adds a second one without
touching the Python-fidelity contract in `docs/port-spec/`.

**Ordering only.** `internal/autoswitch/tick.go` `selectCandidates` changes
exactly the sort of the already-built `qualifying` slice; it changes no gate.
Known and positive headroom (`headroom != nil && > 0`), the cooldown,
quarantine, and the API-key last resort are IDENTICAL for both strategies and
every trigger (`proactive`, `at-limit`, `failover`). The threshold-landing
and hysteresis checks apply only when `trigger == "proactive"`: an `at-limit`
or `failover` tick has no in-force active account to land under the
threshold against or beat by the hysteresis margin, so any candidate with
positive headroom qualifies under those two triggers regardless of where it
sits relative to the threshold.

`sortQualifying` gains the threshold as a parameter —
`sortQualifying(qualifying []qual, strategy string, threshold float64)`,
called with `s.Threshold` — because the two-tier `soonest-reset` order below
needs it even though it changes no qualification gate. `best` stays
byte-identical to today: headroom descending, `sort.SliceStable` so ties keep
sequence order; the threshold parameter is unused on this branch.

`soonest-reset` becomes a two-tier comparator. Tier A holds every qualifying
candidate whose utilization sits below the threshold — `(100 - h) <
threshold` — and orders exactly as before: known renewal before unknown,
known renewal ascending, then headroom descending, then sequence order (via
`sort.SliceStable` over the sequence-ordered list) for remaining ties. Tier B
holds every remaining qualifying candidate — utilization at or above the
threshold, headroom still positive since an at-limit account never qualifies
at all — and orders by headroom descending alone, as a last resort. Every
tier-A candidate sorts before every tier-B candidate: an account at or above
the threshold is never preferred over one below it for an earlier renewal.
Under the `proactive` trigger tier B is always empty, since the
threshold-landing gate above already excludes any candidate at or above the
threshold from qualifying — proactive ordering is therefore unchanged from
tier A alone. The tiering is observable only under the `at-limit` and
`failover` triggers, where the threshold-landing gate does not apply and an
at/above-threshold candidate can reach `qualifying`. Engine events are
UNCHANGED — no new fields, `Poll`/`Switch`/`NoSwitch` stay byte-stable; the
strategy is observable only through which account gets picked.

**`oauth.RenewalTS` projection.** `internal/oauth/windows.go` gets one new
exported function, `RenewalTS(u *Usage, models []string) *float64`, returning
an account's renewal as epoch seconds: the LATEST parseable `resets_at` among
`RelevantWindows(u, models)` restricted to weekly-scope windows — the `7d`
window plus every scoped per-model window `autoswitch.model` matches — with
the `5h` window excluded (it is not weekly). An entry with an absent or
unparseable `resets_at` is skipped; if no weekly window has a parseable
`resets_at`, the result is `nil` (unknown). Parsing reuses the accepted
layouts from `autoswitch/reset.go`'s private `parseResetTS`
(`time.RFC3339Nano` then `time.RFC3339`, empty → `nil`) via a small unexported
helper local to `oauth` — `autoswitch`'s `parseResetTS` stays unexported;
`oauth.RenewalTS` is the one shared, exported entry point and both
`autoswitch` and `tui` call it. `selectCandidates` threads the engine's
per-account usage (the `usageMap` built in `tickInner`) alongside the existing
headroom map so it can compute renewal per candidate.

**TUI visibility (`internal/tui/autoview.go`).** `summaryText` appends
` · soonest-reset` (plain segment, after the `poll every Ns` segment) only
when `a.settings.Strategy != "best"`. `candidatesText` computes
`oauth.RenewalTS` from `acc.Usage.LastGood` via `oauth.NewUsage` (mirroring how
it already derives `bindingPct`) and, only under `soonest-reset`, ranks by a
six-tier ladder (`candidateRank.tier`, ordered by `candidateLessSoonest`)
that mirrors the engine's tier-A/tier-B split by the same threshold —
`a.settings.Threshold`, the session-adjusted value the engine itself receives
via `ApplyThreshold`. The method is on `autoScreen`, which already holds
`a.settings`, so the threshold reaches both the tier assignment in
`candidatesText` and `candidateLessSoonest` without a new parameter on either.
The ladder: tier 0 (binding pct below the threshold, known renewal) by
renewal ascending; tier 1 (pct below the threshold, unknown renewal) by pct
ascending; tier 2 (pct at or above the threshold and below 100 — headroom
remains, but the candidate is a last resort, mirroring the engine's tier B)
by pct ascending, i.e. headroom descending; tier 3 (pct at or over 100,
at/over limit) by renewal ascending with unknown renewal last in the tier,
then pct ascending; tier 4 the existing sentinel (`bestKey` `998`); tier 5
the existing usage-unknown case (`999`). Ties at every tier resolve by
account number ascending, same as today. Under `best` the panel's ranking is
untouched.

## A18. TUI "Next best" panel excludes disabled accounts (Go-side deviation)

`internal/tui/autoview.go`'s `candidatesText` filter (spec 09§4.7) gains one
clause: skip `acc.Disabled`, alongside the existing exclusion of the active
account and `!acc.Switchable`. This is a deliberate Go-side deviation from
the Python original, recorded as an amendment rather than folded silently
into the port, because the two filters read identically on paper while
`Switchable` alone does not carry the meaning the auto engine assigns to
its own candidate set.

**The defect.** `reporting.AccountSnapshot.Switchable` reports only whether
a slot has stored credentials and config (`store.AccountIsSwitchable`);
`Disabled` is a separate, independent field. The auto engine's candidate
set, `store.SwitchableAccountNumbers` (`internal/store/identity.go`),
requires both: `AccountIsSwitchable && !disabled`. Python's
`_candidates_text` carries the identical `switchable`-only filter
(`switchable and account.number != active_number`; spec 09§4.7), so a
disabled account with stored credentials can top the Python panel's ranking
as well, though the engine can never select it. The defect is inherited
from the Python original, not introduced by the port.

**The correction.** The panel's documented contract, stated in Python's own
source comment and carried into spec 09§4.7, is that it "uses the exact
same model axis... so the displayed ranking can never disagree with the
account it picks." A disabled account ranked at the top of a display the
engine will never act on breaks that contract on its face. The Go filter
adds the missing disabled clause; the panel never shows a disabled
account.

**Quarantine labeling.** The panel also reads the engine's quarantine
state. `autoswitch.ReadQuarantine(statePath)` (`internal/autoswitch/state.go`)
is a read-only, unlocked reader over `autoswitch_state.json`'s `quarantine`
map, returning slot number to reason with the empty string standing for an
entry that carries no readable reason. Its read is tolerant and lock-free,
matching the engine's own `readState` at tick start exactly — missing
file, parse error, or a non-object top level all yield an empty map — and
`readState` and `ReadQuarantine` share one tolerant-read helper, so the two
stay byte-identical by construction. `autoScreen` holds
the result in a `quarantined map[string]string`, refreshed in both
`onMount` and `onSnapshot` from `m.facade.BackupDir()` (mirroring how
`loadThreshold` locates the settings file).

A quarantined slot is not dropped the way a disabled slot is: disabling is
a durable, user-chosen exclusion from the auto surface, so hiding the row
costs the user nothing, while quarantine is transient and self-healing — it
clears itself, with no action on the auto surface, the moment the slot's
stored credential is replaced (`account-replaced` / `credentials-replaced`,
spec 05§14). Dropping a quarantined row would tell the user only that a
healthy-looking candidate is gone, not why or what to do about it.
`candidatesText` labels the row instead: number and email render as
usual, and the usage/sentinel cell is replaced by `"quarantined (<reason>)"`,
or plain `"quarantined"` when the reason is empty, in the warning color
(`colSevWarn`). Quarantine takes precedence over sentinel and usage
rendering for that row — a quarantined slot's cached usage or sentinel
value never reaches the panel once quarantined.

Quarantined rows rank into the non-viable tail, above the sentinel and
usage-unknown rows. Under `best`, `bestKey` `997` sorts after every pct row
(pct is `<=100`) and before the `998` sentinel and `999` usage-unknown
keys. Under `soonest-reset`, the tier ladder gains a non-viable tier and
runs tier 0/1/2/3 (viable, as above; 3 = at/over limit) < tier 4
(quarantined) < tier 5 (sentinel) < tier 6 (usage-unknown). Ties resolve by
account number ascending, as at every other tier. This is the panel's
contract restated in its strict form: a row ranked as a viable target
(tier 0-3, pct `<=100`) is always one the engine could pick this tick,
modulo cache freshness; every row the engine cannot pick — disabled
excepted, which is dropped rather than shown — is labeled with the reason
it cannot be picked. The existing "no other switchable accounts"
empty-state line is unchanged and covers the all-remaining-disabled case;
it does not cover an all-remaining-quarantined case, since a quarantined
slot still contributes a labeled row rather than being excluded from the
ranked set.

**Marker color.** The account card and the mini list line
(`internal/tui/widgets.go`, `~173` and `~249`) render their `(disabled)`
marker in the warning color (`colSevWarn`) in place of the muted color
(`colMuted`); text, spacing, and position are unchanged. This tracks the
marker's meaning under this amendment: a disabled slot is a valid explicit
switch/watch target but is never an automatic one. CLI (non-TUI) output is
untouched — `cswap list`'s `(disabled)` marker
(`internal/reporting/list.go`, via `printer.Muted`) stays exactly as it is,
preserving byte-fidelity with the Python original for CLI surfaces.
