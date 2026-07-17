// core_test.go — interface-satisfaction compile checks (DESIGN A2/A13) and
// delegation smoke tests (DESIGN §5 WP10): each Switcher method is exercised
// once to prove it reaches the underlying store/lifecycle/switching/reporting
// call, not to re-verify that call's own exhaustively-tested business logic.
package core

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/autoswitch"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
	"git.dpemmons.com/dpemmons/cswap/internal/lifecycle"
	"git.dpemmons.com/dpemmons/cswap/internal/session"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/testutil"
)

// ---- interface-satisfaction compile checks (DESIGN A2/A13) ------------------

// session.Accounts is satisfied directly: plain *store.Store promotion plus
// the three adapters in session_adapters.go.
var _ session.Accounts = (*Switcher)(nil)

// autoswitchAdapter demonstrates the fix documented in
// autoswitch_adapters.go's ReadAccountCredentials note: *Switcher plus a
// shadowing no-error ReadAccountCredentials satisfies autoswitch.Switcher in
// full. This is the "final" compile assertion cli (WP15) is expected to carry
// once it wires an engine.
type autoswitchAdapter struct{ *Switcher }

func (a autoswitchAdapter) ReadAccountCredentials(num, email string) string {
	v, _ := a.Switcher.ReadAccountCredentials(num, email)
	return v
}

var _ autoswitch.Switcher = autoswitchAdapter{}

// ---- scaffolding --------------------------------------------------------

// newTestSwitcher builds a Switcher rooted at a fresh empty $HOME with a fixed
// clock, its backup directories created, and human output/prompts silenced.
func newTestSwitcher(t *testing.T) *Switcher {
	t.Helper()
	home := t.TempDir()
	testutil.Setenv(t, "HOME", home)
	testutil.Unsetenv(t, "CLAUDE_CONFIG_DIR")
	testutil.Unsetenv(t, "XDG_DATA_HOME")
	testutil.Setenv(t, "NO_COLOR", "1")
	clk := testutil.FixedClock(t, "2026-07-17T09:00:00Z")
	sw, err := New(store.Options{Clock: clk, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if err := sw.SetupDirectories(); err != nil {
		t.Fatalf("SetupDirectories: %v", err)
	}
	prevOut := lifecycle.Output
	lifecycle.Output = &bytes.Buffer{}
	t.Cleanup(func() { lifecycle.Output = prevOut })
	return sw
}

func oauthCreds(access string) string {
	m := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": access, "refreshToken": "ref-" + access, "expiresAt": 4102444800000,
	}}
	b, _ := json.Marshal(m)
	return string(b)
}

func backupConfig(email string) string {
	m := map[string]any{"oauthAccount": map[string]any{"emailAddress": email, "organizationUuid": ""}}
	b, _ := json.Marshal(m)
	return string(b)
}

// seedManaged writes a fully-backed managed slot's credential + config.
func seedManaged(t *testing.T, sw *Switcher, num, email, creds string) {
	t.Helper()
	if err := sw.WriteAccountCredentials(num, email, creds); err != nil {
		t.Fatalf("seedManaged WriteAccountCredentials(%s): %v", num, err)
	}
	if err := sw.WriteAccountConfig(num, email, backupConfig(email)); err != nil {
		t.Fatalf("seedManaged WriteAccountConfig(%s): %v", num, err)
	}
}

