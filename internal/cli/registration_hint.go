package cli

import (
	"fmt"
	"io"
	"strings"
)

// printRegistrationHint renders the "Register with Claude" banner section
// that prompts the user to add this dir2mcp instance to their MCP client
// under a unique alias. Auto-derived names (dir2mcp-<folder>-<hash>)
// disambiguate multiple corpora in the same client list.
//
// requiresAuth controls whether the suggested command includes an
// Authorization header line (only printed when the server is not
// running in auth=none mode).
func printRegistrationHint(w io.Writer, s styles, name, url, protocolVersion string, requiresAuth bool) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(url) == "" {
		return
	}
	writef(w, "  %s\n", s.sectionHeader("Register with Claude"))
	cmd := buildRegistrationCommand(name, url, protocolVersion, requiresAuth)
	for _, line := range cmd {
		writef(w, "    %s\n", line)
	}
	writeln(w)
}

// buildRegistrationCommand returns the suggested `claude mcp add`
// invocation, split per line so the banner can indent each
// continuation. Lines that the user can paste verbatim use a trailing
// backslash; the final line never does. Server name and URL are
// shell-quoted when they contain anything outside the bare-safe charset
// so user-supplied overrides (`server.name: "my alias"`) still produce
// a paste-safe command.
func buildRegistrationCommand(name, url, protocolVersion string, requiresAuth bool) []string {
	lines := []string{
		fmt.Sprintf("claude mcp add --transport http %s %s \\", shellQuoteIfNeeded(name), shellQuoteIfNeeded(url)),
	}
	headers := []string{}
	if v := strings.TrimSpace(protocolVersion); v != "" {
		headers = append(headers, fmt.Sprintf(`--header "MCP-Protocol-Version: %s"`, v))
	}
	if requiresAuth {
		headers = append(headers, `--header "Authorization: Bearer <token>"`)
	}
	for i, h := range headers {
		if i == len(headers)-1 {
			lines = append(lines, "  "+h)
		} else {
			lines = append(lines, "  "+h+" \\")
		}
	}
	if len(headers) == 0 {
		// Drop the trailing backslash on the only line.
		lines[0] = strings.TrimSuffix(lines[0], " \\")
	}
	return lines
}

// shellQuoteIfNeeded returns s wrapped in POSIX single quotes if it
// contains any character outside the bare-safe charset; otherwise it
// returns s unchanged. Embedded single quotes are escaped using the
// standard POSIX close/escape/reopen idiom so the result is paste-safe
// regardless of how pathological the override is.
func shellQuoteIfNeeded(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/', r == ':', r == '@', r == '+', r == '%', r == ',', r == '=':
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
