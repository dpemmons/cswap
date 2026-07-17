package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStringAndIsWindowsConsistent(t *testing.T) {
	cases := map[Platform]string{
		MacOS:   "macos",
		Linux:   "linux",
		WSL:     "wsl",
		Windows: "windows",
		Unknown: "unknown",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", int(p), got, want)
		}
	}
	// Detect and IsWindows must agree about Windows.
	if (Detect() == Windows) != IsWindows() {
		t.Errorf("Detect()==Windows (%v) disagrees with IsWindows() (%v)", Detect() == Windows, IsWindows())
	}
}

func TestRunningInContainerEnvShortCircuit(t *testing.T) {
	// Env var wins before any file/platform probe.
	for _, key := range []string{"CONTAINER", "container"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("CONTAINER", "")
			t.Setenv("container", "")
			t.Setenv(key, "1")
			// Point all probe paths at nonexistent files: still true via env.
			withProbePaths(t, "/nonexistent-a", "/nonexistent-b", "/nonexistent-c")
			if !RunningInContainer() {
				t.Fatalf("expected true when %s set", key)
			}
		})
	}
}

func TestRunningInContainerFileProbes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name                  string
		dockerenv             string
		cgroupBody, mountBody string
		want                  bool
	}{
		{"none", missing, "0::/init.scope\n", "rootfs / rootfs rw\n", false},
		{"dockerenv present", write("dockerenv", ""), "", "", true},
		{"cgroup kubepods", missing, "12:cpuset:/kubepods/pod\n", "rootfs / rootfs rw\n", true},
		{"cgroup lxc", missing, "1:name=systemd:/lxc/abc\n", "rootfs / rootfs rw\n", true},
		{"mountinfo overlay", missing, "0::/init.scope\n", "1 1 0:1 / / rw - overlay overlay rw\n", true},
		// overlay is NOT in the cgroup set; lxc is NOT in the mountinfo set:
		{"cgroup has overlay only -> no match", missing, "1:name:/overlay/x\n", "rootfs / rootfs rw\n", false},
		{"mountinfo has lxc only -> no match", missing, "0::/init.scope\n", "1 1 0:1 / / rw - lxc lxc rw\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONTAINER", "")
			t.Setenv("container", "")
			cg := missing
			if tt.cgroupBody != "" {
				cg = write("cgroup", tt.cgroupBody)
			}
			mi := missing
			if tt.mountBody != "" {
				mi = write("mountinfo", tt.mountBody)
			}
			withProbePaths(t, tt.dockerenv, cg, mi)
			if got := RunningInContainer(); got != tt.want {
				t.Errorf("RunningInContainer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func withProbePaths(t *testing.T, docker, cgroup, mount string) {
	t.Helper()
	od, oc, om := containerDockerEnvPath, containerCgroupPath, containerMountinfoPath
	containerDockerEnvPath, containerCgroupPath, containerMountinfoPath = docker, cgroup, mount
	t.Cleanup(func() {
		containerDockerEnvPath, containerCgroupPath, containerMountinfoPath = od, oc, om
	})
}