// seedLive writes the live ~/.claude.json (personal org) and .credentials.json.
func seedLive(t *testing.T, sw *Switcher, email, creds string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sw.Home, ".claude.json"), []byte(backupConfig(email)), 0o600); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(sw.Home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakePrompter answers a scripted sequence of prompts/secrets/stdin lines.
type fakePrompter struct {
	answers []string
	i       int
}

func (p *fakePrompter) next() (string, bool) {
	if p.i >= len(p.answers) {
		return "", false
	}
	v := p.answers[p.i]
	p.i++
	return v, true
}
func (p *fakePrompter) Prompt(string) (string, bool) { return p.next() }
func (p *fakePrompter) Secret(string) (string, bool) { return p.next() }
func (p *fakePrompter) StdinLine() (string, bool)    { return p.next() }

func withAnswers(t *testing.T, answers ...string) {
	t.Helper()
	prev := lifecycle.ActivePrompter
	lifecycle.ActivePrompter = &fakePrompter{answers: answers}
	t.Cleanup(func() { lifecycle.ActivePrompter = prev })
}

func writeSeqDirect(t *testing.T, sw *Switcher, active *int, sequence []int, accounts map[string]json.RawMessage) {
	t.Helper()
	if err := sw.WriteSequence(&store.SequenceData{
		ActiveAccountNumber: active,
		LastUpdated:         "2026-07-17T08:00:00Z",
		Sequence:            sequence,
		Accounts:            accounts,
	}); err != nil {
		t.Fatalf("WriteSequence: %v", err)
	}
}

func rawRecord(fields map[string]any) json.RawMessage {
	b, _ := json.Marshal(fields)
	return json.RawMessage(b)
}

func ptrInt(n int) *int { return &n }

func asPayload(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any payload, got %T (%v)", v, v)
	}
	return m
}

// refNumber returns the integer slot number in a from/to ref (dereferencing
// the *int jsonout.AccountRef stores), or -1 for a null ref/number.
func refNumber(m map[string]any, key string) int {
	ref, _ := m[key].(map[string]any)
	if ref == nil {
		return -1
	}
	switch n := ref["number"].(type) {
	case *int:
		if n == nil {
			return -1
		}
		return *n
	case int:
		return n
	}
	return -1
}

// ---- delegation smoke tests -----------------------------------------------

// TestNewAndPromotedAccessors: New wraps store.New and the frozen-interface
// accessors that plain *store.Store promotion satisfies read through.
func TestNewAndPromotedAccessors(t *testing.T) {
	sw := newTestSwitcher(t)
	if sw.BackupDir() == "" || sw.BackupDir() != sw.Store.BackupDir() {
		t.Fatalf("BackupDir() = %q, want sw.Store.BackupDir()=%q", sw.BackupDir(), sw.Store.BackupDir())
	}
	if sw.Platform() != sw.Store.Platform {
		t.Fatalf("Platform() = %v, want %v", sw.Platform(), sw.Store.Platform)
	}
	if sw.CurrentAccountNumber() != nil {
		t.Fatalf("CurrentAccountNumber() = %v, want nil (no live login)", sw.CurrentAccountNumber())
	}
	if sw.HasLiveLogin() {
		t.Fatalf("HasLiveLogin() = true, want false")
	}
}

// TestSwitchAndSwitchToOnEmptyStore_ReturnsConfigError: Switch/SwitchTo/
// SwitchToForce all reach switching's "No accounts are managed yet" guard.
func TestSwitchAndSwitchToOnEmptyStore_ReturnsConfigError(t *testing.T) {
	sw := newTestSwitcher(t)
	if _, err := sw.Switch(nil, true, nil, nil); cerr.TypeName(err) != "ConfigError" {
		t.Fatalf("Switch: err = %v, want ConfigError", err)
	}
	if _, err := sw.SwitchTo("1", true); cerr.TypeName(err) != "ConfigError" {
		t.Fatalf("SwitchTo: err = %v, want ConfigError", err)
	}
	if _, err := sw.SwitchToForce("1", true, true); cerr.TypeName(err) != "ConfigError" {
		t.Fatalf("SwitchToForce: err = %v, want ConfigError", err)
	}
}

// TestListAccountsEmptyPayload: ListAccounts delegates through to reporting's
// schema-v1 empty payload when no sequence.json exists yet.
func TestListAccountsEmptyPayload(t *testing.T) {
	sw := newTestSwitcher(t)
	out, err := sw.ListAccounts(false, true, nil)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	m := asPayload(t, out)
	if m["activeAccountNumber"] != nil {
		t.Fatalf("activeAccountNumber = %v, want nil", m["activeAccountNumber"])
	}
	accts, ok := m["accounts"].([]any)
	if !ok || len(accts) != 0 {
		t.Fatalf("accounts = %v, want empty slice", m["accounts"])
	}
}

