// Tests for the credential store (spec 03§5, 01§3): .enc-wins reads with the
// corrupt/empty/whitespace fall-through matrix, the usability cache flip / 60s
// cooldown / sticky pin distinction, the .prev one-generation lifecycle (incl.
// Keychain-not-a-file on macOS), the fail-closed strict clear, the
// entry-before-manifest unclaimed stash, and the classifiers.

package credstore

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/ccfile"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/clock"
	"git.dpemmons.com/dpemmons/cswap/internal/keychain"
	"git.dpemmons.com/dpemmons/cswap/internal/logging"
	"git.dpemmons.com/dpemmons/cswap/internal/paths"
	"git.dpemmons.com/dpemmons/cswap/internal/platform"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// -- test doubles --------------------------------------------------------------

// fakeKC is a configurable in-memory KeychainClient. Get/Set/Delete can be made
// to fail with a keychain.KeychainError (which IsUnusable recognizes), and
// noopDelete makes Delete succeed without removing the item (to exercise the
// strict clear's final read-back belt).
type fakeKC struct {
	mu         sync.Mutex
	m          map[[2]string]string
	failGet    bool
	failSet    bool
	failDelete bool
	noopDelete bool
	getCalls   int
	setCalls   int
	delCalls   int
}

func newFakeKC() *fakeKC { return &fakeKC{m: map[[2]string]string{}} }

func kckey(s, a string) [2]string { return [2]string{s, a} }

func (f *fakeKC) Get(service, account string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.failGet {
		return "", false, &keychain.KeychainError{Msg: "get boom"}
	}
	v, ok := f.m[kckey(service, account)]
	return v, ok, nil
}

func (f *fakeKC) Set(service, account, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.failSet {
		return &keychain.KeychainError{Msg: "set boom"}
	}
	f.m[kckey(service, account)] = password
	return nil
}

func (f *fakeKC) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls++
	if f.failDelete {
		return &keychain.KeychainError{Msg: "delete boom"}
	}
	if !f.noopDelete {
		delete(f.m, kckey(service, account))
	}
	return nil
}

func (f *fakeKC) Exists(service, account string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[kckey(service, account)]
	return ok
}

func (f *fakeKC) put(service, account, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[kckey(service, account)] = v
}

func (f *fakeKC) peek(service, account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[kckey(service, account)]
	return v, ok
}

func newStore(t *testing.T, plat platform.Platform, credDir string, kc keychain.KeychainClient, clk clock.Clock) *FileKeychainStore {
	t.Helper()
	if clk == nil {
		clk = clock.System{}
	}
	log := logging.New(t.TempDir(), false)
	return New(Config{Platform: plat, CredentialsDir: credDir}, kc, clk, log)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// -- classifiers ---------------------------------------------------------------

func TestLooksLikeAPIKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"api key", "sk-ant-api03-abc", true},
		{"api key with whitespace", "  sk-ant-api03-abc  ", true},
		{"oauth setup token", "sk-ant-oat01-abc", false},
		{"oauth json", `{"claudeAiOauth": {"accessToken": "x"}}`, false},
		{"json containing key", `{"key":"sk-ant-api03-abc"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeAPIKey(tc.in); got != tc.want {
				t.Fatalf("LooksLikeAPIKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestApprovedForm(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"long key last 20", "sk-ant-api03-0123456789ABCDEFGHIJ", "0123456789ABCDEFGHIJ"},
		{"short key whole", "short", "short"},
		{"exactly 20", "01234567890123456789", "01234567890123456789"},
		{"strips whitespace then last 20", "  sk-ant-api03-XXXXXXXXXXXXXXXXXXXX  ", "XXXXXXXXXXXXXXXXXXXX"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApprovedForm(tc.in); got != tc.want {
				t.Fatalf("ApprovedForm(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApprovedForm_LastTwenty(t *testing.T) {
	key := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz" // > 20 chars
	got := ApprovedForm(key)
	want := key[len(key)-20:]
	if got != want {
		t.Fatalf("ApprovedForm = %q, want %q", got, want)
	}
	if len(got) != 20 {
		t.Fatalf("approved form length = %d, want 20", len(got))
	}
}

// -- .enc-wins reads + fall-through matrix (Linux) -----------------------------

func TestReadBackup_FixtureDecode(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	credDir := filepath.Join(fh.BackupRoot, "credentials")
	s := newStore(t, platform.Linux, credDir, newFakeKC(), nil)

	cases := []struct {
		num, email, want string
	}{
		{"1", "alice@example.com", `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-AAAAfixture0000000000000000000000000000000000000000", "scopes": ["user:inference"]}}`},
		{"3", "key@example.com", "sk-ant-api03-CCCCfixture000000000000000000000000000000000000000"},
		{"5", "carol@example.com", `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-DDDDfixture0000000000000000000000000000000000000000", "scopes": ["user:inference"]}}`},
	}
	for _, tc := range cases {
		got, err := s.ReadBackup(tc.num, tc.email)
		if err != nil {
			t.Fatalf("ReadBackup(%s): %v", tc.num, err)
		}
		if got != tc.want {
			t.Fatalf("ReadBackup(%s) = %q, want %q", tc.num, got, tc.want)
		}
	}
}

