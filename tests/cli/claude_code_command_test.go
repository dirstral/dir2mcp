package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// fakeClaudeScript is a stand-in for the `claude` CLI. It keeps one file per
// registered server in $FAKE_CLAUDE_STATE and appends every argv to
// $FAKE_CLAUDE_STATE/calls.log. Its messages copy the text of the real CLI
// (claude 2.1.x) for the cases dir2mcp parses. `mcp get` prints the headers
// in clear text, as the real CLI does, so the tests can prove that dir2mcp
// never passes that output through.
const fakeClaudeScript = `#!/bin/sh
state="$FAKE_CLAUDE_STATE"
printf '%s\n' "$*" >> "$state/calls.log"
case "$2" in
add)
  name="$7"
  if [ -f "$state/server-$name" ]; then
    echo "MCP server $name already exists in $6 config"
    exit 1
  fi
  printf '%s\n' "$*" > "$state/server-$name"
  echo "Added HTTP MCP server $name"
  ;;
remove)
  name="$3"
  if [ ! -f "$state/server-$name" ]; then
    echo "No MCP server named \"$name\" in $5 scope"
    exit 1
  fi
  rm -f "$state/server-$name"
  echo "Removed MCP server $name"
  ;;
get)
  name="$3"
  if [ ! -f "$state/server-$name" ]; then
    echo "No MCP server named \"$name\". Configured servers: none"
    exit 1
  fi
  echo "$name:"
  echo "  Type: http"
  set -- $(cat "$state/server-$name")
  echo "  URL: $8"
  echo "  Headers: $*"
  ;;
esac
exit 0
`

// installFakeClaude puts the fake CLI first on PATH and returns its state dir.
// PATH keeps only the system dirs the script needs, so a real claude binary
// on the developer machine is never called.
func installFakeClaude(t *testing.T, tmp string) string {
	t.Helper()
	binDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "fake-claude-state")
	for _, d := range []string{binDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	t.Setenv("FAKE_CLAUDE_STATE", stateDir)
	return stateDir
}

func fakeClaudeCalls(t *testing.T, stateDir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "calls.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read calls.log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), args)
	return code, stdout.String(), stderr.String()
}

func assertNoToken(t *testing.T, token string, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if strings.Contains(out, token) {
			t.Fatalf("output leaks the token value: %q", out)
		}
	}
}

func TestClaudeCodeInstallCallsClaudeMCPAddWithHTTPEntry(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cc-install")
	fake := installFakeClaude(t, tmp)

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "install", "claude-code", "--name", "notes-dir2mcp")
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	assertNoToken(t, "tok-cc-install", stdout, stderr)

	calls := fakeClaudeCalls(t, fake)
	if len(calls) != 2 {
		t.Fatalf("want remove + add calls, got %q", calls)
	}
	if calls[0] != "mcp remove notes-dir2mcp --scope user" {
		t.Fatalf("first call = %q, want the pre-add remove", calls[0])
	}
	want := "mcp add --transport http --scope user notes-dir2mcp http://127.0.0.1:9882/mcp " +
		"--header Authorization: Bearer tok-cc-install --header MCP-Protocol-Version: 2025-11-25"
	if calls[1] != want {
		t.Fatalf("add call:\n got %q\nwant %q", calls[1], want)
	}
	if !strings.Contains(stdout, `added MCP server "notes-dir2mcp" to Claude Code (user scope)`) {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

func TestClaudeCodeReinstallIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cc-again")
	fake := installFakeClaude(t, tmp)

	for i, wantReplaced := range []bool{false, true} {
		code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "--json", "install", "claude-code", "--name", "notes", "--scope", "local")
		if code != 0 {
			t.Fatalf("install #%d exit code = %d stderr=%s", i+1, code, stderr)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("decode install #%d json: %v raw=%s", i+1, err, stdout)
		}
		if payload["replaced"] != wantReplaced || payload["scope"] != "local" || payload["updated"] != true {
			t.Fatalf("install #%d payload = %v, want replaced=%v scope=local", i+1, payload, wantReplaced)
		}
	}
	entries, err := filepath.Glob(filepath.Join(fake, "server-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one registered server after two installs, got %v", entries)
	}
}

func TestClaudeCodeInstallRejectsProjectScope(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cc-project")
	fake := installFakeClaude(t, tmp)

	code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "claude-code", "--scope", "project")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, ".mcp.json") {
		t.Fatalf("stderr should explain the project scope risk: %q", stderr)
	}
	if calls := fakeClaudeCalls(t, fake); len(calls) != 0 {
		t.Fatalf("claude must not run for a rejected scope, got %q", calls)
	}
}