// TestAddAccountRefreshInPlaceAndAlias: AddAccount delegates to
// lifecycle.AddAccount (new account, then refresh-in-place), and
// SetAlias/UnsetAlias/ListAliases round-trip through lifecycle.
func TestAddAccountRefreshInPlaceAndAlias(t *testing.T) {
	sw := newTestSwitcher(t)
	seedLive(t, sw, "alice@example.com", oauthCreds("tok-1"))

	if err := sw.AddAccount(nil, false, nil); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := sw.AddAccount(nil, false, nil); err != nil { // refresh-in-place, same identity
		t.Fatalf("AddAccount (refresh-in-place): %v", err)
	}
	data, err := sw.ReadSequence()
	if err != nil || data == nil {
		t.Fatalf("ReadSequence: data=%v err=%v", data, err)
	}
	if len(data.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1 (refresh-in-place must not duplicate)", len(data.Accounts))
	}

	num, normalized, err := sw.SetAlias("1", "Work")
	if err != nil || num != "1" || normalized != "work" {
		t.Fatalf("SetAlias: num=%q normalized=%q err=%v", num, normalized, err)
	}
	rows, err := sw.ListAliases()
	if err != nil || len(rows) != 1 || rows[0].Alias != "work" || rows[0].Email != "alice@example.com" {
		t.Fatalf("ListAliases: rows=%v err=%v", rows, err)
	}
	if num, err := sw.UnsetAlias("1"); err != nil || num != "1" {
		t.Fatalf("UnsetAlias: num=%q err=%v", num, err)
	}
	if rows, err := sw.ListAliases(); err != nil || len(rows) != 0 {
		t.Fatalf("ListAliases after unset: rows=%v err=%v", rows, err)
	}
}

// TestSessionAdapters_ReadAccountConfigPlatformSlotForDirectory exercises the
// three explicit session.Accounts adapters (session_adapters.go).
func TestSessionAdapters_ReadAccountConfigPlatformSlotForDirectory(t *testing.T) {
	sw := newTestSwitcher(t)
	seedManaged(t, sw, "1", "alice@example.com", oauthCreds("tok-1"))
	writeSeqDirect(t, sw, ptrInt(1), []int{1}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "alice@example.com", "organizationUuid": ""}),
	})

	cfg, err := sw.ReadAccountConfig("1", "alice@example.com")
	if err != nil {
		t.Fatalf("ReadAccountConfig: %v", err)
	}
	if _, ok := cfg["oauthAccount"]; !ok {
		t.Fatalf("ReadAccountConfig missing oauthAccount: %v", cfg)
	}

	missing, err := sw.ReadAccountConfig("99", "nope@example.com")
	if err != nil || len(missing) != 0 {
		t.Fatalf("ReadAccountConfig(missing) = %v, err=%v, want empty map, nil", missing, err)
	}

	if sw.Platform() != sw.Store.Platform {
		t.Fatalf("Platform() = %v, want %v", sw.Platform(), sw.Store.Platform)
	}

	slot, email, err := sw.SlotForDirectory(sw.Home)
	if err != nil || slot != nil || email != nil {
		t.Fatalf("SlotForDirectory(unmapped) = (%v, %v, %v), want (nil, nil, nil)", slot, email, err)
	}
}

// TestBackfillAccountUUID: autoswitch.Switcher's no-error adapter persists the
// uuid via the promoted (fallible) store method and swallows a benign result.
func TestBackfillAccountUUID(t *testing.T) {
	sw := newTestSwitcher(t)
	seedManaged(t, sw, "1", "alice@example.com", oauthCreds("tok-1"))
	writeSeqDirect(t, sw, nil, []int{1}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "alice@example.com", "organizationUuid": ""}),
	})

	sw.BackfillAccountUUID("1", "uuid-123") // no return value to check; must not panic
	if got := sw.AccountIdentity("1")["uuid"]; got != "uuid-123" {
		t.Fatalf("uuid after BackfillAccountUUID = %q, want uuid-123", got)
	}
	sw.BackfillAccountUUID("1", "uuid-456") // existing uuid never rewritten
	if got := sw.AccountIdentity("1")["uuid"]; got != "uuid-123" {
		t.Fatalf("uuid after second BackfillAccountUUID = %q, want unchanged uuid-123", got)
	}
}

