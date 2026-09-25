package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Claude Code support.
//
// The documented interface to add an MCP server to Claude Code is its CLI
// (`claude mcp add`, `claude mcp add-json`). Claude Code
// keeps user and local scope servers in ~/.claude.json, a large file that the
// running Claude Code process also rewrites. dir2mcp therefore does not edit
// that file. When `claude` is on PATH, install and uninstall call the CLI.
// When it is not, install prints the exact command and exits non-zero.
//
// The entry is a native Streamable HTTP entry (type "http"), so Claude Code
// needs no stdio bridge such as mcp-remote. See claudeCodeEntry for how the
// token reaches Claude Code.

const (
	claudeCodeBinary       = "claude"
	claudeCodeDefaultScope = "user"
	claudeCodeCallTimeout  = 60 * time.Second
	// claudeCodeNotFoundMarker is the text `claude mcp remove` and
	// `claude mcp get` print when no server has the given name.
	claudeCodeNotFoundMarker = "No MCP server named"
)

// claudeCodeTarget holds what every claude-code verb resolves before it acts.
type claudeCodeTarget struct {
	name       string
	scope      string
	connection connectionPayload
	token      string
	tokenPath  string
}

// validateClaudeCodeScope accepts the scopes that keep the token private.
// The project scope writes .mcp.json into the working tree, which a user can
// commit by mistake, so dir2mcp refuses it.
func validateClaudeCodeScope(scope string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(scope))
	switch s {
	case "user", "local":
		return s, nil
	case "project":
		return "", fmt.Errorf("scope %q writes the bearer token into .mcp.json in the working tree; use --scope user or --scope local", s)
	default:
		return "", fmt.Errorf("unsupported scope %q; use user or local", scope)
	}
}

// parseClientFlags parses the flags of one client verb and rejects stray
// positional arguments. It writes the error and returns false on failure.
func (a *App) parseClientFlags(global globalOptions, fs *flag.FlagSet, label string, args []string) bool {
	fs.SetOutput(ioDiscard{})
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid %s flags: %v", label, err))
		return false
	}
	if len(fs.Args()) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("%s does not accept positional arguments: %s", label, strings.Join(fs.Args(), " ")))
		return false
	}
	return true
}

// resolveClientConnection loads the config, resolves the server name and the
// state directory, and reads connection.json plus the token. On failure it
// writes the error and returns a non-zero exit code.
func (a *App) resolveClientConnection(global globalOptions, nameOverride, stateDirOverride string) (name string, connection connectionPayload, token, tokenPath string, code int) {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return "", connection, "", "", exitConfigInvalid
	}
	name = resolveClaudeServerName(&cfg, nameOverride)
	stateDir := cfg.StateDir
	if t := strings.TrimSpace(stateDirOverride); t != "" {
		stateDir = t
	}
	connection, token, err = readConnectionAndToken(stateDir)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return "", connection, "", "", exitGeneric
	}
	return name, connection, token, connectionTokenPath(stateDir, connection), exitSuccess
}

// connectionTokenPath returns the absolute path of the token file that
// readConnectionAndToken reads.
func connectionTokenPath(stateDir string, connection connectionPayload) string {
	p := strings.TrimSpace(connection.TokenFile)
	if p == "" {
		p = filepath.Join(stateDir, secretTokenName)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// resolveClaudeCodeTarget parses the shared flags and resolves the target.
func (a *App) resolveClaudeCodeTarget(global globalOptions, label string, args []string, needConnection bool) (claudeCodeTarget, int) {
	fs := flag.NewFlagSet(label, flag.ContinueOnError)
	serverName := fs.String("name", "", "server name (defaults to configured/auto-derived name)")
	stateDir := fs.String("state-dir", "", "state directory containing connection.json")
	scope := fs.String("scope", claudeCodeDefaultScope, "Claude Code scope: user or local")
	if !a.parseClientFlags(global, fs, label, args) {
		return claudeCodeTarget{}, exitConfigInvalid
	}
	s, err := validateClaudeCodeScope(*scope)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return claudeCodeTarget{}, exitConfigInvalid
	}
	t := claudeCodeTarget{scope: s}
	if !needConnection {
		cfg, err := loadConfigWithGlobalOptions(global)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
			return claudeCodeTarget{}, exitConfigInvalid
		}
		t.name = resolveClaudeServerName(&cfg, *serverName)
		return t, exitSuccess
	}
	var code int
	t.name, t.connection, t.token, t.tokenPath, code = a.resolveClientConnection(global, *serverName, *stateDir)
	return t, code
}

