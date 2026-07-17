// runaction.go — the RunAction structured-result seam (DESIGN A11, Deviation
// #7): the TUI runs its mutating actions through this instead of capturing ANSI
// stdout (spec 09§6.2/§11.4).
//
// Python's run_action does a process-global redirect_stdout + Text.from_ansi
// round-trip to capture the CLI-equivalent's printed output and re-render it in
// a modal. The Go port deliberately departs (DESIGN Deviation #7): the switcher
// functions return (payload, error), and this seam projects that into a small
// RunResult the bubbletea layer formats itself — no fd-level redirect, no ANSI
// capture. The rendered modal text is therefore NOT byte-identical to Python's
// captured output, by design. The type lives here (A11) but the TUI consumes it
// only through tui.Facade.
package reporting

// RunResult is the outcome of a TUI-triggered mutating switcher action. Message
// is the single user-facing line the modal/toast shows (the handled-error text
// on failure); Payload is the json-capable structured result, or nil for a
// non-JSON action or an error.
type RunResult struct {
	OK      bool
	Message string
	Payload any
}

// RunAction runs a mutating switcher action and projects its (payload, error)
// return into a RunResult (spec 09§6.2, DESIGN Deviation #7). Any error becomes
// OK=false with the "Error: <msg>" message Python's run_action printed for a
// handled ClaudeSwitchError, rather than panicking the UI thread. No stdout is
// captured — the bubbletea layer formats its own modal text from this struct.
func RunAction(fn func() (any, error)) RunResult {
	payload, err := fn()
	if err != nil {
		return RunResult{OK: false, Message: "Error: " + err.Error()}
	}
	return RunResult{OK: true, Payload: payload}
}
