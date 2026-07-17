# Spec 04 — Anthropic API Surface & Usage Pipeline

## Overview

This module family is `claude-swap`'s (`cswap`) bridge to Anthropic's OAuth and
usage APIs plus the local machinery that caches, schedules, and interprets the
results. It covers four Python modules — `oauth.py` (all network I/O: token
refresh, profile resolution, usage fetch, and the normalization + error
classification around them), `usage_store.py` (the per-account on-disk usage
table with staleness/trust/backoff/quarantine semantics), `poll_policy.py` (the
adaptive polling cadence keyed to a measured per-token rate limit), and
`cache.py` (a trivial TTL JSON cache helper) — plus the two small dataclasses in
`models.py` that this area produces/consumes (`AccountInfo`, `UsageEntry`
re-export via `AccountSnapshot`). Every number, URL, JSON key, header, and error
token below is load-bearing: the Go port must reproduce them exactly, because
callers (the switcher, the auto engine, the TUI) branch on these exact string
tokens and the on-disk formats are shared with other `cswap` processes and
persist across versions.

All network I/O uses Python's `urllib.request` with **no** custom TLS/truststore
configuration — it relies on the system CA store and Python's default HTTPS
verification. There is no `truststore`, no `certifi`, no proxy handling, and no
retry library. The User-Agent on every cswap-originated request is the literal
`"claude-swap/1.0"`. The choice of a non-first-party User-Agent is
**deliberate and consequential**: the usage endpoint enforces a per-access-token
request budget specifically on non-first-party UA classes (see the poll policy
section), which is the entire reason the adaptive cadence exists.

---

## 1. `oauth.py` — Anthropic API surface

### 1.1 Module constants

```python
OAUTH_BETA_HEADER      = "oauth-2025-04-20"
OAUTH_EXPIRY_BUFFER_MS = 5 * 60 * 1000        # 300000  (5 minutes, in ms)
OAUTH_TOKEN_URL        = "https://platform.claude.com/v1/oauth/token"
OAUTH_CLIENT_ID        = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
```

Logger name (shared across the whole package): `logging.getLogger("claude-swap")`.

Additional endpoint URLs used (hard-coded at call sites, not constants):

| Purpose            | Method | URL                                              |
|--------------------|--------|--------------------------------------------------|
| Token refresh      | POST   | `https://platform.claude.com/v1/oauth/token`     |
| Account profile    | GET    | `https://api.anthropic.com/api/oauth/profile`    |
| Usage / rate-limit | GET    | `https://api.anthropic.com/api/oauth/usage`      |

Note the **two different hosts**: token refresh goes to `platform.claude.com`;
profile and usage go to `api.anthropic.com`.

### 1.2 Credential shape (external contract — Claude Code's on-disk format)

cswap reads/writes credentials as a **JSON string** whose top-level object may
contain a `claudeAiOauth` object. This is Claude Code's own credentials format
(from `~/.claude/.credentials.json` on Linux/WSL, or the macOS Keychain). The
OAuth payload shape cswap depends on:

```jsonc
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",   // string; the bearer token
    "refreshToken": "...",               // string; may be absent (setup-token/api-key style)
    "expiresAt": 1752000000000,          // epoch MILLISECONDS (int or float)
    "scopes": ["user:profile", "user:inference", "user:sessions:claude_code"],
    "subscriptionType": "pro",           // opaque passthrough (seen in tests)
    "rateLimitTier": "default_claude_ai" // opaque passthrough
  },
  "organizationUuid": "org-..."          // sibling of claudeAiOauth, preserved on refresh
}
```

Anything at the top level that is **not** `claudeAiOauth` (e.g.
`organizationUuid`) is preserved verbatim across a refresh — refresh mutates the
nested `claudeAiOauth` dict and re-serializes the whole top-level object.

**Credential kinds** (distinguished only by what fields are present):
- **OAuth (subscription)**: has `claudeAiOauth.refreshToken` → refreshable.
- **Setup-token**: `claudeAiOauth` present with an `accessToken` (e.g.
  `sk-ant-oat01-...`) but **no** `refreshToken`. Cannot be refreshed. Test
  `test_full_content_fallback_for_api_keys_and_setup_tokens` uses
  `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-abc"}}` as a setup-token.
- **API key**: a **raw string** that is not JSON at all (e.g.
  `"sk-ant-api03-xyz"`). `extract_oauth_data` returns `None` for it; it never
  gets refreshed and never hits the usage endpoint as an OAuth token.

### 1.3 `extract_access_token(credentials: str) -> str | None`

Parse `credentials` as JSON, return `data["claudeAiOauth"]["accessToken"]`, or
`None`. Catches `json.JSONDecodeError` and `AttributeError` (the latter guards
against `data` being a non-dict where `.get` chain fails). Returns `None` on
invalid JSON, empty string, or missing key.

### 1.4 `extract_oauth_data(credentials: str) -> dict | None`

Parse JSON; return the `claudeAiOauth` object **only if it is a dict**, else
`None`. Returns `None` on `json.JSONDecodeError`. (Does not catch
`AttributeError` — but `data.get` is only called guarded by returning the parse
result; if `data` is a non-dict JSON scalar, `.get` would raise `AttributeError`
which is **not** caught here — however in practice a raw API-key string is not
valid JSON so it hits the `JSONDecodeError` path. A JSON scalar like `"5"` or
`5` would raise. Go port: guard `data` is a dict/object before `.get`.)

### 1.5 `credential_fingerprint(credentials: str) -> str | None`

