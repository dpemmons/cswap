// Package version holds the build-time-injected program version.
//
// Implements DESIGN A5 / audit 10 Gap 1. There is no importlib.metadata in Go:
// the version is embedded at build time via
//
//	-ldflags "-X git.dpemmons.com/dpemmons/cswap/internal/version.Version=v0.1.0"
//
// The port's own versions are semver with a leading v; Display strips the v for
// parity with Python's --version / swapVersion presentation.
package version

import "strings"

// Version is the semver build version (leading v). Overridden at link time; the
// default marks an un-injected local build.
var Version = "v0.0.0-dev"

// Display returns Version without its leading v, the form shown by --version and
// carried in the export envelope's swapVersion.
func Display() string {
	return strings.TrimPrefix(Version, "v")
}
