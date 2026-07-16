package tests

import (
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// TestMCPStats_FormatVersion asserts the dir2mcp_stats output carries the df-000
// cross-version signal (#468): a semver format_version, declared in the served
// additionalProperties:false outputSchema and equal to protocol.FormatVersion.
// It is the payload-shape version, independent of the pinned protocol_version.
func TestMCPStats_FormatVersion(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	fv, ok := sc["format_version"].(string)
	if !ok || fv == "" {
		t.Fatalf("stats output missing format_version: %#v", sc["format_version"])
	}
	if fv != protocol.FormatVersion {
		t.Fatalf("format_version = %q, want %q", fv, protocol.FormatVersion)
	}
}
