package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cursor support.
//
// Cursor reads MCP servers from ~/.cursor/mcp.json (global) and from
// .cursor/mcp.json (project). Cursor has no CLI to add a server, so the
// documented interface is the file. A remote server entry has a "url" and
// optional "headers"; Cursor speaks Streamable HTTP to it directly, so no
// stdio bridge is necessary.

// cursorTokenEnvVar is the variable that print-config puts in the
// Authorization header, so that the printed snippet never holds the token.
const cursorTokenEnvVar = "DIR2MCP_TOKEN"

type cursorServerConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func buildCursorEntry(connection connectionPayload, authValue string) cursorServerConfig {
	return cursorServerConfig{
		URL: connection.URL,
		Headers: map[string]string{
			"Authorization":        authValue,
			"MCP-Protocol-Version": claudeCodeProtocolVersion(connection),
		},
	}
}

func defaultCursorConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".cursor", "mcp.json")
	}
	return filepath.Join(home, ".cursor", "mcp.json")
}

// loadCursorServers reads the config file and returns the root object and
// its mcpServers object. It refuses a file where mcpServers is not an
// object, so that dir2mcp never replaces entries it does not understand.
func loadCursorServers(path string) (root, servers map[string]interface{}, err error) {
	root, err = loadJSONFileOrEmpty(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read Cursor config: %w", err)
	}
	servers = map[string]interface{}{}
	if raw, ok := root["mcpServers"]; ok && raw != nil {
		typed, isObject := raw.(map[string]interface{})
		if !isObject {
			return nil, nil, fmt.Errorf("invalid Cursor config: mcpServers must be an object in %s", path)
		}
		servers = typed
	}
	return root, servers, nil
}

func (a *App) runCursorPrintConfig(global globalOptions, args []string) int {
	fs := flag.NewFlagSet("print-config cursor", flag.ContinueOnError)
	serverName := fs.String("name", "", "server name (defaults to configured/auto-derived name)")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	if !a.parseClientFlags(global, fs, "print-config cursor", args) {
		return exitConfigInvalid
	}
	name, connection, _, tokenPath, code := a.resolveClientConnection(global, *serverName, *stateDir)
	if code != exitSuccess {
		return code
	}
	entry := buildCursorEntry(connection, "Bearer ${env:"+cursorTokenEnvVar+"}")
	payload := map[string]interface{}{
		"mcpServers": map[string]interface{}{name: entry},
	}
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	if !global.jsonOutput {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(payload); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("encode cursor print-config json: %v", err))
		return exitGeneric
	}
	if !global.jsonOutput && !global.quiet {
		writef(a.stderr, "Cursor reads the token from $%s. Set it before you start Cursor:\n", cursorTokenEnvVar)
		writef(a.stderr, "  export %s=\"$(cat %s)\"\n", cursorTokenEnvVar, shellQuote(tokenPath))
		writef(a.stderr, "Or run `dir2mcp install cursor` to write the token into the config file.\n")
	}
	return exitSuccess
}

func (a *App) runCursorInstall(global globalOptions, args []string) int {
	fs := flag.NewFlagSet("install cursor", flag.ContinueOnError)
	serverName := fs.String("name", "", "server name (defaults to configured/auto-derived name)")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	configPath := fs.String("config-path", defaultCursorConfigPath(), "Cursor mcp.json path")
	if !a.parseClientFlags(global, fs, "install cursor", args) {
		return exitConfigInvalid
	}
	name, connection, token, _, code := a.resolveClientConnection(global, *serverName, *stateDir)
	if code != exitSuccess {
		return code
	}
	root, servers, err := loadCursorServers(*configPath)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	entryRaw, err := jsonMarshalMap(buildCursorEntry(connection, "Bearer "+token))
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("prepare Cursor MCP entry: %v", err))
		return exitGeneric
	}
	_, replaced := servers[name]
	servers[name] = entryRaw
	root["mcpServers"] = servers
	// writeJSONFile writes atomically with 0600, because the entry holds the token.
	if err := writeJSONFile(*configPath, root); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write Cursor config: %v", err))
		return exitGeneric
	}
	if global.jsonOutput {
		return a.emitClientJSON("cursor install", map[string]interface{}{
			"path":        *configPath,
			"server_name": name,
			"url":         connection.URL,
			"replaced":    replaced,
			"updated":     true,
		})
	}
	writef(a.stdout, "updated %s with MCP server %q\n", *configPath, name)
	writef(a.stdout, "open Cursor Settings > MCP to see the server; restart Cursor if it does not show\n")
	return exitSuccess
}

