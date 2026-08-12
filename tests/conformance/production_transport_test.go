package conformance

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

// This file covers the production transport surface that the suite could not see
// while it drove Server.Handler() directly (issue #664), plus the anti-drift gate
// between the production path and the direct handler.

// toolDescriptors calls tools/list and returns the descriptors keyed by name.
func toolDescriptors(t *testing.T, mcpURL string) map[string]map[string]interface{} {
	t.Helper()
	sid := initSession(t, mcpURL)
	resp := sendRPC(t, mcpURL, sid, `{"jsonrpc":"2.0","id":20,"method":"tools/list","params":{}}`, nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d want=200 body=%s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result struct {
			Tools []map[string]interface{} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("tools/list: decode: %v body=%s", err, raw)
	}
	if len(envelope.Result.Tools) == 0 {
		t.Fatalf("tools/list: no tools returned body=%s", raw)
	}
	out := make(map[string]map[string]interface{}, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tools/list: descriptor without a name: %#v", tool)
		}
		out[name] = tool
	}
	return out
}

// TestParity_ToolDescriptors fails when the production transport and the direct
// handler advertise different tools, descriptions or schemas. Both build from the
// same registry, so any drift is a bug in one of the two adapters.
func TestParity_ToolDescriptors(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	production := newServer(t, cfg)
	defer production.Close()
	direct := newDirectHandlerServer(t, cfg)
	defer direct.Close()

	prodTools := toolDescriptors(t, production.URL+cfg.MCPPath)
	directTools := toolDescriptors(t, direct.URL+cfg.MCPPath)

	for name, want := range directTools {
		got, ok := prodTools[name]
		if !ok {
			t.Errorf("tool %q is advertised by the direct handler but not by the production transport", name)
			continue
		}
		for _, field := range []string{"description", "inputSchema", "outputSchema"} {
			if !reflect.DeepEqual(got[field], want[field]) {
				t.Errorf("tool %q field %q diverges:\n production=%#v\n direct    =%#v", name, field, got[field], want[field])
			}
		}
	}
	for name := range prodTools {
		if _, ok := directTools[name]; !ok {
			t.Errorf("tool %q is advertised by the production transport but not by the direct handler", name)
		}
	}
}

// TestParity_ToolExecutionErrorContract fails when a tool that runs and fails
// reports a different canonical contract on the two paths. A bad argument to a
// real tool is a tool execution failure, so both paths must return isError=true
// with the same canonical code (SPEC §12.4, §14.4).
func TestParity_ToolExecutionErrorContract(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	production := newServer(t, cfg)
	defer production.Close()
	direct := newDirectHandlerServer(t, cfg)
	defer direct.Close()

	const call = `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":""}}}`

	prodCode := toolErrorCode(t, production.URL+cfg.MCPPath, call)
	directCode := toolErrorCode(t, direct.URL+cfg.MCPPath, call)
	if prodCode == "" {
		t.Fatal("production transport returned no canonical tool error code")
	}
	if prodCode != directCode {
		t.Fatalf("canonical tool error code diverges: production=%q direct=%q", prodCode, directCode)
	}
}

// toolErrorCode runs a tools/call that must fail and returns the canonical code
// carried in the structured output.
func toolErrorCode(t *testing.T, mcpURL, call string) string {
	t.Helper()
	sid := initSession(t, mcpURL)
	resp := sendRPC(t, mcpURL, sid, call, nil)
	raw := readBody(t, resp)
	var envelope struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("tool error: decode: %v body=%s", err, raw)
	}
	if !envelope.Result.IsError {
		t.Fatalf("tool error: expected isError=true body=%s", raw)
	}
	return envelope.Result.StructuredContent.Error.Code
}