// TestSwitchRotationAndForce: Switch (plain rotation, JSON mode) and
// SwitchToForce (cross-slot, force=true) both delegate through switching and
// return a typed map[string]any (not the raw `any` switching.Switch/SwitchTo
// return).
func TestSwitchRotationAndForce(t *testing.T) {
	sw := newTestSwitcher(t)
	credsA, credsB := oauthCreds("tok-a"), oauthCreds("tok-b")
	seedManaged(t, sw, "1", "a@x.com", credsA)
	seedManaged(t, sw, "2", "b@x.com", credsB)
	writeSeqDirect(t, sw, ptrInt(1), []int{1, 2}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": rawRecord(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
	})
	seedLive(t, sw, "a@x.com", credsA)

	out, err := sw.Switch(nil, true, nil, nil)
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if out["switched"] != true {
		t.Fatalf("switched = %v, want true", out["switched"])
	}
	if n := refNumber(out, "to"); n != 2 {
		t.Fatalf("to.number = %v, want 2", n)
	}

	out2, err := sw.SwitchToForce("1", true, true)
	if err != nil {
		t.Fatalf("SwitchToForce: %v", err)
	}
	if n := refNumber(out2, "to"); n != 1 {
		t.Fatalf("to.number = %v, want 1", n)
	}
}

// TestRemoveMoveSwapAccounts exercises the remaining lifecycle delegations.
func TestRemoveMoveSwapAccounts(t *testing.T) {
	sw := newTestSwitcher(t)
	seedManaged(t, sw, "1", "a@x.com", oauthCreds("tok-a"))
	seedManaged(t, sw, "2", "b@x.com", oauthCreds("tok-b"))
	seedManaged(t, sw, "3", "c@x.com", oauthCreds("tok-c"))
	writeSeqDirect(t, sw, ptrInt(1), []int{1, 2, 3}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
		"2": rawRecord(map[string]any{"email": "b@x.com", "organizationUuid": ""}),
		"3": rawRecord(map[string]any{"email": "c@x.com", "organizationUuid": ""}),
	})

	numA, numB, err := sw.SwapAccounts("2", "3")
	if err != nil || numA != "2" || numB != "3" {
		t.Fatalf("SwapAccounts: numA=%q numB=%q err=%v", numA, numB, err)
	}
	data, _ := sw.ReadSequence()
	if got := recEmail(t, data, "2"); got != "c@x.com" {
		t.Fatalf("slot 2 email after swap = %q, want c@x.com", got)
	}

	srcNum, tgtNum, swapped, err := sw.MoveAccount("2", "4")
	if err != nil || srcNum != "2" || tgtNum != "4" || swapped {
		t.Fatalf("MoveAccount: src=%q tgt=%q swapped=%v err=%v", srcNum, tgtNum, swapped, err)
	}

	if err := sw.RemoveAccount("1", true); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	data, _ = sw.ReadSequence()
	if _, ok := data.Accounts["1"]; ok {
		t.Fatalf("account 1 still present after RemoveAccount")
	}
}

func recEmail(t *testing.T, data *store.SequenceData, num string) string {
	t.Helper()
	raw, ok := data.Accounts[num]
	if !ok {
		t.Fatalf("record %s missing", num)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m["email"].(string)
	return s
}

// TestAddAccountFromTokenKinds: AddAccountFromToken delegates to
// lifecycle.AddAccountFromToken for both the OAuth setup-token and the
// API-key kind.
func TestAddAccountFromTokenKinds(t *testing.T) {
	sw := newTestSwitcher(t)
	oauthEmail, apiEmail := "setup@x.com", "key@x.com"
	if err := sw.AddAccountFromToken("sk-ant-oat01-AAAA", &oauthEmail, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken(oauth): %v", err)
	}
	if err := sw.AddAccountFromToken("sk-ant-api03-BBBB", &apiEmail, nil, false); err != nil {
		t.Fatalf("AddAccountFromToken(api key): %v", err)
	}
	data, _ := sw.ReadSequence()
	if len(data.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(data.Accounts))
	}
	if sw.AccountKindFor("1") != "oauth" {
		t.Fatalf("slot 1 kind = %q, want oauth", sw.AccountKindFor("1"))
	}
	if sw.AccountKindFor("2") != "api_key" {
		t.Fatalf("slot 2 kind = %q, want api_key", sw.AccountKindFor("2"))
	}
}

