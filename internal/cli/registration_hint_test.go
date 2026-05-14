package cli

import (
	"strings"
	"testing"
)

func TestBuildRegistrationCommand(t *testing.T) {
	cases := []struct {
		name            string
		serverName      string
		url             string
		protocolVersion string
		requiresAuth    bool
		wantLines       []string
	}{
		{
			name:            "no headers no backslash",
			serverName:      "dir2mcp-foo-abc123",
			url:             "http://127.0.0.1:1234/mcp",
			protocolVersion: "",
			requiresAuth:    false,
			wantLines: []string{
				"claude mcp add --transport http dir2mcp-foo-abc123 http://127.0.0.1:1234/mcp",
			},
		},
		{
			name:            "protocol header only",
			serverName:      "dir2mcp-foo-abc123",
			url:             "http://127.0.0.1:1234/mcp",
			protocolVersion: "2025-11-25",
			requiresAuth:    false,
			wantLines: []string{
				`claude mcp add --transport http dir2mcp-foo-abc123 http://127.0.0.1:1234/mcp \`,
				`  --header "MCP-Protocol-Version: 2025-11-25"`,
			},
		},
		{
			name:            "auth header trails",
			serverName:      "dir2mcp-foo-abc123",
			url:             "http://127.0.0.1:1234/mcp",
			protocolVersion: "2025-11-25",
			requiresAuth:    true,
			wantLines: []string{
				`claude mcp add --transport http dir2mcp-foo-abc123 http://127.0.0.1:1234/mcp \`,
				`  --header "MCP-Protocol-Version: 2025-11-25" \`,
				`  --header "Authorization: Bearer <token>"`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRegistrationCommand(tc.serverName, tc.url, tc.protocolVersion, tc.requiresAuth)
			if len(got) != len(tc.wantLines) {
				t.Fatalf("got %d lines (%v), want %d (%v)", len(got), got, len(tc.wantLines), tc.wantLines)
			}
			for i := range got {
				if got[i] != tc.wantLines[i] {
					t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], tc.wantLines[i])
				}
			}
			// Final line must never end with a continuation backslash.
			last := got[len(got)-1]
			if strings.HasSuffix(last, "\\") {
				t.Fatalf("final line ends with backslash: %q", last)
			}
		})
	}
}

func TestBuildRegistrationCommand_QuotesUnsafeNames(t *testing.T) {
	cases := []struct {
		name       string
		serverName string
		url        string
		want       string
	}{
		{
			name:       "name with space",
			serverName: "my alias",
			url:        "http://127.0.0.1:1234/mcp",
			want:       "claude mcp add --transport http 'my alias' http://127.0.0.1:1234/mcp",
		},
		{
			name:       "name with single quote",
			serverName: "joe's-alias",
			url:        "http://127.0.0.1:1234/mcp",
			want:       `claude mcp add --transport http 'joe'\''s-alias' http://127.0.0.1:1234/mcp`,
		},
		{
			name:       "auto-derived names stay bare",
			serverName: "dir2mcp-foo-abc123",
			url:        "http://127.0.0.1:1234/mcp",
			want:       "claude mcp add --transport http dir2mcp-foo-abc123 http://127.0.0.1:1234/mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRegistrationCommand(tc.serverName, tc.url, "", false)
			if len(got) != 1 {
				t.Fatalf("unexpected line count %d (%v)", len(got), got)
			}
			if got[0] != tc.want {
				t.Fatalf("\n got  %q\n want %q", got[0], tc.want)
			}
		})
	}
}

func TestPrintRegistrationHintSkipsWhenIncomplete(t *testing.T) {
	var buf strings.Builder
	s := newStyles(&buf, true)
	printRegistrationHint(&buf, s, "", "http://x/mcp", "v", false)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty name, got %q", buf.String())
	}
	printRegistrationHint(&buf, s, "name", "  ", "v", false)
	if buf.Len() != 0 {
		t.Fatalf("expected no output for empty url, got %q", buf.String())
	}
}
