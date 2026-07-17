// autoswitch_adapters.go — the one explicit adapter method the frozen
// autoswitch.Switcher (DESIGN §2.18/A13) needs beyond plain *store.Store
// promotion (BackfillAccountUUID), plus the documented, unresolved
// ReadAccountCredentials name/signature collision between autoswitch.Switcher
// and session.Accounts.
//
// Implements spec 01§ (backfill_account_uuid) and DESIGN Amendment A13.
package core

// BackfillAccountUUID records a discovered uuid on a blank-uuid slot (spec
// 01§ backfill_account_uuid). autoswitch.Switcher (DESIGN §2.18, FROZEN by
// A13) pins this method with NO error return — Python's equivalent is a
// best-effort call the engine never checks — while store.Store's own
// BackfillAccountUUID(num, uuid string) error returns a fallible result (it
// acquires the FileLock, spec 01§10.1). This method therefore shadows the
// promoted store method: any write failure is logged and swallowed rather
// than surfaced, matching the interface's contract.
func (sw *Switcher) BackfillAccountUUID(num, uuid string) {
	if err := sw.Store.BackfillAccountUUID(num, uuid); err != nil && sw.Store.Log != nil {
		sw.Store.Log.Warningf("BackfillAccountUUID(%s): %v", num, err)
	}
}

// ---- ReadAccountCredentials: a genuine, unresolved cross-package conflict --
//
// autoswitch.Switcher (DESIGN §2.18, FROZEN by A13) pins:
//
//	ReadAccountCredentials(num, email string) string
//
// session.Accounts (DESIGN A2, WP9) pins:
//
//	ReadAccountCredentials(num, email string) (string, error)
//
// Go permits exactly one method named ReadAccountCredentials per type, so
// *Switcher cannot structurally satisfy both interfaces' literal signatures
// simultaneously — no matter which shape core.Switcher's method takes, one of
// the two frozen/pinned interfaces will not compile against it. This is a
// genuine coordination gap between WP13 (autoswitch, built against a fake
// before core existed) and WP9 (session, whose own upstream note lists
// exactly three methods needing an explicit adapter here and does NOT
// include this one — i.e. WP9 expected plain promotion to work).
//
// This package resolves it by NOT overriding the promoted
// store.Store.ReadAccountCredentials(num, email string) (string, error),
// which:
//   - satisfies session.Accounts exactly, as WP9 expected (zero adapter code);
//   - matches Python's real _read_account_credentials signature, which never
//     raises (every failure is caught, logged, and folded into "" — spec
//     03§5.3/credentials.py) — so the *error-carrying* shape is the
//     Python-unfaithful one of the two, but propagating it is real, tested
//     production behavior in session/bootstrap.go's credential-read guard;
//   - matches the prevailing convention used by every OTHER caller of this
//     method throughout switching/lifecycle/reporting/store, which uniformly
//     swallow the error (`backup, _ := s.ReadAccountCredentials(...)`).
//
// Consequence: *core.Switcher does NOT compile against autoswitch.Switcher as
// literally written. This is recorded as an interfaceChange for integration
// (see the WP10 summary) rather than silently invented away. The recommended
// fix, and the one requiring no further change here, is a one-line adapter
// type at the cli (WP15) integration layer — the same layer DESIGN's "cli
// will carry the final [compile assertions]" note anticipates:
//
//	type autoswitchFacade struct{ *core.Switcher }
//	func (a autoswitchFacade) ReadAccountCredentials(num, email string) string {
//		v, _ := a.Switcher.ReadAccountCredentials(num, email)
//		return v
//	}
//	var _ autoswitch.Switcher = autoswitchFacade{}
//
// autoswitchFacade satisfies autoswitch.Switcher's method set: it inherits
// every other promoted/adapter method from *core.Switcher unchanged, and its
// own ReadAccountCredentials shadows the embedded one with exactly the
// no-error shape autoswitch.Switcher requires.
