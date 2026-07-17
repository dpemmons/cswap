// prog.go — program-name derivation for usage/help (spec 08§1 _prog_name).
//
// Implements spec 08§1 (_prog_name) and 10-audit Gap 2 (the __main__→"cswap"
// fallback). A Go binary has no importlib metadata; the name shown in usage is
// the invoked basename (stripping a trailing .exe), falling back to "cswap".
package cli

import (
	"path/filepath"
	"strings"
)

// progName computes the command name shown in usage/help from argv[0].
//
// Mirrors Python _prog_name (spec 08§1): basename(argv0), strip a trailing
// ".exe"/".pyw"/".py" (case-insensitive, first match), and map an empty or
// launcher-shim name ("__main__"/"python"/"python3"/"py") to the literal
// "cswap".
func progName(argv0 string) string {
	name := filepath.Base(argv0)
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	for _, ext := range []string{".exe", ".pyw", ".py"} {
		if len(name) >= len(ext) && strings.EqualFold(name[len(name)-len(ext):], ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	switch name {
	case "", "__main__", "python", "python3", "py":
		return "cswap"
	}
	return name
}
