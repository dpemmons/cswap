// result.go — the switch-result payload builders.
//
// Implements spec 02§8.6 (_switch_result_from_op / _switch_noop) and 02§10.3
// (the switch/switch-to JSON schema). switched is identity-based (from != to),
// covering recorded/live drift, not just the explicit already-active case; every
// switched:false payload reports from == to.
package switching

import "git.dpemmons.com/dpemmons/cswap/internal/jsonout"

// switchOp is the return of performSwitch: the left/landed identities captured
// under the lock plus any warnings. From may be nil (fresh machine).
type switchOp struct {
	From     map[string]any
	To       map[string]any
	Warnings []string
}

// switchResultFromOp builds a switch result payload from a performSwitch op
// (spec 02§8.6). switched = from != to. extra warnings precede the op's.
func switchResultFromOp(op switchOp, strategy string, extra []string) map[string]any {
	switched := !refEqual(op.From, op.To)
	var reason, message string
	if switched {
		reason = "switched"
		message = "Switched to Account-" + refNum(op.To) + " (" + refEmail(op.To) + ")"
	} else {
		reason = "already-active"
		message = "Already on Account-" + refNum(op.To) + " (" + refEmail(op.To) + ")"
	}
	warnings := append([]string{}, extra...)
	warnings = append(warnings, op.Warnings...)
	return map[string]any{
		"schemaVersion": jsonout.SchemaVersion,
		"switched":      switched,
		"from":          op.From,
		"to":            op.To,
		"strategy":      strategy,
		"reason":        reason,
		"message":       message,
		"warnings":      warnings,
	}
}

// noopArgs configures a switchNoop (spec 02§8.6 _switch_noop). FromRef defaults
// to ToRef so every switched:false payload reports from == to.
type noopArgs struct {
	strategy string
	reason   string
	message  string
	fromRef  map[string]any
	toRef    map[string]any
	warnings []string
}

// switchNoop builds a no-op switch result (switched:false).
func switchNoop(a noopArgs) map[string]any {
	from := a.fromRef
	if from == nil {
		from = a.toRef
	}
	w := a.warnings
	if w == nil {
		w = []string{}
	}
	return map[string]any{
		"schemaVersion": jsonout.SchemaVersion,
		"switched":      false,
		"from":          from,
		"to":            a.toRef,
		"strategy":      a.strategy,
		"reason":        a.reason,
		"message":       a.message,
		"warnings":      w,
	}
}

// refEqual compares two account_ref dicts by (number, email), matching Python's
// dict equality. Both nil ⇒ equal; exactly one nil ⇒ not equal.
func refEqual(a, b map[string]any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return refNumKey(a) == refNumKey(b) && refEmail(a) == refEmail(b)
}

// refNumKey normalizes a ref's number (*int or nil) into a comparable string
// key: "" for null, else the decimal value.
func refNumKey(m map[string]any) string {
	if m == nil {
		return ""
	}
	switch n := m["number"].(type) {
	case *int:
		if n == nil {
			return ""
		}
		return itoa(*n)
	case int:
		return itoa(n)
	default:
		return ""
	}
}

// refNum returns a ref's slot number rendered for a message ("" for null).
func refNum(m map[string]any) string { return refNumKey(m) }

// refEmail returns a ref's email ("" when absent).
func refEmail(m map[string]any) string {
	if m == nil {
		return ""
	}
	s, _ := m["email"].(string)
	return s
}
