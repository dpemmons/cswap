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
	"strings"
	"sync"
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
	restoreL := lifecycle.RedirectOutput(&seam)
	restoreO := oauth.RedirectOutput(&seam)
	defer func() { restoreO(); restoreL() }()

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
	// Install prior as the base destination through the atomic seam, as a real
	// pre-TUI CLI setup would.
	restoreBaseL := lifecycle.RedirectOutput(&prior)
	restoreBaseO := oauth.RedirectOutput(&prior)
	defer func() { restoreBaseO(); restoreBaseL() }()

	notices := &noticeCollector{}
	restore := redirectHumanOutput(notices)

	// lifecycle output is silenced entirely; oauth output is captured for the UI.
	fmt.Fprintln(lifecycle.Output, "lifecycle noise")
	fmt.Fprintln(oauth.Output, "Warning: failed to save refreshed token for account 3")
	if prior.Len() != 0 {
		t.Fatalf("redirected seams still wrote %q to the prior destination", prior.String())
	}
	got := notices.drain()
	if len(got) != 1 || got[0] != "Warning: failed to save refreshed token for account 3" {
		t.Fatalf("oauth persist warning did not reach the notice collector: %#v", got)
	}

	restore()
	// After restore, both seams write to the prior destination again.
	fmt.Fprint(lifecycle.Output, "post-restore lifecycle")
	fmt.Fprint(oauth.Output, "post-restore oauth")
	if prior.String() != "post-restore lifecyclepost-restore oauth" {
		t.Fatalf("restore() must put both seams back to the prior writer; prior=%q", prior.String())
	}
}

// -- FINDING 6: oauth persist-failure warning reaches the TUI notice model ---

// TestPersistWarningSurfacesAsToast proves the oauth persist-failure warning —
// the only user-visible surface for a lost-refresh-token condition — is not
// discarded under the TUI but reaches the toast model as a warning, while still
// writing nothing to os.Stdout.
func TestPersistWarningSurfacesAsToast(t *testing.T) {
	// Model os.Stdout with a pipe so any stray byte is observable.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	f := &fakeFacade{snap: snapshotOf("1", acct("1", "a@x.com", true, nil))}
	m := newModel(f, "dashboard")
	m.snapshot = f.snap

	restore := redirectHumanOutput(m.notices)
	defer restore()

	// The persist path (oauth.persistCredentials, 04§1.25) writes a warning like
	// this to oauth.Output, potentially from the auto-engine goroutine.
	fmt.Fprintln(oauth.Output, "Warning: failed to save refreshed token for account 2 (b@x.com). "+
		"If the next refresh fails, re-run `cswap --add-account` after logging in.")

	// A poll tick drains the collector into toasts on the Update goroutine.
	m.Update(pollTickMsg{})

	found := false
	for _, tst := range m.toasts {
		if strings.Contains(tst.message, "failed to save refreshed token for account 2") {
			if tst.severity != "warning" {
				t.Fatalf("persist warning toast severity = %q, want \"warning\"", tst.severity)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("persist-failure warning did not surface as a toast; toasts=%#v", m.toasts)
	}

	// Nothing must have reached os.Stdout.
	os.Stdout = origStdout
	_ = w.Close()
	leaked, _ := io.ReadAll(r)
	if len(leaked) != 0 {
		t.Fatalf("persist warning leaked %q to os.Stdout while the TUI owns the terminal", leaked)
	}
}

// -- FINDING 5: seam redirect/restore is race-free with concurrent writers ---

// TestSeamRedirectRaceFree exercises the exact FINDING 5 hazard: a goroutine
// (standing in for the auto-switch engine, whose Stop does not join) writing the
// warning to a seam while the TUI redirects and restores it. Reassigning the
// package Output variable — the old implementation — is a data race the -race
// detector flags; the atomic seam swap is clean. Run under `go test -race`.
func TestSeamRedirectRaceFree(t *testing.T) {
	// Base both seams at io.Discard so restore windows don't spam os.Stdout and
	// concurrent writes stay safe.
	restoreBaseO := oauth.RedirectOutput(io.Discard)
	restoreBaseL := lifecycle.RedirectOutput(io.Discard)
	defer func() { restoreBaseO(); restoreBaseL() }()

	notices := &noticeCollector{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	writer := func(read func() io.Writer) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fmt.Fprintln(read(), "Warning: failed to save refreshed token for account 1")
			}
		}
	}
	wg.Add(2)
	go writer(func() io.Writer { return oauth.Output })
	go writer(func() io.Writer { return lifecycle.Output })

	for i := 0; i < 3000; i++ {
		restore := redirectHumanOutput(notices)
		restore()
	}
	close(stop)
	wg.Wait()
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
