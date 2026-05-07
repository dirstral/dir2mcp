package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dir2mcp/internal/cli"
)

func TestClaudePrintConfigEmitsMCPServerBlock(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	tokenPath := filepath.Join(stateDir, "secret.token")
	if err := os.WriteFile(tokenPath, []byte("tok123\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	connection := map[string]interface{}{
		"transport":    "mcp_streamable_http",
		"url":          "http://127.0.0.1:9882/mcp",
		"headers":      map[string]string{"MCP-Protocol-Version": "2025-11-25"},
		"session":      map[string]interface{}{"uses_mcp_session_id": true, "header_name": "MCP-Session-Id", "assigned_on_initialize": true},
		"public":       false,
		"token_source": "secret.token",
		"token_file":   tokenPath,
	}
	raw, _ := json.Marshal(connection)
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), raw, 0o644); err != nil {
		t.Fatalf("write connection: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{
		"--state-dir", stateDir,
		"--json",
		"claude", "print-config",
		"--name", "stas-legal-dir2mcp",
	})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v raw=%s", err, stdout.String())
	}
	mcpServers, _ := payload["mcpServers"].(map[string]interface{})
	entryRaw, ok := mcpServers["stas-legal-dir2mcp"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing server entry: %+v", payload)
	}
	command, ok := entryRaw["command"].(string)
	if !ok || command == "" {
		t.Fatalf("missing command: %+v", entryRaw)
	}
}

func TestClaudeInstallUpdatesConfigFile(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	tokenPath := filepath.Join(stateDir, "secret.token")
	if err := os.WriteFile(tokenPath, []byte("tok456\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	connection := map[string]interface{}{
		"transport":    "mcp_streamable_http",
		"url":          "http://127.0.0.1:9882/mcp",
		"headers":      map[string]string{"MCP-Protocol-Version": "2025-11-25"},
		"session":      map[string]interface{}{"uses_mcp_session_id": true, "header_name": "MCP-Session-Id", "assigned_on_initialize": true},
		"public":       false,
		"token_source": "secret.token",
		"token_file":   tokenPath,
	}
	raw, _ := json.Marshal(connection)
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), raw, 0o644); err != nil {
		t.Fatalf("write connection: %v", err)
	}

	configPath := filepath.Join(tmp, "claude_desktop_config.json")
	initial := []byte("{\"preferences\":{\"coworkWebSearchEnabled\":true}}\n")
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{
		"--state-dir", stateDir,
		"claude", "install",
		"--name", "stas-legal-dir2mcp",
		"--config-path", configPath,
	})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	updatedRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatalf("unmarshal updated config: %v raw=%s", err, string(updatedRaw))
	}
	if _, ok := updated["preferences"].(map[string]interface{}); !ok {
		t.Fatalf("preferences missing after install: %+v", updated)
	}
	mcpServers, ok := updated["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcpServers missing after install: %+v", updated)
	}
	if _, ok := mcpServers["stas-legal-dir2mcp"]; !ok {
		t.Fatalf("named server not written: %+v", mcpServers)
	}
}

func TestClaudeDoctorPassesWithValidState(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-doctor")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{
		"--state-dir", stateDir,
		"claude", "doctor",
	})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); !containsAll(got, "bridge check: ok", "url check: ok", "token file check: ok") {
		t.Fatalf("unexpected doctor output: %s", got)
	}
}

func TestClaudeDoctorFailsMissingToken(t *testing.T) {
	tmp := t.TempDir()
	stateDir, tokenPath := writeClaudeStateFixture(t, tmp, "tok-missing")
	if err := os.Remove(tokenPath); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{
		"--state-dir", stateDir,
		"claude", "doctor",
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit code, got stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "read token file") {
		t.Fatalf("expected missing token diagnostic, got: %s", got)
	}
}

func TestClaudeInstallFailsMissingBridge(t *testing.T) {
	tmp := t.TempDir()
	stateDir, _ := writeClaudeStateFixture(t, tmp, "tok-install")
	configPath := filepath.Join(tmp, "claude_desktop_config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", "")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{
		"--state-dir", stateDir,
		"claude", "install",
		"--config-path", configPath,
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit code with missing bridge, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "could not find bunx or npx in PATH") {
		t.Fatalf("expected missing bridge error, got: %s", got)
	}
}

func writeClaudeStateFixture(t *testing.T, root, token string) (stateDir string, tokenPath string) {
	t.Helper()
	stateDir = filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	tokenPath = filepath.Join(stateDir, "secret.token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	connection := map[string]interface{}{
		"transport":    "mcp_streamable_http",
		"url":          "http://127.0.0.1:9882/mcp",
		"headers":      map[string]string{"MCP-Protocol-Version": "2025-11-25"},
		"session":      map[string]interface{}{"uses_mcp_session_id": true, "header_name": "MCP-Session-Id", "assigned_on_initialize": true},
		"public":       false,
		"token_source": "secret.token",
		"token_file":   tokenPath,
	}
	raw, err := json.Marshal(connection)
	if err != nil {
		t.Fatalf("marshal connection fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), raw, 0o644); err != nil {
		t.Fatalf("write connection fixture: %v", err)
	}
	return stateDir, tokenPath
}

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
