package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"git.dpemmons.com/dpemmons/cswap/internal/core"
	"git.dpemmons.com/dpemmons/cswap/internal/session"
)

// TestEnvPreDispatchRegistered: `env` is pre-dispatched (like run/map/alias),
// not routed through memorable-verb translation. --help returns 0 with the
// env-specific usage and never touches the switcher.
func TestEnvPreDispatchRegistered(t *testing.T) {
	code, out, errStr := runSub(t, "env", "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errStr)
	}
	for _, want := range []string{"env [-h]", "--shell {sh,fish,pwsh}", "--unset", `eval "$(cswap env 2)"`} {
		if !strings.Contains(out, want) {
			t.Errorf("env --help missing %q\n%s", want, out)
		}
	}
	// translateSubcommand must leave a leading `env` untouched (it is pre-dispatched).
	if got := translateSubcommand([]string{"env", "2"}); got[0] != "env" {
		t.Errorf("translateSubcommand rewrote env: %v", got)
	}
}

// TestEnvInHelpList: the main --help command list and epilog document env.
func TestEnvInHelpList(t *testing.T) {
	var out bytes.Buffer
	renderMainHelp("cswap", &out)
	help := out.String()
	if !strings.Contains(help, "cswap env <num|email>") {
		t.Errorf("main help missing the env command line:\n%s", help)
	}
	if !strings.Contains(help, `eval "$(cswap env 2)"`) {
		t.Errorf("main help epilog missing the env example:\n%s", help)
	}
}

// TestEnvUnsetForms: --unset prints only the CLAUDE_CONFIG_DIR unset line for the
// chosen shell, on stdout, with nothing on stderr — and needs no switcher/home.
func TestEnvUnsetForms(t *testing.T) {
	cases := []struct {
		shell string
		want  string
	}{
		{"sh", "unset CLAUDE_CONFIG_DIR"},
		{"fish", "set -e CLAUDE_CONFIG_DIR"},
		{"pwsh", "Remove-Item Env:CLAUDE_CONFIG_DIR -ErrorAction SilentlyContinue"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			args := []string{"env", "--unset"}
			if tc.shell != "sh" {
				args = append(args, "--shell", tc.shell)
			}
			code, out, errStr := runSub(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errStr)
			}
			if strings.TrimSpace(out) != tc.want {
				t.Errorf("stdout = %q, want exactly %q", out, tc.want)
			}
			if errStr != "" {
				t.Errorf("stderr = %q, want empty (unset emits nothing else)", errStr)
			}
		})
	}
}

// TestEnvUsageErrors: the exit-2 usage errors, all firing before switcher
// construction (no home needed).
func TestEnvUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		msg  string
	}{
		{"unset with account", []string{"env", "--unset", "2"}, "--unset does not take a NUM|EMAIL|ALIAS argument"},
		{"invalid shell", []string{"env", "--shell", "zsh"}, "invalid choice: 'zsh'"},
		{"shell missing arg", []string{"env", "--shell"}, "expected one argument"},
		{"unknown flag", []string{"env", "--bogus"}, "unrecognized arguments: --bogus"},
		{"extra positional", []string{"env", "2", "3"}, "unrecognized arguments: 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errStr := runSub(t, tc.argv...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr=%q)", code, errStr)
			}
			if !strings.Contains(errStr, tc.msg) {
				t.Errorf("stderr = %q, want %q", errStr, tc.msg)
			}
			if strings.Contains(tc.name, "invalid shell") &&
				(!strings.Contains(errStr, "sh") || !strings.Contains(errStr, "fish") || !strings.Contains(errStr, "pwsh")) {
				t.Errorf("invalid-shell error must list the choices: %q", errStr)
			}
		})
	}
}

