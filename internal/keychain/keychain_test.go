package keychain

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestFakeRoundTripSpecialChars(t *testing.T) {
	f := NewFake()
	secrets := map[string]string{
		"plain":     "hello",
		"quotes":    `he said "hi"`,
		"backslash": `a\b\c`,
		"unicode":   "café é",
	}
	for name, secret := range secrets {
		if err := f.Set("claude-swap", name, secret); err != nil {
			t.Fatal(err)
		}
		got, found, err := f.Get("claude-swap", name)
		if err != nil || !found {
			t.Fatalf("%s: Get found=%v err=%v", name, found, err)
		}
		if got != secret {
			t.Errorf("%s: round-trip = %q, want %q", name, got, secret)
		}
	}
	// Delete then Get → not found; Delete of absent is a no-op.
	if err := f.Delete("claude-swap", "plain"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.Get("claude-swap", "plain"); found {
		t.Error("expected not found after delete")
	}
	if err := f.Delete("claude-swap", "does-not-exist"); err != nil {
		t.Errorf("Delete of absent key should be nil (rc44 parity), got %v", err)
	}
}

// recordExec captures the last invocation.
type recordExec struct {
	argv  []string
	stdin string
	res   execResult
	err   error
}

func (r *recordExec) fn(ctx context.Context, argv []string, stdin string) (execResult, error) {
	r.argv = argv
	r.stdin = stdin
	return r.res, r.err
}

func TestSetSmallPayloadUsesStdinHex(t *testing.T) {
	rec := &recordExec{res: execResult{rc: 0}}
	s := Security{Exec: rec.fn}
	if err := s.Set("Claude Code-credentials", "alice", `tok"en\x`); err != nil {
		t.Fatal(err)
	}
	if len(rec.argv) != 2 || rec.argv[0] != "/usr/bin/security" || rec.argv[1] != "-i" {
		t.Fatalf("small payload argv = %v, want [/usr/bin/security -i]", rec.argv)
	}
	if !strings.HasPrefix(rec.stdin, "add-generic-password -U") {
		t.Errorf("stdin does not start with add-generic-password -U: %q", rec.stdin)
	}
	// Quoted account and service (service contains a space).
	if !strings.Contains(rec.stdin, `-a "alice"`) {
		t.Errorf("stdin missing quoted account: %q", rec.stdin)
	}
	if !strings.Contains(rec.stdin, `-s "Claude Code-credentials"`) {
		t.Errorf("stdin missing quoted service: %q", rec.stdin)
	}
	// Hex round-trips back to the secret.
	hexStr := extractHex(t, rec.stdin)
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `tok"en\x` {
		t.Errorf("decoded hex = %q, want the original secret", raw)
	}
}

func TestSetLargePayloadFallsBackToArgv(t *testing.T) {
	rec := &recordExec{res: execResult{rc: 0}}
	s := Security{Exec: rec.fn}
	// Hex doubles length; a secret of SecurityStdinLineLimit bytes overflows.
	big := strings.Repeat("x", SecurityStdinLineLimit)
	if err := s.Set("claude-swap", "acct", big); err != nil {
		t.Fatal(err)
	}
	if rec.stdin != "" {
		t.Errorf("large payload should not use stdin, got %q...", rec.stdin[:20])
	}
	want := []string{"/usr/bin/security", "add-generic-password", "-U", "-a", "acct", "-s", "claude-swap", "-X"}
	if len(rec.argv) != len(want)+1 {
		t.Fatalf("argv len = %d, want %d: %v", len(rec.argv), len(want)+1, rec.argv)
	}
	for i, w := range want {
		if rec.argv[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, rec.argv[i], w)
		}
	}
	// Last element is the raw hex value.
	if rec.argv[len(rec.argv)-1] != toHex(big) {
		t.Errorf("last argv element is not the hex value")
	}
}