// TestUsageAndPollAndSnapshotDelegation covers the reporting-backed read
// surface: UsageByAccount, UsageEntriesByAccount, UsageFetchStamps,
// AccountsSnapshot, Status, and the poll-policy-inputs pins both frozen
// interfaces share.
func TestUsageAndPollAndSnapshotDelegation(t *testing.T) {
	sw := newTestSwitcher(t)
	seedManaged(t, sw, "1", "a@x.com", oauthCreds("tok-a"))
	writeSeqDirect(t, sw, nil, []int{1}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "a@x.com", "organizationUuid": ""}),
	})

	fetch := map[string]bool{}
	entries := sw.UsageEntriesByAccount(fetch)
	if _, ok := entries["1"]; !ok {
		t.Fatalf("UsageEntriesByAccount missing slot 1: %v", entries)
	}
	usageMap := sw.UsageByAccount()
	if _, ok := usageMap["1"]; !ok {
		t.Fatalf("UsageByAccount missing slot 1: %v", usageMap)
	}
	_ = sw.UsageFetchStamps() // must not panic; no fetch has run yet

	snap := sw.AccountsSnapshot(fetch)
	if snap == nil || len(snap.Accounts) != 1 {
		t.Fatalf("AccountsSnapshot = %+v, want 1 account", snap)
	}

	statusOut, err := sw.Status(true)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, ok := asPayload(t, statusOut)["schemaVersion"]; !ok {
		t.Fatalf("Status payload missing schemaVersion: %v", statusOut)
	}

	sw.SetPollPolicyInputs(75, []string{"Fable"})
	sw.ClearPollPolicyInputs() // neither must panic; no observable getter exists
}

// TestAutoAddCurrentSeamWiring: the human (non-JSON) Switch path's
// unmanaged-live-account branch drives switching.AutoAddCurrent, which
// core.go wires to lifecycle.AddAccount — proving the seam is actually
// connected, not left nil.
func TestAutoAddCurrentSeamWiring(t *testing.T) {
	sw := newTestSwitcher(t)
	seedManaged(t, sw, "1", "managed@x.com", oauthCreds("tok-managed"))
	writeSeqDirect(t, sw, ptrInt(1), []int{1}, map[string]json.RawMessage{
		"1": rawRecord(map[string]any{"email": "managed@x.com", "organizationUuid": ""}),
	})
	seedLive(t, sw, "unmanaged@x.com", oauthCreds("tok-unmanaged"))

	if _, err := sw.Switch(nil, false, nil, nil); err != nil {
		t.Fatalf("Switch (human, unmanaged-live): %v", err)
	}
	data, err := sw.ReadSequence()
	if err != nil {
		t.Fatalf("ReadSequence: %v", err)
	}
	if len(data.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 (AutoAddCurrent should have added the live login)", len(data.Accounts))
	}
	found := false
	for _, raw := range data.Accounts {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["email"] == "unmanaged@x.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unmanaged@x.com was not auto-added: %v", data.Accounts)
	}
}

// TestFirstRunSetupSeamWiring: reporting's FirstRunSetup seam (wired to
// core's firstRunSetup) prompts to add the current live login when
// list_accounts finds no managed accounts yet.
func TestFirstRunSetupSeamWiring(t *testing.T) {
	sw := newTestSwitcher(t)
	seedLive(t, sw, "fresh@x.com", oauthCreds("tok-fresh"))
	withAnswers(t, "y")

	if _, err := sw.ListAccounts(false, false, nil); err != nil {
		t.Fatalf("ListAccounts (human, first run): %v", err)
	}
	data, err := sw.ReadSequence()
	if err != nil || data == nil || len(data.Accounts) != 1 {
		t.Fatalf("ReadSequence after first-run setup: data=%v err=%v", data, err)
	}
}

// TestPurge: a smoke check that Purge delegates and actually removes the
// backup directory (lifecycle's own purge_test.go covers the full edge-case
// list; this only proves the delegation reaches it).
func TestPurge(t *testing.T) {
	sw := newTestSwitcher(t)
	withAnswers(t, "y")
	if err := sw.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := os.Stat(sw.BackupDir()); !os.IsNotExist(err) {
		t.Fatalf("backup dir still present after Purge: err=%v", err)
	}
}