func claudeCodeProtocolVersion(connection connectionPayload) string {
	if v := strings.TrimSpace(connection.Headers["MCP-Protocol-Version"]); v != "" {
		return v
	}
	return "2025-11-25"
}

// claudeCodeEntry is the server JSON that `claude mcp add-json` takes.
//
// The entry holds no token. HeadersHelper is a shell command that Claude Code
// runs at each connection; it reads the token file and prints the
// Authorization header as JSON. So the token is not in ~/.claude.json and not
// in the argv of any process, and a new token takes effect at the next
// connection without a reinstall.
type claudeCodeEntry struct {
	Type          string            `json:"type"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers,omitempty"`
	HeadersHelper string            `json:"headersHelper"`
}

// claudeCodeHeadersHelper returns the shell command that prints
// {"Authorization":"Bearer <token>"} from the token file. It deletes every
// whitespace character, as the server trims the token, and a valid bearer
// token holds none (see bearerTokenSyntax).
func claudeCodeHeadersHelper(tokenPath string) string {
	return `printf '{"Authorization":"Bearer %s"}' "$(tr -d ' \t\r\n' < ` + shellQuote(tokenPath) + `)"`
}

// bearerTokenSyntax is the RFC 6750 b64token grammar that a Bearer credential
// must match. The helper prints the token into a JSON string without escaping,
// and a b64token holds no character that JSON must escape.
var bearerTokenSyntax = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

// checkBearerToken rejects a token that is not a valid RFC 6750 bearer
// credential. The error never quotes the token.
func checkBearerToken(token string) error {
	if !bearerTokenSyntax.MatchString(strings.TrimSpace(token)) {
		return errors.New("the auth token is not a valid bearer token (RFC 6750: letters, digits and -._~+/ with optional trailing =); set a token of that form, or remove the token file to have one generated")
	}
	return nil
}