func TestClaudeCodeInstallWithoutClaudePrintsCommand(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-cc-nocli")
	t.Setenv("PATH", filepath.Join(tmp, "empty-bin"))

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "install", "claude-code", "--name", "notes")
	if code == 0 {
		t.Fatalf("expected non-zero exit without claude on PATH, stdout=%s", stdout)
	}
	assertNoToken(t, "tok-cc-nocli", stdout, stderr)
	for _, want := range []string{
		"could not find the claude CLI in PATH",
		"claude mcp add --transport http --scope user notes http://127.0.0.1:9882/mcp",
		`"Authorization: Bearer $(cat '` + tokenPath + `')"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

func TestClaudeCodeUninstallRemovesOnlyOurServer(t *testing.T) {
	tmp := t.TempDir()
	fake := installFakeClaude(t, tmp)
	for _, name := range []string{"notes", "some-other-tool"} {
		if err := os.WriteFile(filepath.Join(fake, "server-"+name), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	code, stdout, stderr := runCLI(t, "uninstall", "claude-code", "--name", "notes")
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `removed MCP server "notes" from Claude Code (user scope)`) {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(fake, "server-notes")); !os.IsNotExist(err) {
		t.Fatalf("our server should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(fake, "server-some-other-tool")); err != nil {
		t.Fatalf("unrelated server must survive: %v", err)
	}
}

func TestClaudeCodeUninstallIsIdempotentWhenAbsent(t *testing.T) {
	tmp := t.TempDir()
	installFakeClaude(t, tmp)

	code, stdout, stderr := runCLI(t, "--json", "uninstall", "claude-code", "--name", "notes")
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode json: %v raw=%s", err, stdout)
	}
	if payload["removed"] != false || payload["reason"] != "entry_not_present" {
		t.Fatalf("unexpected payload: %v", payload)
	}
}

func TestClaudeCodeDoctorReportsRegistration(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cc-doctor")
	installFakeClaude(t, tmp)

	doctor := func() map[string]interface{} {
		_, stdout, stderr := runCLI(t, "--state-dir", stateDir, "--json", "doctor", "claude-code", "--name", "notes")
		assertNoToken(t, "tok-cc-doctor", stdout, stderr)
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("decode doctor json: %v raw=%s", err, stdout)
		}
		return payload
	}

	before := doctor()
	if got, _ := before["registered_error"].(string); !strings.Contains(got, "not registered") {
		t.Fatalf("registered_error before install = %q, want not registered", got)
	}
	if before["ok"] != false || before["claude_error"] != "" || before["token_file_error"] != "" {
		t.Fatalf("unexpected doctor payload before install: %v", before)
	}

	if code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "claude-code", "--name", "notes"); code != 0 {
		t.Fatalf("install exit code = %d stderr=%s", code, stderr)
	}
	// The fake `mcp get` prints the token; doctor must hide that output.
	after := doctor()
	if after["registered_error"] != "" {
		t.Fatalf("registered_error after install = %v", after["registered_error"])
	}
}

func TestClaudeCodePrintConfigNeverPrintsToken(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-cc-print")

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "print-config", "claude-code", "--name", "notes")
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	assertNoToken(t, "tok-cc-print", stdout, stderr)
	want := `claude mcp add --transport http --scope user notes http://127.0.0.1:9882/mcp ` +
		`--header "Authorization: Bearer $(cat '` + tokenPath + `')" --header 'MCP-Protocol-Version: 2025-11-25'`
	if strings.TrimSpace(stdout) != want {
		t.Fatalf("print-config:\n got %q\nwant %q", strings.TrimSpace(stdout), want)
	}
}

func TestClaudeCodeDoctorFlagsStaleURL(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-cc-stale")
	installFakeClaude(t, tmp)
	if code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "claude-code", "--name", "notes"); code != 0 {
		t.Fatalf("install exit code = %d stderr=%s", code, stderr)
	}

	// The daemon comes back on another port: the registered entry is stale.
	connection := map[string]interface{}{
		"transport":  "mcp_streamable_http",
		"url":        "http://127.0.0.1:9883/mcp",
		"headers":    map[string]string{"MCP-Protocol-Version": "2025-11-25"},
		"token_file": tokenPath,
	}
	raw, err := json.Marshal(connection)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), raw, 0o644); err != nil {
		t.Fatalf("rewrite connection: %v", err)
	}

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "doctor", "claude-code", "--name", "notes")
	if code == 0 {
		t.Fatalf("doctor must fail for a stale url, stdout=%s", stdout)
	}
	assertNoToken(t, "tok-cc-stale", stdout, stderr)
	if !strings.Contains(stdout, `registered url "http://127.0.0.1:9882/mcp" does not match the daemon url`) {
		t.Fatalf("unexpected doctor output: %s", stdout)
	}
}
