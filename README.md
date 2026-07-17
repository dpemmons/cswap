# cswap

Multi-account switcher for Claude Code, as a single static Go binary. Switch
between multiple Claude accounts without logging out, let it auto-switch
before you hit a rate limit, track every account's usage in a live TUI
dashboard, and run accounts in parallel. Works with the Claude Code CLI and
the VS Code extension.

This is a full-fidelity Go port of
[claude-swap](https://github.com/realiti4/claude-swap) (Python, MIT, by Onur
Cetinkol) — same commands, same on-disk formats, same `--json` schemas. A
backup directory created by the Python tool works in place with this binary,
and vice versa. The behavioral specs the port was built against live in
[`docs/port-spec/`](docs/port-spec/), and the architecture in
[`docs/DESIGN.md`](docs/DESIGN.md).

## Install

```bash
go install git.dpemmons.com/dpemmons/cswap/cmd/cswap@latest
# or from a checkout:
make install
```

## Usage

Log into Claude Code, then:

```bash
cswap add                       # snapshot the current login as an account
cswap add                       # (after logging in with another account)
cswap switch                    # rotate to the next account
cswap switch 2                  # or by number, email, or alias
cswap list                      # dashboard: 5h/7d usage + reset times
cswap                           # full-screen TUI (also: cswap tui / cswap watch)
```

Automatic switching before you hit a limit:

```bash
cswap auto                      # foreground loop, polls every 60s
cswap auto --threshold 80       # switch earlier
cswap auto --once --json        # single check, for cron (exit 0/1/2/3)
```

Run two accounts in parallel (session mode — this terminal only):

```bash
cswap run 2                     # launch Claude Code as account 2, here only
cswap run 2 -- --resume         # args after '--' go to claude
cswap map 2 ~/work/client-app   # bare `cswap run` in that dir → account 2
```

Everything else:

```bash
cswap status | add-token | remove | disable | enable | alias | move | swap
cswap config [set KEY VALUE]    # settings.json with validation
cswap export backup.cswap       # accounts to a file (import on another box)
cswap import backup.cswap
cswap upgrade | purge | help
```

`list`, `status`, and `switch` take `--json` (schemaVersion 1, exactly one
JSON document on stdout). The original long-flag spellings (`cswap --switch`,
`cswap --list`, …) keep working.

## Data locations

| Platform | Credentials | Backup root |
|----------|-------------|-------------|
| Linux / WSL | file-based, under the backup root | `${XDG_DATA_HOME:-~/.local/share}/claude-swap/` |
| macOS | login Keychain (service `claude-swap`) | `~/.claude-swap-backup/` |
| Windows | file-based, under the backup root | `~/.claude-swap-backup/` |

Switches cooperate with Claude Code's own credential locks (the npm
`proper-lockfile` directory-lock protocol), so a swap never interleaves with a
token refresh.

## Differences from the Python original

- Single static binary; no Python runtime.
- The macOS menu bar app is not included.
- `cswap upgrade` / the passive update notice use this repo's releases and
  `go install` instead of PyPI/uv/pipx.
- Pre-release builds do get update notices (fixes a quirk in the original's
  version comparator).
- `cswap list` / `cswap status` (human and `--json`) and the TUI surface an
  "at limit" marker when an account has a relevant window at/over its rate
  limit, folding in the per-model weekly windows configured via the existing
  `autoswitch.model` setting (an exhausted "Fable 5" weekly window reads as
  at-limit even when the account-wide 7d window has room). This is a Go-side
  additive extension: the Python original shows per-model pct rows but its
  `usageStatus` and list markers ignore them. The `--json` fields (`atLimit`,
  `limitingWindows`) are additive under schemaVersion 1 — present only when
  at-limit, omitted otherwise; no existing field changes (see `docs/DESIGN.md`
  Amendment A15).
- All other observable behavior — command grammar, JSON schemas, exit codes,
  file formats, lock protocol — is preserved; deliberate deviations are listed
  in `docs/DESIGN.md` §6 and its Amendments.

## Development

```bash
make help          # authoritative target list
make build         # binary with embedded version
make test          # go test ./...
make race          # go test -race ./...
```

`testdata/python-fixtures/` holds data produced by actually running the
Python claude-swap; golden tests parse it to enforce cross-implementation
compatibility.

## License

MIT. Behavioral design and CLI surface derived from
[claude-swap](https://github.com/realiti4/claude-swap), © Onur Cetinkol, MIT.
