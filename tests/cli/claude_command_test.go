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
		"print-config", "claude",
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
		"install", "claude",
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
		"--json",
		"doctor", "claude",
	})
	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal doctor output: %v raw=%s", err, stdout.String())
	}
	if payload["bridge_error"] != "" {
		t.Fatalf("expected bridge_error empty, got=%v payload=%v", payload["bridge_error"], payload)
	}
	if payload["url_error"] != "" {
		t.Fatalf("expected url_error empty, got=%v payload=%v", payload["url_error"], payload)
	}
	if payload["token_file_error"] != "" {
		t.Fatalf("expected token_file_error empty, got=%v payload=%v", payload["token_file_error"], payload)
	}
	endpointErr, _ := payload["endpoint_error"].(string)
	switch code {
	case 0:
		if endpointErr != "" {
			t.Fatalf("expected empty endpoint_error on success, got=%q payload=%v", endpointErr, payload)
		}
	case 1:
		if endpointErr == "" {
			t.Fatalf("expected endpoint_error on failure, payload=%v", payload)
		}
	default:
		t.Fatalf("unexpected exit code: %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
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
		"doctor", "claude",
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
		"install", "claude",
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

func TestClaudeUninstallRemovesEntryAndPreservesUnrelatedKeys(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "claude_desktop_config.json")
	initial := map[string]interface{}{
		"preferences": map[string]interface{}{"coworkWebSearchEnabled": true},
		"mcpServers": map[string]interface{}{
			"dir2mcp": map[string]interface{}{
				"command": "bunx",
				"args":    []string{"mcp-remote", "http://127.0.0.1:9882/mcp"},
			},
			"some-other-tool": map[string]interface{}{
				"command": "node",
				"args":    []string{"/opt/other/server.js"},
			},
		},
	}
	raw, _ := json.Marshal(initial)
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{
		"uninstall", "claude",
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
		t.Errorf("preferences must survive uninstall: %+v", updated)
	}
	mcpServers, ok := updated["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcpServers should still exist (other entries remain): %+v", updated)
	}
	if _, removed := mcpServers["dir2mcp"]; removed {
		t.Errorf("dir2mcp entry should have been removed: %+v", mcpServers)
	}
	if _, kept := mcpServers["some-other-tool"]; !kept {
		t.Errorf("unrelated MCP server entry should survive: %+v", mcpServers)
	}
}

func TestClaudeUninstallDropsEmptyMCPServersBlock(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "claude_desktop_config.json")
	initial := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"dir2mcp": map[string]interface{}{"command": "bunx"},
		},
	}
	raw, _ := json.Marshal(initial)
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{
		"uninstall", "claude",
		"--config-path", configPath,
	})
	if code != 0 {
		t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
	}

	var updated map[string]interface{}
	updatedRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatalf("unmarshal updated config: %v raw=%s", err, string(updatedRaw))
	}
	if _, present := updated["mcpServers"]; present {
		t.Errorf("mcpServers should be dropped when uninstall empties it: %+v", updated)
	}
}

func TestClaudeUninstallIsIdempotentWhenEntryAbsent(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "claude_desktop_config.json")
	if err := os.WriteFile(configPath, []byte("{\"preferences\":{\"x\":1}}\n"), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{
		"uninstall", "claude",
		"--config-path", configPath,
	})
	if code != 0 {
		t.Fatalf("unexpected exit code on no-op uninstall: %d stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "nothing to remove") {
		t.Errorf("expected informational stdout about nothing to remove, got: %q", got)
	}
}
