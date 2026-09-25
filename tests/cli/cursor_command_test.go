package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSONObject(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v raw=%s", path, err, raw)
	}
	return out
}

// writeCursorConfig seeds a Cursor mcp.json that holds an unrelated server
// and an unknown top-level key, so the tests can prove install keeps both.
func writeCursorConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initial := `{"futureKey":{"keep":true},"mcpServers":{"some-other-tool":{"command":"node","args":["/opt/other/server.js"]}}}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("write cursor config: %v", err)
	}
	return path
}

func cursorServers(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	servers, ok := readJSONObject(t, path)["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcpServers missing in %s", path)
	}
	return servers
}

func TestCursorInstallWritesHTTPEntryAndPreservesOthers(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cursor")
	configPath := writeCursorConfig(t, tmp)

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "install", "cursor", "--name", "notes", "--config-path", configPath)
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	assertNoToken(t, "tok-cursor", stdout, stderr)

	root := readJSONObject(t, configPath)
	if _, ok := root["futureKey"].(map[string]interface{}); !ok {
		t.Fatalf("unknown top-level key must survive: %v", root)
	}
	servers := cursorServers(t, configPath)
	if _, ok := servers["some-other-tool"]; !ok {
		t.Fatalf("unrelated server must survive: %v", servers)
	}
	entry, _ := servers["notes"].(map[string]interface{})
	if entry["url"] != "http://127.0.0.1:9882/mcp" {
		t.Fatalf("entry url = %v, want the daemon url", entry["url"])
	}
	if _, hasCommand := entry["command"]; hasCommand {
		t.Fatalf("entry must be a remote url entry, not stdio: %v", entry)
	}
	headers, _ := entry["headers"].(map[string]interface{})
	if headers["Authorization"] != "Bearer tok-cursor" || headers["MCP-Protocol-Version"] != "2025-11-25" {
		t.Fatalf("unexpected headers: %v", headers)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perms = %o, want 0600 (it holds a bearer token)", perm)
	}
}

func TestCursorReinstallIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cursor-again")
	configPath := writeCursorConfig(t, tmp)

	var snapshots [][]byte
	for i, wantReplaced := range []bool{false, true} {
		code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "--json", "install", "cursor", "--name", "notes", "--config-path", configPath)
		if code != 0 {
			t.Fatalf("install #%d exit code = %d stderr=%s", i+1, code, stderr)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("decode #%d: %v raw=%s", i+1, err, stdout)
		}
		if payload["replaced"] != wantReplaced {
			t.Fatalf("install #%d replaced = %v, want %v", i+1, payload["replaced"], wantReplaced)
		}
		raw, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		snapshots = append(snapshots, raw)
	}
	if string(snapshots[0]) != string(snapshots[1]) {
		t.Fatalf("second install changed the file:\n%s\n---\n%s", snapshots[0], snapshots[1])
	}
	if n := len(cursorServers(t, configPath)); n != 2 {
		t.Fatalf("want 2 servers (ours + unrelated), got %d", n)
	}
}

func TestCursorInstallRefusesNonObjectMCPServers(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cursor-bad")
	configPath := filepath.Join(tmp, "mcp.json")
	initial := []byte(`{"mcpServers":["not","an","object"]}`)
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "cursor", "--config-path", configPath)
	if code == 0 {
		t.Fatalf("expected failure for a non-object mcpServers")
	}
	if !strings.Contains(stderr, "mcpServers must be an object") {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != string(initial) {
		t.Fatalf("file must stay untouched, got %s", raw)
	}
}

func TestCursorUninstallRemovesOnlyOurEntry(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cursor-rm")
	configPath := writeCursorConfig(t, tmp)
	if code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "cursor", "--name", "notes", "--config-path", configPath); code != 0 {
		t.Fatalf("install exit code = %d stderr=%s", code, stderr)
	}

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "uninstall", "cursor", "--name", "notes", "--config-path", configPath)
	if code != 0 {
		t.Fatalf("uninstall exit code = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `removed MCP server "notes"`) {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	servers := cursorServers(t, configPath)
	if _, present := servers["notes"]; present {
		t.Fatalf("our entry should be gone: %v", servers)
	}
	if _, ok := servers["some-other-tool"]; !ok {
		t.Fatalf("unrelated server must survive: %v", servers)
	}
	if _, ok := readJSONObject(t, configPath)["futureKey"]; !ok {
		t.Fatalf("unknown top-level key must survive uninstall")
	}
}

func TestCursorUninstallDropsEmptyBlockAndIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "mcp.json")
	if err := os.WriteFile(configPath, []byte(`{"mcpServers":{"notes":{"url":"http://x/mcp"}}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stateDir := filepath.Join(tmp, "state")
	if code, _, stderr := runCLI(t, "--state-dir", stateDir, "uninstall", "cursor", "--name", "notes", "--config-path", configPath); code != 0 {
		t.Fatalf("uninstall exit code = %d stderr=%s", code, stderr)
	}
	if _, present := readJSONObject(t, configPath)["mcpServers"]; present {
		t.Fatalf("empty mcpServers should be dropped")
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "uninstall", "cursor", "--name", "notes", "--config-path", configPath)
	if code != 0 || !strings.Contains(stdout, "nothing to remove") {
		t.Fatalf("second uninstall: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("no-op uninstall must not rewrite the file")
	}
}

func cursorDoctor(t *testing.T, stateDir, configPath, token string) map[string]interface{} {
	t.Helper()
	_, stdout, stderr := runCLI(t, "--state-dir", stateDir, "--json", "doctor", "cursor", "--name", "notes", "--config-path", configPath)
	assertNoToken(t, token, stdout, stderr)
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode doctor json: %v raw=%s", err, stdout)
	}
	return payload
}

