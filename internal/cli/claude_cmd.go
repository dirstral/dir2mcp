package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type claudeServerConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func (a *App) runClaude(ctx context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		writeln(a.stdout, "claude command: supported subcommands are print-config, install, and doctor")
		return exitSuccess
	}
	switch args[0] {
	case "print-config":
		return a.runClaudePrintConfig(global, args[1:])
	case "install":
		return a.runClaudeInstall(global, args[1:])
	case "doctor":
		return a.runClaudeDoctor(ctx, global, args[1:])
	default:
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("unknown claude subcommand: %s", args[0]))
		return exitConfigInvalid
	}
}

func (a *App) runClaudePrintConfig(global globalOptions, args []string) int {
	fs := flag.NewFlagSet("claude print-config", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	serverName := fs.String("name", "dir2mcp", "server name")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid claude print-config flags: %v", err))
		return exitConfigInvalid
	}
	if len(fs.Args()) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("claude print-config does not accept positional arguments: %s", strings.Join(fs.Args(), " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	effectiveStateDir := cfg.StateDir
	if strings.TrimSpace(*stateDir) != "" {
		effectiveStateDir = strings.TrimSpace(*stateDir)
	}

	connection, token, err := readConnectionAndToken(effectiveStateDir)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	entry := buildClaudeMCPRemoteEntry(connection.URL, token)

	payload := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			*serverName: entry,
		},
	}
	if global.jsonOutput {
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode claude print-config json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}

	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		writeCLIError(a.stderr, false, exitGeneric, fmt.Sprintf("encode claude config: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

func (a *App) runClaudeInstall(global globalOptions, args []string) int {
	fs := flag.NewFlagSet("claude install", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	serverName := fs.String("name", "dir2mcp", "server name")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	configPath := fs.String("config-path", defaultClaudeDesktopConfigPath(), "Claude Desktop config path")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid claude install flags: %v", err))
		return exitConfigInvalid
	}
	if len(fs.Args()) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("claude install does not accept positional arguments: %s", strings.Join(fs.Args(), " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	effectiveStateDir := cfg.StateDir
	if strings.TrimSpace(*stateDir) != "" {
		effectiveStateDir = strings.TrimSpace(*stateDir)
	}

	connection, token, err := readConnectionAndToken(effectiveStateDir)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}

	commandPath, err := resolveClaudeBridgeCommand()
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error(), "Install bun (recommended) or npm/npx and retry.")
		return exitGeneric
	}

	entry := buildClaudeMCPRemoteEntry(connection.URL, token)
	entry.Command = commandPath

	root, err := loadJSONFileOrEmpty(*configPath)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read Claude config: %v", err))
		return exitGeneric
	}

	mcpServers, ok := root["mcpServers"].(map[string]interface{})
	if !ok || mcpServers == nil {
		mcpServers = map[string]interface{}{}
	}
	entryRaw, err := jsonMarshalMap(entry)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("prepare Claude MCP entry: %v", err))
		return exitGeneric
	}
	mcpServers[*serverName] = entryRaw
	root["mcpServers"] = mcpServers

	if err := os.MkdirAll(filepath.Dir(*configPath), 0o755); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("create Claude config dir: %v", err))
		return exitGeneric
	}
	if err := writeJSONFile(*configPath, root); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write Claude config: %v", err))
		return exitGeneric
	}

	if global.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"path":        *configPath,
			"server_name": *serverName,
			"command":     commandPath,
			"updated":     true,
		})
		return exitSuccess
	}
	writef(a.stdout, "updated %s with MCP server %q\n", *configPath, *serverName)
	writef(a.stdout, "restart Claude Desktop to load the updated MCP server entry\n")
	return exitSuccess
}

