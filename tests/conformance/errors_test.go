package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"dir2mcp/internal/config"
)

// TestErrors_JSONRPCErrorShape verifies that all JSON-RPC error responses
// contain code (int) and message (string) fields.
func TestErrors_JSONRPCErrorShape(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid,
		`{"jsonrpc":"2.0","id":1,"method":"unknown.method","params":{}}`,
		nil,
	)
	body := readBody(t, resp)

	if !hasError(body) {
		t.Fatalf("error shape: expected error field body=%s", body)
	}

	var envelope struct {
		Error struct {
			Code    json.Number `json:"code"`
			Message string      `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error shape: decode: %v body=%s", err, body)
	}
	if envelope.Error.Code == "" {
		t.Fatalf("error shape: code field is missing body=%s", body)
	}
	if _, err := envelope.Error.Code.Int64(); err != nil {
		t.Fatalf("error shape: code is not an integer: %v body=%s", err, body)
	}
	if strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatalf("error shape: message field is empty body=%s", body)
	}
}

// TestErrors_HTTPUnauthorized verifies that when auth is required and no token
// is provided, the server returns HTTP 401 with a well-formed JSON-RPC error
// body containing code and message.
func TestErrors_HTTPUnauthorized(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.AuthMode = "token"
	cfg.ResolvedAuthToken = "secret-token"

	srv := newServer(cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized: status=%d want=401 body=%s", resp.StatusCode, body)
	}
	if !hasError(body) {
		t.Fatalf("unauthorized: expected error field body=%s", body)
	}
	code, msg := decodeRPCError(t, body)
	if code == 0 {
		t.Fatalf("unauthorized: code=0 body=%s", body)
	}
	if strings.TrimSpace(msg) == "" {
		t.Fatalf("unauthorized: message empty body=%s", body)
	}
}

// TestErrors_HTTPMethodNotAllowed verifies that a GET (or any non-POST) to the
// MCP endpoint returns HTTP 405.
func TestErrors_HTTPMethodNotAllowed(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create GET request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method not allowed: status=%d want=405", resp.StatusCode)
	}
}

// TestErrors_ParseErrorHasStandardCode verifies that a parse error (malformed
// JSON) returns a standard JSON-RPC error code in the -326xx range.
func TestErrors_ParseErrorHasStandardCode(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	resp := sendRPC(t, srv.URL+cfg.MCPPath, "", `{bad json`, nil)
	body := readBody(t, resp)

	if !hasError(body) {
		t.Fatalf("parse error: expected error field body=%s", body)
	}

	code, message := decodeRPCError(t, body)
	// -32700 = parse error; -32600 = invalid request. Accept either since the
	// server maps both malformed-JSON cases to -32600.
	if code != -32700 && code != -32600 {
		t.Fatalf("parse error: code=%d want=-32700 or -32600 message=%q body=%s", code, message, body)
	}
	if strings.TrimSpace(message) == "" {
		t.Fatalf("parse error: message is empty body=%s", body)
	}
}
