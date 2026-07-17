// Tests for Checker.CheckForUpdate: httptest release endpoint, semver
// compare table incl. pre-release, cache TTL/negative-result caching.
//
// Implements spec 08§13.2 test coverage (check_for_update), Amendment A6.
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

func fakeAt(epoch float64) *clock.Fake {
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return clock.NewFake(time.Unix(sec, nsec))
}

// releaseServer returns an httptest server that serves tagName as the
// releases endpoint's tag_name field (or a non-200 when tagName is "").
func releaseServer(t *testing.T, tagName string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tagName == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tagName})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withEndpoint swaps the package-level Endpoint var for the test's duration.
func withEndpoint(t *testing.T, url string) {
	t.Helper()
	prev := Endpoint
	Endpoint = url
	t.Cleanup(func() { Endpoint = prev })
}

func TestCheckForUpdate_NewerVersionAvailable(t *testing.T) {
	srv := releaseServer(t, "v0.3.0")
	withEndpoint(t, srv.URL)

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	msg := c.CheckForUpdate("", "v0.2.0", platform.Linux)
	want := "A newer version of claude-swap is available (0.3.0). You are using 0.2.0. " +
		"Run `cswap upgrade` for upgrade instructions."
	if msg != want {
		t.Errorf("CheckForUpdate = %q, want %q", msg, want)
	}
}

func TestCheckForUpdate_UpToDate(t *testing.T) {
	srv := releaseServer(t, "v0.2.0")
	withEndpoint(t, srv.URL)

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	if msg := c.CheckForUpdate("", "v0.2.0", platform.Linux); msg != "" {
		t.Errorf("CheckForUpdate at same version = %q, want \"\"", msg)
	}
}

// TestCheckForUpdate_SemverCompareTable covers the pre-release comparator
// (Deviation #1: Go's real semver DOES notify a pre-release build of a
// newer release, unlike Python's int-tuple parser which silently never did).
func TestCheckForUpdate_SemverCompareTable(t *testing.T) {
	cases := []struct {
		name       string
		latest     string
		current    string
		wantNotice bool
	}{
		{"newer patch", "v0.2.1", "v0.2.0", true},
		{"newer minor", "v0.3.0", "v0.2.9", true},
		{"newer major", "v1.0.0", "v0.9.9", true},
		{"same version", "v0.2.0", "v0.2.0", false},
		{"older version", "v0.1.0", "v0.2.0", false},
		{"pre-release current, newer release available", "v0.22.0", "v0.22.0-beta.1", true},
		{"pre-release current, same release not newer", "v0.22.0-beta.1", "v0.22.0-beta.1", false},
		{"pre-release latest newer than release current", "v0.23.0-beta.1", "v0.22.0", true},
		{"invalid latest (no v prefix, e.g. legacy Python data)", "0.21.0", "v0.2.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := releaseServer(t, tc.latest)
			withEndpoint(t, srv.URL)

			c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
			msg := c.CheckForUpdate("", tc.current, platform.Linux)
			if (msg != "") != tc.wantNotice {
				t.Errorf("CheckForUpdate(latest=%s, current=%s) = %q, wantNotice=%v",
					tc.latest, tc.current, msg, tc.wantNotice)
			}
		})
	}
}

func TestCheckForUpdate_FetchFailureNoNotice(t *testing.T) {
	srv := releaseServer(t, "") // 404
	withEndpoint(t, srv.URL)

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	if msg := c.CheckForUpdate("", "v0.1.0", platform.Linux); msg != "" {
		t.Errorf("CheckForUpdate on fetch failure = %q, want \"\"", msg)
	}
}

func TestCheckForUpdate_UnreachableEndpointNoNotice(t *testing.T) {
	// Point at a server that immediately closes so the client sees a
	// connection error (any error -> no notice, spec 08§13.2).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable
	withEndpoint(t, url)

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	if msg := c.CheckForUpdate("", "v0.1.0", platform.Linux); msg != "" {
		t.Errorf("CheckForUpdate on unreachable endpoint = %q, want \"\"", msg)
	}
}

