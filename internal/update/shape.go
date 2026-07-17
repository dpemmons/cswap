// Install-shape detection: does the running binary live in a Go-managed bin
// directory (and can therefore self-upgrade via `go install`), or not.
//
// Implements the "SelfUpgrade install-shape detection" half of Amendment A6,
// replacing Python's _detect_install_method (uv/pipx sys.prefix sniffing,
// spec 08§13.3) — there is no uv/pipx equivalent for a Go binary.
package update

import "path/filepath"

// InstallShape classifies how the running binary was installed, driving both
// the update-notice hint (UpgradeHint) and SelfUpgrade's behavior.
type InstallShape int

const (
	// ShapeUnknown means the binary's directory isn't a recognized Go bin dir;
	// SelfUpgrade falls back to printing manual guidance.
	ShapeUnknown InstallShape = iota
	// ShapeGoInstall means the binary resolves inside $GOBIN, $GOPATH/bin, or
	// $HOME/go/bin; SelfUpgrade can run `go install <ModulePath>@latest`.
	ShapeGoInstall
)

// String returns a short label for the shape (used in guidance text/logs).
func (s InstallShape) String() string {
	if s == ShapeGoInstall {
		return "go-install"
	}
	return "unknown"
}

// DetectInstallShape classifies exePath (typically the symlink-resolved
// result of os.Executable()) by checking whether its containing directory is
// one of the three Go-managed bin dirs named in Amendment A6: $GOBIN (if
// set), $GOPATH/bin (GOPATH defaulting to $HOME/go, mirroring `go env
// GOPATH`), and — always, even when GOPATH is set to something else —
// $HOME/go/bin explicitly.
//
// getenv and homeDir are seams: production callers pass os.Getenv and the
// result of os.UserHomeDir(); tests pass fakes to exercise every branch
// without touching the real environment.
func DetectInstallShape(exePath string, getenv func(string) string, homeDir string) InstallShape {
	if exePath == "" {
		return ShapeUnknown
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	dir := filepath.Clean(filepath.Dir(exePath))
	for _, cand := range goBinDirs(getenv, homeDir) {
		if cand != "" && filepath.Clean(cand) == dir {
			return ShapeGoInstall
		}
	}
	return ShapeUnknown
}

// goBinDirs returns the candidate Go-managed bin directories, in the order
// Amendment A6 lists them.
func goBinDirs(getenv func(string) string, homeDir string) []string {
	var dirs []string
	if gobin := getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	gopath := getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(homeDir, "go")
	}
	dirs = append(dirs, filepath.Join(gopath, "bin"))
	if homeDir != "" {
		dirs = append(dirs, filepath.Join(homeDir, "go", "bin"))
	}
	return dirs
}