func TestGetRCHandling(t *testing.T) {
	tests := []struct {
		name      string
		res       execResult
		wantVal   string
		wantFound bool
		wantErr   bool
		unusable  bool
	}{
		{"found strips one newline", execResult{rc: 0, stdout: "value\n"}, "value", true, false, false},
		{"found preserves inner newline", execResult{rc: 0, stdout: "value\n\n"}, "value\n", true, false, false},
		{"rc44 not found", execResult{rc: 44}, "", false, false, false},
		{"rc51 locked raises", execResult{rc: 51, stderr: "denied"}, "", false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordExec{res: tt.res}
			s := Security{Exec: rec.fn}
			val, found, err := s.Get("svc", "acct")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if val != tt.wantVal || found != tt.wantFound {
				t.Errorf("Get = (%q,%v), want (%q,%v)", val, found, tt.wantVal, tt.wantFound)
			}
			if tt.wantErr {
				// argv omits nothing special; verify -w present for reads.
				if !contains(rec.argv, "-w") {
					t.Errorf("read argv should contain -w: %v", rec.argv)
				}
			}
			if IsUnusable(err) != tt.unusable {
				t.Errorf("IsUnusable(err) = %v, want %v", IsUnusable(err), tt.unusable)
			}
		})
	}
}

func TestExistsOmitsW(t *testing.T) {
	rec := &recordExec{res: execResult{rc: 0}}
	s := Security{Exec: rec.fn}
	if !s.Exists("svc", "acct") {
		t.Error("Exists = false on rc0")
	}
	if contains(rec.argv, "-w") {
		t.Errorf("Exists argv must omit -w: %v", rec.argv)
	}
	// rc44/rc51/timeout all → false, never raises.
	for _, rc := range []int{44, 51} {
		s2 := Security{Exec: (&recordExec{res: execResult{rc: rc}}).fn}
		if s2.Exists("svc", "acct") {
			t.Errorf("Exists rc=%d = true, want false", rc)
		}
	}
}

func TestDeleteRCHandling(t *testing.T) {
	for _, rc := range []int{0, 44} {
		s := Security{Exec: (&recordExec{res: execResult{rc: rc}}).fn}
		if err := s.Delete("svc", "acct"); err != nil {
			t.Errorf("Delete rc=%d = %v, want nil", rc, err)
		}
	}
	s := Security{Exec: (&recordExec{res: execResult{rc: 51, stderr: "denied"}}).fn}
	err := s.Delete("svc", "acct")
	if err == nil || !IsUnusable(err) {
		t.Errorf("Delete rc=51 err = %v, want unusable KeychainError", err)
	}
}

func TestTimeoutClassifiedUnusable(t *testing.T) {
	blocking := func(ctx context.Context, argv []string, stdin string) (execResult, error) {
		<-ctx.Done()
		return execResult{}, ctx.Err()
	}
	s := Security{Exec: blocking, Timeout: time.Millisecond}
	_, _, err := s.Get("svc", "acct")
	if err == nil || !IsUnusable(err) {
		t.Errorf("timeout err = %v, want unusable", err)
	}
}

// TestSuccessAtDeadlineHonored pins the fix for the race where a subprocess
// completes successfully at ~the deadline: the result must be honored, never
// discarded as a timeout. This mirrors Python's subprocess.run, whose
// TimeoutExpired fires only when the process is actually killed, not when a
// result was obtained just as the clock ran out. The fake exec seam returns a
// successful rc=0 result while the context is already expired.
func TestSuccessAtDeadlineHonored(t *testing.T) {
	raced := func(ctx context.Context, argv []string, stdin string) (execResult, error) {
		<-ctx.Done() // deadline has fired; ctx.Err() == DeadlineExceeded
		return execResult{rc: 0, stdout: "value\n"}, nil
	}
	s := Security{Exec: raced, Timeout: time.Millisecond}
	val, found, err := s.Get("svc", "acct")
	if err != nil {
		t.Fatalf("success at deadline discarded as %v; want honored result", err)
	}
	if !found || val != "value" {
		t.Errorf("Get = (%q,%v), want (\"value\",true)", val, found)
	}
}

func TestAccountNamePrefersUSER(t *testing.T) {
	t.Setenv("USER", "bob")
	if AccountName() != "bob" {
		t.Errorf("AccountName() = %q, want bob", AccountName())
	}
	t.Setenv("USER", "")
	got := AccountName()
	if got == "" || got == "user" {
		t.Errorf("AccountName() with USER unset = %q; must be non-empty and never \"user\"", got)
	}
}

func extractHex(t *testing.T, command string) string {
	t.Helper()
	idx := strings.Index(command, "-X ")
	if idx < 0 {
		t.Fatalf("no -X in command: %q", command)
	}
	rest := command[idx+3:]
	rest = strings.TrimRight(rest, "\n")
	return strings.TrimSpace(rest)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
