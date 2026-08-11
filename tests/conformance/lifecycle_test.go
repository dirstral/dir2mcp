package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// TestLifecycle_Initialize verifies that an initialize request returns HTTP 200
// with a JSON-RPC result containing serverInfo and capabilities, and that the
// MCP-Session-Id header is set on the response.
func TestLifecycle_Initialize(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if strings.TrimSpace(resp.Header.Get(protocol.MCPSessionHeader)) == "" {
		t.Fatal("initialize: expected MCP-Session-Id header")
	}
	if !hasResult(body) {
		t.Fatalf("initialize: expected result field in body=%s", body)
	}

	var envelope struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
			Capabilities map[string]interface{} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("initialize: decode result: %v body=%s", err, body)
	}
	if envelope.Result.ServerInfo.Name == "" {
		t.Fatalf("initialize: serverInfo.name is empty body=%s", body)
	}
	if len(envelope.Result.Capabilities) == 0 {
		t.Fatalf("initialize: capabilities is empty body=%s", body)
	}
}

// TestLifecycle_UnknownMethod verifies that an unknown JSON-RPC method returns
// HTTP 200 with a JSON-RPC error whose code is -32601.
func TestLifecycle_UnknownMethod(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":2,"method":"nonexistent/method","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown method: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if !hasError(body) {
		t.Fatalf("unknown method: expected error field body=%s", body)
	}

	code, _ := decodeRPCError(t, body)
	if code != -32601 {
		t.Fatalf("unknown method: code=%d want=-32601 body=%s", code, body)
	}
}

// TestLifecycle_MalformedJSON verifies that malformed JSON returns a 400 with a
// JSON-RPC parse/invalid-request error.
func TestLifecycle_MalformedJSON(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{not valid json`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: status=%d want=400 body=%s", resp.StatusCode, body)
	}
	if !hasError(body) {
		t.Fatalf("malformed json: expected error field body=%s", body)
	}
	// -32700 = parse error; -32600 = invalid request. Accept either since the
	// server maps both malformed-JSON cases to -32600.
	code, _ := decodeRPCError(t, body)
	if code != -32700 && code != -32600 {
		t.Fatalf("malformed json: code=%d want=-32700 or -32600 body=%s", code, body)
	}
}

// TestLifecycle_MissingSession verifies that a non-initialize method without a
// session ID returns 404 with a JSON-RPC error.
func TestLifecycle_MissingSession(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session: status=%d want=404 body=%s", resp.StatusCode, body)
	}
	if !hasError(body) {
		t.Fatalf("missing session: expected error field body=%s", body)
	}
	code, _ := decodeRPCError(t, body)
	if code != -32001 {
		t.Fatalf("missing session: code=%d want=-32001 body=%s", code, body)
	}
}

// TestLifecycle_InitializeMissingJSONRPC verifies that a request missing the
// jsonrpc field returns 400.
func TestLifecycle_InitializeMissingJSONRPC(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"id":1,"method":"initialize","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing jsonrpc: status=%d want=400 body=%s", resp.StatusCode, body)
	}
	if !hasError(body) {
		t.Fatalf("missing jsonrpc: expected error field body=%s", body)
	}
}

// TestLifecycle_AuthRequired verifies that when AuthMode is not "none", a
// request without an Authorization header returns HTTP 401.
func TestLifecycle_AuthRequired(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "token"
	cfg.ResolvedAuthToken = "supersecret"

	srv := newServer(t, cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth required: status=%d want=401 body=%s", resp.StatusCode, body)
	}
}
