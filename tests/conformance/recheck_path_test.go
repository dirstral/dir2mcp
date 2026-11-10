package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// stubRechecker answers every request with one canned result. It stands in for
// the file watcher so the production transport can be driven without one.
type stubRechecker struct{}

func (stubRechecker) RecheckPath(_ context.Context, relPath string, _ bool) (ingest.RecheckResult, error) {
	return ingest.RecheckResult{RelPath: relPath, Kind: ingest.RecheckKindFile, Queued: true, Pending: 1}, nil
}

func listedToolNames(t *testing.T, mcpURL, sid string) map[string]bool {
	t.Helper()
	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d body=%s", resp.StatusCode, body)
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("tools/list: decode: %v body=%s", err, body)
	}
	names := make(map[string]bool, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

// TestProduction_RecheckPathListedOnlyWhenServed pins the §15.13 availability
// rule on the production transport: the optional tool is absent from
// tools/list without a rechecker and present with one.
func TestProduction_RecheckPathListedOnlyWhenServed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()

	without := newServer(t, cfg)
	defer without.Close()
	withoutURL := without.URL + cfg.MCPPath
	if listedToolNames(t, withoutURL, initSession(t, withoutURL))[protocol.ToolNameRecheckPath] {
		t.Fatalf("%s is listed by a server that does not serve it", protocol.ToolNameRecheckPath)
	}

	with := newServer(t, cfg, mcp.WithPathRechecker(stubRechecker{}))
	defer with.Close()
	withURL := with.URL + cfg.MCPPath
	if !listedToolNames(t, withURL, initSession(t, withURL))[protocol.ToolNameRecheckPath] {
		t.Fatalf("%s is not listed by a server that serves it", protocol.ToolNameRecheckPath)
	}
}

// TestProduction_RecheckPathUnavailableIsToolError: on the production
// transport, a call that names the unserved tool is a TOOL_UNAVAILABLE tool
// result (§14.3), not the -32602 protocol error an unknown name gets.
func TestProduction_RecheckPathUnavailableIsToolError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath

	const call = `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"a.txt"}}}`
	if got := toolErrorCode(t, mcpURL, call); got != protocol.ErrorCodeToolUnavailable {
		t.Fatalf("canonical code=%q want=%s", got, protocol.ErrorCodeToolUnavailable)
	}
}

// TestProduction_RecheckPathCallSucceedsWhenServed drives a successful call
// through the production transport, so the registered SDK tool and its
// structured result are exercised end to end.
func TestProduction_RecheckPathCallSucceedsWhenServed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg, mcp.WithPathRechecker(stubRechecker{}))
	defer srv.Close()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)

	resp := sendRPC(t, mcpURL, sid,
		`{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"docs/a.md","recursive":true}}}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if isToolError(body) {
		t.Fatalf("expected a successful result, got isError body=%s", body)
	}
	var envelope struct {
		Result struct {
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	sc := envelope.Result.StructuredContent
	if sc["rel_path"] != "docs/a.md" || sc["kind"] != "file" || sc["queued"] != true || sc["pending"] != float64(1) {
		t.Fatalf("unexpected structuredContent: %#v", sc)
	}
	if len(sc) != 4 {
		t.Fatalf("structuredContent carries %d fields, want exactly 4: %#v", len(sc), sc)
	}
}