func (a *App) runCursorUninstall(global globalOptions, args []string) int {
	fs := flag.NewFlagSet("uninstall cursor", flag.ContinueOnError)
	serverName := fs.String("name", "", "server name to remove (defaults to configured/auto-derived name)")
	configPath := fs.String("config-path", defaultCursorConfigPath(), "Cursor mcp.json path")
	if !a.parseClientFlags(global, fs, "uninstall cursor", args) {
		return exitConfigInvalid
	}
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	name := resolveClaudeServerName(&cfg, *serverName)
	removed, err := removeCursorServer(*configPath, name)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	if global.jsonOutput {
		payload := map[string]interface{}{"path": *configPath, "server_name": name, "removed": removed}
		if !removed {
			payload["reason"] = "entry_not_present"
		}
		return a.emitClientJSON("cursor uninstall", payload)
	}
	if !removed {
		writef(a.stdout, "no MCP server %q in %s; nothing to remove\n", name, *configPath)
		return exitSuccess
	}
	writef(a.stdout, "removed MCP server %q from %s\n", name, *configPath)
	return exitSuccess
}

// removeCursorServer deletes only the named entry. It does not write the file
// when the entry is absent, and it keeps all other keys and servers.
func removeCursorServer(path, name string) (bool, error) {
	root, servers, err := loadCursorServers(path)
	if err != nil {
		return false, err
	}
	if _, present := servers[name]; !present {
		return false, nil
	}
	delete(servers, name)
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}
	if err := writeJSONFile(path, root); err != nil {
		return false, fmt.Errorf("write Cursor config: %w", err)
	}
	return true, nil
}

func (a *App) runCursorDoctor(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("doctor cursor", flag.ContinueOnError)
	serverName := fs.String("name", "", "server name (defaults to configured/auto-derived name)")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	configPath := fs.String("config-path", defaultCursorConfigPath(), "Cursor mcp.json path")
	if !a.parseClientFlags(global, fs, "doctor cursor", args) {
		return exitConfigInvalid
	}
	name, connection, token, _, code := a.resolveClientConnection(global, *serverName, *stateDir)
	if code != exitSuccess {
		return code
	}
	entryErr := checkCursorEntry(*configPath, name, connection.URL, token)
	urlErr := validateConnectionURL(connection.URL)
	reachErr := checkEndpointReachable(ctx, connection.URL)
	tokenErr := error(nil)
	if strings.TrimSpace(token) == "" {
		tokenErr = fmt.Errorf("token file is empty")
	}
	ok := entryErr == nil && urlErr == nil && reachErr == nil && tokenErr == nil
	if global.jsonOutput {
		if rc := a.emitClientJSON("cursor doctor", map[string]interface{}{
			"ok":               ok,
			"path":             *configPath,
			"server_name":      name,
			"entry_error":      errString(entryErr),
			"url":              connection.URL,
			"url_error":        errString(urlErr),
			"endpoint_error":   errString(reachErr),
			"token_file_error": errString(tokenErr),
		}); rc != exitSuccess {
			return rc
		}
		return doctorExit(ok)
	}
	writef(a.stdout, "cursor config: %s\n", *configPath)
	writef(a.stdout, "connection url: %s\n", connection.URL)
	writef(a.stdout, "server %q entry: %s\n", name, checkStatus(entryErr))
	writef(a.stdout, "url check: %s\n", checkStatus(urlErr))
	writef(a.stdout, "endpoint reachability: %s\n", checkStatus(reachErr))
	writef(a.stdout, "token file check: %s\n", checkStatus(tokenErr))
	return doctorExit(ok)
}

// checkCursorEntry verifies that the config holds our entry with the current
// URL and token. Error texts never include the token value.
func checkCursorEntry(path, name, wantURL, token string) error {
	_, servers, err := loadCursorServers(path)
	if err != nil {
		return err
	}
	raw, present := servers[name]
	if !present {
		return errors.New("not installed; run: dir2mcp install cursor")
	}
	entry, _ := raw.(map[string]interface{})
	if got, _ := entry["url"].(string); got != wantURL {
		return fmt.Errorf("url %q does not match the daemon url; run: dir2mcp install cursor", got)
	}
	headers, _ := entry["headers"].(map[string]interface{})
	auth, _ := headers["Authorization"].(string)
	if auth != "Bearer "+token && !strings.Contains(auth, "${env:") {
		return errors.New("authorization header does not match the current token; run: dir2mcp install cursor")
	}
	return nil
}
