// The passive update check: query the release endpoint (through a 24h TTL
// cache), compare against the running build's version with real semver, and
// compose the human-readable notice string cli prints muted to stderr.
//
// Implements spec 08§13.2 (check_for_update), redesigned per DESIGN.md §6
// Deviations 1–2 and Amendment A6 (Forgejo releases endpoint, tag_name field,
// 3s timeout, any-error→no-notice, negative-result caching, {timestamp,data}
// 24h-TTL cache at <backup_root>/cache/update_check.json).
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// Checker performs the passive update check. The zero value is usable in
// production: it queries the real Endpoint via http.DefaultClient against the
// real wall clock and OS environment. Tests override every seam (CacheDir is
// mandatory — there is no sane default).
type Checker struct {
	// CacheDir is <backup_root>/cache — the same directory usage.NewStore uses
	// for cache/usage.json (spec 04§2.5); this checker stores
	// cache/update_check.json alongside it.
	CacheDir string
	// HTTPClient issues the releases-endpoint request; nil -> http.DefaultClient.
	HTTPClient *http.Client
	// Clk supplies the cache read/write timestamp; nil -> clock.System{}.
	Clk clock.Clock
	// Getenv looks up GOBIN/GOPATH for install-shape detection (feeds the
	// notice's hint); nil -> os.Getenv.
	Getenv func(string) string
	// HomeDir is $HOME for install-shape detection; "" -> os.UserHomeDir().
	HomeDir string
}

func (c Checker) clock() clock.Clock {
	if c.Clk != nil {
		return c.Clk
	}
	return clock.System{}
}

func (c Checker) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Checker) getenv() func(string) string {
	if c.Getenv != nil {
		return c.Getenv
	}
	return os.Getenv
}

func (c Checker) homeDir() string {
	if c.HomeDir != "" {
		return c.HomeDir
	}
	h, _ := os.UserHomeDir()
	return h
}

// CheckForUpdate mirrors update_check.check_for_update: it returns a
// human-readable notice if a newer version is available, or "" otherwise.
// Every failure path (cache I/O, network, malformed JSON/version) is folded
// into "" — this never fails the caller, matching Python's blanket
// try/except → None (spec 08§13.2).
//
// currentVersion is the running build's v-prefixed semver string (Amendment
// A5, e.g. version.Version — NOT version.Display()'s stripped form; the
// comparator needs the "v" prefix). exePath is the running binary's path,
// used only for the install-shape hint (see DetectInstallShape/UpgradeHint).
func (c Checker) CheckForUpdate(exePath, currentVersion string, plat platform.Platform) string {
	cachePath := filepath.Join(c.CacheDir, "update_check.json")
	now := clock.Seconds(c.clock())

	var latest string
	if cached, ok := usage.ReadCache(cachePath, CacheTTL.Seconds(), now); ok {
		// A cached nil (a previously failed/empty fetch, negative-cached per
		// Amendment A6) type-asserts to "" here — correctly "no notice".
		latest, _ = cached.(string)
	} else {
		latest = c.fetchLatestTag()
		var data any
		if latest != "" {
			data = latest
		}
		// Write regardless of success/failure, caching negative results too
		// (spec 08§13.2). Write errors are swallowed — never fail the check.
		_ = usage.WriteCache(cachePath, data, now)
	}

	if latest == "" || !semver.IsValid(latest) || !semver.IsValid(currentVersion) {
		return ""
	}
	if semver.Compare(latest, currentVersion) <= 0 {
		return ""
	}

	shape := DetectInstallShape(exePath, c.getenv(), c.homeDir())
	hint := UpgradeHint(shape, plat)
	return fmt.Sprintf(
		"A newer version of claude-swap is available (%s). You are using %s. %s",
		strings.TrimPrefix(latest, "v"), strings.TrimPrefix(currentVersion, "v"), hint,
	)
}

// releaseResponse is the subset of the Forgejo (GitHub-compatible) releases
// schema this checker needs (Amendment A6).
type releaseResponse struct {
	TagName string `json:"tag_name"`
}

// fetchLatestTag queries Endpoint for the latest release's tag_name. Any
// error (build, network, non-200, decode) returns "" — the "any error → no
// notification" contract (spec 08§13.2).
func (c Checker) fetchLatestTag() string {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint, nil)
	if err != nil {
		return ""
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var rel releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return ""
	}
	return rel.TagName
}
