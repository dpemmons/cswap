// The update-notice hint clause, keyed by install shape and platform.
//
// Implements the hint half of spec 08§13.2 (check_for_update), redesigned per
// Amendment A6 to key off InstallShape instead of Python's uv/pipx
// _detect_install_method.
package update

import "git.dpemmons.com/dpemmons/cswap/internal/platform"

// UpgradeHint returns the actionable clause appended to an update notice.
//
// Python's check_for_update picks among three hints keyed by install method
// (uv/pipx/unknown) and platform (spec 08§13.2); the Go redesign has no
// uv/pipx equivalent, so the same three-way shape is reproduced against
// InstallShape instead (Amendment A6): a go-install shape on a
// self-upgrade-capable platform gets the "cswap upgrade does it" hint, the
// same shape on Windows gets the literal command (SelfUpgrade there is
// print-only — the running .exe is locked), and an unknown shape gets the
// generic "see instructions" hint.
func UpgradeHint(shape InstallShape, plat platform.Platform) string {
	switch {
	case shape == ShapeGoInstall && plat != platform.Windows:
		return "Run `cswap upgrade` to update."
	case shape == ShapeGoInstall:
		return "Run `go install " + ModulePath + "@latest` to update."
	default:
		return "Run `cswap upgrade` for upgrade instructions."
	}
}
