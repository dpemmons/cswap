// Install-shape detection with fake paths, and the UpgradeHint table.
//
// Implements the "install-shape detection with fake paths" test coverage
// called for by DESIGN §5 WP14 and Amendment A6.
package update

import (
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/platform"
)

func TestDetectInstallShape(t *testing.T) {
	home := string(filepath.Separator) + filepath.Join("home", "fake")
	gobin := string(filepath.Separator) + filepath.Join("opt", "gobin")
	gopath := string(filepath.Separator) + filepath.Join("gopath", "custom")

	envWith := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}

	cases := []struct {
		name    string
		exePath string
		getenv  func(string) string
		homeDir string
		want    InstallShape
	}{
		{
			name:    "GOBIN set, binary inside it",
			exePath: filepath.Join(gobin, "cswap"),
			getenv:  envWith(map[string]string{"GOBIN": gobin}),
			homeDir: home,
			want:    ShapeGoInstall,
		},
		{
			name:    "GOBIN set, binary elsewhere",
			exePath: filepath.Join(string(filepath.Separator), "usr", "local", "bin", "cswap"),
			getenv:  envWith(map[string]string{"GOBIN": gobin}),
			homeDir: home,
			want:    ShapeUnknown,
		},
		{
			name:    "GOPATH set, binary in GOPATH/bin",
			exePath: filepath.Join(gopath, "bin", "cswap"),
			getenv:  envWith(map[string]string{"GOPATH": gopath}),
			homeDir: home,
			want:    ShapeGoInstall,
		},
		{
			name:    "no GOBIN/GOPATH, binary in default $HOME/go/bin",
			exePath: filepath.Join(home, "go", "bin", "cswap"),
			getenv:  envWith(nil),
			homeDir: home,
			want:    ShapeGoInstall,
		},
		{
			name: "GOPATH set to something else, binary still in $HOME/go/bin",
			// Amendment A6 lists $HOME/go/bin as always checked, even when
			// GOPATH points elsewhere.
			exePath: filepath.Join(home, "go", "bin", "cswap"),
			getenv:  envWith(map[string]string{"GOPATH": gopath}),
			homeDir: home,
			want:    ShapeGoInstall,
		},
		{
			name:    "no env, binary in an unrelated dir",
			exePath: filepath.Join(string(filepath.Separator), "usr", "bin", "cswap"),
			getenv:  envWith(nil),
			homeDir: home,
			want:    ShapeUnknown,
		},
		{
			name:    "empty exePath",
			exePath: "",
			getenv:  envWith(map[string]string{"GOBIN": gobin}),
			homeDir: home,
			want:    ShapeUnknown,
		},
		{
			name:    "nil getenv treated as all-unset",
			exePath: filepath.Join(home, "go", "bin", "cswap"),
			getenv:  nil,
			homeDir: home,
			want:    ShapeGoInstall,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectInstallShape(tc.exePath, tc.getenv, tc.homeDir)
			if got != tc.want {
				t.Errorf("DetectInstallShape(%q) = %v, want %v", tc.exePath, got, tc.want)
			}
		})
	}
}

func TestInstallShapeString(t *testing.T) {
	if got := ShapeGoInstall.String(); got != "go-install" {
		t.Errorf("ShapeGoInstall.String() = %q, want go-install", got)
	}
	if got := ShapeUnknown.String(); got != "unknown" {
		t.Errorf("ShapeUnknown.String() = %q, want unknown", got)
	}
}

func TestUpgradeHint(t *testing.T) {
	cases := []struct {
		name  string
		shape InstallShape
		plat  platform.Platform
		want  string
	}{
		{"go-install, linux", ShapeGoInstall, platform.Linux, "Run `cswap upgrade` to update."},
		{"go-install, macos", ShapeGoInstall, platform.MacOS, "Run `cswap upgrade` to update."},
		{"go-install, windows", ShapeGoInstall, platform.Windows,
			"Run `go install " + ModulePath + "@latest` to update."},
		{"unknown, linux", ShapeUnknown, platform.Linux, "Run `cswap upgrade` for upgrade instructions."},
		{"unknown, windows", ShapeUnknown, platform.Windows, "Run `cswap upgrade` for upgrade instructions."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpgradeHint(tc.shape, tc.plat); got != tc.want {
				t.Errorf("UpgradeHint(%v, %v) = %q, want %q", tc.shape, tc.plat, got, tc.want)
			}
		})
	}
}