func TestCursorDoctorReportsEntryState(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-cursor-doc")
	configPath := filepath.Join(tmp, "mcp.json")

	missing := cursorDoctor(t, stateDir, configPath, "tok-cursor-doc")
	if got, _ := missing["entry_error"].(string); !strings.Contains(got, "not installed") || missing["ok"] != false {
		t.Fatalf("doctor before install: %v", missing)
	}

	if code, _, stderr := runCLI(t, "--state-dir", stateDir, "install", "cursor", "--name", "notes", "--config-path", configPath); code != 0 {
		t.Fatalf("install exit code = %d stderr=%s", code, stderr)
	}
	installed := cursorDoctor(t, stateDir, configPath, "tok-cursor-doc")
	if installed["entry_error"] != "" || installed["token_file_error"] != "" || installed["url_error"] != "" {
		t.Fatalf("doctor after install: %v", installed)
	}

	// A new token makes the installed entry stale.
	if err := os.WriteFile(tokenPath, []byte("tok-cursor-rotated\n"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	stale := cursorDoctor(t, stateDir, configPath, "tok-cursor-rotated")
	got, _ := stale["entry_error"].(string)
	if !strings.Contains(got, "does not match the current token") {
		t.Fatalf("doctor after rotation: %v", stale)
	}
	assertNoToken(t, "tok-cursor-doc", got)
}

func TestCursorPrintConfigUsesEnvPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-cursor-print")

	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "print-config", "cursor", "--name", "notes")
	if code != 0 {
		t.Fatalf("exit code = %d stderr=%s", code, stderr)
	}
	assertNoToken(t, "tok-cursor-print", stdout, stderr)
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout must be pure JSON: %v raw=%s", err, stdout)
	}
	servers, _ := payload["mcpServers"].(map[string]interface{})
	entry, _ := servers["notes"].(map[string]interface{})
	headers, _ := entry["headers"].(map[string]interface{})
	if headers["Authorization"] != "Bearer ${env:DIR2MCP_TOKEN}" {
		t.Fatalf("Authorization = %v, want the env placeholder", headers["Authorization"])
	}
	if !strings.Contains(stderr, "export DIR2MCP_TOKEN=\"$(cat '"+tokenPath+"')\"") {
		t.Fatalf("stderr should show how to set the variable: %q", stderr)
	}
}

func TestCursorDoctorResolvesEnvPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-cursor-env")
	configPath := filepath.Join(tmp, "mcp.json")
	// Write the print-config snippet as a user would paste it.
	code, stdout, stderr := runCLI(t, "--state-dir", stateDir, "print-config", "cursor", "--name", "notes")
	if code != 0 {
		t.Fatalf("print-config exit code = %d stderr=%s", code, stderr)
	}
	if err := os.WriteFile(configPath, []byte(stdout), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := []struct {
		name, value, wantErr string
		set                  bool
	}{
		{name: "unset", wantErr: "DIR2MCP_TOKEN is not set here"},
		{name: "wrong", value: "tok-other", set: true, wantErr: "$DIR2MCP_TOKEN does not match the current token"},
		{name: "match", value: "tok-cursor-env", set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("DIR2MCP_TOKEN", tc.value)
			} else {
				t.Setenv("DIR2MCP_TOKEN", "")
				_ = os.Unsetenv("DIR2MCP_TOKEN")
			}
			payload := cursorDoctor(t, stateDir, configPath, "tok-cursor-env")
			got, _ := payload["entry_error"].(string)
			if tc.wantErr == "" && got != "" {
				t.Fatalf("entry_error = %q, want none", got)
			}
			if tc.wantErr != "" && !strings.Contains(got, tc.wantErr) {
				t.Fatalf("entry_error = %q, want %q", got, tc.wantErr)
			}
		})
	}
}
