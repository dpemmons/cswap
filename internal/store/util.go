// util.go — small formatting helpers shared across the store package
// (spec 01§7: the ", "-joined PID list in the live-session error message).
package store

import (
	"strconv"
	"strings"
)

// joinPIDs renders a PID slice as a comma-space list, matching Python's
// ", ".join(map(str, pids)) in the live-session error message (spec 01§7).
func joinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}
