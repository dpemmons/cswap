// Test scaffolding: an in-memory Switcher fake, usage-entry/window builders, an
// event recorder, and a controllable loop sleeper. Everything is deterministic
// (clock.Fake throughout, no real sleeps), per DESIGN §5 WP13.

package autoswitch

import (
	"context"
	"sort"
	"sync"
	"time"

	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// fakeSwitcher implements Switcher against in-memory state.
type fakeSwitcher struct {
	current      *string
	liveLogin    bool
	emails       map[string]string
	switchable   []string
	kinds        map[string]string // num -> "api_key" (else oauth)
	identities   map[string]map[string]string
	creds        map[string]string
	liveSessions map[string][]int
	entries      map[string]usage.UsageEntry
	backupDir    string

	// SwitchTo behavior. Default: set current=num, return a success payload.
	switchTo func(f *fakeSwitcher, num string) (map[string]any, error)
	// PersistBackupCredentials error injection.
	persistErr error

	// Recorded interactions.
	fetchCalls [][]string
	persisted  map[string]string
	backfilled map[string]string
	pollInputs []pollInput
	mu         sync.Mutex
}

type pollInput struct {
	threshold float64
	models    []string
}

func newFake() *fakeSwitcher {
	return &fakeSwitcher{
		emails:       map[string]string{},
		kinds:        map[string]string{},
		identities:   map[string]map[string]string{},
		creds:        map[string]string{},
		liveSessions: map[string][]int{},
		entries:      map[string]usage.UsageEntry{},
		backupDir:    "/tmp/does-not-matter",
		persisted:    map[string]string{},
		backfilled:   map[string]string{},
	}
}

func strp(s string) *string { return &s }

func (f *fakeSwitcher) CurrentAccountNumber() *string { return f.current }
func (f *fakeSwitcher) HasLiveLogin() bool            { return f.liveLogin }
func (f *fakeSwitcher) AccountEmail(num string) string {
	return f.emails[num]
}
func (f *fakeSwitcher) SwitchableAccountNumbers() []string { return f.switchable }
func (f *fakeSwitcher) AccountKindFor(num string) string {
	if k := f.kinds[num]; k != "" {
		return k
	}
	return "oauth"
}
func (f *fakeSwitcher) AccountIdentity(num string) map[string]string {
	if id := f.identities[num]; id != nil {
		return id
	}
	return map[string]string{"email": f.emails[num], "organizationUuid": "", "uuid": ""}
}
func (f *fakeSwitcher) ReadAccountCredentials(num, email string) string { return f.creds[num] }
func (f *fakeSwitcher) PersistBackupCredentials(num, email, creds string) error {
	if f.persistErr != nil {
		return f.persistErr
	}
	f.mu.Lock()
	f.persisted[num] = creds
	f.creds[num] = creds
	f.mu.Unlock()
	return nil
}
func (f *fakeSwitcher) BackfillAccountUUID(num, uuid string) {
	f.mu.Lock()
	f.backfilled[num] = uuid
	if id := f.identities[num]; id != nil {
		id["uuid"] = uuid
	}
	f.mu.Unlock()
}
func (f *fakeSwitcher) UsageEntriesByAccount(fetch map[string]bool) map[string]usage.UsageEntry {
	keys := make([]string, 0, len(fetch))
	for k := range fetch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	f.mu.Lock()
	f.fetchCalls = append(f.fetchCalls, keys)
	f.mu.Unlock()
	out := make(map[string]usage.UsageEntry, len(f.entries))
	for k, v := range f.entries {
		out[k] = v
	}
	return out
}
func (f *fakeSwitcher) SwitchTo(num string, jsonOut bool) (map[string]any, error) {
	if f.switchTo != nil {
		return f.switchTo(f, num)
	}
	from := map[string]any{"number": atoi(deref(f.current)), "email": f.emails[deref(f.current)]}
	f.current = strp(num)
	to := map[string]any{"number": atoi(num), "email": f.emails[num]}
	return map[string]any{
		"schemaVersion": 1,
		"switched":      true,
		"from":          from,
		"to":            to,
		"strategy":      "best",
		"reason":        "",
		"message":       "",
		"warnings":      []any{},
	}, nil
}
func (f *fakeSwitcher) LiveSessionPidsFor(num, email string) []int { return f.liveSessions[num] }
func (f *fakeSwitcher) SetPollPolicyInputs(threshold float64, models []string) {
	f.mu.Lock()
	f.pollInputs = append(f.pollInputs, pollInput{threshold, append([]string(nil), models...)})
	f.mu.Unlock()
}
func (f *fakeSwitcher) ClearPollPolicyInputs() {}
func (f *fakeSwitcher) BackupDir() string      { return f.backupDir }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func atoi(s string) int {
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

// -- usage-entry / window builders ----------------------------------------

// win builds a normalized 5h/7d window {pct, resets_at?}.
func win(pct float64, resetsAt string) map[string]any {
	m := map[string]any{"pct": pct}
	if resetsAt != "" {
		m["resets_at"] = resetsAt
	}
	return m
}

// scopedWin builds a normalized scoped window {name, pct, resets_at?}.
func scopedWin(name string, pct float64, resetsAt string) map[string]any {
	m := map[string]any{"name": name, "pct": pct}
	if resetsAt != "" {
		m["resets_at"] = resetsAt
	}
	return m
}

// usageOf builds a usage dict from a 5h and 7d pct (no reset times).
func usageOf(fiveH, sevenD float64) map[string]any {
	return map[string]any{"five_hour": win(fiveH, ""), "seven_day": win(sevenD, "")}
}

// dictEntry is a UsageEntry whose decision value is the given trusted usage dict.
func dictEntry(u map[string]any) usage.UsageEntry {
	age := 0.0
	fetched := 1.0
	return usage.UsageEntry{LastGood: u, AgeS: &age, FetchedAt: &fetched}
}

// sentinelEntry is a UsageEntry whose decision value is the given sentinel.
func sentinelEntry(s string) usage.UsageEntry {
	return usage.UsageEntry{Sentinel: s}
}

// nilEntry is a UsageEntry whose decision value is nil (usage unknown).
func nilEntry() usage.UsageEntry { return usage.UsageEntry{} }

// errEntry is a usage-unknown entry carrying a last-error string.
func errEntry(lastError string) usage.UsageEntry {
	return usage.UsageEntry{LastError: lastError}
}

// -- event recorder --------------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) on(ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Kind()
	}
	return out
}

