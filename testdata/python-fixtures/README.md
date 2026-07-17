# Python-produced golden fixtures

These files are the output of the Python `claude-swap` (v0.22.0b1, from
https://github.com/realiti4/claude-swap), run from source on Linux against a
throwaway `$HOME`. They are the data-compatibility contract for the Go port:
golden tests read them to prove the Go implementation parses, and where
applicable round-trips, exactly what the Python tool writes. They are not
hand-edited.

## Layout

- `claude-swap-data/` — the backup root (`$XDG_DATA_HOME/claude-swap`) as the
  Python tool leaves it:
  - `sequence.json` — the roster with slots 1, 2, 3, 5 (`sequence: [1,2,3,5]`,
    `activeAccountNumber: 1`). Account 2 carries `alias`, account 3 carries
    `kind: "api_key"`, account 5 carries `disabled: true`. Every optional key is
    absent from the records that do not need it; that omission is load-bearing.
  - `configs/` — each account's configuration snapshot.
  - `credentials/*.enc` — each account's credential blob (base64).
  - `mappings.json` — one directory-to-account mapping.
  - `settings.json` — auto-switch settings (`threshold 80`, `model Fable`).
  - `cache/usage.json` — usage cache at schemaVersion 2, holding http-401 error
    records. `backoffUntil`/`lastAttemptAt` are epoch-second floats.
  - `cache/update_check.json` — the update-check cache.
  - `claude-swap.log` — the switch log in the exact
    `YYYY-MM-DD HH:MM:SS,mmm - LEVEL - message` format, including a
    `Switched from account 2 to 1` line.
- `claude-home/dot-claude.json`, `claude-home/dot-credentials.json` — what the
  Python tool writes into the fake Claude Code home: `~/.claude.json` after
  `switch 1`, and `~/.claude/.credentials.json` holding account 1's token blob.
- `backup-all.cswap`, `backup-acct2.cswap` — export files, all accounts and one
  account respectively.

## Invariants the fixtures pin

- Optional keys are omitted, not null: an account record carries `alias`,
  `kind`, or `disabled` only when that state applies.
- `oauthAccount` organization fields are JSON `null` in `claude-home`, while the
  `sequence.json` records use `""` for the same fields. The port preserves this
  asymmetry.
- `cache/usage.json` is schemaVersion 2, and its error records survive a
  round-trip.
- The switch-log format is byte-exact, including comma-milliseconds and the
  `Switched from account X to Y` message.
- Export files import byte-for-byte.

`mappings.json` records an absolute path under the generation-time scratch
`$FIXHOME`; tests treat the path as opaque. No `.migrations.json` is present
because no registry migration applies on a fresh Linux install. The
`cache/usage.json` timestamps are generation-time epoch floats; tests inject a
fake clock rather than compare against wall-clock now. The runtime lock
artifacts (`.lock`, `cache/.usage.lock`) are not included.

## Regeneration

Run the Python `claude-swap` from a source checkout against a throwaway home.
No network is required: the fake tokens produce http-401 usage errors, which is
itself useful state.

```
run() { HOME="$FIXHOME" XDG_DATA_HOME="$FIXHOME/.local/share" \
        PYTHONPATH="src:$SHIM" python3 -m claude_swap "$@"; }
run add-token sk-ant-oat01-AAAAfixture… --email alice@example.com   # slot 1
run add-token sk-ant-oat01-BBBBfixture… --email bob@example.com     # slot 2
run add-token sk-ant-api03-CCCCfixture… --email key@example.com     # slot 3 (API key)
run add-token sk-ant-oat01-DDDDfixture… --email carol@example.com --slot 5
run alias 2 dev
run disable 5
run map 2 "$FIXHOME/work/client-app"
run config set autoswitch.threshold 80
run config set autoswitch.model Fable
run switch 2
run switch 1
run export "$FIXHOME/backup-all.cswap"
run export "$FIXHOME/backup-acct2.cswap" --account 2
```

`$FIXHOME` is a throwaway directory. `$SHIM` is a directory holding a fake
`claude_swap-<version>.dist-info/METADATA` so `importlib.metadata.version()`
resolves outside an installed environment.

After the run, copy `$XDG_DATA_HOME/claude-swap` to `claude-swap-data/`, the
fake `~/.claude.json` and `~/.claude/.credentials.json` to `claude-home/`, and
the two export files here. Remove the runtime lock artifacts (`.lock`,
`cache/.usage.lock`).
</content>