Stable identity fingerprint used to answer "did this credential change?"
(issue #117 guard).

Algorithm:
1. If `credentials` is empty/falsy → return `None`. **This is the only `None`
   case** — real bytes must never fingerprint to `None`.
2. Extract oauth data; if it has a non-empty **string** `refreshToken` →
   return `"sha256:" + sha256(refreshToken.encode()).hexdigest()`.
3. Otherwise → return `"sha256-full:" + sha256(credentials.encode()).hexdigest()`.

Rationale (verbatim intent): refresh-token hash survives access-token rotation
(two generations of the same OAuth lineage compare equal); full-content hash for
API keys and setup-tokens (which never rotate, so content identity = lineage
identity). The two prefixes (`sha256:` vs `sha256-full:`) guarantee a refresh-hash
can never collide with a full-content hash.

Edge cases from tests:
- `{"claudeAiOauth": {"accessToken": "sk-old", "refreshToken": "rt-1"}}` and
  `{"claudeAiOauth": {"accessToken": "sk-new", "refreshToken": "rt-1", "expiresAt": 5}}`
  fingerprint **equal** (rotation-stable).
- Different `refreshToken` → different fingerprint.
- Raw string `"raw-token"` → `sha256-full:...`.
- `""` → `None`.

### 1.6 `is_oauth_token_expired(expires_at: object) -> bool`

- If `expires_at` is not an `int` or `float` → return `False` (unknown expiry
  treated as not-expired).
- Else compute `now_ms = int(datetime.now(timezone.utc).timestamp() * 1000)`
  and return `now_ms + OAUTH_EXPIRY_BUFFER_MS >= int(expires_at)`.

So a token is "expired" if it expires within the next 5 minutes (buffer).
`expires_at` is epoch **milliseconds**.

### 1.7 `RefreshOutcome` (frozen dataclass)

```python
@dataclass(frozen=True)
class RefreshOutcome:
    credentials: str | None        # full rotated credentials JSON on success, else None
    error: str | None              # None on success; else classification token
    token_account: dict | None = None
```

`error` tokens:
- `None` — success (`credentials` set).
- `"invalid_grant"` — token endpoint rejected the grant; **permanent** (dead
  refresh token, re-login required).
- `"no_refresh_token"` — stored credential carries no usable refresh token;
  **permanent** for retry purposes.
- `"transient"` — network/server error; token may still be valid, retry later.

`token_account` (opportunistic identity, or `None`): shape
`{"uuid": str, "email": str|None, "organizationUuid": str|None}`.

### 1.8 `try_refresh_oauth_credentials(credentials: str) -> RefreshOutcome`

The verbatim refresh flow:

1. Parse `credentials` as JSON. On `json.JSONDecodeError` →
   `RefreshOutcome(None, "no_refresh_token")`.
2. `oauth = data["claudeAiOauth"]` if `data` is a dict, else `None`. If `oauth`
   is not a dict **or** has no truthy `refreshToken` →
   `RefreshOutcome(None, "no_refresh_token")`.
3. Build the POST body (JSON, then `.encode()` to UTF-8 bytes):

   ```json
   {
     "grant_type": "refresh_token",
     "refresh_token": "<oauth.refreshToken>",
     "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
   }
   ```

   **No `scope`/`scopes` field is ever sent** (explicitly asserted by tests
   `test_refresh_sends_correct_body` and `test_refreshes_when_scopes_are_missing`).
4. Build the request:
   - URL: `OAUTH_TOKEN_URL`
   - Method: `POST`
   - Headers: `Content-Type: application/json`, `User-Agent: claude-swap/1.0`
   - Body: the encoded JSON above
5. `urlopen(req, timeout=10)` (**10-second timeout**). Read and JSON-decode the
   response body.
6. On success, mutate `oauth` in place:
   - `oauth["accessToken"] = resp["access_token"]` (required — a `KeyError`
     here would be caught by the generic `except` → `"transient"`).
   - `now_ms = int(datetime.now(timezone.utc).timestamp() * 1000)`;
     `oauth["expiresAt"] = now_ms + resp["expires_in"] * 1000` (`expires_in` is
     **seconds**; stored as ms).
   - If `resp.get("refresh_token")` truthy → `oauth["refreshToken"] = resp["refresh_token"]`
     (rotation; otherwise the old refresh token is kept).
   - If `resp.get("scope")` truthy → `oauth["scopes"] = resp["scope"].split()`
     (space-delimited scope string → list).
   - `data["claudeAiOauth"] = oauth`; return
     `RefreshOutcome(json.dumps(data), None, _parse_token_account(resp))`.

**Token endpoint success response shape** (external contract):
```json
{
  "access_token": "new-access",
  "refresh_token": "new-refresh",     // optional; when present, rotates the stored one
  "expires_in": 3600,                 // SECONDS
  "scope": "user:profile user:inference user:sessions:claude_code",  // optional, space-delimited
  "account": {"uuid": "...", "email_address": "..."},  // optional identity
  "organization": {"uuid": "..."}                      // optional
}
```

### 1.9 Error classification for refresh (the dead-vs-transient distinction)

On `urllib.error.HTTPError`:
- Read the error body (`e.read().decode(errors="replace")` if `e` has `read`,
  else `""`). Log at DEBUG: `"OAuth refresh failed: %r, body: %s"` with body
  truncated to first 500 chars.
- **Permanent (`invalid_grant`) requires BOTH**:
  - `e.code in (400, 401, 403)`, **AND**
  - the body contains the substring `"invalid_grant"` **or** `"invalid_client"`.
- Otherwise → `"transient"`.

On any other exception → log DEBUG `"OAuth refresh failed: %r"` → `"transient"`.

**Critical classification rule** (verbatim intent): "Permanent only when the
server itself rejected the grant: a 4xx AND an explicit marker in the body.
Anything ambiguous stays transient — a misclassified transient costs one retry,
a misclassified permanent would wrongly quarantine a live token."

Test matrix (`TestTryRefreshOAuthCredentials`):

| Scenario                                        | code | body                              | `error`         |
|-------------------------------------------------|------|-----------------------------------|-----------------|
| success                                         | 200  | valid token JSON                  | `None`          |
| invalid_grant marker on 400                     | 400  | `{"error": "invalid_grant"}`      | `invalid_grant` |
| 400 without marker                              | 400  | `{"error":"temporarily_unavailable"}` | `transient` |
| 5xx even with marker                            | 500  | `{"error": "invalid_grant"}`      | `transient`     |
| network error (`URLError`)                      | —    | —                                 | `transient`     |
| missing refresh token                           | —    | (no request made)                 | `no_refresh_token` |
| invalid JSON credentials                        | —    | (no request made)                 | `no_refresh_token` |

The substring check is on the raw body text — `"invalid_client"` also triggers
permanent classification (not exercised by a test but in the code).

### 1.10 `_parse_token_account(resp_data: dict) -> dict | None`

Extract optional identity from the token-endpoint response:
1. `account = resp_data.get("account")`; if not a dict → `None`.
2. `uuid = account.get("uuid")`; if not a non-empty (after `.strip()`) string →
   `None`.
3. `email = account.get("email_address")` (note: **`email_address`**, the token
   endpoint's key).
4. `org_uuid = resp_data["organization"]["uuid"]` if `organization` is a dict.
5. Return `{"uuid": uuid.strip(), "email": email if isinstance(email,str) else None,
   "organizationUuid": org_uuid if isinstance(org_uuid,str) else None}`.

Normalization: uuid is stripped; non-string optionals become `None`. Tests
confirm: absent account → `None`; missing uuid → `None`; non-string uuid (e.g.
`12345`) → `None`; padded uuid `"  acc-uuid  "` → `"acc-uuid"`; non-string
`email_address`/`organization.uuid` → `None` for those fields but still resolves.

### 1.11 `refresh_oauth_credentials(credentials: str) -> str | None`

Thin wrapper: `return try_refresh_oauth_credentials(credentials).credentials`
(None on any failure).

### 1.12 `fetch_oauth_profile(access_token: str) -> dict | None`

Resolve an access token to account identity via
`GET https://api.anthropic.com/api/oauth/profile`.

- Headers: `Authorization: Bearer <access_token>`,
  `Content-Type: application/json`, `User-Agent: claude-swap/1.0`.
  (**No `anthropic-beta` header here** — that's usage-only.)
- `urlopen(req, timeout=5)` (**5-second timeout**).
- Parse response JSON.

Response boundary (strict — advisory oracle):
- `account = data["account"]` if `data` is a dict; if not a dict → DEBUG
  `"OAuth profile response missing account object"` → `None`.
- `uuid = account["uuid"]`; if not a non-empty-after-strip string → DEBUG
  `"OAuth profile response missing account.uuid"` → `None`.
- `email = account.get("email")` (note: profile endpoint uses **`email`**, not
  `email_address`).
- `org_uuid = data["organization"]["uuid"]` if `organization` is a dict.
- Return `{"uuid": uuid.strip(), "email": str-or-None, "organizationUuid": str-or-None}`.

Error handling:
- `HTTPError` with `code == 401` → log at **WARNING**:
  `"OAuth profile returned 401 while resolving credential ownership; proceeding
  without identity (pre-fix behavior)."` then return `None`. (401 is "evidence
  not proof" — fail open.)
- Any other `HTTPError` → DEBUG `"OAuth profile fetch failed: %r"` → `None`.
- Any other `Exception` (incl. malformed JSON) → DEBUG same → `None`.

**Concurrency contract (verbatim):** "Must not be called while any
credential/config lock is held (network under locks is forbidden)."

**Profile response shape** (external contract):
```json
{
  "account": {"uuid": "acc-uuid", "email": "a@b.c"},
  "organization": {"uuid": "org-uuid"}
}
```

### 1.13 `build_token_status(credentials: str) -> str | None`

Debug summary of stored OAuth token state:
- No oauth data → `None`.
- `has_refresh_token = bool(oauth.get("refreshToken"))`; `refresh_str = "yes"|"no"`.
- `expires_at = oauth.get("expiresAt")`. If not int/float →
  `f"oauth: unknown expiry, refresh token {refresh_str}"`.
- Else: `expires_utc = datetime.fromtimestamp(expires_at/1000, tz=utc)`;
  `state = "expired" if is_oauth_token_expired(expires_at) else "fresh"`;
  `(countdown, clock) = format_reset(expires_utc.isoformat())`; return
  `f"oauth: {state}, refresh token {refresh_str}, expires {clock} in {countdown}"`.

Tests: fresh token 1h30m out → contains `"oauth: fresh, refresh token yes"` and
`"in 1h 30m"`. Unknown expiry → exactly
`"oauth: unknown expiry, refresh token yes"`.

### 1.14 Time formatting helpers

**`format_reset(resets_at: str) -> tuple[str, str]`** → `(countdown, clock)`:
- `reset_utc = datetime.fromisoformat(resets_at)`;
  `now = datetime.now(timezone.utc)`.
- `total_seconds = max(0, int((reset_utc - now).total_seconds()))`.
- `days, rem = divmod(total_seconds, 86400)`; `hours, rem = divmod(rem, 3600)`;
  `minutes = rem // 60`.
- Countdown format:
  - `days > 0` → `f"{days}d {hours}h"`
  - `hours > 0` → `f"{hours}h {minutes}m"`
  - else → `f"{minutes}m"`
- Clock via `reset_clock_string`.

**`reset_clock_string(reset_utc, now_utc) -> str`** — absolute reset time in
**local** time:
- Convert both to local via `.astimezone()`.
- Same local date → `strftime("%H:%M")` (e.g. `"20:39"`).
- Different date → `strftime(f"%b {day} %H:%M")` where `day = str(reset_local.day)`
  (no zero padding on day), e.g. `"Jul 5 08:59"`.

**`fresh_reset_strings(window: dict) -> tuple[str, str] | None`** — recompute
`(countdown, clock)` at render time:
- If `window.get("resets_at")` truthy → try `format_reset(resets_at)`; on
  `ValueError`/`TypeError` fall through.
- Else/fallback: if `"clock"` in window → `(window.get("countdown", "?"), window["clock"])`.
- Else → `None`.

Rationale: cached countdown/clock drift as measurement ages; recompute from
`resets_at`. Entries persisted without `resets_at` fall back to fetch-time
strings ("stale beats blank").

### 1.15 `request_usage_data(access_token: str) -> dict`

Raw usage fetch (raises on any error — callers wrap):
- URL: `https://api.anthropic.com/api/oauth/usage`
- Headers:
  - `Authorization: Bearer <access_token>`
  - `anthropic-beta: oauth-2025-04-20` (**the beta header — usage-only**)
  - `User-Agent: claude-swap/1.0`
  - (**No `Content-Type` header** on this GET.)
- `urlopen(req, timeout=5)` (**5-second timeout**), JSON-decode, return dict.

### 1.16 `_classify_usage_error(e: Exception) -> tuple[str, float | None]`

Maps a usage-fetch exception to `(kind, retry_after_s)`:

| Exception                                  | kind                       | retry_after_s |
|--------------------------------------------|----------------------------|---------------|
| `HTTPError` code N                         | `f"http-{N}"` e.g. `http-429` | parsed Retry-After (see below) |
| `TimeoutError` (incl. `socket.timeout`)    | `"timeout"`                | `None`        |
| `URLError` whose `.reason` is `TimeoutError`| `"timeout"`               | `None`        |
| `URLError` (other reason)                  | `"network"`                | `None`        |
| `json.JSONDecodeError`                     | `"bad-response"`           | `None`        |
| anything else                              | `type(e).__name__`         | `None`        |

Retry-After parsing (HTTPError only):
- Read `e.headers.get("Retry-After")` if `e.headers` present.
- Try `max(0.0, float(raw.strip()))` — **seconds form only**.
- On `ValueError` (e.g. HTTP-date form `"Fri, 04 Jul 2026 12:00:00 GMT"`) →
  `None` (date form deliberately ignored).
- Negative values clamp to `0.0` (test: `"-5"` → `0.0`).

Ordering note for the Go port: the `TimeoutError` branch is checked **before**
`URLError`, and `HTTPError` (a subclass of `URLError`) is checked first of all.
Preserve this precedence.

### 1.17 `_log_usage_failure(context, e, kind, retry_after_s=None)`

Emits **one WARNING line** (so failures land in the default log file — issue #85
was undiagnosable at DEBUG) plus the full exception repr at DEBUG.

- `where = f" {context}"` if context else `""`.
- `cause = kind` if `retry_after_s is None` else `f"{kind}, retry-after {retry_after_s:.0f}s"`
  (retry-after rounded to whole seconds, no decimals).
- If `kind == "http-429"`: append `" (per-token usage budget reached; backing off)"`.
- WARNING: `"Usage fetch failed%s: %s"` % (where, cause).
- DEBUG: `"Usage fetch failure detail%s: %r"` % (where, e).

**Privacy invariant (verbatim):** the `context` string must never carry the
email — it is what users paste into public issues. Callers pass
`context = f"for account {account_num}"` (account number only). Test asserts the
email `a@b.c` never appears in the warning line, while `"account 1"` and
`"retry-after 42s"` and `"per-token usage budget"` do.

### 1.18 `build_usage_result(data: dict) -> dict | None`

Normalizes the raw usage-API response into cswap's internal usage dict. Logs the
full raw response at DEBUG (`json.dumps(data, indent=2)`). Returns `None` if the
result would be empty.

**Raw usage-API response shape** (external contract — Anthropic's
`/api/oauth/usage`):
```jsonc
{
  "five_hour": {"utilization": 22.0, "resets_at": "2026-...Z" | null},
  "seven_day": {"utilization": 61.0, "resets_at": "..." | null},
  "seven_day_opus": null,                     // seen but ignored
  "extra_usage": {
    "is_enabled": true,
    "used_credits": 72900,                    // integer, CENTS (nullable)
    "monthly_limit": 500000,                  // integer, CENTS (null = unlimited)
    "utilization": 14.58,                     // percent (nullable)
    "currency": "USD"                         // optional, default "USD"
  },
  "limits": [                                 // newer per-model array (optional)
    {"kind": "session",       "group": "session", "percent": 7,  "resets_at": null, "scope": null,               "is_active": false},
    {"kind": "weekly_all",    "group": "weekly",  "percent": 72, "resets_at": null, "scope": null,               "is_active": false},
    {"kind": "weekly_scoped", "group": "weekly",  "percent": 100,"severity": "critical", "resets_at": "...Z",
     "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": true}
  ]
}
```

**Normalized output shape** (internal — what the rest of cswap consumes):
```jsonc
{
  "five_hour": {"pct": 22.0, "resets_at": "...", "countdown": "1h 0m", "clock": "20:39"},
  "seven_day": {"pct": 61.0, ...},
  "spend":     {"used": 729.0, "limit": 5000.0, "pct": 14.58, "currency": "USD",
                "resets_at": "...", "countdown": "...", "clock": "..."},
  "scoped":    [{"name": "Fable", "pct": 100.0, "resets_at": "...", "countdown": "3h 0m", "clock": "..."}]
}
```

Transformation rules:

**`five_hour` / `seven_day`** — if the raw key is truthy:
- `entry = {"pct": raw["utilization"]}`. **Note: no coercion** — `pct` is stored
  as whatever type `utilization` is (int or float from JSON).
- If `raw.get("resets_at")` truthy → set `resets_at`, and
  `(countdown, clock) = format_reset(resets_at)`.
- When `resets_at` is `null`/absent, the entry has `pct` but **no** `clock`/`countdown`
  keys (test `test_null_resets_at`).

**`extra_usage` → `spend`** — only if `eu` truthy **and** `eu.get("is_enabled")`:
- Read `used_credits`, `monthly_limit`, `utilization`.
- **Only produce a `spend` entry when all three are non-`None`.** If any is
  `None` (e.g. `monthly_limit=None` = unlimited, or a partial null) → skip the
  spend entry entirely, but leave `five_hour`/`seven_day`/`scoped` untouched.
- `spend = {"used": float(used_credits)/100, "limit": float(monthly_limit)/100,
  "pct": float(utilization), "currency": eu.get("currency", "USD")}`.
  **Credits are in cents → divide by 100 to get currency units.**
  (72900 → 729.0; 500000 → 5000.0.)
- If `eu.get("resets_at")` truthy → set `resets_at` + `format_reset`.
- A `TypeError`/`ValueError` during float conversion → DEBUG
  `"extra_usage parse failed: %r"`, skip spend only.

**`limits` → `scoped`** — only if `data.get("limits")` is a list:
- For each `lim` that is a dict:
  - `scope = lim.get("scope")`; `model = scope["model"]` if scope is a dict;
    `name = model["display_name"]` if model is a dict.
  - `pct = lim.get("percent")`.
  - Skip unless `name` is truthy **and** `pct` is int/float.
  - `scoped_entry = {"name": name, "pct": float(pct)}` (**pct coerced to float
    here**, unlike five_hour/seven_day).
  - If `lim.get("resets_at")` truthy → set `resets_at` + `format_reset`.
- Only set `result["scoped"]` if the list is non-empty.
- Entries with `scope: null` (session, weekly_all) are **not** surfaced — only
  model-scoped entries with a `display_name`. A response with no `limits` key →
  no `scoped` key (backward compat).

Return `result` if non-empty, else `None`.

### 1.19 `relevant_windows(usage, models=()) -> list[tuple[str, float, str|None]]`

The **single canonical window source** for decisions, scheduling, reset math.
Returns `(label, pct, resets_at)` tuples.

- If `usage` is not a dict → `[]`.
- Always: `five_hour` → label `"5h"`; `seven_day` → label `"7d"` — each included
  only when the window dict has an int/float `pct`. `resets_at` is
  `window.get("resets_at")` (ISO string as fetched, or `None`).
- If `models` non-empty:
  - `wanted = {m.lower() for m in models}`; `match_all = "all" in wanted`.
  - For each `s` in `usage.get("scoped")` (if a list) with int/float `pct` and
    string `name`: include `(s["name"], float(s["pct"]), s.get("resets_at"))`
    when `match_all` **or** `s["name"].lower() in wanted`.
- **`spend` is deliberately excluded** — it's a separate (pay-as-you-go) axis.

Model matching is case-insensitive; sentinel `"all"` (any case) matches every
scoped window. Scoped windows use the model's original-cased `display_name` as
the label.

### 1.20 `account_headroom(usage, models=()) -> float | None`

Remaining percent before the account hits a rate-limit window:
- `pcts = [pct for _, pct, _ in relevant_windows(usage, models)]`.
- If empty → `None` (unknown; callers never auto-skip on unknown).
- Else `100.0 - max(pcts)` (headroom of the **binding** window). `<= 0` means
  at/over a limit.

Tests confirm: binding window is the higher utilization; 7d can bind; spend
ignored; a maxed named model (100%) yields 0 headroom even with 5h/7d slack;
unlisted models don't bind; `"all"` folds every scoped window.

### 1.21 `UsageOutcome` (frozen dataclass)

```python
@dataclass(frozen=True)
class UsageOutcome:
    usage: dict | None
    error: str | None = None
    retry_after_s: float | None = None
```

`error` values: `None` on success; a `_classify_usage_error` kind
(`http-NNN`, `timeout`, `network`, `bad-response`, type-name); plus the
pre-request tokens `"no-access-token"`, `"refresh-failed"`, and the permanent
`"invalid_grant"`.

### 1.22 `fetch_usage(access_token: str) -> dict | None`

Simple wrapper: `request_usage_data` → `build_usage_result`. On any exception:
`_classify_usage_error`, `_log_usage_failure("", e, kind)` (empty context), return
`None`.

### 1.23 `try_fetch_usage_for_account(...) -> UsageOutcome`

**The central usage-fetch orchestration.** Signature:

```python
def try_fetch_usage_for_account(
    account_num: str,
    email: str,
    credentials: str,
    is_active: bool,
    persist_credentials: Callable[[str, str, str], None] | None = None,
) -> UsageOutcome
```

`context = f"for account {account_num}"` (**no email** — paste-safe).

Flow:
1. `oauth = extract_oauth_data(credentials)`; `access_token = oauth.get("accessToken")`
   if oauth else `None`. If no access token → `UsageOutcome(None, error="no-access-token")`.
2. `working_credentials = credentials`.
3. **Proactive refresh** — only when **all** of: `not is_active` **and**
   `oauth.get("refreshToken")` **and** `is_oauth_token_expired(oauth.get("expiresAt"))`:
   - `refresh = try_refresh_oauth_credentials(working_credentials)`.
   - On success (`refresh.credentials`): adopt new credentials, persist via
     `_persist(...)`, re-extract oauth + access_token.
   - Elif `refresh.error == "invalid_grant"` → **short-circuit**
     `UsageOutcome(None, error="invalid_grant")` **without hitting the usage
     endpoint** (don't add a 401/429 to a lost cause; lets the store quarantine).
   - A **transient** refresh failure falls through and tries the (expired) token
     anyway — the 401 path below will retry the refresh.
4. **Active accounts are NEVER proactively refreshed.** (Verbatim: "Claude Code
   owns the active account's credentials and coordinates its own refresh via a
   lockfile on `~/.claude/` that cswap doesn't honor, so cswap must never touch
   the active account's tokens.")
5. `request_usage_data(access_token)` → on success `UsageOutcome(build_usage_result(data))`.
6. **On `HTTPError`**:
   - `(kind, retry_after) = _classify_usage_error(e)`.
   - If **not** a retry-eligible 401 — i.e. `e.code != 401` OR `is_active` OR no
     oauth OR no `refreshToken` — then `_log_usage_failure(context, e, kind, retry_after)`
     and return `UsageOutcome(None, error=kind, retry_after_s=retry_after)`.
   - Else (**inactive account, 401, has refresh token**): retry once after
     refreshing:
     - `refresh = try_refresh_oauth_credentials(working_credentials)`.
     - If refresh failed (`not refresh.credentials`): `_log_usage_failure(context, e, kind)`;
       `dead = refresh.error == "invalid_grant"`; return
       `UsageOutcome(None, error="invalid_grant" if dead else "refresh-failed")`.
     - Else adopt + persist new credentials; extract `new_token`; if no new token
       → `UsageOutcome(None, error="refresh-failed")`.
     - Retry `request_usage_data(new_token)` → success → `UsageOutcome(build_usage_result(data))`.
     - Retry raises → classify, `_log_usage_failure(context + " after refresh", ...)`,
       return `UsageOutcome(None, error=kind, retry_after_s=retry_after)`.
7. **On any other `Exception`** (timeout, network, bad-response): classify,
   `_log_usage_failure(context, e, kind, retry_after)`, return
   `UsageOutcome(None, error=kind, retry_after_s=retry_after)`.

**Test-verified auth-header sequence on the 401-retry path:** first usage call
uses `Bearer old-access`, raises 401; refresh happens; second usage call uses
`Bearer new-access`; two usage calls total. Persist called once.

### 1.24 `fetch_usage_for_account(...) -> dict | None`

Wrapper returning `.usage` of the outcome.

### 1.25 `_persist(callback, account_num, email, credentials)`

Calls the persist callback; on **any exception**:
- WARNING log (with email — this is the internal log, not the paste-safe path):
  `"Refreshed OAuth token for account %s (%s) but failed to persist it: %r. The
  refresh token on disk may now be stale; if the next refresh fails with
  invalid_grant, re-run \`cswap --add-account\` after logging in."`
  (args: account_num, email, exception).
- Also `printer.warning(...)` to stdout (user-visible, yellow):
  `"Warning: failed to save refreshed token for account {account_num} ({email}).
  If the next refresh fails, re-run \`cswap --add-account\` after logging in."`

If `callback` is `None` → no-op.

---

## 2. `usage_store.py` — per-account usage table

### 2.1 Purpose & on-disk format

Replaces an older all-or-nothing 15s snapshot. Persists **measurements**
(`lastGood`) and **fetch state** (failures, backoff, poll schedule) per account,
so one failed round trip never blanks every account (stale-on-error). Shared by
`--list`/`--status` (on-demand refresh of stale entries) and `cswap auto`
(scheduled polling).

- **File path**: `<cache_dir>/usage.json` where `cache_dir` is passed to the
  `UsageStore` constructor. In production `cache_dir = get_backup_root()/"cache"`
  (see `cache.py`'s `CACHE_DIR`).
- **Lock file**: `<cache_dir>/.usage.lock`.
- **Schema**: `SCHEMA_VERSION = 2`.

On-disk JSON (written by `atomic_write_json`, `indent=2`, mode 0600 file / 0700
parent on non-Windows):
```jsonc
{
  "schemaVersion": 2,
  "accounts": {
    "1": {
      "email": "a@x.com",
      "organizationUuid": "",            // "" for personal
      "lastGood": { ...normalized usage dict... } | null,
      "fetchedAt": 1752000000.0,         // epoch SECONDS (float), success only
      "lastAttemptAt": 1752000000.0,     // epoch seconds; claim/record stamp
      "consecutiveFailures": 0,
      "lastError": "http-429" | null,
      "backoffUntil": 1752000030.0 | null,
      "nextPollAt": 1752000180.0 | null,
      "pollIntervalS": 180.0 | null,
      "last429At": 1752000000.0 | null,
      "authDeadStrikes": 0
    }
  }
}
```

**Legacy/foreign handling**: `_read_rows` returns `{}` (empty) when the file is
missing, unreadable, corrupt JSON, not a dict, or `schemaVersion != 2`. A
version-less legacy snapshot (`{"timestamp":..., "data":...}`) is treated as
empty — "its data had a 15s shelf life anyway."

### 2.2 Constants

From `usage_store.py`:
```python
SCHEMA_VERSION          = 2
STALE_OK_S              = 300.0    # trusted for switch decisions; older → headroom unknown
CLAIM_TTL_S             = 10.0     # in-flight claim window
TRUST_MAX_AGE_S         = 3600.0   # hard ceiling on decision-trust extension
BACKOFF_BASE_S          = 30.0     # failure backoff base
BACKOFF_CAP_S           = 600.0    # failure backoff cap
RETRY_AFTER_FLOOR_CAP_S = 900.0    # safety cap on honoring a server Retry-After
AUTH_DEAD_STRIKES       = 1        # invalid_grant strikes → quarantine
PERMANENT_AUTH_ERRORS   = frozenset({"invalid_grant"})
```
Re-exported from `poll_policy`: `SERVE_TTL_S` (=180.0), `EDGE_BACKOFF_S` (=300.0).

`Identity = tuple[str, str]` — `(email, organizationUuid)`.

### 2.3 `FetchRecord` (frozen dataclass)

```python
@dataclass(frozen=True)
class FetchRecord:
    usage: dict | None = None
    error: str | None = None
    retry_after_s: float | None = None
    sentinel: str | None = None
```
Exactly one of three shapes: success (error & sentinel None; usage may be None),
failure (error set, optional retry_after_s), sentinel (sentinel set → recorded
as a no-op).

### 2.4 `UsageEntry` (frozen dataclass — read model)

Fields (all default-initialized): `sentinel`, `last_good`, `fetched_at`,
`age_s`, `last_attempt_at`, `consecutive_failures=0`, `last_error`,
`backoff_until`, `next_poll_at`, `poll_interval_s`, `last_429_at`,
`auth_dead_strikes=0`, `trust_extended=False`.

Methods:
- `fresh(now, ttl=SERVE_TTL_S)` → `fetched_at is not None and (now - fetched_at) <= ttl`.
- `in_backoff(now)` → `backoff_until is not None and now < backoff_until`.
- `claimed(now)` → `last_attempt_at is not None and (now - last_attempt_at) < CLAIM_TTL_S`.
- `token_dead(threshold=AUTH_DEAD_STRIKES)` → `auth_dead_strikes >= threshold`.
- `decision_value() -> dict | str | None`:
  - `sentinel` wins (returned directly) if not None.
  - Else `last_good` if `last_good is not None and age_s is not None and
    (age_s <= STALE_OK_S or trust_extended)`.
  - Else `None`.

`sentinel` is a **live overlay, never persisted**. `age_s` and `trust_extended`
are computed at snapshot time.

### 2.5 `UsageStore` class

Constructor: `UsageStore(cache_dir: Path, clock: Callable[[], float] = time.time)`.
`self.path = cache_dir/"usage.json"`; `self._lock_path = cache_dir/".usage.lock"`.

**Identity guard (critical):** Every method takes the caller's `identities` map
(slot number → `(email, organizationUuid)`) and only touches rows for those
slots. A row whose stored identity differs is **invisible to reads and replaced
on write** — slot reuse never serves the previous account's usage. `_matches`:
row is a dict AND `row["email"] == identity[0]` AND
`row.get("organizationUuid", "") == identity[1]`.

**Locking protocol (verbatim — never holds the lock across network I/O):**
```
(a) lock → read, decide/claim the fetch set (stamp lastAttemptAt) → unlock;
(b) fetch with no lock held;
(c) lock → re-read, merge outcomes, write → unlock.
```

#### `entries(identities) -> dict[str, UsageEntry]`

Lock-free read (writes are atomic replaces). For each slot:
- If row missing or identity mismatch → `UsageEntry()` (empty).
- Else build the entry from the row. `age_s = now - fetched_at` (or None).
- **`trust_extended` computation** — True when `age_s is not None` AND
  `age_s <= TRUST_MAX_AGE_S` AND at least one of:
  - `consecutive_failures > 0` (server refusing fresher data), OR
  - `next_poll_at is not None and now < next_poll_at` (scheduler chose this
    cadence; **strict `<`** — at `next_poll_at` the entry is due and no longer
    scheduler-trusted), OR
  - `last_attempt_at is not None and (now - last_attempt_at) < CLAIM_TTL_S` (a
    live claim — another collector just won the fetch; keeps the trust bridge up
    so the loser doesn't flip trusted→unknown while the result is in flight).

#### `claim(nums, identities)`

Stamp `lastAttemptAt = now` on the given slots (read-modify-write under lock).
No-op for empty `nums`. Does not touch measurements.

#### `reserve(nums, identities, *, respect_plans) -> list[str]`

Atomically win the right to fetch: re-check eligibility **and** stamp
`lastAttemptAt` in one locked pass, returning only the slots won. Closes the
double-fetch race that a separate read-then-claim would allow.

Per-slot logic under the lock:
- Identity mismatch/missing row → replace with fresh row, **win it** (eligible
  immediately).
- Else `_row_eligible(row, now, respect_plans)`; skip if ineligible.
- Winners: set `row["lastAttemptAt"] = now`, append to `won`. Write only if any
  won.

`_row_eligible(row, now, respect_plans)`:
1. `authDeadStrikes >= AUTH_DEAD_STRIKES` → ineligible (quarantined).
2. `backoffUntil` set and `now < backoffUntil` → ineligible.
3. `lastAttemptAt` set and `(now - lastAttemptAt) < CLAIM_TTL_S` → ineligible
   (claimed).
4. `stale = fetchedAt is None or (now - fetchedAt) > SERVE_TTL_S`.
   `poll_due = nextPollAt is not None and now >= nextPollAt`.
   - `respect_plans=True` (on-demand: list/status/switch/dashboards): eligible
     iff `stale and (poll_due or nextPollAt is None)`.
   - `respect_plans=False` (auto engine): eligible iff `poll_due or stale`.

#### `record(outcomes, identities)`

Merge fetch outcomes. Sentinel records are dropped (`sentinel is None` filter);
if nothing remains, no-op. Per slot, `apply(num, row)`:
- Always `row["lastAttemptAt"] = now`.
- **Success** (`rec.error is None`): set `lastGood = rec.usage`,
  `fetchedAt = now`, `consecutiveFailures = 0`, `lastError = None`,
  `backoffUntil = None`, `authDeadStrikes = 0` (a success proves the token
  alive).
- **Failure**: `failures = old + 1`; `consecutiveFailures = failures`;
  `lastError = rec.error`; if `rec.error == "http-429"` set `last429At = now`
  (**kept across later successes** — see planner); `backoffUntil = now +
  _failure_backoff_s(failures, rec.retry_after_s)`; if `rec.error in
  PERMANENT_AUTH_ERRORS` → `authDeadStrikes = old + 1`.

Success and failure are **mutually exclusive writers**: success resets the
failure fields, failure never touches `lastGood`/`fetchedAt` (stale-on-error).
Only a permanent-auth failure advances the dead-token count; a transient error
(429/timeout) leaves it untouched (no evidence either way).

#### `set_poll_plan(plans, identities)`

`plans: dict[str, tuple[float|None, float|None]]` → per slot set
`row["nextPollAt"], row["pollIntervalS"]` from the tuple. `(None, None)` clears
the plan. No-op for empty plans. Does not touch measurements.

#### `clear_dead_token(nums, identities)`

Lift the quarantine after a re-login/add rewrites a credential: set
`authDeadStrikes = 0`, `consecutiveFailures = 0`, `lastError = None`,
`backoffUntil = None`. No-op for empty nums (but note: for a slot with a fresh
row it will still create/replace the row via `_mutate`). "A no-op for rows with
no strikes."

### 2.6 `_failure_backoff_s(consecutive_failures, retry_after_s) -> float`

```python
computed = min(BACKOFF_BASE_S * 2**max(0, consecutive_failures - 1), BACKOFF_CAP_S)
if retry_after_s is None:        return computed                     # pure exponential, capped
if retry_after_s == 0:           return min(max(computed, EDGE_BACKOFF_S), BACKOFF_CAP_S)
else:                            return max(min(retry_after_s, RETRY_AFTER_FLOOR_CAP_S), computed)
```

Interpretations:
- **No Retry-After**: `30·2^(n-1)` capped at 600. Sequence (n=1..):
  30, 60, 120, 240, 480, 600, 600, ...
- **Retry-After: 0** (saturated-window edge): floor at `EDGE_BACKOFF_S`=300, but
  the exponential curve may exceed it, capped at 600. Sequence:
  300, 300, 300, 300, 480, 600, 600.
- **Retry-After: N>0** (burst rule): honor N as the floor, capped at
  `RETRY_AFTER_FLOOR_CAP_S`=900; our own curve may wait longer.
  - `_failure_backoff_s(1, 90.0)` → 90 (server floor > computed 30).
  - `_failure_backoff_s(5, 10.0)` → 480 (own curve wins: 30·2^4).
  - `_failure_backoff_s(1, 5000.0)` → 900 (capped).
  - `_failure_backoff_s(1, 300.0)` → 300 (measured burst block honored exactly).
  - `_failure_backoff_s(50, None)` → 600 (cap).

### 2.7 `due_candidate(candidates, entries, now) -> str | None`

The **stalest due candidate**, shared by the auto engine and TUI watch view so
both pick the same single alternate to poll per pass.

Per candidate `num`:
- Missing entry (`entries.get(num) is None`) → most-due: `(0, 0.0, num)`.
- `entry.sentinel is not None` → **skip** (nothing to fetch).
- `entry.token_dead()` → **skip** (quarantined).
- `entry.in_backoff(now)` → **skip**.
- `entry.next_poll_at is not None and now < entry.next_poll_at` → **skip** (not
  yet due).
- `entry.fetched_at is None` → never-fetched: `(0, 0.0, num)`.
- Else `(1, entry.fetched_at, num)`.

`due.sort()` then return `due[0][2]`. Sort key `(rank, fetched_at, num)`:
rank-0 (missing/never-fetched) beats rank-1 (fetched); among fetched, the
smallest `fetched_at` (stalest) wins; ties break lexicographically by slot
number string. Empty → `None`.

### 2.8 `with_sentinel(entry, sentinel) -> UsageEntry`

`None` sentinel → returns the same entry object (identity). Else
`replace(entry, sentinel=sentinel)` (overlay, read-model only).

### 2.9 `_num_or_none(value)` → `float(value)` if int/float else `None`.

---

## 3. `poll_policy.py` — adaptive polling cadence

### 3.1 The measured rate limit (external system knowledge — quote verbatim)

The `/api/oauth/usage` endpoint enforces a **per-access-token budget on
non-first-party clients**: a **rolling ~60-minute window of ~28–30 requests per
token × UA-class** (measured 2026-07-11, probe3). Key facts:

- It is **NOT a refilling bucket**. Capacity returns only as old requests age out
  of the trailing hour, so a burst saturates the token for up to a full hour —
  **pausing does not restore headroom early**.
- Error bars: horizon bracketed to ~55–64 minutes from a single transition
  event; the exact edge algorithm (likely a Cloudflare sliding-window
  approximation) is undocumented and Anthropic can retune it any day.
- The constants lean only on robust parts: a sustained rate safely under the cap
  and an ~hour recovery horizon.
- **Budget target**: an average of at most ~1 request / 3 minutes per token
  (20/hour vs the ~28–30/hour cap), leaving ~8–10 requests/hour headroom for
  manual commands, wake-from-sleep catch-up, and bounded urgent mode.
- **Health invariant** (watch in logs): steady state shows zero `http-429`; any
  post-burst 429 clears within ≤60 minutes. An episode outlasting an hour at
  modest rates means the model needs revisiting.
- Two 429 rules distinguished by Retry-After:
  - `Retry-After: 0` = saturated-budget edge (trailing hour spent, frees as old
    requests age out; immediate retries prolong the oscillation).
  - `Retry-After: N>0` = burst rule (several rapid requests on one token → hard
    block; measured accurate, counts down, not extended by probing).

Plans computed here are persisted per-account in the usage store
(`nextPollAt`/`pollIntervalS`) by whichever collector fetched, so **every
surface** (`cswap list`, TUI, menu bar, auto engine) inherits the same cadence.

### 3.2 Constants

```python
SERVE_TTL_S               = 180.0   # entry younger than this served without fetch (also the sustained-rate governor)
MIN_INTERVAL_S            = 180.0   # normal cadence floor
URGENT_INTERVAL_S         = 60.0    # active acct near threshold & moving
ACTIVE_MAX_INTERVAL_S     = 300.0   # decay ceiling, active
CANDIDATE_DEFAULT_INTERVAL_S = 300.0
CANDIDATE_MAX_INTERVAL_S  = 600.0   # decay ceiling, candidate
MOVEMENT_DELTA_PCT        = 1.0     # binding-pct change ≥ this = "moving"
JITTER_FRAC               = 0.1     # ±10% jitter on scheduled interval
EDGE_BACKOFF_S            = 300.0   # Retry-After:0 probe floor
POST_429_MIN_INTERVAL_S   = 360.0   # cadence floor while a recent 429 exists
RECENT_429_WINDOW_S       = 3600.0  # "recent 429" window
ESCALATION_MARGIN_PCT     = 15.0    # active within this of threshold → escalate/urgent band
RESET_SLACK_S             = 60.0    # never schedule past a window reset + this
```

### 3.3 Helper functions

- `binding_pct(usage, models=()) -> float | None` — `None` if
  `account_headroom` is None, else `100 - headroom` (utilization of the binding
  window).
- `limiting_reset_ts(usage, models=()) -> float | None` — epoch when the **last**
  of the ≥100% relevant windows resets (account usable again). Iterates
  `relevant_windows`, skips `pct < 100`, takes the **latest** parseable
  `resets_at`.
- `earliest_future_reset_ts(usage, now, models=()) -> float | None` — epoch of
  the **next** relevant-window reset strictly ahead of `now`, any utilization
  (the **earliest** such).
- `parse_reset_ts(resets_at) -> float | None` — `None` for falsy;
  `datetime.fromisoformat(str(resets_at).replace("Z", "+00:00")).timestamp()`;
  `None` on `ValueError`. **Note the `Z`→`+00:00` substitution** (Python's
  `fromisoformat` in this codebase's target doesn't accept a bare `Z` reliably;
  Go's `time.RFC3339` handles `Z` natively but the port must still parse both).

### 3.4 `plan_after_fetch(...) -> tuple[float, float]`

Returns `(next_poll_at, interval_s)` for an account **just fetched
successfully**. Signature (all keyword-only after `*`):

```python
def plan_after_fetch(*, prev_interval_s, prev_usage, new_usage, is_active,
                     threshold, models, recent_429, now,
                     rng: Callable[[], float] = random.random) -> tuple[float, float]
```

Algorithm:
1. `default = MIN_INTERVAL_S if is_active else CANDIDATE_DEFAULT_INTERVAL_S`
   (180 vs 300).
2. `ceiling = ACTIVE_MAX_INTERVAL_S if is_active else CANDIDATE_MAX_INTERVAL_S`
   (300 vs 600).
3. `base = prev_interval_s or default`.
4. `prev_pct = binding_pct(prev_usage, models)`; `new_pct = binding_pct(new_usage, models)`.
5. **Movement branching**:
   - If either pct is `None` → `moving = False`, `interval = default` (unknown
     utilization uses the default).
   - Elif `abs(new_pct - prev_pct) >= MOVEMENT_DELTA_PCT` → `moving = True`,
     `interval = max(MIN_INTERVAL_S, base/2)` (halve toward floor).
   - Else (unmoved) → `moving = False`,
     `interval = min(ceiling, max(MIN_INTERVAL_S, base * 1.5))` (back off ×1.5
     toward ceiling, floored at MIN so a sub-floor urgent base snaps straight
     back to normal).
6. **Urgent mode**: if `is_active and moving and not recent_429 and
   new_pct is not None and new_pct >= threshold - ESCALATION_MARGIN_PCT` →
   `interval = URGENT_INTERVAL_S` (60).
7. **Post-429 floor**: if `recent_429` → `interval = max(interval,
   POST_429_MIN_INTERVAL_S)` (≥360; also suppresses urgent via step 6's guard).
8. **Jitter & schedule**:
   `next_poll = now + interval * (1.0 + JITTER_FRAC * (2.0 * rng() - 1.0))`
   — `rng()` in [0,1); factor in `[1-JITTER_FRAC, 1+JITTER_FRAC]`;
   `rng()==0.5` → factor exactly 1.0.
9. **Reset capping**:
   - `headroom = account_headroom(new_usage, models)`.
   - If `headroom is not None and headroom <= 0` (at/over limit): `reset_ts =
     limiting_reset_ts(...)`; if `reset_ts is not None and reset_ts > next_poll`
     → `next_poll = reset_ts` (**skip straight to the reset that frees it**; the
     learned interval is still returned for its return).
   - Else: `reset_ts = earliest_future_reset_ts(new_usage, now, models)`; if not
     None → `next_poll = min(next_poll, reset_ts + RESET_SLACK_S)` (**never
     scheduled past a future reset + slack**; stored usage is obsolete once the
     window rolls over).
10. Return `(next_poll, interval)`.

**Worked test values** (jitter zeroed by rng=0.5 in tests):
- First fetch (no prev): active→180, candidate→300.
- Unmoved decay: `prev=300 → 450`; `prev=500 → 600 (cap)`; active `prev=250 →
  300 (ACTIVE cap)`.
- Movement halving: `prev=600, moved → 300`; `prev=200, moved → 180 (floor)`.
- Sub-delta wiggle (10→10.5, <1.0) is **not** movement → decays (300→450).
- Urgent: active, prev=180, 78→82 (moving, in 75..90 band), threshold 90 → 60.
  Candidate same inputs → 180 (plain halving, never urgent).
- Urgent suppressed by recent_429 → 360.
- Urgent base 60 then unmoved → snaps to 180 (never 60→90→135).
- Reset cap: future reset at now+90, usage 40% → next_poll ≈ reset+60, interval
  300. At-limit (100%) reset at now+7200 → next_poll ≈ reset_ts exactly, interval
  300.
- Jitter bounds: rng=0.0 → now + interval·0.9; rng=1.0 → now + interval·1.1.

---

## 4. `cache.py` — generic TTL JSON cache

```python
CACHE_DIR = get_backup_root() / "cache"   # module-level; the shared cache dir
MISSING = object()                          # sentinel distinguishing "no cache" from cached None
```

**`read_cache(path, ttl, default=MISSING)`**:
- Read + JSON-parse `path`. If `time.time() - raw["timestamp"] < ttl` → return
  `raw["data"]`.
- Catches `OSError, json.JSONDecodeError, UnicodeDecodeError, KeyError,
  TypeError` → return `default`.
- When `default` not provided, returns the `MISSING` sentinel, so a cached
  `None` value is distinguishable from a miss.
- Expired entries return `default` (fall through without returning data).

**`write_cache(path, data)`**:
- `path.parent.mkdir(parents=True, exist_ok=True)`.
- Write `json.dumps({"timestamp": time.time(), "data": data})` (UTF-8, **no
  indent, no atomic replace, no chmod** — this is the low-value cache, unlike
  `usage_store`/`atomic_write_json`).

On-disk cache format: `{"timestamp": <epoch seconds float>, "data": <any JSON>}`.

---

## 5. `models.py` — relevant data structures

Scoped to this area's producers/consumers. (Alias/`Platform`/`SwitchTransaction`
belong to other specs but `Platform.detect` and `get_timestamp` are referenced
here.)

### 5.1 `Platform` enum + `detect()`

`MACOS, LINUX, WSL, WINDOWS, UNKNOWN` (`enum.auto()`). `detect()` uses
`sys.platform` (**not** `platform.system()` — the latter runs a WMI query on
Windows that can hang):
- `"darwin"` → MACOS; `"win32"` → WINDOWS;
- starts with `"linux"` → WSL if `os.environ.get("WSL_DISTRO_NAME")` else LINUX;
- else UNKNOWN.

Used by `paths.get_backup_root()` to choose the backup root layout.

### 5.2 `AccountInfo` (dataclass)

Fields: `email: str`, `uuid: str`, `organization_uuid: str`,
`organization_name: str`, `added: str`, `number: int`.
- `is_organization` → `bool(organization_uuid)`.
- `display_label` → `f"{email} [{org_name or 'personal'}]"`.
- `from_dict(number, data)`: reads keys `email`, `uuid`, `organizationUuid`,
  `organizationName`, `added` (missing → `""`; `None` org fields coerced to `""`
  via `... or ""`).
- `to_dict()`: emits keys `email`, `uuid`, `organizationUuid`, `organizationName`,
  `added` (**no `number`** — number is the map key, not serialized).

### 5.3 `AccountSnapshot` / `AccountsSnapshot` (frozen dataclasses)

`AccountSnapshot` fields: `number: str`, `email: str`, `org_name: str`,
`org_uuid: str`, `is_active: bool`, `kind: str` (`"oauth"` | `"api_key"`),
`switchable: bool`, `usage: UsageEntry`, `alias: str = ""`,
`disabled: bool = False`. `display_tag` → `org_name or "personal"`.

`AccountsSnapshot` fields: `active_number: str | None`,
`accounts: tuple[AccountSnapshot, ...]`, `taken_at: float`. A coherent one-pass
view (metadata + active detection + usage all from the same collect pass).

`get_timestamp() -> str` → `datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")`.

---

## 6. Edge cases & subtleties (from tests)

- **`fetch_usage` HTTP-error test** asserts the WARNING message contains
  `"Usage fetch failed"` and the exception repr
  `"<HTTPError 429: 'Too Many Requests'>"` — the repr formatting matters.
- **`build_usage_result` `pct` typing asymmetry**: `five_hour`/`seven_day`
  `pct` is the raw `utilization` value **uncoerced** (could be int); `spend.pct`
  and `scoped.pct` are explicitly `float(...)`. Tests read `== 22.0` so a JSON
  int/float both satisfy, but the Go port should store these as float64
  consistently — note the Python source's inconsistency is not behaviorally
  observable in the tests but the raw value passes through for 5h/7d.
- **Null `resets_at`** → the window entry omits `clock`/`countdown` keys
  entirely (not empty strings). Renderers must handle a window with only `pct`.
- **`extra_usage` gating**: spend is produced only when `is_enabled` truthy AND
  all of `used_credits`/`monthly_limit`/`utilization` are non-None. `monthly_limit
  is None` = unlimited → no spend row, but 5h/7d/scoped survive. Disabled
  (`is_enabled: False`) with valid numbers → no spend.
- **Cents conversion**: `used_credits`/`monthly_limit` are integer cents;
  divided by 100. `currency` defaults to `"USD"`.
- **Scoped surfacing**: only `limits[]` entries whose `scope.model.display_name`
  is truthy and whose `percent` is numeric become `scoped`. `scope: null`
  entries (session, weekly_all) are dropped. No `limits` key → no `scoped` key.
- **`account_headroom` "unknown" vs "at-limit"**: only-spend usage → `None`
  (unknown, never auto-skipped); a scoped window with an unlisted model and no
  5h/7d → `None`. This is distinct from `0.0` (at-limit).
- **`try_refresh` KeyError safety**: a success response missing `access_token`
  raises `KeyError` inside the try → caught by the generic `except Exception` →
  `"transient"` (not a crash).
- **Proactive-refresh short-circuit**: expired token + `invalid_grant` refresh
  returns `error="invalid_grant"` and **never calls `request_usage_data`**
  (verified: `usage.assert_not_called()`).
- **401-retry classification**: after a usage 401 on an inactive account, a
  refresh that returns `invalid_grant` → outcome `"invalid_grant"`; a `transient`
  refresh → outcome `"refresh-failed"`. These two must stay distinct (quarantine
  vs retry).
- **Active-account invariants**: an active account with an expired token
  performs **no** refresh POST and returns `None` on a usage 401; an active
  account that 401s does **not** retry-with-refresh. `persist` is never called
  for active accounts.
- **Retry-After parsing**: seconds form only; HTTP-date form → `None`; negative
  → clamped to `0.0`; missing header → `None`.
- **`_classify_usage_error` timeout aliasing**: `TimeoutError`,
  `socket.timeout` (alias of `TimeoutError` since 3.10), and
  `URLError(TimeoutError())` all classify as `"timeout"`;
  `URLError(ConnectionRefusedError())` → `"network"`.
- **Store legacy-file handling**: a version-less `{"timestamp":..,"data":..}`
  file and a corrupt file both read as empty (`UsageEntry()`); no exception.
- **Stale-on-error**: a failure record keeps `lastGood`/`fetchedAt`/`age_s`
  intact while bumping `consecutiveFailures`/`lastError`/`backoffUntil`.
- **Decision-trust ceiling**: `decision_value()` returns `None` once `age_s >
  TRUST_MAX_AGE_S` **even in failure state** (test: 2 consecutive failures across
  a >3600s gap → `None`, but `last_good` still visible to display).
- **Extended trust conditions** (any one, and `age_s <= TRUST_MAX_AGE_S`):
  `consecutive_failures > 0`, OR within `next_poll_at` (strict `<`), OR a live
  claim (`now - last_attempt_at < CLAIM_TTL_S`). Once **overdue** (past
  `next_poll_at`) with no failures/claim, staleness reverts to unknown at
  `STALE_OK_S`.
- **`last429At` persistence**: set on any `http-429` failure; **not cleared by a
  later success** (survives recovery so the planner can floor the cadence). Only
  `http-429` sets it — a `timeout` failure leaves it `None`.
- **Dead-token quarantine**: a **single** `invalid_grant` (`AUTH_DEAD_STRIKES=1`)
  marks `token_dead()`. Transient errors never advance or reset the strike count
  (5×`http-429` → still not dead). A success resets strikes to 0.
  `clear_dead_token` resets strikes + failure/backoff fields. A dead token is
  never nominated by `due_candidate` and never won by `reserve` (both modes),
  even after backoff has long expired.
- **`reserve` race semantics**: the stamp *is* the claim — an immediate second
  `reserve` of the same slot returns `[]` (loser). A fresh entry (within
  `SERVE_TTL_S`) is not won by `respect_plans=True`. A due plan wins for
  `respect_plans=False` even inside the serve TTL (urgent cadence beats the TTL);
  on-demand callers still respect freshness. Backoff blocks both modes. An
  identity-mismatch slot is won immediately (row replaced).
- **Claim trust bridge**: after another process wins `reserve` (stamping
  `lastAttemptAt`), a concurrent `entries()` read keeps `trust_extended=True` and
  `decision_value()==last_good` for the `CLAIM_TTL_S` window, then reverts to
  `None` if no result is recorded (crashed claimer ages out).
- **`due_candidate` ordering**: missing entry and never-fetched entry both rank
  0 (most due); among fetched, stalest `fetched_at` wins; sentinel/dead/backoff/
  not-yet-due are skipped.
- **Poll-policy `Z` handling**: reset timestamps arrive as ISO with a trailing
  `Z`; `parse_reset_ts` substitutes `Z`→`+00:00` before `fromisoformat`.
- **Jitter determinism**: `conftest.py` sets `JITTER_FRAC = 0.0` (autouse) for
  all non-`test_poll_policy` tests, so store/cadence integration tests are
  clock-exact. The jitter itself is tested via injected `rng` in
  `test_poll_policy`.
- **cache `MISSING` sentinel**: `read_cache` returns a unique object (not `None`)
  on miss/expiry/corruption so callers can cache a genuine `None`.

---

## 7. Go port notes

### 7.1 Concurrency / threading model
- **No in-process threads or async** in these modules. Concurrency is
  **cross-process** via a file lock (`FileLock` over `fcntl.flock` on POSIX,
  `msvcrt.locking` on Windows) on `<cache_dir>/.usage.lock`. The Go port needs an
  equivalent advisory file lock (`golang.org/x/sys/unix` `Flock` on POSIX;
  `LockFileEx` on Windows) with a **10-second acquire timeout** polling every
  **0.1s** (`time.monotonic`), raising a `LockError`-equivalent on timeout with
  message `"Failed to acquire lock - another instance may be running"`.
- **Lock discipline is a hard contract**: never hold the lock across network
  I/O. The read-decide-claim / fetch-unlocked / re-read-merge-write cycle must be
  preserved. `entries()` reads are lock-free and rely on atomic file replacement
  for consistency.
- The `reserve()` re-check-under-lock is the race fix vs a naive
  read-then-claim; the Go port must keep eligibility evaluation and the
  `lastAttemptAt` stamp inside the same locked section.

### 7.2 Atomic writes & file modes
- `usage_store` writes go through `atomic_write_json`: `mkstemp` in the target
  dir, write `json.dumps(indent=2)`, `os.replace` (atomic rename), then
  `chmod 0600` on the file and `chmod 0700` on the parent dir — **only on
  non-Windows** (`sys.platform != "win32"` guards both). Go: `os.CreateTemp` +
  `os.Rename` + `os.Chmod`, skipping chmod on Windows.
- `cache.py`'s `write_cache` is **not** atomic and does **not** chmod — a plain
  `write_text`. Preserve this difference (it's a low-value cache).

### 7.3 Time & epoch units (easy to get wrong)
- OAuth `expiresAt` and token `expires_in`: **`expiresAt` is epoch
  milliseconds**, `expires_in` is **seconds** — refresh computes
  `now_ms + expires_in*1000`. `OAUTH_EXPIRY_BUFFER_MS` is in **ms**.
- Usage store timestamps (`fetchedAt`, `backoffUntil`, `nextPollAt`,
  `lastAttemptAt`, `last429At`) and cache `timestamp` are epoch **seconds**
  (Python `time.time()` float).
- `now_ms = int(datetime.now(timezone.utc).timestamp() * 1000)` — Go:
  `time.Now().UnixMilli()`.
- The store takes an injectable `clock() -> float` (seconds) for tests — the Go
  port should keep a `clock func() float64` seam.

### 7.4 Datetime parsing / formatting
- `datetime.fromisoformat` is used for `resets_at`. In `poll_policy` a bare `Z`
  is converted to `+00:00` first; in `oauth.format_reset` the raw string is passed
  straight to `fromisoformat`, which means the API's `resets_at` values there
  must be `fromisoformat`-parseable as-is (they include offset/`Z` per the test
  fixtures using `future.isoformat()`). Go: parse with `time.RFC3339`/`RFC3339Nano`,
  and also accept the offset form; normalize `Z`.
- `format_reset` clock string uses **local time** (`astimezone()`), day without
  zero-pad (`str(reset_local.day)`), `%b` month abbreviation, `%H:%M` 24-hour.
  Go equivalents: `time.Local`, format `"15:04"` same-day, `"Jan 2 15:04"`
  cross-day (note `2` not `02` for the day, matching `str(day)`).
- `get_timestamp` format `"%Y-%m-%dT%H:%M:%SZ"` → Go `"2006-01-02T15:04:05Z"`
  in UTC.

### 7.5 Error classification is string-token driven
- Callers branch on exact string tokens (`"invalid_grant"`, `"transient"`,
  `"no_refresh_token"`, `"no-access-token"`, `"refresh-failed"`, `"http-429"`,
  `"timeout"`, `"network"`, `"bad-response"`). The `http-{code}` token is
  `fmt.Sprintf("http-%d", code)`. `PERMANENT_AUTH_ERRORS = {"invalid_grant"}`.
  Keep these literals identical.
- The dead-vs-transient refresh rule is a **substring** check on the raw HTTP
  error body for `"invalid_grant"` or `"invalid_client"`, gated on status
  ∈ {400,401,403}. Go: read the error body (bounded), lowercase-insensitive?
  **No** — the Python check is case-sensitive substring; match exactly.

### 7.6 HTTP client specifics
- `urllib` default: system CA verification, no proxy config, no retries. Go:
  `net/http` with default `http.Client`, an explicit per-request timeout
  (context or `Client.Timeout`). Timeouts: **10s** for token refresh, **5s** for
  profile and usage.
- `HTTPError` in Python is raised for any non-2xx; Go must treat
  `resp.StatusCode >= 400` (or `>= 300`?) as the error path. Python's urllib
  raises `HTTPError` for 4xx/5xx (3xx are followed by default). Match: any
  non-successful final status → the error path; `resp.StatusCode` supplies the
  `http-{code}` token.
- Retry-After: read from response header, parse as float seconds, clamp negative
  to 0, ignore non-numeric (HTTP-date). Only present on `HTTPError` (4xx/5xx).
- Request bodies: token refresh sends `Content-Type: application/json`; profile
  sends `Content-Type: application/json` (a GET with that header — harmless);
  usage sends **no** Content-Type. All send `User-Agent: claude-swap/1.0`. Usage
  additionally sends `anthropic-beta: oauth-2025-04-20`. Authorization is
  `Bearer <access_token>` on profile and usage.

### 7.7 JSON handling nuances
- Preserve unknown top-level credential fields across a refresh (mutate only the
  nested `claudeAiOauth` object, re-serialize the whole map). Go: decode into
  `map[string]any`, mutate the nested map, re-encode.
- `refresh_token` in the response rotates the stored one only when present/truthy;
  otherwise keep the old refresh token.
- `scope` (space-delimited) → `scopes` (list) only when present.
- The normalized usage dict is stored verbatim into `lastGood`; the Go port
  should keep it as a JSON-serializable structure (a typed struct is fine but
  must round-trip the exact keys: `five_hour`/`seven_day` with
  `pct`/`resets_at`/`countdown`/`clock`; `spend` with
  `used`/`limit`/`pct`/`currency`/`resets_at`/`countdown`/`clock`; `scoped` as an
  array of `name`/`pct`/`resets_at`/`countdown`/`clock`).

### 7.8 Platform-conditional logic
- `Platform.detect()` via `sys.platform` (avoid a hanging WMI query). Go:
  `runtime.GOOS` (`darwin`/`windows`/`linux`) plus a `WSL_DISTRO_NAME` env check
  for WSL.
- Backup root: Linux/WSL follow XDG (`$XDG_DATA_HOME/claude-swap`, default
  `~/.local/share/claude-swap`; XDG var ignored unless set, non-empty, and
  **absolute**, with `~` expansion); macOS/Windows/unknown use legacy
  `~/.claude-swap-backup`. (Detail lives in the paths spec, but `cache.py`'s
  `CACHE_DIR` and the usage store's default cache dir depend on it.)
- Credentials file location and macOS Keychain vs Linux/WSL
  `~/.claude/.credentials.json` distinction is Claude Code's own contract (see
  paths spec); this module is agnostic — it takes a credentials **string**.

### 7.9 Python-isms needing deliberate Go equivalents
- `MISSING = object()` sentinel (identity-based) → Go: a distinct sentinel value
  or a `(value, found bool)` return.
- Frozen dataclasses with defaults → Go structs; `dataclasses.replace` (used by
  `with_sentinel`) → return a copy with one field changed.
- `decision_value()` returns a union `dict | str | None` — Go needs an `any` or a
  small tagged type distinguishing "usage dict", "sentinel string", "unknown".
- `_num_or_none` tolerant numeric coercion (accept int or float from JSON) → Go
  must accept `json.Number`/float64 and reject other types.
- Logging: a single named logger `"claude-swap"` with WARNING lines going to the
  default log file and DEBUG lines gated behind `--debug` console handler. The
  **paste-safe invariant** (never log email in the usage-failure WARNING) must be
  preserved.
- `printer.warning` prints yellow-styled text to stdout — the `_persist` failure
  path emits both a WARNING log (with email) **and** a stdout warning (with email);
  keep both channels.
