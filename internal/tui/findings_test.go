// findings_test.go — regressions for verified code-review findings:
//   - FINDING 4: the human-output seams (lifecycle.Output, oauth.Output) are
//     redirected while the TUI owns the alt-screen, so a mutating action drives
//     no bytes to os.Stdout.
//   - FINDING 5-part: the switch toast renders an account-ref "number" that is a
//     *int (jsonout.AccountRef) as its slot, not a pointer address.
//   - FINDING 13: restartEngine never strands the old engine goroutine on the
//     abandoned, full events channel.
package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/lifecycle"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
)

// -- FINDING 5-part: switch-toast number rendering ---------------------------

func TestSwitchTargetRendersAccountRefNumber(t *testing.T) {
	n := 4
	// A real switch's from/to come from jsonout.AccountRef, whose number is a
	// *int; the old local helper rendered it via %v as a pointer address.
	payload := map[string]any{"switched": true, "to": jsonout.AccountRef(&n, "")}
	if got := switchTarget(payload); got != "account 4" {
		t.Fatalf("switchTarget = %q, want 'account 4' (must not print a *int pointer address)", got)
	}
	// A nil-number ref (unmanaged live account) renders Python's f"account {None}".
	payload = map[string]any{"switched": true, "to": jsonout.AccountRef(nil, "")}
	if got := switchTarget(payload); got != "account None" {
		t.Fatalf("switchTarget nil-number = %q, want 'account None'", got)
	}
	// A non-empty email still wins over the number.
	payload = map[string]any{"switched": true, "to": jsonout.AccountRef(&n, "e@x.com")}
	if got := switchTarget(payload); got != "e@x.com" {
		t.Fatalf("switchTarget = %q, want 'e@x.com'", got)
	}
}

// -- FINDING 4: human-output seams silenced under the TUI --------------------

// printingFacade is a fakeFacade whose mutating op prints human text to the
// package output seams, exactly as the real lifecycle/oauth code does.
type printingFacade struct {
	*fakeFacade
}

func (p *printingFacade) SetAccountDisabled(id string, disabled bool) error {
	fmt.Fprintln(lifecycle.Output, "Disabled Account-"+id)
	fmt.Fprintln(oauth.Output, "Warning: failed to save refreshed token for account "+id)
	return p.fakeFacade.SetAccountDisabled(id, disabled)
}

func TestActionOutputDoesNotReachStdout(t *testing.T) {
	// Model os.Stdout with a pipe so any stray byte is observable.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	// Capture the seams (as the TUI's redirect does, but into a buffer so we can
	// prove the action actually wrote there and not to os.Stdout).
	var seam bytes.Buffer
	prevL, prevO := lifecycle.Output, oauth.Output
	lifecycle.Output = &seam
	oauth.Output = &seam
	defer func() { lifecycle.Output = prevL; oauth.Output = prevO }()

	f := &fakeFacade{snap: snapshotOf("2", acct("1", "a@x.com", false, nil), acct("2", "b@x.com", true, nil))}
	m := newModel(&printingFacade{fakeFacade: f}, "dashboard")
	m.snapshot = f.snap

	// Drive a mutating action through the action pipeline + Update loop.
	cmd := m.toggleDisabled("1")
	msg := runCmd(cmd).(actionDoneMsg) // executes the facade fn → prints to the seams
	m.Update(msg)

	if seam.Len() == 0 {
		t.Fatal("action produced no human output at all — test would pass vacuously")
	}

	// Nothing must have reached os.Stdout.
	os.Stdout = origStdout
	_ = w.Close()
	leaked, _ := io.ReadAll(r)
	if len(leaked) != 0 {
		t.Fatalf("action wrote %q to os.Stdout while the TUI owns the terminal", leaked)
	}
}

func TestRedirectHumanOutputSilencesAndRestores(t *testing.T) {
	var prior bytes.Buffer // stands in for the pre-TUI stdout destination
	prevL, prevO := lifecycle.Output, oauth.Output
	lifecycle.Output = &prior
	oauth.Output = &prior
	defer func() { lifecycle.Output = prevL; oauth.Output = prevO }()

	restore := redirectHumanOutput()
	fmt.Fprint(lifecycle.Output, "lifecycle noise")
	fmt.Fprint(oauth.Output, "oauth noise")
	if prior.Len() != 0 {
		t.Fatalf("redirected seams still wrote %q to the prior destination", prior.String())
	}

	restore()
	if lifecycle.Output != io.Writer(&prior) || oauth.Output != io.Writer(&prior) {
		t.Fatal("restore() must put both seams back to their prior writers")
	}
}

// -- FINDING 13: restartEngine does not strand the old goroutine -------------

func TestRestartEngineOldGoroutineNotStranded(t *testing.T) {
	dir := t.TempDir()
	host := &engineHost{}
	m := newModel(&fakeFacade{backupDir: dir}, "dashboard", WithEngineFactory(host.factory()))
	m.pushScreen(newAutoScreen())
	a := m.top().(*autoScreen)

	oldCh := a.events
	oldEng := host.built[0]
	// Fill the old channel's buffer so the goroutine's final engineStoppedMsg
	// send has nowhere to go — a blocking send would strand it forever.
	for i := 0; i < cap(oldCh); i++ {
		oldEng.onEvent(fakeEvent{kind: "poll"})
	}
	if len(oldCh) != cap(oldCh) {
		t.Fatalf("expected the old channel buffer full, got %d/%d", len(oldCh), cap(oldCh))
	}

	baseline := runtime.NumGoroutine()
	// Restart: stops the old engine (its RunLoop returns and it tries the final
	// send) and installs a fresh channel nobody drains the old one from.
	a.restartEngine(m, false)
	defer host.built[1].Stop() // release the new engine goroutine

	// The old goroutine must exit; poll the count back down with a timeout.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("old engine goroutine stranded: goroutines=%d > baseline=%d",
				runtime.NumGoroutine(), baseline)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
