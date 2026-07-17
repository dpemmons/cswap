// Package wincred is the legacy Windows Credential Manager reader used only by
// the migrations package's windows_keyring_to_files migration (spec
// 07§5.3, DESIGN Amendment A9).
//
// Windows claude-swap installs prior to issue #45's fix stored per-account
// *backup* credentials (not the live/active credential) in Windows Credential
// Manager via the third-party Python `keyring` library, under service
// "claude-code" with per-account usernames "account-{num}-{email}". A Go
// binary never writes there — this package exists solely to *read and delete*
// those legacy entries during the one-time relocation to file-backed .enc
// storage, exactly mirroring the read-then-delete shape `internal/keychain`
// gives the macOS migration.
//
// Real (build tag windows) wraps advapi32.dll's CredReadW/CredDeleteW and
// reproduces the Python `keyring` library's WinVaultKeyring resolution: a
// credential is first looked up under the plain service name, and — because
// Windows Credential Manager has no native concept of multiple users under one
// target name — falls back to the compound "{account}@{service}" target name
// `keyring` uses to disambiguate a second account under the same service. Every
// non-Windows build compiles Real as a stub that always reports not-found
// (DESIGN A9), so the shared migration logic in `internal/migrations` links and
// runs identically on every GOOS; only a real Windows binary ever has legacy
// data to find.
package wincred

import "sync"

// Client is the read/delete seam a migration needs against the legacy
// per-account Windows Credential Manager entries. Get mirrors
// keychain.KeychainClient.Get's (value, found, err) shape so migrations.go can
// treat the macOS (keychain.KeychainClient) and Windows (wincred.Client) legacy
// backends uniformly.
type Client interface {
	// Get returns the stored value and true when found; ("", false, nil) when
	// no entry exists for (service, account) under any resolution strategy; a
	// non-nil error only for a genuine backend failure.
	Get(service, account string) (value string, found bool, err error)
	// Delete removes the entry; a missing entry is a no-op success (rc-44
	// parity with keychain.KeychainClient.Delete).
	Delete(service, account string) error
}

// Fake is an in-memory Client for tests on any platform — Windows Credential
// Manager access needs no real backend to unit-test the shared relocation
// logic in internal/migrations. Keyed by the plain (service, account) pair;
// it does not model the compound-name collision resolution Real reproduces,
// since that is a Windows Credential Manager storage-layout detail invisible
// at this interface.
type Fake struct {
	mu sync.Mutex
	m  map[[2]string]string
}

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{m: make(map[[2]string]string)} }

func fakeKey(service, account string) [2]string { return [2]string{service, account} }

// Set seeds an entry (test helper — Real never writes; only Set to arrange
// fixtures).
func (f *Fake) Set(service, account, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = make(map[[2]string]string)
	}
	f.m[fakeKey(service, account)] = value
}

// Get implements Client.
func (f *Fake) Get(service, account string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[fakeKey(service, account)]
	return v, ok, nil
}

// Delete implements Client.
func (f *Fake) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, fakeKey(service, account))
	return nil
}

var _ Client = (*Fake)(nil)