func TestReadBackup_FallThroughMatrix_Linux(t *testing.T) {
	cases := []struct {
		name    string
		content string // raw .enc content; "" means no file
		present bool
		want    string
	}{
		{"valid", b64("hello-creds"), true, "hello-creds"},
		{"corrupt junk falls through", "!!!!", true, ""},
		{"empty falls through", "", true, ""},
		{"whitespace falls through", "   \n\t ", true, ""},
		{"missing", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credDir := t.TempDir()
			s := newStore(t, platform.Linux, credDir, newFakeKC(), nil)
			enc := s.backupEncPath("7", "x@e.com")
			if tc.present {
				writeFile(t, enc, tc.content)
			}
			got, err := s.ReadBackup("7", "x@e.com")
			if err != nil {
				t.Fatalf("ReadBackup: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ReadBackup = %q, want %q", got, tc.want)
			}
		})
	}
}

// -- .enc-wins reads on macOS (keychain fall-through) --------------------------

func TestReadBackup_EncWins_macOS(t *testing.T) {
	const num, email = "1", "u@e.com"
	kcVal := "keychain-copy"
	username := "account-" + num + "-" + email

	t.Run("valid .enc wins over keychain, no keychain read", func(t *testing.T) {
		credDir := t.TempDir()
		kc := newFakeKC()
		kc.put(securityService, username, kcVal)
		s := newStore(t, platform.MacOS, credDir, kc, nil)
		writeFile(t, s.backupEncPath(num, email), b64("enc-copy"))
		got, _ := s.ReadBackup(num, email)
		if got != "enc-copy" {
			t.Fatalf("got %q, want enc-copy (.enc wins)", got)
		}
		if kc.getCalls != 0 {
			t.Fatalf("keychain Get called %d times; .enc-wins must not read keychain", kc.getCalls)
		}
	})

	t.Run("corrupt .enc falls through to keychain", func(t *testing.T) {
		credDir := t.TempDir()
		kc := newFakeKC()
		kc.put(securityService, username, kcVal)
		s := newStore(t, platform.MacOS, credDir, kc, nil)
		writeFile(t, s.backupEncPath(num, email), "!!!!")
		got, _ := s.ReadBackup(num, email)
		if got != kcVal {
			t.Fatalf("got %q, want %q (fall through to keychain)", got, kcVal)
		}
	})

	t.Run("no .enc reads keychain and does not materialize an .enc", func(t *testing.T) {
		credDir := t.TempDir()
		kc := newFakeKC()
		kc.put(securityService, username, kcVal)
		s := newStore(t, platform.MacOS, credDir, kc, nil)
		got, _ := s.ReadBackup(num, email)
		if got != kcVal {
			t.Fatalf("got %q, want %q", got, kcVal)
		}
		if _, err := os.Stat(s.backupEncPath(num, email)); err == nil {
			t.Fatal("reading a healthy-Keychain backup must not materialize an .enc file")
		}
	})

	t.Run("keychain read failure returns empty", func(t *testing.T) {
		credDir := t.TempDir()
		kc := newFakeKC()
		kc.failGet = true
		s := newStore(t, platform.MacOS, credDir, kc, nil)
		got, _ := s.ReadBackup(num, email)
		if got != "" {
			t.Fatalf("got %q, want empty on keychain failure", got)
		}
	})
}

// -- .prev one-generation lifecycle (file mode) --------------------------------

