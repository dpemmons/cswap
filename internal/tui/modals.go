// modals.go — the confirmation, add-token, and captured-output modal screens.
//
// Implements spec 09§7: ConfirmModal (§7.1, dismisses True only on explicit
// confirm), AddTokenModal (§7.2, field validation in order), and OutputModal
// (§7.3). Each modal stores its dismissal callback (Textual's
// push_screen(modal, callback)); dismissing pops the modal and invokes it.
// Per Deviation #7 the OutputModal shows structured-result text, not captured
// ANSI stdout.
package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tokenForm is what the add-token modal collects (09§7.2 TokenForm).
type tokenForm struct {
	Token string
	Email *string
	Slot  *int
}

// confirmModal is a yes/no confirmation (09§7.1). onDone receives the boolean
// dismissal value; only an explicit true runs a follow-up.
type confirmModal struct {
	title    string
	message  string
	yesLabel string
	focusYes bool
	onDone   func(m *Model, confirmed bool) tea.Cmd
}

func (c *confirmModal) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "y":
		return m.dismissModal(func(m *Model) tea.Cmd { return c.onDone(m, true) })
	case "n", "esc":
		return m.dismissModal(func(m *Model) tea.Cmd { return c.onDone(m, false) })
	case "left":
		c.focusYes = true
	case "right":
		c.focusYes = false
	case "enter":
		confirmed := c.focusYes
		return m.dismissModal(func(m *Model) tea.Cmd { return c.onDone(m, confirmed) })
	}
	return nil
}

func (c *confirmModal) view(m *Model) string {
	var b strings.Builder
	b.WriteString(modalTitleStyle.Render(c.title))
	b.WriteString("\n\n")
	b.WriteString(c.message)
	b.WriteString("\n\n")
	b.WriteString(button(c.yesLabel, c.focusYes) + "  " + button("Cancel", !c.focusYes))
	b.WriteString("\n\n")
	hint := "← → · enter  ·  y " + strings.ToLower(c.yesLabel) + "  ·  n / esc cancel"
	b.WriteString(modalHintStyle.Render(hint))
	return modalBox(b.String(), false)
}

// addTokenModal collects a token, optional email, and optional slot (09§7.2).
type addTokenModal struct {
	token     string
	email     string
	slot      string
	focus     int // 0 token, 1 email, 2 slot, 3 Add, 4 Cancel
	formError string
	onDone    func(m *Model, form *tokenForm) tea.Cmd
}

const addTokenFields = 5

func (a *addTokenModal) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "esc":
		return m.dismissModal(func(m *Model) tea.Cmd { return a.onDone(m, nil) })
	case "tab", "down":
		a.focus = (a.focus + 1) % addTokenFields
		return nil
	case "shift+tab", "up":
		a.focus = (a.focus - 1 + addTokenFields) % addTokenFields
		return nil
	case "left":
		if a.focus >= 3 {
			a.focus = 3
		}
		return nil
	case "right":
		if a.focus >= 3 {
			a.focus = 4
		}
		return nil
	case "enter":
		if a.focus == 4 {
			return m.dismissModal(func(m *Model) tea.Cmd { return a.onDone(m, nil) })
		}
		return a.submit(m)
	case "backspace":
		a.editFocused(func(s string) string {
			if s == "" {
				return s
			}
			return s[:len(s)-1]
		})
		return nil
	default:
		if len(key.String()) == 1 || key.Type == tea.KeyRunes {
			a.editFocused(func(s string) string { return s + string(key.Runes) })
		}
		return nil
	}
}

// editFocused applies fn to the currently-focused text field (no-op on buttons).
func (a *addTokenModal) editFocused(fn func(string) string) {
	switch a.focus {
	case 0:
		a.token = fn(a.token)
	case 1:
		a.email = fn(a.email)
	case 2:
		a.slot = fn(a.slot)
	}
}

// submit validates the form in the exact order of 09§7.2. Failures set the
// form error without dismissing.
func (a *addTokenModal) submit(m *Model) tea.Cmd {
	token := strings.TrimSpace(a.token)
	if token == "" {
		a.formError = "Token is required."
		return nil
	}
	var emailPtr *string
	if e := strings.TrimSpace(a.email); e != "" {
		emailPtr = &e
	}
	var slotPtr *int
	if slotRaw := strings.TrimSpace(a.slot); slotRaw != "" {
		n, err := strconv.Atoi(slotRaw)
		if err != nil {
			a.formError = "Slot must be a number."
			return nil
		}
		if n < 1 {
			a.formError = "Slot must be >= 1."
			return nil
		}
		slotPtr = &n
	}
	form := &tokenForm{Token: token, Email: emailPtr, Slot: slotPtr}
	return m.dismissModal(func(m *Model) tea.Cmd { return a.onDone(m, form) })
}

func (a *addTokenModal) view(m *Model) string {
	var b strings.Builder
	b.WriteString(modalTitleStyle.Render("Add account from token"))
	b.WriteString("\n\n")
	b.WriteString("OAuth setup-token (sk-ant-oat…) or managed API key (sk-ant-api…); the type is auto-detected.")
	b.WriteString("\n\n")
	b.WriteString(field("token (required)", strings.Repeat("•", len(a.token)), a.focus == 0))
	b.WriteString("\n")
	b.WriteString(field("email label (optional)", a.email, a.focus == 1))
	b.WriteString("\n")
	b.WriteString(field("slot number (optional)", a.slot, a.focus == 2))
	if a.formError != "" {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(colSevCrit)).Render(a.formError))
	}
	b.WriteString("\n\n")
	b.WriteString(button("Add", a.focus == 3) + "  " + button("Cancel", a.focus == 4))
	b.WriteString("\n\n")
	b.WriteString(modalHintStyle.Render("enter add  ·  tab next field  ·  esc cancel"))
	return modalBox(b.String(), false)
}

// outputModal shows a mutating action's structured-result text (09§7.3).
type outputModal struct {
	title  string
	output string
}

func (o *outputModal) update(m *Model, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "esc", "q", "enter":
		return m.dismissModal(nil)
	}
	return nil
}

func (o *outputModal) view(m *Model) string {
	var b strings.Builder
	b.WriteString(modalTitleStyle.Render(o.title))
	b.WriteString("\n\n")
	body := strings.TrimRight(o.output, " \n\t")
	if body == "" {
		body = "(no output)"
	}
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString(modalHintStyle.Render("esc close"))
	return modalBox(b.String(), true)
}

// -- modal chrome ------------------------------------------------------------

var (
	modalTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colForeground))
	modalHintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted))
)

// modalBox frames modal content in a bordered box (09§8.2: width 64, wide 90).
func modalBox(content string, wide bool) string {
	width := 64
	if wide {
		width = 90
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colPanel)).
		Padding(1, 2).
		Width(width).
		Render(content)
}

// button renders a modal button, accent-filled when focused (09§8.2).
func button(label string, focused bool) string {
	st := lipgloss.NewStyle().Padding(0, 2)
	if focused {
		st = st.Background(lipgloss.Color(colAccent)).Foreground(lipgloss.Color(colBackground)).Bold(true)
	} else {
		st = st.Background(lipgloss.Color(colPanel)).Foreground(lipgloss.Color(colForeground))
	}
	return st.Render(label)
}

// field renders a labelled text input line, accent-underlined when focused.
func field(placeholder, value string, focused bool) string {
	shown := value
	if shown == "" {
		shown = lipgloss.NewStyle().Foreground(lipgloss.Color(colMuted)).Render(placeholder)
	}
	marker := "  "
	if focused {
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent)).Render("▸ ")
	}
	return marker + shown
}