func claudeCodeEntryJSON(t claudeCodeTarget) (string, error) {
	if err := checkBearerToken(t.token); err != nil {
		return "", err
	}
	raw, err := json.Marshal(claudeCodeEntry{
		Type:          "http",
		URL:           t.connection.URL,
		Headers:       map[string]string{"MCP-Protocol-Version": claudeCodeProtocolVersion(t.connection)},
		HeadersHelper: claudeCodeHeadersHelper(t.tokenPath),
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// claudeCodeAddArgs returns the argv (without the binary) of the
// `claude mcp add-json` call.
func claudeCodeAddArgs(t claudeCodeTarget, entryJSON string) []string {
	return []string{"mcp", "add-json", "--scope", t.scope, t.name, entryJSON}
}

// claudeCodeShellCommand renders the `claude mcp add-json` command for a
// shell. Like the entry, it holds no token.
func claudeCodeShellCommand(t claudeCodeTarget, entryJSON string) string {
	parts := []string{claudeCodeBinary}
	for _, arg := range claudeCodeAddArgs(t, entryJSON) {
		parts = append(parts, shellQuoteIfNeeded(arg))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runClaudeCodeCLI runs the claude binary and returns its combined output.
// Callers must not print the output of `claude mcp get`: it shows the static
// headers of a server in clear text, and an older entry can hold a token.
func runClaudeCodeCLI(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeCodeCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// removeClaudeCodeServer removes the named server from one scope. It returns
// removed=false and no error when the server is not present.
func removeClaudeCodeServer(ctx context.Context, bin, name, scope string) (bool, error) {
	out, err := runClaudeCodeCLI(ctx, bin, "mcp", "remove", name, "--scope", scope)
	if err == nil {
		return true, nil
	}
	if strings.Contains(out, claudeCodeNotFoundMarker) {
		return false, nil
	}
	return false, fmt.Errorf("claude mcp remove failed: %v: %s", err, out)
}

func (a *App) runClaudeCodePrintConfig(global globalOptions, args []string) int {
	t, code := a.resolveClaudeCodeTarget(global, "print-config claude-code", args, true)
	if code != exitSuccess {
		return code
	}
	entryJSON, err := claudeCodeEntryJSON(t)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("prepare Claude Code MCP entry: %v", err))
		return exitGeneric
	}
	command := claudeCodeShellCommand(t, entryJSON)
	if global.jsonOutput {
		if err := emitJSON(a.stdout, map[string]interface{}{
			"server_name": t.name,
			"scope":       t.scope,
			"url":         t.connection.URL,
			"token_file":  t.tokenPath,
			"entry":       json.RawMessage(entryJSON),
			"command":     command,
		}); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode claude-code print-config json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}
	writeln(a.stdout, command)
	return exitSuccess
}

func (a *App) runClaudeCodeInstall(ctx context.Context, global globalOptions, args []string) int {
	t, code := a.resolveClaudeCodeTarget(global, "install claude-code", args, true)
	if code != exitSuccess {
		return code
	}
	entryJSON, err := claudeCodeEntryJSON(t)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("prepare Claude Code MCP entry: %v", err))
		return exitGeneric
	}
	bin, err := exec.LookPath(claudeCodeBinary)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			"could not find the claude CLI in PATH",
			"Install Claude Code, or run this command yourself:",
			claudeCodeShellCommand(t, entryJSON),
		)
		return exitGeneric
	}
	// `claude mcp add-json` refuses a name that exists. Remove our entry first
	// so a second install replaces it (for example after the port changes).
	replaced, err := removeClaudeCodeServer(ctx, bin, t.name, t.scope)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	if out, err := runClaudeCodeCLI(ctx, bin, claudeCodeAddArgs(t, entryJSON)...); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			fmt.Sprintf("claude mcp add-json failed: %v: %s", err, out),
			"Fix the cause, then run `dir2mcp install claude-code` again. The server is not registered now.")
		return exitGeneric
	}
	if global.jsonOutput {
		return a.emitClientJSON("claude-code install", map[string]interface{}{
			"server_name": t.name,
			"scope":       t.scope,
			"command":     bin,
			"url":         t.connection.URL,
			"replaced":    replaced,
			"updated":     true,
		})
	}
	writef(a.stdout, "added MCP server %q to Claude Code (%s scope)\n", t.name, t.scope)
	writef(a.stdout, "start a new Claude Code session, then run /mcp to see the server\n")
	return exitSuccess
}

func (a *App) runClaudeCodeUninstall(ctx context.Context, global globalOptions, args []string) int {
	t, code := a.resolveClaudeCodeTarget(global, "uninstall claude-code", args, false)
	if code != exitSuccess {
		return code
	}
	bin, err := exec.LookPath(claudeCodeBinary)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			"could not find the claude CLI in PATH",
			"Run this command yourself: "+strings.Join([]string{claudeCodeBinary, "mcp", "remove", shellQuoteIfNeeded(t.name), "--scope", t.scope}, " "),
		)
		return exitGeneric
	}
	removed, err := removeClaudeCodeServer(ctx, bin, t.name, t.scope)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
		return exitGeneric
	}
	if global.jsonOutput {
		payload := map[string]interface{}{
			"server_name": t.name,
			"scope":       t.scope,
			"removed":     removed,
		}
		if !removed {
			payload["reason"] = "entry_not_present"
		}
		return a.emitClientJSON("claude-code uninstall", payload)
	}
	if !removed {
		writef(a.stdout, "no MCP server %q in Claude Code (%s scope); nothing to remove\n", t.name, t.scope)
		return exitSuccess
	}
	writef(a.stdout, "removed MCP server %q from Claude Code (%s scope)\n", t.name, t.scope)
	return exitSuccess
}