func (r *recorder) last(kind string) Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Kind() == kind {
			return r.events[i]
		}
	}
	return nil
}

func (r *recorder) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Kind() == kind {
			n++
		}
	}
	return n
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

// -- oauth fake ------------------------------------------------------------

func fakeOAuth(refresh func(creds string) oauth.RefreshOutcome) oauth.Client {
	return &oauth.FakeClient{
		RefreshFn: func(ctx context.Context, creds string) oauth.RefreshOutcome {
			return refresh(creds)
		},
	}
}

// -- controllable loop sleeper --------------------------------------------

// blockingSleeper hands out After channels that fire only when the test calls
// fire(); it records each requested delay. Used for RunLoop tests.
type blockingSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
	chans  []chan time.Time
}

func (s *blockingSleeper) After(d time.Duration) <-chan time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan time.Time, 1)
	s.delays = append(s.delays, d)
	s.chans = append(s.chans, ch)
	return ch
}

// fireLast releases the most recently requested After channel.
func (s *blockingSleeper) fireLast() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chans) > 0 {
		ch := s.chans[len(s.chans)-1]
		select {
		case ch <- time.Now():
		default:
		}
	}
}

func (s *blockingSleeper) lastDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.delays) == 0 {
		return 0
	}
	return s.delays[len(s.delays)-1]
}

func (s *blockingSleeper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delays)
}
