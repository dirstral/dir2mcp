package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// supportedClients enumerates the MCP clients dir2mcp can configure
// today. As more clients land, add them here and route the new entry
// through each verb's switch — the top-level surface (`install`,
// `uninstall`, `doctor`, `print-config`) stays flat.
var supportedClients = []string{"claude"}

// extractClient reads the leading positional argument as the client
// name and returns it (lowercased, trimmed) along with the remaining
// args. When the leading positional is missing, an actionable usage
// error is written and ok is false; the caller should return
// exitConfigInvalid in that case.
func extractClient(a *App, jsonOutput bool, verb string, args []string) (client string, rest []string, ok bool) {
	if len(args) == 0 {
		writeCLIError(a.stderr, jsonOutput, exitConfigInvalid,
			fmt.Sprintf("dir2mcp %s requires a client name", verb),
			"Supported clients: "+strings.Join(supportedClientsSorted(), ", "),
			fmt.Sprintf("Example: dir2mcp %s claude", verb),
		)
		return "", nil, false
	}
	return strings.ToLower(strings.TrimSpace(args[0])), args[1:], true
}

func supportedClientsSorted() []string {
	out := append([]string(nil), supportedClients...)
	sort.Strings(out)
	return out
}

// unknownClientError writes the standard "we don't know that client"
// message and returns exitConfigInvalid. Centralized so every verb
// dispatcher emits the same hint.
func unknownClientError(a *App, jsonOutput bool, verb, client string) int {
	writeCLIError(a.stderr, jsonOutput, exitConfigInvalid,
		fmt.Sprintf("unknown client %q for %s", client, verb),
		"Supported clients: "+strings.Join(supportedClientsSorted(), ", "),
	)
	return exitConfigInvalid
}

// runInstall installs the dir2mcp MCP server entry into the requested
// client's configuration. Today the only supported client is claude
// (Claude Desktop); future clients (chatgpt, cursor, ...) plug into
// the same switch.
func (a *App) runInstall(_ context.Context, global globalOptions, args []string) int {
	client, rest, ok := extractClient(a, global.jsonOutput, "install", args)
	if !ok {
		return exitConfigInvalid
	}
	switch client {
	case "claude":
		return a.runClaudeInstall(global, rest)
	default:
		return unknownClientError(a, global.jsonOutput, "install", client)
	}
}

// runUninstall removes the dir2mcp MCP server entry from the
// requested client's configuration. Idempotent — uninstalling a
// not-installed client is a clean no-op.
func (a *App) runUninstall(_ context.Context, global globalOptions, args []string) int {
	client, rest, ok := extractClient(a, global.jsonOutput, "uninstall", args)
	if !ok {
		return exitConfigInvalid
	}
	if !a.confirmDestructive(global, fmt.Sprintf("Remove dir2mcp from %s?", client), "Deletes the dir2mcp MCP server entry from the client configuration.") {
		writeln(a.stderr, "uninstall aborted")
		return exitSuccess
	}
	switch client {
	case "claude":
		return a.runClaudeUninstall(global, rest)
	default:
		return unknownClientError(a, global.jsonOutput, "uninstall", client)
	}
}

// runDoctor dispatches to either the daemon-side preflight (no
// positional client argument) or a client-specific integration check
// (`dir2mcp doctor claude`). The two paths are intentionally separate
// commands sharing one CLI verb: the daemon flavour answers "is this
// install healthy?", the client flavour answers "is this client wired
// up correctly?".
func (a *App) runDoctor(ctx context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		return a.runServerDoctor(ctx, global, args)
	}
	client, rest, ok := extractClient(a, global.jsonOutput, "doctor", args)
	if !ok {
		return exitConfigInvalid
	}
	switch client {
	case "claude":
		return a.runClaudeDoctor(ctx, global, rest)
	default:
		return unknownClientError(a, global.jsonOutput, "doctor", client)
	}
}

// runPrintConfig emits the JSON snippet the requested client expects
// in its MCP-server configuration block. Useful for users who want to
// merge into a config they manage manually instead of running install.
func (a *App) runPrintConfig(_ context.Context, global globalOptions, args []string) int {
	client, rest, ok := extractClient(a, global.jsonOutput, "print-config", args)
	if !ok {
		return exitConfigInvalid
	}
	switch client {
	case "claude":
		return a.runClaudePrintConfig(global, rest)
	default:
		return unknownClientError(a, global.jsonOutput, "print-config", client)
	}
}
