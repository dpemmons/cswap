// Package update implements the Go-redesigned update check and self-upgrade
// subsystem.
//
// Implements spec 08§13 (update_check.py, redesigned) and 10-audit.md
// §Corrections.2, per DESIGN.md §6 Deviations 1–3 and Amendments A5/A6.
// Unlike Python's PyPI/uv/pipx-based checker, this package queries a Forgejo
// releases endpoint and compares versions with a real semver comparator
// (golang.org/x/mod/semver) on v-prefixed strings — so, deliberately unlike
// Python, a pre-release build DOES receive an update notice (Deviation #1).
// "Any error → no notification" and "cache failures too" are preserved
// (spec 08§13.2); the on-disk cache format is byte-compatible with Python's
// {"timestamp","data"} 24h-TTL shape via internal/usage's cache helpers
// (Amendment A6, A8).
package update

import "time"

// Endpoint is the Forgejo releases API for the canonical repo (Amendment A6).
// It is a package-level var (not a const) so it is overridable both via
// -ldflags "-X git.dpemmons.com/dpemmons/cswap/internal/update.Endpoint=..."
// at build time and by tests, which point it at an httptest server.
var Endpoint = "https://git.dpemmons.com/api/v1/repos/dpemmons/cswap/releases/latest"

// ModulePath is the Go module path `go install` targets for a self-upgrade
// (Amendment A6): `go install <ModulePath>@latest`.
var ModulePath = "git.dpemmons.com/dpemmons/cswap/cmd/cswap"

// ReleasesURL is shown in manual-upgrade guidance when the install shape
// cannot be determined.
var ReleasesURL = "https://git.dpemmons.com/dpemmons/cswap/releases"

// CacheTTL is the update-check cache freshness window: 24h, unchanged from
// Python (spec 08§13.1: CACHE_TTL = 24 * 3600).
const CacheTTL = 24 * time.Hour

// FetchTimeout bounds the releases-endpoint HTTP request (Amendment A6: 3s,
// vs. Python's 2s PyPI timeout). A var so tests can shrink it to exercise the
// timeout path without a slow test.
var FetchTimeout = 3 * time.Second
