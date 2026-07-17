// list.go — list_accounts: the schema-v1 --list --json payload and the human
// account list, including the duplicate/lockstep warnings and the running-
// instances block (spec 02§10.1, 02§11).
//
// Implements spec 02§11 (list_accounts) and 02§10.1 (_build_list_payload). In
// JSON mode ListAccounts returns the payload (the CLI does the single
// json.MarshalIndent); in human mode it prints and returns nil. The empty-list
// payload is returned without prompting; only the human no-accounts path runs
// the first-run setup seam.
package reporting

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/oauth"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/procdetect"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// ListAccounts lists every managed account (spec 02§11). In JSON mode it returns
// the schema-v1 payload (printing nothing); in human mode it renders to stdout
// and returns nil. fetch restricts which accounts may be fetched this pass;
// nil — the CLI default — leaves every stale account eligible.
func ListAccounts(s *store.Store, showTokenStatus, jsonOut bool, fetch map[string]bool) (any, error) {
	if _, err := os.Stat(s.SequenceFile); errors.Is(err, fs.ErrNotExist) {
		// JSON mode must never prompt — emit an empty list instead of the
		// interactive first-run setup.
		if jsonOut {
			return map[string]any{
				"schemaVersion":       jsonout.SchemaVersion,
				"activeAccountNumber": nil,
				"accounts":            []any{},
			}, nil
		}
		fmt.Fprintln(os.Stdout, printer.Dimmed("No accounts are managed yet."))
		if FirstRunSetup != nil {
			_ = FirstRunSetup(s)
		}
		return nil, nil
	}

	infos := BuildAccountsInfo(s)
	entries := CollectUsageEntries(s, infos, fetch)

	if jsonOut {
		return buildListPayload(s, infos, entries), nil
	}

	renderAccounts(os.Stdout, s, infos, entries, showTokenStatus)
	return nil, nil
}

// buildListPayload assembles the --list --json payload (spec 02§10.1). Each row
// carries the decision-grade usage value (last-good only while ≤ STALE_OK_S or
// trust-extended); older reads report unavailable even though the human list
// still shows the numbers with an age note. Additive top-level fields are
// present only when non-empty.
func buildListPayload(s *store.Store, infos []AccountInfo, entries map[string]usage.UsageEntry) map[string]any {
	data, _ := s.ReadSequence()
	models := configuredModels(s)
	var activeNum any
	accounts := make([]any, 0, len(infos))
	for _, info := range infos {
		num := strconv.Itoa(info.Number)
		if info.IsActive {
			activeNum = info.Number
		}
		entry := entries[num]
		atLimit, limiting := atLimitFor(entry.DecisionValue(), models)
		accounts = append(accounts, jsonout.AccountRow(
			info.Number, info.Email, info.OrgName, info.OrgUUID, info.IsActive,
			entry.DecisionValue(),
			jsonout.RowOpts{
				UsageFetchedAt:  entry.FetchedAt,
				UsageAgeS:       entry.AgeS,
				Alias:           info.Alias,
				Disabled:        recordDisabled(data, num),
				AtLimit:         atLimit,
				LimitingWindows: limiting,
			},
		))
	}
	payload := map[string]any{
		"schemaVersion":       jsonout.SchemaVersion,
		"activeAccountNumber": activeNum,
		"accounts":            accounts,
	}
	if dup := duplicateAccountWarnings(s, infos); len(dup) > 0 {
		payload["duplicateAccountWarnings"] = dup
	}
	if lockstep := lockstepUsageWarnings(infos, entries); len(lockstep) > 0 {
		payload["lockstepUsageWarnings"] = lockstep
	}
	if unclaimed, _ := s.Creds.ListUnclaimed(); len(unclaimed) > 0 {
		keys := make([]string, 0, len(unclaimed))
		for k := range unclaimed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		payload["unclaimedCredentials"] = keys
	}
	return payload
}

