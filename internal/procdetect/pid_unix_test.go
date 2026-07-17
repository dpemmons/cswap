//go:build !windows

package procdetect

import (
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func withKillProbe(t *testing.T, fn func(pid int, sig syscall.Signal) error) {
	t.Helper()
	orig := killProbe
	killProbe = fn
	t.Cleanup(func() { killProbe = orig })
}

func TestIsPIDAlive_EPERMMeansAlive(t *testing.T) {
	withKillProbe(t, func(pid int, sig syscall.Signal) error { return unix.EPERM })
	if !IsPIDAlive(12345) {
		t.Error("EPERM (process exists, owned by another user) must be treated as alive")
	}
}

func TestIsPIDAlive_ESRCHMeansDead(t *testing.T) {
	withKillProbe(t, func(pid int, sig syscall.Signal) error { return unix.ESRCH })
	if IsPIDAlive(12345) {
		t.Error("ESRCH (no such process) must be treated as dead")
	}
}

func TestIsPIDAlive_SuccessMeansAlive(t *testing.T) {
	withKillProbe(t, func(pid int, sig syscall.Signal) error { return nil })
	if !IsPIDAlive(12345) {
		t.Error("nil error from kill(pid,0) must be treated as alive")
	}
}

func TestIsPIDAlive_KillProbeCalledWithSignalZero(t *testing.T) {
	var gotPID int
	var gotSig syscall.Signal
	called := false
	withKillProbe(t, func(pid int, sig syscall.Signal) error {
		called = true
		gotPID = pid
		gotSig = sig
		return nil
	})
	IsPIDAlive(999)
	if !called {
		t.Fatal("killProbe was not called")
	}
	if gotPID != 999 {
		t.Errorf("killProbe pid = %d, want 999", gotPID)
	}
	if gotSig != 0 {
		t.Errorf("killProbe signal = %d, want 0", gotSig)
	}
}

func TestIsPIDAlive_LowPIDsNeverProbe(t *testing.T) {
	// pid<=1 must short-circuit to false without even calling killProbe,
	// regardless of what the probe would say.
	withKillProbe(t, func(pid int, sig syscall.Signal) error { return nil })
	for _, pid := range []int{1, 0, -1, -100} {
		if IsPIDAlive(pid) {
			t.Errorf("IsPIDAlive(%d) = true, want false", pid)
		}
	}
}

func TestIsPIDAlive_RealCurrentProcess(t *testing.T) {
	// Smoke test against the real platform branch (no probe override).
	if !IsPIDAlive(os.Getpid()) {
		t.Error("the current process should be alive")
	}
}