// TestCheckForUpdate_TimeoutNoNotice proves a slow endpoint is treated like
// any other failure — no notice — without slowing the test suite down (a
// shrunk FetchTimeout, restored after).
func TestCheckForUpdate_TimeoutNoNotice(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang past the shrunk timeout
	}))
	// Cleanups run LIFO: unblock the handler *before* srv.Close() waits for it,
	// or Close would deadlock forever on the still-blocked in-flight handler.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	withEndpoint(t, srv.URL)

	prevTimeout := FetchTimeout
	FetchTimeout = 20 * time.Millisecond
	t.Cleanup(func() { FetchTimeout = prevTimeout })

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	if msg := c.CheckForUpdate("", "v0.1.0", platform.Linux); msg != "" {
		t.Errorf("CheckForUpdate on timeout = %q, want \"\"", msg)
	}
}

// TestCheckForUpdate_CacheHitSkipsNetwork proves a fresh cache entry is used
// without a second network round-trip (spec 08§13.2: "this skips the network
// entirely while fresh").
func TestCheckForUpdate_CacheHitSkipsNetwork(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	t.Cleanup(srv.Close)
	withEndpoint(t, srv.URL)

	dir := t.TempDir()
	c := Checker{CacheDir: dir, Clk: fakeAt(1000)}
	msg1 := c.CheckForUpdate("", "v0.2.0", platform.Linux)
	if msg1 == "" {
		t.Fatal("first CheckForUpdate should notice v0.3.0")
	}
	if calls != 1 {
		t.Fatalf("expected 1 network call after first check, got %d", calls)
	}

	// Same clock (well within the 24h TTL) -> cache hit, no second call.
	c2 := Checker{CacheDir: dir, Clk: fakeAt(1000 + 60)}
	msg2 := c2.CheckForUpdate("", "v0.2.0", platform.Linux)
	if msg2 != msg1 {
		t.Errorf("cached CheckForUpdate = %q, want %q", msg2, msg1)
	}
	if calls != 1 {
		t.Errorf("expected still 1 network call after cache hit, got %d", calls)
	}
}

// TestCheckForUpdate_NegativeResultCached proves a failed fetch is cached too
// (spec 08§13.2: "write_cache regardless of success/failure ... caches
// failures as None, so a down endpoint is retried at most once per TTL").
func TestCheckForUpdate_NegativeResultCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	withEndpoint(t, srv.URL)

	dir := t.TempDir()
	c1 := Checker{CacheDir: dir, Clk: fakeAt(1000)}
	if msg := c1.CheckForUpdate("", "v0.1.0", platform.Linux); msg != "" {
		t.Fatalf("first CheckForUpdate = %q, want \"\" (fetch failed)", msg)
	}
	if calls != 1 {
		t.Fatalf("expected 1 network call, got %d", calls)
	}

	// Within TTL: should read the cached "" (negative-cached) result, not fetch
	// again, even though the endpoint would now succeed.
	c2 := Checker{CacheDir: dir, Clk: fakeAt(1000 + 60)}
	if msg := c2.CheckForUpdate("", "v0.1.0", platform.Linux); msg != "" {
		t.Errorf("second (cached) CheckForUpdate = %q, want \"\"", msg)
	}
	if calls != 1 {
		t.Errorf("expected still 1 network call (negative result cached), got %d", calls)
	}
}

// TestCheckForUpdate_CacheExpiresRefetches proves an expired cache entry
// triggers a fresh network call.
func TestCheckForUpdate_CacheExpiresRefetches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	t.Cleanup(srv.Close)
	withEndpoint(t, srv.URL)

	dir := t.TempDir()
	c1 := Checker{CacheDir: dir, Clk: fakeAt(1000)}
	c1.CheckForUpdate("", "v0.2.0", platform.Linux)
	if calls != 1 {
		t.Fatalf("expected 1 network call, got %d", calls)
	}

	// 25 hours later -> past the 24h TTL -> refetch.
	c2 := Checker{CacheDir: dir, Clk: fakeAt(1000 + 25*3600)}
	c2.CheckForUpdate("", "v0.2.0", platform.Linux)
	if calls != 2 {
		t.Errorf("expected a refetch after TTL expiry, got %d calls", calls)
	}
}