// renderAccounts prints the human account list to w (spec 02§11). Rows carry the
// alias-first label, the org tag, and active/disabled markers; usage lines are
// indented 5 spaces; duplicate/lockstep warnings follow (unclaimed credentials
// are deliberately not shown); then the running-instances block.
func renderAccounts(w io.Writer, s *store.Store, infos []AccountInfo, entries map[string]usage.UsageEntry, showTokenStatus bool) {
	data, _ := s.ReadSequence()
	models := configuredModels(s)
	fmt.Fprintln(w, printer.Bolded("Accounts:"))
	for i, info := range infos {
		num := strconv.Itoa(info.Number)
		tag := displayTag(info.OrgName)
		label := info.Email
		if info.Alias != "" {
			label = printer.Accent(info.Alias) + " (" + info.Email + ")"
		}
		markers := ""
		if info.IsActive {
			markers += " " + printer.BoldAccent("(active)")
		}
		if recordDisabled(data, num) {
			markers += " " + printer.Muted("(disabled)")
		}
		if mk := atLimitMarker(entries[num].DecisionValue(), models); mk != "" {
			markers += " " + mk
		}
		fmt.Fprintf(w, "  %s: %s %s%s\n", num, label, printer.Muted("["+tag+"]"), markers)
		for _, line := range usageEntryLines(entries[num]) {
			fmt.Fprintf(w, "     %s\n", line)
		}
		if showTokenStatus {
			if ts, ok := oauth.BuildTokenStatus(info.Creds, s.Clk.Now()); ok && ts != "" {
				fmt.Fprintf(w, "     %s %s\n", printer.Dimmed("•"), printer.Muted(ts))
			}
		}
		if i < len(infos)-1 {
			fmt.Fprintln(w)
		}
	}

	dup := duplicateAccountWarnings(s, infos)
	lockstep := lockstepUsageWarnings(infos, entries)
	if len(dup) > 0 || len(lockstep) > 0 {
		fmt.Fprintln(w)
		for _, msg := range dup {
			fmt.Fprintln(w, printer.Yellowed(msg))
		}
		for _, msg := range lockstep {
			fmt.Fprintln(w, printer.Yellowed(msg))
		}
	}

	renderRunningInstances(w)
}

// renderRunningInstances prints the "Running instances:" block grouped by
// (entrypoint label, cwd) in first-seen order (spec 02§11). procdetect never
// raises, so the Python try/except-and-log around this block is unnecessary.
func renderRunningInstances(w io.Writer) {
	sessions, ides := procdetect.GetRunningInstances(procdetect.GetClaudeDir())
	if len(sessions) == 0 && len(ides) == 0 {
		return
	}
	type group struct {
		label    string
		cwd      string
		sessions int
		ide      int
	}
	var order []*group
	index := map[string]*group{}
	get := func(label, cwd string) *group {
		key := label + "\x00" + cwd
		g, ok := index[key]
		if !ok {
			g = &group{label: label, cwd: cwd}
			index[key] = g
			order = append(order, g)
		}
		return g
	}
	for _, sess := range sessions {
		get(printer.EntrypointLabel(sess.Entrypoint), printer.AbbreviatePath(sess.CWD)).sessions++
	}
	for _, ide := range ides {
		name := printer.IDEShortName(ide.IDEName)
		for _, folder := range ide.WorkspaceFolders {
			get(name, printer.AbbreviatePath(folder)).ide++
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, printer.Bolded("Running instances:"))
	for _, g := range order {
		var parts []string
		if g.sessions > 0 {
			plural := ""
			if g.sessions > 1 {
				plural = "s"
			}
			parts = append(parts, fmt.Sprintf("%d session%s", g.sessions, plural))
		}
		if g.ide > 0 {
			parts = append(parts, "IDE")
		}
		fmt.Fprintf(w, "  %s %s   %s  %s\n",
			printer.Dimmed("●"), printer.Muted(g.label), printer.Muted(g.cwd),
			printer.Dimmed("("+strings.Join(parts, ", ")+")"))
	}
}

// displayTag returns an account's org tag for display: the org name, or
// "personal" (spec 02§11 _get_display_tag).
func displayTag(orgName string) string {
	if orgName != "" {
		return orgName
	}
	return "personal"
}