func (a *App) runClaudeCodeDoctor(ctx context.Context, global globalOptions, args []string) int {
	t, code := a.resolveClaudeCodeTarget(global, "doctor claude-code", args, true)
	if code != exitSuccess {
		return code
	}
	bin, cliErr := exec.LookPath(claudeCodeBinary)
	registeredErr := errors.New("claude CLI not found")
	if cliErr == nil {
		registeredErr = claudeCodeRegistered(ctx, bin, t.name, t.connection.URL)
	}
	urlErr := validateConnectionURL(t.connection.URL)
	reachErr := checkEndpointReachable(ctx, t.connection.URL)
	tokenErr := error(nil)
	if strings.TrimSpace(t.token) == "" {
		tokenErr = fmt.Errorf("token file is empty")
	} else if err := checkBearerToken(t.token); err != nil {
		tokenErr = err
	}
	ok := cliErr == nil && registeredErr == nil && urlErr == nil && reachErr == nil && tokenErr == nil
	if global.jsonOutput {
		if rc := a.emitClientJSON("claude-code doctor", map[string]interface{}{
			"ok":               ok,
			"server_name":      t.name,
			"claude_command":   bin,
			"claude_error":     errString(cliErr),
			"registered_error": errString(registeredErr),
			"url":              t.connection.URL,
			"url_error":        errString(urlErr),
			"endpoint_error":   errString(reachErr),
			"token_file_error": errString(tokenErr),
		}); rc != exitSuccess {
			return rc
		}
		return doctorExit(ok)
	}
	writef(a.stdout, "claude command: %s\n", valueOrNA(bin))
	writef(a.stdout, "connection url: %s\n", t.connection.URL)
	writef(a.stdout, "claude CLI check: %s\n", checkStatus(cliErr))
	writef(a.stdout, "server %q registered: %s\n", t.name, checkStatus(registeredErr))
	writef(a.stdout, "url check: %s\n", checkStatus(urlErr))
	writef(a.stdout, "endpoint reachability: %s\n", checkStatus(reachErr))
	writef(a.stdout, "token file check: %s\n", checkStatus(tokenErr))
	return doctorExit(ok)
}

// claudeCodeRegistered asks `claude mcp get` whether the server exists and
// points at wantURL. The command output shows the headers in clear text, so
// this function reads only the "URL:" line and never returns or prints the
// output.
func claudeCodeRegistered(ctx context.Context, bin, name, wantURL string) error {
	out, err := runClaudeCodeCLI(ctx, bin, "mcp", "get", name)
	if err == nil {
		if got, found := claudeCodeGetURL(out); found && got != wantURL {
			return fmt.Errorf("registered url %q does not match the daemon url; run: dir2mcp install claude-code", got)
		}
		return nil
	}
	if strings.Contains(out, claudeCodeNotFoundMarker) {
		return fmt.Errorf("not registered; run: dir2mcp install claude-code")
	}
	return fmt.Errorf("claude mcp get failed: %v", err)
}

// claudeCodeGetURL returns the value of the "URL:" line that
// `claude mcp get` prints for an HTTP server.
func claudeCodeGetURL(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "URL:"); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func (a *App) emitClientJSON(label string, payload map[string]interface{}) int {
	if err := emitJSON(a.stdout, payload); err != nil {
		writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode %s json: %v", label, err))
		return exitGeneric
	}
	return exitSuccess
}

func doctorExit(ok bool) int {
	if ok {
		return exitSuccess
	}
	return exitGeneric
}
