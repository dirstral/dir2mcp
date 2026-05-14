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
// backslash; the final line never does.
func buildRegistrationCommand(name, url, protocolVersion string, requiresAuth bool) []string {
	lines := []string{
		fmt.Sprintf("claude mcp add --transport http %s %s \\", name, url),
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
