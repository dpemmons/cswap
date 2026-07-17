// status.go — status: the schema-v1 --status --json payload and the human
// active-account status line (spec 02§10.2, 02§12).
//
// Implements spec 02§12 (status) and 02§10.2 (_build_status_payload). The active
// account's usage runs through the same shared collector as list
// (_active_account_usage), so freshness/backoff/claim gating and the shared
// cache/usage.json behave identically. The JSON payload carries the decision-
// grade projection: stale beyond STALE_OK_S reports unavailable, not "ok" with
// old numbers.
package reporting

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"git.dpemmons.com/dpemmons/cswap/internal/jsonout"
	"git.dpemmons.com/dpemmons/cswap/internal/printer"
	"git.dpemmons.com/dpemmons/cswap/internal/store"
	"git.dpemmons.com/dpemmons/cswap/internal/usage"
)

// Status displays the current account status (spec 02§12). In JSON mode it
// returns the schema-v1 payload; in human mode it prints to stdout and returns
// nil.
func Status(s *store.Store, jsonOut bool) (any, error) {
	if jsonOut {
		return buildStatusPayload(s), nil
	}
	renderStatus(os.Stdout, s)
	return nil, nil
}

// buildStatusPayload assembles the --status --json payload (spec 02§10.2): no
// active login → active null; live but unmanaged → active {email, managed:false};
// managed → the full active object plus totalManagedAccounts.
func buildStatusPayload(s *store.Store) map[string]any {
	email, orgUUID, ok := s.GetCurrentAccount()
	if !ok {
		return map[string]any{"schemaVersion": jsonout.SchemaVersion, "active": nil}
	}
	data, _ := s.SequenceMigrated()
	if data == nil {
		return unmanagedStatus(email)
	}
	accountNum := s.FindAccountSlot(data, email, orgUUID)
	if accountNum == "" {
		return unmanagedStatus(email)
	}

	rec, _ := recordFor(data, accountNum)
	orgName := recStr(rec, "organizationName")
	recOrgUUID := recStr(rec, "organizationUuid")
	alias := recStr(rec, "alias")

	entry := activeAccountUsage(s, accountNum, email, recOrgUUID)
	status, usageJSON := jsonout.UsageFields(entry.DecisionValue())

	n, _ := strconv.Atoi(accountNum)
	active := map[string]any{
		"number":           n,
		"email":            email,
		"organizationName": orgName,
		"organizationUuid": recOrgUUID,
		"isOrganization":   recOrgUUID != "",
		"managed":          true,
		"usageStatus":      status,
	}
	if usageJSON == nil {
		active["usage"] = nil
	} else {
		active["usage"] = usageJSON
	}
	if alias != "" {
		active["alias"] = alias
	}
	atLimit, limiting := atLimitFor(entry.DecisionValue(), configuredModels(s))
	for k, v := range jsonout.AtLimitFields(atLimit, limiting) {
		active[k] = v
	}
	if usageJSON != nil {
		for k, v := range jsonout.UsageFreshnessFields(entry.FetchedAt, entry.AgeS) {
			active[k] = v
		}
	}
	return map[string]any{
		"schemaVersion":        jsonout.SchemaVersion,
		"active":               active,
		"totalManagedAccounts": len(data.Accounts),
	}
}

// unmanagedStatus is the {email, managed:false} active object shared by the
// no-sequence and unmatched-slot cases.
func unmanagedStatus(email string) map[string]any {
	return map[string]any{
		"schemaVersion": jsonout.SchemaVersion,
		"active":        map[string]any{"email": email, "managed": false},
	}
}

// renderStatus prints the human status view to w (spec 02§12).
func renderStatus(w io.Writer, s *store.Store) {
	email, orgUUID, ok := s.GetCurrentAccount()
	if !ok {
		fmt.Fprintf(w, "%s %s\n", printer.Bolded("Status:"), printer.Dimmed("No active Claude account"))
		return
	}
	data, _ := s.SequenceMigrated()
	if data == nil {
		fmt.Fprintf(w, "%s %s %s\n", printer.Bolded("Status:"), email, printer.Dimmed("(not managed)"))
		return
	}
	accountNum := s.FindAccountSlot(data, email, orgUUID)
	if accountNum == "" {
		fmt.Fprintf(w, "%s %s %s\n", printer.Bolded("Status:"), email, printer.Dimmed("(not managed)"))
		return
	}

	rec, _ := recordFor(data, accountNum)
	tag := displayTag(recStr(rec, "organizationName"))
	total := len(data.Accounts)
	entry := activeAccountUsage(s, accountNum, email, orgUUID)
	marker := ""
	if mk := atLimitMarker(entry.DecisionValue(), configuredModels(s)); mk != "" {
		marker = " " + mk
	}
	fmt.Fprintf(w, "%s %s (%s %s)%s\n",
		printer.Bolded("Status:"), printer.Accent("Account-"+accountNum), email, printer.Muted("["+tag+"]"), marker)
	fmt.Fprintf(w, "  %s\n", printer.Dimmed(fmt.Sprintf("Total managed accounts: %d", total)))
	for _, line := range usageEntryLines(entry) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// activeAccountUsage builds a single-account info row for the active slot and
// runs it through the shared collector (spec 02§12 _active_account_usage), so
// freshness/backoff/claim gating matches list exactly.
func activeAccountUsage(s *store.Store, accountNum, email, orgUUID string) usage.UsageEntry {
	val, kcUnavail, _ := s.Creds.ReadActive()
	n, _ := strconv.Atoi(accountNum)
	info := AccountInfo{
		Number:              n,
		Email:               email,
		OrgUUID:             orgUUID,
		IsActive:            true,
		Creds:               val,
		KeychainUnavailable: kcUnavail,
	}
	entries := CollectUsageEntries(s, []AccountInfo{info}, nil)
	return entries[accountNum]
}
