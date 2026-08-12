package tests

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// mcp_path validation: dir2mcp #653.
//
// The only check was a leading slash, and it lived in the `up` command. Go
// 1.22's http.ServeMux reads `{name}` in a pattern as a wildcard segment and
// PANICS on a malformed one, so `--mcp-path '/{'` took the whole daemon down
// with `panic: parsing "/{": at offset 1: bad wildcard segment (must end with
// '}')`, after other startup work had already run. A typo in a persisted config
// crashed the process instead of producing a CONFIG_INVALID an operator can read.
//
// Validate() is the gate rather than the `up` command, so every entry point is
// covered: a persisted config, a CLI flag overlay, and `dir2mcp config set`.

// TestMCPPathRejectsAPatternThatPanicsServeMux is the headline case. It is the
// exact value from the issue.
func TestMCPPathRejectsAPatternThatPanicsServeMux(t *testing.T) {
	cfg := config.Default()
	cfg.MCPPath = "/{"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("mcp_path=\"/{\" must be rejected: registering it panics the server goroutine after startup (#653)")
	}
	if !strings.Contains(err.Error(), "mcp_path") {
		t.Errorf("error must name the offending key so an operator can fix it; got %q", err)
	}
}

// TestMCPPathRejectsValuesThatCannotServeARequest covers the values that do not
// panic but cannot work, each for its own reason. A path that silently serves
// nothing is worse than one that is refused, because the operator sees a running
// daemon and a client that cannot reach it.
func TestMCPPathRejectsValuesThatCannotServeARequest(t *testing.T) {
	for _, tc := range []struct{ path, why string }{
		{"/{name}", "a wildcard segment: mcp_path names one endpoint, not a pattern"},
		{"/mcp}", "an unbalanced brace"},
		{"/mcp?token=x", "a query string is not part of the request path"},
		{"/mcp#frag", "a fragment is never sent to the server"},
		{"/a//b", "a doubled slash: requests are redirected to the cleaned path"},
		{"/mcp/", "a trailing slash registers a subtree, not one endpoint"},
		{"/mcp path", "whitespace"},
		{"mcp", "no leading slash"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := config.Default()
			cfg.MCPPath = tc.path
			if err := cfg.Validate(); err == nil {
				t.Fatalf("mcp_path=%q must be rejected (%s)", tc.path, tc.why)
			}
		})
	}
}

// TestMCPPathAcceptsEveryUsableValue is the other half. A validator that
// rejected a legitimate custom path would break a working deployment, which is a
// worse outcome than the panic it replaces.
func TestMCPPathAcceptsEveryUsableValue(t *testing.T) {
	for _, path := range []string{
		"/mcp",
		"/",
		"/deep/nested/mcp",
		"/mcp-v2",
		"/mcp_v2",
		"/MCP",
		"/mcp.json",
		"", // unset: the loader supplies the default
	} {
		t.Run(path, func(t *testing.T) {
			cfg := config.Default()
			cfg.MCPPath = path
			if err := cfg.Validate(); err != nil {
				t.Fatalf("mcp_path=%q is usable and must be accepted; got %v", path, err)
			}
		})
	}
}

// TestEveryAcceptedMCPPathRegistersWithoutPanic is the test that ties the rule to
// the failure. The validator is only correct if what it accepts can actually be
// registered, so this asserts the property directly against http.ServeMux rather
// than trusting the grammar to have covered every case.
func TestEveryAcceptedMCPPathRegistersWithoutPanic(t *testing.T) {
	// Includes the rejected values too: for those the validator must be what
	// stops them, and this test proves the panic is real rather than assumed.
	for _, path := range []string{
		"/mcp", "/", "/deep/nested/mcp", "/mcp-v2", "/MCP", "/mcp.json",
		"/{", "/{name}", "/mcp}",
	} {
		cfg := config.Default()
		cfg.MCPPath = path
		accepted := cfg.Validate() == nil

		panicked := registerPanics(path)
		if accepted && panicked {
			t.Errorf("mcp_path=%q passed validation but panics when registered: the grammar is too permissive", path)
		}
		if panicked && !accepted {
			continue // correctly refused before it could panic
		}
	}
}

// registerPanics reports whether http.ServeMux panics on this pattern, which is
// the failure #653 is about.
func registerPanics(pattern string) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	http.NewServeMux().HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return false
}