// TestEnvExportQuoting pins the per-shell export/unset spellings and quoting,
// including a profile dir containing a single quote.
func TestEnvExportQuoting(t *testing.T) {
	const tricky = `/home/o'brien/.local/share/claude-swap/sessions/2-me`
	cases := []struct {
		shell      string
		wantExport string
		wantUnset  string
	}{
		{"sh", `export CLAUDE_CONFIG_DIR='/home/o'\''brien/.local/share/claude-swap/sessions/2-me'`, "unset ANTHROPIC_API_KEY"},
		{"fish", `set -gx CLAUDE_CONFIG_DIR '/home/o\'brien/.local/share/claude-swap/sessions/2-me'`, "set -e ANTHROPIC_API_KEY"},
		{"pwsh", `$env:CLAUDE_CONFIG_DIR = '/home/o''brien/.local/share/claude-swap/sessions/2-me'`, "Remove-Item Env:ANTHROPIC_API_KEY -ErrorAction SilentlyContinue"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			if got := envExportLine(tc.shell, "CLAUDE_CONFIG_DIR", tricky); got != tc.wantExport {
				t.Errorf("export line = %q, want %q", got, tc.wantExport)
			}
			if got := envUnsetLine(tc.shell, "ANTHROPIC_API_KEY"); got != tc.wantUnset {
				t.Errorf("unset line = %q, want %q", got, tc.wantUnset)
			}
		})
	}
}

// TestEmitEnvExportStdoutPurity: emitEnvExport writes ONLY the eval-able lines
// (unset lines for each scrubbed var, then the export) to the stdout seam — the
// scrubbed-var order is preserved and nothing else appears.
func TestEmitEnvExportStdoutPurity(t *testing.T) {
	var out bytes.Buffer
	res := session.EnvResult{
		Dir:      "/profiles/2-me",
		Scrubbed: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	}
	emitEnvExport(&out, "sh", res)
	want := "unset ANTHROPIC_API_KEY\n" +
		"unset CLAUDE_CODE_OAUTH_TOKEN\n" +
		"export CLAUDE_CONFIG_DIR='/profiles/2-me'\n"
	if out.String() != want {
		t.Errorf("emitEnvExport stdout =\n%q\nwant\n%q", out.String(), want)
	}
}

// TestEnvNoticesLandOnStderr drives the full command with a faked preparer: the
// SessionManager seam writes its notice to the (stderr) sink it is handed, while
// the command's stdout carries only the unset+export eval lines. Proves the
// output-discipline routing end to end.
func TestEnvNoticesLandOnStderr(t *testing.T) {
	cleanHome(t)
	prev := newEnvPreparer
	newEnvPreparer = func(_ *core.Switcher, sink io.Writer) envPreparer {
		return &fakeEnvPreparer{sink: sink}
	}
	t.Cleanup(func() { newEnvPreparer = prev })

	code, out, errStr := runSub(t, "env", "2")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errStr)
	}
	wantOut := "unset ANTHROPIC_API_KEY\nexport CLAUDE_CONFIG_DIR='/prep/2-me'\n"
	if out != wantOut {
		t.Errorf("stdout =\n%q\nwant only eval lines\n%q", out, wantOut)
	}
	if !strings.Contains(errStr, "Prepared Account-2") {
		t.Errorf("preparer notice did not reach stderr sink: %q", errStr)
	}
	if strings.Contains(out, "Prepared") {
		t.Errorf("notice leaked onto stdout: %q", out)
	}
}

// fakeEnvPreparer records the identifier and writes a notice to its sink (the
// stderr writer env hands it), returning a canned result with one scrubbed var.
type fakeEnvPreparer struct {
	sink       io.Writer
	identifier string
}

func (f *fakeEnvPreparer) SetupEnv(identifier string, _, _ bool) (session.EnvResult, error) {
	f.identifier = identifier
	// A notice, exactly as the real Manager would emit to its Stdout sink.
	io.WriteString(f.sink, "Prepared Account-2 (user@example.com) [session mode]\n")
	return session.EnvResult{
		Dir:      "/prep/2-me",
		Scrubbed: []string{"ANTHROPIC_API_KEY"},
	}, nil
}