func TestWriteBackup_PrevLifecycle_FileMode(t *testing.T) {
	const num, email = "2", "bob@e.com"
	credDir := t.TempDir()
	s := newStore(t, platform.Linux, credDir, newFakeKC(), nil)

	// First write: nothing to retain.
	if err := s.WriteBackup(num, email, "gen1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.prevBackupPath(num, email)); err == nil {
		t.Fatal(".prev must not exist after the first write")
	}

	// Second write with a changed value: gen1 retained as .prev.
	if err := s.WriteBackup(num, email, "gen2"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.ReadBackup(num, email); cur != "gen2" {
		t.Fatalf("current = %q, want gen2", cur)
	}
	if prev, _ := s.ReadPrev(num, email); prev != "gen1" {
		t.Fatalf("prev = %q, want gen1", prev)
	}

	// Same-value rewrite must not clobber .prev.
	if err := s.WriteBackup(num, email, "gen2"); err != nil {
		t.Fatal(err)
	}
	if prev, _ := s.ReadPrev(num, email); prev != "gen1" {
		t.Fatalf("prev after same-value rewrite = %q, want gen1 (unchanged)", prev)
	}

	// Third distinct write: gen2 becomes the retained generation.
	if err := s.WriteBackup(num, email, "gen3"); err != nil {
		t.Fatal(err)
	}
	if prev, _ := s.ReadPrev(num, email); prev != "gen2" {
		t.Fatalf("prev = %q, want gen2", prev)
	}
}

func TestWriteBackup_PrevIsKeychainNotAFile_macOS(t *testing.T) {
	const num, email = "1", "alice@e.com"
	username := "account-" + num + "-" + email
	credDir := t.TempDir()
	kc := newFakeKC()
	s := newStore(t, platform.MacOS, credDir, kc, nil)

	if err := s.WriteBackup(num, email, "gen1"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBackup(num, email, "gen2"); err != nil {
		t.Fatal(err)
	}

	// Retention must not weaken storage posture: the .prev lives in the Keychain,
	// never as a plaintext file on disk.
	if _, err := os.Stat(s.prevBackupPath(num, email)); err == nil {
		t.Fatal("a Keychain-backed Mac must not grow a plaintext .prev file")
	}
	if v, ok := kc.peek(securityService, username+".prev"); !ok || v != "gen1" {
		t.Fatalf(".prev keychain item = %q (ok=%v), want gen1", v, ok)
	}
	if v, ok := kc.peek(securityService, username); !ok || v != "gen2" {
		t.Fatalf("backup keychain item = %q (ok=%v), want gen2", v, ok)
	}
	// A successful Keychain write reconciles the .enc away.
	if _, err := os.Stat(s.backupEncPath(num, email)); err == nil {
		t.Fatal(".enc must be reconciled away after a Keychain backup write")
	}
}

func TestWriteBackup_ReconcileEnc_macOS(t *testing.T) {
	const num, email = "4", "d@e.com"
	credDir := t.TempDir()
	kc := newFakeKC()
	s := newStore(t, platform.MacOS, credDir, kc, nil)
	// Pre-existing .enc (e.g. written while Keychain was down).
	writeFile(t, s.backupEncPath(num, email), b64("stale-file"))

	if err := s.WriteBackup(num, email, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err == nil {
		t.Fatal("reconcile must delete the leftover .enc after a Keychain write")
	}
	if v, _ := kc.peek(securityService, "account-"+num+"-"+email); v != "fresh" {
		t.Fatalf("keychain backup = %q, want fresh", v)
	}
}

// -- usability cache: read cooldown re-probe vs write pin ----------------------

func TestUsabilityCache_ReadCooldownReprobe_macOS(t *testing.T) {
	const num, email = "1", "u@e.com"
	credDir := t.TempDir()
	kc := newFakeKC()
	kc.failSet = true
	clk := testutil.FixedClock(t, "2026-07-17T00:00:00Z")
	s := newStore(t, platform.MacOS, credDir, kc, clk)

	// Write #1: keychain Set fails → falls back to .enc and schedules a re-probe.
	if err := s.WriteBackup(num, email, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err != nil {
		t.Fatalf(".enc should exist after keychain-write fallback: %v", err)
	}

	// Keychain recovers, but we are still inside the cooldown window.
	kc.failSet = false
	clk.Advance(30 * time.Second) // < 60s cooldown
	if err := s.WriteBackup(num, email, "v2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc.peek(securityService, "account-"+num+"-"+email); ok {
		t.Fatal("still in cooldown: write must stay in file mode, not touch keychain")
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err != nil {
		t.Fatalf(".enc should still exist during cooldown: %v", err)
	}

	// Past the 60s cooldown → re-probe → keychain used again → .enc reconciled away.
	clk.Advance(31 * time.Second) // now +61s from the failure
	if err := s.WriteBackup(num, email, "v3"); err != nil {
		t.Fatal(err)
	}
	if v, ok := kc.peek(securityService, "account-"+num+"-"+email); !ok || v != "v3" {
		t.Fatalf("after cooldown, keychain backup = %q (ok=%v), want v3", v, ok)
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err == nil {
		t.Fatal(".enc must be reconciled away after the keychain re-probe write")
	}
}

func TestUsabilityCache_WritePinNoReprobe_macOS(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	_ = fh
	// Ensure a clean active state.
	_ = os.Remove(paths.GetCredentialsPath())

	const num, email = "1", "u@e.com"
	credDir := t.TempDir()
	kc := newFakeKC()
	kc.failSet = true
	clk := testutil.FixedClock(t, "2026-07-17T00:00:00Z")
	s := newStore(t, platform.MacOS, credDir, kc, clk)

	// An active OAuth write whose keychain Set fails pins file mode (no re-probe).
	if err := s.WriteActive(`{"claudeAiOauth":{"accessToken":"tok"}}`); err != nil {
		t.Fatal(err)
	}
	if s.LastActiveBackend() != "file" {
		t.Fatalf("last active backend = %q, want file", s.LastActiveBackend())
	}

	// Keychain "recovers" and a long time passes, but the pin has no deadline, so
	// there is no re-probe: a backup write stays in file mode.
	kc.failSet = false
	clk.Advance(120 * time.Second) // well past any cooldown
	if err := s.WriteBackup(num, email, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc.peek(securityService, "account-"+num+"-"+email); ok {
		t.Fatal("pinned file mode must never re-probe onto the keychain")
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err != nil {
		t.Fatalf(".enc should exist (pinned file mode): %v", err)
	}
}

// -- fail-closed strict clear --------------------------------------------------

func TestDeleteBackupStrict_HappyPath_Linux(t *testing.T) {
	const num, email = "3", "x@e.com"
	credDir := t.TempDir()
	s := newStore(t, platform.Linux, credDir, newFakeKC(), nil)
	writeFile(t, s.backupEncPath(num, email), b64("secret"))

	if err := s.DeleteBackupStrict(num, email); err != nil {
		t.Fatalf("strict clear should succeed: %v", err)
	}
	if _, err := os.Stat(s.backupEncPath(num, email)); err == nil {
		t.Fatal(".enc must be gone after strict clear")
	}
}

func TestDeleteBackupStrict_KeychainDeleteFails_Aborts(t *testing.T) {
	const num, email = "2", "bob@e.com"
	credDir := t.TempDir()
	kc := newFakeKC()
	kc.failDelete = true
	s := newStore(t, platform.MacOS, credDir, kc, nil)

	err := s.DeleteBackupStrict(num, email)
	if err == nil {
		t.Fatal("strict clear must fail closed when the keychain delete fails")
	}
	if cerr.TypeName(err) != "CredentialError" {
		t.Fatalf("error type = %q, want CredentialError", cerr.TypeName(err))
	}
	msg := err.Error()
	if !strings.Contains(msg, "aborting before commit") {
		t.Fatalf("message %q missing 'aborting before commit'", msg)
	}
	if !strings.Contains(msg, "—") { // em-dash
		t.Fatalf("message %q missing the em-dash separator", msg)
	}
	if !strings.Contains(msg, "slot "+num) || !strings.Contains(msg, email) {
		t.Fatalf("message %q missing slot/email", msg)
	}
}

func TestDeleteBackupStrict_FinalBeltCatchesResidual(t *testing.T) {
	const num, email = "1", "a@e.com"
	username := "account-" + num + "-" + email
	credDir := t.TempDir()
	kc := newFakeKC()
	kc.noopDelete = true // Delete "succeeds" but never removes the item
	kc.put(securityService, username, "still-here")
	s := newStore(t, platform.MacOS, credDir, kc, nil)

	err := s.DeleteBackupStrict(num, email)
	if err == nil {
		t.Fatal("strict clear must fail when a read-back still serves material")
	}
	if cerr.TypeName(err) != "CredentialError" {
		t.Fatalf("error type = %q, want CredentialError", cerr.TypeName(err))
	}
	// The final-belt message carries no ": <err>" detail suffix.
	if !strings.HasSuffix(err.Error(), "aborting before commit") {
		t.Fatalf("final-belt message %q should end with 'aborting before commit'", err.Error())
	}
}

// -- best-effort delete sweep --------------------------------------------------

func TestDeleteBackup_SweepsLegacyAliasAndPrev(t *testing.T) {
	const email = "sweep@e.com"
	credDir := t.TempDir()
	s := newStore(t, platform.Linux, credDir, newFakeKC(), nil)

	writeFile(t, s.backupEncPath("1", email), b64("c1"))
	writeFile(t, s.prevBackupPath("1", email), b64("p1"))
	writeFile(t, s.backupEncPath("None", email), b64("legacy"))
	writeFile(t, s.prevBackupPath("None", email), b64("legacy-prev"))

	if err := s.DeleteBackup("1", email); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		s.backupEncPath("1", email), s.prevBackupPath("1", email),
		s.backupEncPath("None", email), s.prevBackupPath("None", email),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s should have been swept", filepath.Base(p))
		}
	}
}

// -- unclaimed stash -----------------------------------------------------------

var entryIDRe = regexp.MustCompile(`^\d{8}T\d{6}-[0-9a-f]{12}-[0-9a-f]{6}$`)

func TestWriteUnclaimed_HappyPath(t *testing.T) {
	credDir := t.TempDir()
	clk := testutil.FixedClock(t, "2026-07-17T12:34:56Z")
	s := newStore(t, platform.Linux, credDir, newFakeKC(), clk)

	id, err := s.WriteUnclaimed("live-creds", map[string]any{"reason": "attributed-elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if !entryIDRe.MatchString(id) {
		t.Fatalf("entry id %q does not match the id schema", id)
	}
	if !strings.HasPrefix(id, "20260717T123456-") {
		t.Fatalf("entry id %q lacks the UTC timestamp prefix", id)
	}

	// Entry file base64-decodes to the raw credentials.
	entry := filepath.Join(credDir, ".unclaimed-"+id+".enc")
	raw, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || string(dec) != "live-creds" {
		t.Fatalf("entry decodes to %q (err=%v), want live-creds", string(dec), err)
	}

	// Manifest carries schemaVersion 1 and the row with createdAt + context.
	mraw, err := os.ReadFile(filepath.Join(credDir, ".unclaimed-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int                       `json:"schemaVersion"`
		Entries       map[string]map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(mraw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", doc.SchemaVersion)
	}
	row, ok := doc.Entries[id]
	if !ok {
		t.Fatalf("manifest missing entry %s", id)
	}
	if row["createdAt"] != "2026-07-17T12:34:56Z" {
		t.Fatalf("createdAt = %v, want 2026-07-17T12:34:56Z", row["createdAt"])
	}
	if row["reason"] != "attributed-elsewhere" {
		t.Fatalf("context not merged: %v", row["reason"])
	}

	// A second stash appends without losing the first.
	id2, err := s.WriteUnclaimed("live-creds-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUnclaimed()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list[id]; !ok {
		t.Fatal("first entry lost after second stash")
	}
	if _, ok := list[id2]; !ok {
		t.Fatal("second entry missing")
	}
}

func TestWriteUnclaimed_EntryWrittenBeforeManifest(t *testing.T) {
	credDir := t.TempDir()
	clk := testutil.FixedClock(t, "2026-07-17T12:34:56Z")
	s := newStore(t, platform.Linux, credDir, newFakeKC(), clk)

	// Make the manifest write fail while entry writes still succeed: the manifest
	// path is a directory (rename-over fails), and the corrupt-aside target is a
	// regular file (so the aside move also fails and can't clear it).
	manifest := filepath.Join(credDir, ".unclaimed-manifest.json")
	if err := os.Mkdir(manifest, 0o700); err != nil {
		t.Fatal(err)
	}
	aside := manifest + ".corrupt-" + itoa(clk.Now().Unix())
	writeFile(t, aside, "blocker")

	_, err := s.WriteUnclaimed("orphan-creds", nil)
	if err == nil {
		t.Fatal("expected the manifest write to fail")
	}

	// The entry bytes were persisted before the manifest attempt (an entry
	// without metadata is recoverable; a manifest row without bytes is not).
	matches, _ := filepath.Glob(filepath.Join(credDir, ".unclaimed-*.enc"))
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 orphan entry file, got %d", len(matches))
	}
	raw, _ := os.ReadFile(matches[0])
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if string(dec) != "orphan-creds" {
		t.Fatalf("orphan entry decodes to %q, want orphan-creds", string(dec))
	}
}

func TestListUnclaimed_OrphanRecovery(t *testing.T) {
	credDir := t.TempDir()
	clk := testutil.FixedClock(t, "2026-07-17T12:34:56Z")
	s := newStore(t, platform.Linux, credDir, newFakeKC(), clk)

	id, err := s.WriteUnclaimed("creds", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	// Lose the manifest; the entry file remains and must still be listed.
	if err := os.Remove(filepath.Join(credDir, ".unclaimed-manifest.json")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUnclaimed()
	if err != nil {
		t.Fatal(err)
	}
	row, ok := list[id]
	if !ok {
		t.Fatalf("orphan entry %s not recovered", id)
	}
	if row["createdAt"] != nil {
		t.Fatalf("orphan createdAt = %v, want nil", row["createdAt"])
	}
}

// -- active credential read/write (file mode) ----------------------------------

func TestReadActive_FilePlaintextWins(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	s := newStore(t, platform.Linux, t.TempDir(), newFakeKC(), nil)

	want, err := os.ReadFile(fh.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	val, kcUnavail, err := s.ReadActive()
	if err != nil {
		t.Fatal(err)
	}
	if kcUnavail {
		t.Fatal("keychainUnavailable must be false on Linux")
	}
	if val != string(want) {
		t.Fatalf("ReadActive value mismatch:\n got %q\nwant %q", val, string(want))
	}
}

func TestReadActive_FileReadErrorIsNone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read 0000-mode files")
	}
	fh := testutil.BuildFixtureHome(t)
	if err := os.Chmod(fh.CredentialsFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fh.CredentialsFile, 0o600) })

	s := newStore(t, platform.Linux, t.TempDir(), newFakeKC(), nil)
	val, _, err := s.ReadActive()
	if err == nil {
		t.Fatal("a present-but-unreadable credentials file must surface an error (Python None)")
	}
	if val != "" {
		t.Fatalf("value = %q, want empty on read error", val)
	}
}

func TestReadActive_ManagedKeyFromConfig(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	if err := os.Remove(fh.CredentialsFile); err != nil {
		t.Fatal(err)
	}
	if err := ccfile.UpdateGlobalConfig(func(c map[string]any) { c["primaryApiKey"] = "sk-ant-api03-managed" }); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, platform.Linux, t.TempDir(), newFakeKC(), nil)
	val, _, err := s.ReadActive()
	if err != nil {
		t.Fatal(err)
	}
	if val != "sk-ant-api03-managed" {
		t.Fatalf("ReadActive = %q, want the managed key", val)
	}
}

func TestWriteActive_OAuthFileMode(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	s := newStore(t, platform.Linux, t.TempDir(), newFakeKC(), nil)

	blob := `{"claudeAiOauth":{"accessToken":"fresh-oauth"}}`
	if err := s.WriteActive(blob); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fh.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != blob {
		t.Fatalf("credentials file = %q, want %q (raw, verbatim)", string(got), blob)
	}
	if s.LastActiveBackend() != "file" {
		t.Fatalf("last backend = %q, want file", s.LastActiveBackend())
	}
}

func TestWriteActive_ManagedKeyFileMode(t *testing.T) {
	fh := testutil.BuildFixtureHome(t)
	s := newStore(t, platform.Linux, t.TempDir(), newFakeKC(), nil)

	key := "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWX" // > 20 chars
	if err := s.WriteActive(key); err != nil {
		t.Fatal(err)
	}
	// primaryApiKey written; approved form recorded; OAuth file cleared.
	cfg, err := ccfile.ReadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["primaryApiKey"] != key {
		t.Fatalf("primaryApiKey = %v, want %q", cfg["primaryApiKey"], key)
	}
	responses, ok := cfg["customApiKeyResponses"].(map[string]any)
	if !ok {
		t.Fatal("customApiKeyResponses missing")
	}
	approved, _ := responses["approved"].([]any)
	if len(approved) != 1 || approved[0] != ApprovedForm(key) {
		t.Fatalf("approved = %v, want [%q]", approved, ApprovedForm(key))
	}
	if _, ok := responses["rejected"]; !ok {
		t.Fatal("rejected list should be present")
	}
	if _, err := os.Stat(fh.CredentialsFile); err == nil {
		t.Fatal("OAuth credentials file must be cleared when a managed key activates")
	}
	if s.LastActiveBackend() != "file" {
		t.Fatalf("last backend = %q, want file", s.LastActiveBackend())
	}
}

// itoa avoids importing strconv just for one call in a test.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