func (a *App) runClaudeDoctor(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("claude doctor", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid claude doctor flags: %v", err))
		return exitConfigInvalid
	}
	if len(fs.Args()) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("claude doctor does not accept positional arguments: %s", strings.Join(fs.Args(), " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	effectiveStateDir := cfg.StateDir
	if strings.TrimSpace(*stateDir) != "" {
		effectiveStateDir = strings.TrimSpace(*stateDir)
	}

	connection, token, err := readConnectionAndToken(effectiveStateDir)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	bridgeCommand, bridgeErr := resolveClaudeBridgeCommand()
	urlErr := validateConnectionURL(connection.URL)
	reachErr := checkEndpointReachable(ctx, connection.URL)
	tokenErr := error(nil)
	if strings.TrimSpace(token) == "" {
		tokenErr = fmt.Errorf("token file is empty")
	}

	ok := bridgeErr == nil && urlErr == nil && reachErr == nil && tokenErr == nil
	if global.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"ok":               ok,
			"bridge_command":   bridgeCommand,
			"bridge_error":     errString(bridgeErr),
			"url":              connection.URL,
			"url_error":        errString(urlErr),
			"endpoint_error":   errString(reachErr),
			"token_file_error": errString(tokenErr),
		})
		if ok {
			return exitSuccess
		}
		return exitGeneric
	}

	writef(a.stdout, "bridge command: %s\n", valueOrNA(bridgeCommand))
	writef(a.stdout, "connection url: %s\n", connection.URL)
	writef(a.stdout, "bridge check: %s\n", checkStatus(bridgeErr))
	writef(a.stdout, "url check: %s\n", checkStatus(urlErr))
	writef(a.stdout, "endpoint reachability: %s\n", checkStatus(reachErr))
	writef(a.stdout, "token file check: %s\n", checkStatus(tokenErr))
	if ok {
		return exitSuccess
	}
	return exitGeneric
}

func buildClaudeMCPRemoteEntry(mcpURL, token string) claudeServerConfig {
	return claudeServerConfig{
		Command: "bunx",
		Args: []string{
			"mcp-remote",
			mcpURL,
			"--header",
			"MCP-Protocol-Version:2025-11-25",
			"--header",
			"Authorization:Bearer " + token,
		},
	}
}

func defaultClaudeDesktopConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "claude_desktop_config.json"
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
}

func readConnectionAndToken(stateDir string) (connectionPayload, string, error) {
	var payload connectionPayload
	connectionPath := filepath.Join(stateDir, connectionFileName)
	raw, err := os.ReadFile(connectionPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return payload, "", fmt.Errorf("read %s: not found; run: dir2mcp up", connectionPath)
		}
		return payload, "", fmt.Errorf("read %s: %w", connectionPath, err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, "", fmt.Errorf("decode %s: %w", connectionPath, err)
	}
	tokenPath := strings.TrimSpace(payload.TokenFile)
	if tokenPath == "" {
		tokenPath = filepath.Join(stateDir, secretTokenName)
	}
	tokenRaw, err := os.ReadFile(tokenPath)
	if err != nil {
		return payload, "", fmt.Errorf("read token file %s: %w", tokenPath, err)
	}
	return payload, strings.TrimSpace(string(tokenRaw)), nil
}

func resolveClaudeBridgeCommand() (string, error) {
	if p, err := exec.LookPath("bunx"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("npx"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("could not find bunx or npx in PATH")
}

func loadJSONFileOrEmpty(path string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeJSONFile(path string, payload map[string]interface{}) error {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func jsonMarshalMap(v interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateConnectionURL(raw string) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Host) == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

func checkEndpointReachable(ctx context.Context, raw string) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		if strings.EqualFold(u.Scheme, "https") {
			hostPort += ":443"
		} else {
			hostPort += ":80"
		}
	}
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func checkStatus(err error) string {
	if err == nil {
		return "ok"
	}
	return "error: " + err.Error()
}

func valueOrNA(v string) string {
	if strings.TrimSpace(v) == "" {
		return "n/a"
	}
	return v
}