// TestCheckForUpdate_HintByInstallShape proves the notice's trailing hint
// varies with install shape and platform (Amendment A6's redesigned
// equivalent of Python's uv/pipx hint dispatch, spec 08§13.2).
func TestCheckForUpdate_HintByInstallShape(t *testing.T) {
	gobin := filepath.Join(t.TempDir(), "gobin")
	exe := filepath.Join(gobin, "cswap")

	cases := []struct {
		name     string
		exePath  string
		plat     platform.Platform
		wantHint string
	}{
		{"go-install shape, linux", exe, platform.Linux, "Run `cswap upgrade` to update."},
		{"go-install shape, windows", exe, platform.Windows,
			"Run `go install " + ModulePath + "@latest` to update."},
		{"unknown shape", filepath.Join(t.TempDir(), "cswap"), platform.Linux,
			"Run `cswap upgrade` for upgrade instructions."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := releaseServer(t, "v0.9.0")
			withEndpoint(t, srv.URL)

			c := Checker{
				CacheDir: t.TempDir(),
				Clk:      fakeAt(1000),
				Getenv: func(k string) string {
					if k == "GOBIN" {
						return gobin
					}
					return ""
				},
				HomeDir: t.TempDir(),
			}
			msg := c.CheckForUpdate(tc.exePath, "v0.1.0", tc.plat)
			if msg == "" {
				t.Fatal("expected a notice")
			}
			want := "A newer version of claude-swap is available (0.9.0). You are using 0.1.0. " + tc.wantHint
			if msg != want {
				t.Errorf("CheckForUpdate = %q, want %q", msg, want)
			}
		})
	}
}

// TestCheckForUpdate_ReadsPythonFixtureCacheGracefully proves that reading a
// Python-produced update_check.json (whose data is a PEP440-style version
// with no "v" prefix — a different scheme entirely, since the whole endpoint
// moved from PyPI to Forgejo) degrades to "no notice" rather than panicking
// (the invalid-semver branch of the "any error -> no notice" contract).
func TestCheckForUpdate_ReadsPythonFixtureCacheGracefully(t *testing.T) {
	fixture := filepath.Join(testutil.FixturesDir(t), "claude-swap-data", "cache")
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(fixture, "update_check.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "update_check.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// The fixture's timestamp is 1784277945.4183247; read well within a day of it.
	c := Checker{CacheDir: dir, Clk: fakeAt(1784277945 + 60)}
	if msg := c.CheckForUpdate("", "v0.2.0", platform.Linux); msg != "" {
		t.Errorf("CheckForUpdate over legacy fixture cache = %q, want \"\"", msg)
	}
}

func TestReleaseEndpointReceivesRequest(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.9.9"})
	}))
	t.Cleanup(srv.Close)
	withEndpoint(t, srv.URL+"/api/v1/repos/dpemmons/cswap/releases/latest")

	c := Checker{CacheDir: t.TempDir(), Clk: fakeAt(1000)}
	msg := c.CheckForUpdate("", "v0.1.0", platform.Linux)
	if msg == "" {
		t.Fatal("expected a notice")
	}
	if want := "/api/v1/repos/dpemmons/cswap/releases/latest"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
}

func TestDefaultEndpointAndCacheTTL(t *testing.T) {
	if Endpoint != "https://git.dpemmons.com/api/v1/repos/dpemmons/cswap/releases/latest" {
		t.Errorf("default Endpoint = %q, unexpected", Endpoint)
	}
	if fmt.Sprint(CacheTTL) != "24h0m0s" {
		t.Errorf("CacheTTL = %v, want 24h0m0s", CacheTTL)
	}
}
