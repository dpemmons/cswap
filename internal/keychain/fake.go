// Implements spec 03§4 conftest block_real_keychain parity: an in-memory
// (service, account) → secret map. Delete on an absent key is a no-op (rc 44
// parity); Get on an absent key reports not-found.

package keychain

import "sync"

// Fake is an in-memory KeychainClient for tests on any platform.
type Fake struct {
	mu sync.Mutex
	m  map[[2]string]string
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{m: make(map[[2]string]string)}
}

func key(service, account string) [2]string { return [2]string{service, account} }

// Get returns the stored value, or ("", false, nil) when absent.
func (f *Fake) Get(service, account string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[key(service, account)]
	return v, ok, nil
}

// Set stores the value.
func (f *Fake) Set(service, account, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = make(map[[2]string]string)
	}
	f.m[key(service, account)] = password
	return nil
}

// Delete removes the item; absent keys are a no-op (rc 44 parity).
func (f *Fake) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key(service, account))
	return nil
}

// Exists reports whether the item is present.
func (f *Fake) Exists(service, account string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[key(service, account)]
	return ok
}

var _ KeychainClient = (*Fake)(nil)
