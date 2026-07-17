// footer.go — the keybinding footer bar, the Go analog of Textual's Footer.
//
// Implements the footer-visible-binding tables of spec 09§3.1 (dashboard: s/w/q
// visible; back/g/f/j/k hidden), 09§3.6 (Switch: enter/b/back visible; j/k
// hidden), 09§3.7 (Watch: s always, enter Confirm only while selecting per the
// check_action gate, back visible; f/nav hidden), and 09§4.1 (Auto: l/t/back
// always, left/right/enter only while adjusting per check_action). Keys render
// in the accent color (Textual's footer-key-foreground = ACCENT, 09§8.1),
// labels muted. State-dependence mirrors Textual's check_action, which both
// hides a binding from the footer and makes it inert (09§11.5). The bar
// truncates gracefully at narrow widths via lipgloss display-width handling.
package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// footerBinding is one key+label hint shown in the footer bar.
type footerBinding struct {
	key   string
	label string
}

// footerScreen is a primary screen that publishes its footer-visible bindings.
// Modals do not implement it: they carry their own inline hint lines (09§7) and
// cover the footer, exactly as a Textual ModalScreen does.
type footerScreen interface {
	footerBindings(m *Model) []footerBinding
}

const (
	footerSep     = "   " // gap between adjacent key+label entries
	footerEllipse = "…"   // graceful "more, truncated" marker
)

// footerText builds the styled footer richText for a set of bindings, keeping
// styling as data so the plain text stays testable. Entries are appended while
// they fit within width (display width, arrow glyphs included); once one would
// overflow, the remainder is dropped and an ellipsis is appended. Width math
// uses lipgloss.Width so multi-byte key glyphs (← →) count as one cell.
func footerText(bindings []footerBinding, width int) richText {
	var t richText
	if width <= 0 {
		width = 80
	}
	used := 0
	for i, b := range bindings {
		sepW := 0
		if i > 0 {
			sepW = lipgloss.Width(footerSep)
		}
		pieceW := lipgloss.Width(b.key + " " + b.label)
		if i > 0 && used+sepW+pieceW > width {
			t.addPlain(footerSep)
			t.addFg(footerEllipse, colMuted)
			return t
		}
		if i > 0 {
			t.addPlain(footerSep)
		}
		t.add(b.key, segStyle{Fg: colAccent, Bold: true})
		t.addFg(" "+b.label, colMuted)
		used += sepW + pieceW
	}
	return t
}

// renderFooter styles the footer bar for View(), hard-capping the result to the
// terminal width as a final guard so a single over-wide binding can never
// overflow the row (lipgloss MaxWidth is ANSI-aware).
func (m *Model) renderFooter(bindings []footerBinding) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	rendered := footerText(bindings, width).render()
	return lipgloss.NewStyle().MaxWidth(width).Render(rendered)
}
