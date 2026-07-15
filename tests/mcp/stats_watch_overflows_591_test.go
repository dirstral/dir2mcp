package tests

import (
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// TestMCPStats_WatchOverflowsSurfacedWhenWatching asserts the optional additive
// watch_overflows field (spec 0.33.0 / stats.json, dir2mcp #591) is present in
// dir2mcp_stats.indexing — and carries the lifetime overflow count — when a
// filesystem watcher is running. The served outputSchema declares indexing with
// additionalProperties:false, so surfacing this field is only spec-legal because
// the schema now allows it (PR #590 had to strip it before the spec caught up).
func TestMCPStats_WatchOverflowsSurfacedWhenWatching(t *testing.T) {
	idx := appstate.NewIndexingState(appstate.ModeIncremental)
	idx.MarkWatchActive()
	idx.AddWatchOverflow(2)

	sc := statsIndexingWithState(t, idx)
	got, present := sc["watch_overflows"]
	if !present {
		t.Fatalf("watch_overflows must be present while watching: indexing=%#v", sc)
	}
	// JSON numbers decode to float64.
	if n, ok := got.(float64); !ok || n != 2 {
		t.Fatalf("watch_overflows want 2, got %#v", got)
	}
}

// TestMCPStats_WatchOverflowsOmittedWhenNotWatching asserts the field is omitted
// (not reported as 0) when no watcher is running — a one-shot index or the
// store-derived fallback. Absence means "not applicable", not "watching, none
// dropped" (spec: consumers MUST treat absence as unknown/NA, not zero).
func TestMCPStats_WatchOverflowsOmittedWhenNotWatching(t *testing.T) {
	idx := appstate.NewIndexingState(appstate.ModeIncremental)
	// No MarkWatchActive / AddWatchOverflow: watcher inactive.

	sc := statsIndexingWithState(t, idx)
	if _, present := sc["watch_overflows"]; present {
		t.Fatalf("watch_overflows must be omitted when not watching: indexing=%#v", sc)
	}
}

// statsIndexingWithState spins up an MCP server whose indexing snapshot is the
// given state, calls dir2mcp_stats, and returns the structured `indexing` map.
func statsIndexingWithState(t *testing.T, idx *appstate.IndexingState) map[string]interface{} {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithIndexingState(idx)).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	indexing, ok := sc["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("indexing object missing: %#v", sc)
	}
	return indexing
}