// TestParity_UnknownToolIsIntentionallyDifferent pins the one known divergence
// between the two paths, so nobody has to rediscover it.
//
// The production transport returns a JSON-RPC -32602 error, which is what MCP
// requires for an unknown tool name. The direct handler returns a
// METHOD_NOT_FOUND tool result instead. The production contract is the correct
// one; this test records the direct handler's behaviour rather than blessing it,
// and it fails if either side changes.
func TestParity_UnknownToolIsIntentionallyDifferent(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	production := newServer(t, cfg)
	defer production.Close()
	direct := newDirectHandlerServer(t, cfg)
	defer direct.Close()

	const call = `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"dir2mcp_does_not_exist","arguments":{}}}`

	// Production: a JSON-RPC protocol error.
	prodURL := production.URL + cfg.MCPPath
	prodResp := sendRPC(t, prodURL, initSession(t, prodURL), call, nil)
	prodBody := readBody(t, prodResp)
	code, _ := decodeRPCError(t, prodBody)
	if code != -32602 {
		t.Fatalf("production: code=%d want=-32602 body=%s", code, prodBody)
	}

	// Direct handler: a tool result carrying METHOD_NOT_FOUND.
	directURL := direct.URL + cfg.MCPPath
	if got := toolErrorCode(t, directURL, call); got != "METHOD_NOT_FOUND" {
		t.Fatalf("direct handler: canonical code=%q want=METHOD_NOT_FOUND", got)
	}
}

// TestProduction_InitializePinsProtocolVersion verifies the production transport
// answers initialize with the pinned protocol version, not the version the client
// asked for (SPEC §11.2).
func TestProduction_InitializePinsProtocolVersion(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "",
		`{"jsonrpc":"2.0","id":23,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, nil)
	raw := readBody(t, resp)

	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("initialize: decode: %v body=%s", err, raw)
	}
	if envelope.Result.ProtocolVersion != protocol.ProtocolDefaultVersion {
		t.Fatalf("protocolVersion = %q, want %q body=%s", envelope.Result.ProtocolVersion, protocol.ProtocolDefaultVersion, raw)
	}
}

// TestProduction_NotificationsInitializedIsAccepted verifies the production
// transport accepts the initialized notification and keeps serving the session.
func TestProduction_NotificationsInitializedIsAccepted(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status=%d want=202", resp.StatusCode)
	}

	after := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":24,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, after)
	if after.StatusCode != http.StatusOK || !hasResult(body) {
		t.Fatalf("tools/list after initialized: status=%d body=%s", after.StatusCode, body)
	}
}

// TestProduction_NegotiatedProtocolVersionHeaderIsAccepted verifies a client that
// echoes the negotiated version keeps working, and that an unsupported version is
// refused with 400.
func TestProduction_NegotiatedProtocolVersionHeaderIsAccepted(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	ok := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":25,"method":"tools/list","params":{}}`,
		map[string]string{protocol.MCPProtocolVersionHeader: protocol.ProtocolDefaultVersion})
	body := readBody(t, ok)
	if ok.StatusCode != http.StatusOK || !hasResult(body) {
		t.Fatalf("tools/list with the negotiated version: status=%d body=%s", ok.StatusCode, body)
	}

	bad := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":26,"method":"tools/list","params":{}}`,
		map[string]string{protocol.MCPProtocolVersionHeader: "1999-01-01"})
	defer func() { _ = bad.Body.Close() }()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("tools/list with an unsupported version: status=%d want=400", bad.StatusCode)
	}
}

// TestProduction_DeleteTerminatesTheSession verifies DELETE ends the session on
// the production transport and that the id cannot be replayed.
func TestProduction_DeleteTerminatesTheSession(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create DELETE request: %v", err)
	}
	req.Header.Set(protocol.MCPSessionHeader, sid)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do DELETE: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d want=204", resp.StatusCode)
	}

	after := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":27,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, after)
	if after.StatusCode != http.StatusNotFound {
		t.Fatalf("replay after DELETE: status=%d want=404 body=%s", after.StatusCode, body)
	}
	code, _ := decodeRPCError(t, body)
	if code != -32001 {
		t.Fatalf("replay after DELETE: code=%d want=-32001 body=%s", code, body)
	}
}

// TestProduction_CORSPreflightIsServed verifies a browser preflight still gets
// the CORS headers on the production transport. OPTIONS is the one MCP-path verb
// the SDK transport hands back to Server.Handler(), so this pins that routing.
func TestProduction_CORSPreflightIsServed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	const origin = "https://allowed.example.com"
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
	srv := newServer(t, cfg)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodOptions, srv.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create OPTIONS request: %v", err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do OPTIONS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight: status=%d want=204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("preflight Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
}

// TestProduction_DisallowedOriginIsRefused verifies the origin guard still runs
// on the production transport.
func TestProduction_DisallowedOriginIsRefused(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)
	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(28), map[string]string{
		"Origin": "https://evil.example.com",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed origin: status=%d want=403", resp.StatusCode)
	}
}
