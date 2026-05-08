// Package conformance contains conformance tests that drive the dir2mcp MCP
// server over HTTP and assert externally-observable behaviour: tool response
// shapes, error codes, session lifecycle, and x402 header behaviour.  Tests
// construct the server in-process using mcp.NewServer + httptest and drive it
// exclusively with raw JSON-RPC payloads via net/http — assertions are made
// only against HTTP responses, not against internal state.
package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/x402"
)

// ---------------------------------------------------------------------------
// Mock facilitator
// ---------------------------------------------------------------------------

// mockFacilitator is a minimal httptest.Handler that records verify/settle
// calls and returns configurable status codes and bodies.
type mockFacilitator struct {
	verifyStatus int
	settleStatus int
	verifyBody   string
	settleBody   string
	verifyCalls  atomic.Int64
	settleCalls  atomic.Int64
}

func newMockFacilitator() *mockFacilitator {
	return &mockFacilitator{
		verifyStatus: http.StatusOK,
		settleStatus: http.StatusOK,
		verifyBody:   `{"isValid":true}`,
		settleBody:   `{"success":true,"transaction":"tx-1","network":"eip155:8453"}`,
	}
}

func (f *mockFacilitator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/x402/verify":
		f.verifyCalls.Add(1)
		w.WriteHeader(f.verifyStatus)
		_, _ = w.Write([]byte(f.verifyBody))
	case "/v2/x402/settle":
		f.settleCalls.Add(1)
		w.WriteHeader(f.settleStatus)
		_, _ = w.Write([]byte(f.settleBody))
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// Server construction helpers
// ---------------------------------------------------------------------------

// defaultConfig returns a minimal config suitable for conformance tests.
// AuthMode is set to "none" so tests don't need to provide tokens.
func defaultConfig() config.Config {
	cfg := config.Default()
	cfg.AuthMode = "none"
	return cfg
}

// x402Config returns a fully-populated x402 config for tests that need a
// working facilitator.  StateDir is set to a per-test temporary directory.
func x402Config(t *testing.T, facilitatorURL string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.AuthMode = "none"
	cfg.X402.ToolsCallEnabled = true
	cfg.X402.FacilitatorURL = facilitatorURL
	cfg.X402.ResourceBaseURL = "https://resource.example.com"
	cfg.X402.Scheme = "exact"
	cfg.X402.PriceAtomic = "1000"
	cfg.X402.Network = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	cfg.X402.Asset = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	cfg.X402.PayTo = "8N5A4rQU8vJrQmH3iiA7kE4m1df4WeyueXQqGb4G9tTj"
	return cfg
}

// newServer starts an httptest.Server with the given config.
// Callers are responsible for calling srv.Close().
func newServer(cfg config.Config, opts ...mcp.ServerOption) *httptest.Server {
	return httptest.NewServer(mcp.NewServer(cfg, nil, opts...).Handler())
}

// ---------------------------------------------------------------------------
// JSON-RPC helpers
// ---------------------------------------------------------------------------

// httpClient is shared across all conformance tests to reuse connections.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// sendRPC posts a JSON-RPC payload to mcpURL with optional session and extra
// headers.  Callers are responsible for closing resp.Body.
func sendRPC(t *testing.T, mcpURL, sessionID, body string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, mcpURL, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(protocol.MCPSessionHeader, sessionID)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// initSession calls MCP initialize and returns the session ID.
func initSession(t *testing.T, mcpURL string) string {
	t.Helper()
	resp := sendRPC(t, mcpURL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: status=%d body=%s", resp.StatusCode, body)
	}
	sid := resp.Header.Get(protocol.MCPSessionHeader)
	if sid == "" {
		t.Fatalf("initialize did not return a session ID")
	}
	return sid
}

// readBody reads and returns the full body from resp, closing it.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// decodeRPCError extracts code and message from a JSON-RPC error envelope.
func decodeRPCError(t *testing.T, body []byte) (code int, message string) {
	t.Helper()
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode rpc error: %v body=%s", err, body)
	}
	if envelope.Error == nil {
		t.Fatalf("expected error field in response body=%s", body)
	}
	return envelope.Error.Code, envelope.Error.Message
}

// statsCallBody returns a JSON-RPC tools/call body for dir2mcp.stats.
func statsCallBody(id int) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`
}

// listFilesCallBody returns a JSON-RPC tools/call body for dir2mcp.list_files
// targeting the given directory.
func listFilesCallBody(id int, dir string) string {
	b, _ := json.Marshal(dir)
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"path_prefix":` + string(b) + `}}}`
}

// headerPresent returns true if the named header is set and non-empty.
func headerPresent(resp *http.Response, name string) bool {
	return strings.TrimSpace(resp.Header.Get(name)) != ""
}

// hasResult returns true if the response body contains a "result" field.
func hasResult(body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m["result"]
	return ok
}

// hasError returns true if the response body contains an "error" field.
func hasError(body []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	_, ok := m["error"]
	return ok
}

const (
	paymentRequiredHeader  = x402.HeaderPaymentRequired
	paymentSignatureHeader = x402.HeaderPaymentSignature
	paymentResponseHeader  = x402.HeaderPaymentResponse
)
