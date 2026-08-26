package elevenlabsbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/elevenlabsbridge"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

type recordedMCPRequest struct {
	Method        string
	ToolName      string
	Authorization string
	SessionID     string
	Protocol      string
	K             int
	Question      string
}

func TestLoadConfigFromEnvAndTokenResolution(t *testing.T) {
	t.Run("explicit env token wins", subtestExplicitEnvTokenWins)
	t.Run("state dir fallback uses cwd not script path", subtestStateDirFallback)
	t.Run("missing token falls back to none", subtestMissingTokenNone)
	t.Run("MCP_URL auto-discovers from connection.json", subtestAutoDiscoverMCPURL)
	t.Run("token_file in connection.json takes precedence", subtestTokenFilePrecedence)
	t.Run("token_file in connection.json errors when file missing", subtestTokenFileMissing)
}

func subtestExplicitEnvTokenWins(t *testing.T) {
	cfg, err := elevenlabsbridge.LoadConfigFromEnv(map[string]string{
		"MCP_URL":   "https://example.test/mcp",
		"MCP_TOKEN": "env-token",
		"STATE_DIR": "/tmp/ignored",
		"PORT":      "9090",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.MCPURL != "https://example.test/mcp" {
		t.Fatalf("MCPURL=%q", cfg.MCPURL)
	}
	if cfg.Port != 9090 {
		t.Fatalf("Port=%d", cfg.Port)
	}

	token, source, tokenPath, err := elevenlabsbridge.ResolveToken(cfg)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if token != "env-token" || source != "env" || tokenPath != "" {
		t.Fatalf("unexpected token resolution: token=%q source=%q path=%q", token, source, tokenPath)
	}
}

func subtestStateDirFallback(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "secret.token"), []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	testutil.WithWorkingDir(t, tmp, func() {
		cfg, err := elevenlabsbridge.LoadConfigFromEnv(map[string]string{})
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		token, source, tokenPath, err := elevenlabsbridge.ResolveToken(cfg)
		if err != nil {
			t.Fatalf("resolve token: %v", err)
		}
		if token != "file-token" {
			t.Fatalf("token=%q", token)
		}
		if source != "file" {
			t.Fatalf("source=%q", source)
		}
		if !filepath.IsAbs(tokenPath) {
			t.Fatalf("expected absolute token path, got %q", tokenPath)
		}
	})
}

func subtestMissingTokenNone(t *testing.T) {
	tmp := t.TempDir()
	testutil.WithWorkingDir(t, tmp, func() {
		cfg, err := elevenlabsbridge.LoadConfigFromEnv(map[string]string{})
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		token, source, tokenPath, err := elevenlabsbridge.ResolveToken(cfg)
		if err != nil {
			t.Fatalf("resolve token: %v", err)
		}
		if token != "" || source != "none" || tokenPath != "" {
			t.Fatalf("unexpected resolution: token=%q source=%q path=%q", token, source, tokenPath)
		}
	})
}

func subtestAutoDiscoverMCPURL(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	payload := `{"transport":"mcp_streamable_http","url":"http://127.0.0.1:9099/mcp"}`
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}

	cfg, err := elevenlabsbridge.LoadConfigFromEnv(map[string]string{
		"STATE_DIR": stateDir,
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.MCPURL != "http://127.0.0.1:9099/mcp" {
		t.Fatalf("MCPURL=%q", cfg.MCPURL)
	}
}

func subtestTokenFilePrecedence(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	customTokenFile := filepath.Join(tmp, "custom.token")
	if err := os.WriteFile(customTokenFile, []byte("custom-token\n"), 0o600); err != nil {
		t.Fatalf("write custom token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "secret.token"), []byte("state-token\n"), 0o600); err != nil {
		t.Fatalf("write secret token: %v", err)
	}
	payload := fmt.Sprintf(`{"transport":"mcp_streamable_http","url":"http://127.0.0.1:9099/mcp","token_file":%q}`, customTokenFile)
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}

	cfg := elevenlabsbridge.DefaultConfig()
	cfg.StateDir = stateDir
	token, source, tokenPath, err := elevenlabsbridge.ResolveToken(cfg)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if token != "custom-token" || source != "file" {
		t.Fatalf("unexpected token resolution: token=%q source=%q", token, source)
	}
	if tokenPath != customTokenFile {
		t.Fatalf("tokenPath=%q want=%q", tokenPath, customTokenFile)
	}
}

func subtestTokenFileMissing(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	nonExistentTokenFile := filepath.Join(tmp, "does-not-exist.token")
	payload := fmt.Sprintf(`{"transport":"mcp_streamable_http","url":"http://127.0.0.1:9099/mcp","token_file":%q}`, nonExistentTokenFile)
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}

	cfg := elevenlabsbridge.DefaultConfig()
	cfg.StateDir = stateDir
	_, _, _, err := elevenlabsbridge.ResolveToken(cfg)
	if err == nil {
		t.Fatalf("expected error when token_file references non-existent file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected IsNotExist error, got %v", err)
	}
}

func TestAskEndpointCallsDir2McpAsk(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []recordedMCPRequest
	)
	handlerErrCh := make(chan error, 4)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleMCPBackend(w, r, &mu, &requests, handlerErrCh)
	}))
	defer backend.Close()

	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = backend.URL
	cfg.MCPToken = "bridge-token"
	cfg.StateDir = t.TempDir()

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}

	srv := httptest.NewServer(bridge.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/ask", "application/json", bytes.NewBufferString(`{"question":"what is alpha?","k":7}`))
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var payload struct {
		Result     string                 `json:"result"`
		Structured map[string]interface{} `json:"structured"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode bridge response: %v", err)
	}
	if payload.Result != "bridge answer" {
		t.Fatalf("result=%q", payload.Result)
	}
	if payload.Structured["answer"] != "bridge answer" {
		t.Fatalf("structured answer=%#v", payload.Structured["answer"])
	}

	mu.Lock()
	defer mu.Unlock()
	assertAskRequestSequence(t, requests)
	close(handlerErrCh)
	for err := range handlerErrCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func handleMCPBackend(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, requests *[]recordedMCPRequest, errCh chan<- error) {
	var payload struct {
		JSONRPC string                 `json:"jsonrpc"`
		ID      int64                  `json:"id"`
		Method  string                 `json:"method"`
		Params  map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		errCh <- fmt.Errorf("decode request: %w", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rec := recordedMCPRequest{
		Method:        payload.Method,
		Authorization: strings.TrimSpace(r.Header.Get("Authorization")),
		SessionID:     strings.TrimSpace(r.Header.Get("MCP-Session-Id")),
		Protocol:      strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")),
	}

	switch payload.Method {
	case "initialize":
		w.Header().Set("MCP-Session-Id", "session-123")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{},"serverInfo":{"name":"dir2mcp","version":"test"}}}`))
	case "notifications/initialized":
		// bs-004: an accepted notification is answered with HTTP 202.
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if !handleToolsCall(w, payload.Params, &rec, errCh) {
			return
		}
	default:
		errCh <- fmt.Errorf("unexpected MCP method %q", payload.Method)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	mu.Lock()
	*requests = append(*requests, rec)
	mu.Unlock()
}

func handleToolsCall(w http.ResponseWriter, params map[string]interface{}, rec *recordedMCPRequest, errCh chan<- error) bool {
	rec.ToolName, _ = params["name"].(string)
	args, _ := params["arguments"].(map[string]interface{})
	if q, _ := args["question"].(string); q != "" {
		rec.Question = q
	}
	if k, ok := args["k"].(float64); ok {
		rec.K = int(k)
	}
	if rec.ToolName != "dir2mcp_ask" {
		errCh <- fmt.Errorf("tool=%q want dir2mcp.ask", rec.ToolName)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":false,"content":[{"type":"text","text":"bridge answer"}],"structuredContent":{"question":"what is alpha?","answer":"bridge answer","citations":[{"chunk_id":1,"rel_path":"docs/a.md","span":{"kind":"lines","start_line":1,"end_line":2}}],"hits":[],"indexing_complete":true}}}`))
	return true
}

func assertAskRequestSequence(t *testing.T, requests []recordedMCPRequest) {
	t.Helper()
	// This pins the bundled client's bs-005-conformant sequence: initialize,
	// then notifications/initialized on the assigned session, then tool traffic.
	if len(requests) != 3 {
		t.Fatalf("unexpected request count: %#v", requests)
	}
	if requests[0].Method != "initialize" || requests[1].Method != "notifications/initialized" || requests[2].Method != "tools/call" {
		t.Fatalf("unexpected method sequence: %#v", requests)
	}
	for i, req := range requests {
		if req.Authorization != "Bearer bridge-token" {
			t.Fatalf("expected authorization header to be forwarded on request %d: %#v", i, requests)
		}
	}
	if requests[1].SessionID != "session-123" {
		t.Fatalf("expected session id on notifications/initialized, got %#v", requests[1].SessionID)
	}
	if requests[2].SessionID != "session-123" {
		t.Fatalf("expected session id to be forwarded, got %#v", requests[2].SessionID)
	}
	if requests[2].ToolName != "dir2mcp_ask" {
		t.Fatalf("tool=%q", requests[2].ToolName)
	}
	if requests[2].Question != "what is alpha?" {
		t.Fatalf("question=%q", requests[2].Question)
	}
	if requests[2].K != 7 {
		t.Fatalf("k=%d", requests[2].K)
	}
}

func TestInboundSecretRejectsUnauthenticated(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []recordedMCPRequest
	)
	handlerErrCh := make(chan error, 8)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleMCPBackend(w, r, &mu, &requests, handlerErrCh)
	}))
	defer backend.Close()

	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = backend.URL
	cfg.MCPToken = "bridge-token"
	cfg.StateDir = t.TempDir()
	cfg.InboundSecret = "top-secret"

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	if !bridge.InboundAuthEnabled() {
		t.Fatalf("expected inbound auth to be enabled")
	}

	srv := httptest.NewServer(bridge.Handler())
	defer srv.Close()

	askBody := `{"question":"what is alpha?","k":7}`

	// No credential -> 401, and the backend must never be reached.
	if got := postAsk(t, srv.URL, askBody, ""); got != http.StatusUnauthorized {
		t.Fatalf("missing secret: status=%d want=%d", got, http.StatusUnauthorized)
	}
	// Wrong credential -> 401.
	if got := postAskWithHeader(t, srv.URL, askBody, "X-Bridge-Secret", "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong secret: status=%d want=%d", got, http.StatusUnauthorized)
	}

	mu.Lock()
	if len(requests) != 0 {
		mu.Unlock()
		t.Fatalf("backend was reached by unauthenticated callers: %#v", requests)
	}
	mu.Unlock()

	// Correct credential via X-Bridge-Secret -> 200.
	if got := postAskWithHeader(t, srv.URL, askBody, "X-Bridge-Secret", "top-secret"); got != http.StatusOK {
		t.Fatalf("valid X-Bridge-Secret: status=%d want=%d", got, http.StatusOK)
	}
	// Correct credential via Authorization: Bearer -> 200.
	if got := postAsk(t, srv.URL, askBody, "Bearer top-secret"); got != http.StatusOK {
		t.Fatalf("valid bearer: status=%d want=%d", got, http.StatusOK)
	}

	// /health stays unauthenticated.
	healthResp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d want=%d", healthResp.StatusCode, http.StatusOK)
	}

	close(handlerErrCh)
	for err := range handlerErrCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func postAsk(t *testing.T, baseURL, body, authorization string) int {
	t.Helper()
	return postAskWithHeader(t, baseURL, body, "Authorization", authorization)
}

func postAskWithHeader(t *testing.T, baseURL, body, headerName, headerValue string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/ask", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if headerValue != "" {
		req.Header.Set(headerName, headerValue)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestValidateListenSecurity(t *testing.T) {
	cases := []struct {
		name          string
		listen        string
		hasSecret     bool
		forceInsecure bool
		wantErr       bool
	}{
		{name: "loopback no secret allowed", listen: "127.0.0.1:8088"},
		{name: "loopback ipv6 allowed", listen: "[::1]:8088"},
		{name: "localhost allowed", listen: "localhost:8088"},
		{name: "wildcard no secret refused", listen: "0.0.0.0:8088", wantErr: true},
		{name: "empty host refused", listen: ":8088", wantErr: true},
		{name: "ipv6 wildcard refused", listen: "[::]:8088", wantErr: true},
		{name: "public ip no secret refused", listen: "192.168.1.10:8088", wantErr: true},
		{name: "public ip with secret allowed", listen: "0.0.0.0:8088", hasSecret: true},
		{name: "public ip force insecure allowed", listen: "0.0.0.0:8088", forceInsecure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := elevenlabsbridge.ValidateListenSecurity(tc.listen, tc.hasSecret, tc.forceInsecure)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q (secret=%v force=%v)", tc.listen, tc.hasSecret, tc.forceInsecure)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.listen, err)
			}
		})
	}
}

func TestRunWithBridgeRefusesNonLoopbackWithoutSecret(t *testing.T) {
	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = "http://127.0.0.1:1/mcp"
	cfg.MCPToken = "token"
	cfg.StateDir = t.TempDir()

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}

	// Non-loopback without a secret must be refused before the server binds.
	if err := elevenlabsbridge.RunWithBridge(context.Background(), bridge, "0.0.0.0:0"); err == nil {
		t.Fatalf("expected RunWithBridge to refuse non-loopback bind without secret")
	} else if !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunWithBridgeAllowsLoopbackWithoutSecret(t *testing.T) {
	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = "http://127.0.0.1:1/mcp"
	cfg.MCPToken = "token"
	cfg.StateDir = t.TempDir()

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- elevenlabsbridge.RunWithBridge(ctx, bridge, "127.0.0.1:0")
	}()

	// The guard must not refuse a loopback bind: cancel and expect a clean
	// context-cancellation rather than a security error.
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestMethodGuardSetsAllowHeader(t *testing.T) {
	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = "http://127.0.0.1:1/mcp"
	cfg.MCPToken = "token"

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	srv := httptest.NewServer(bridge.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/ask", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow=%q want=%q", allow, http.MethodPost)
	}
}

func TestAskRejectsOversizedBody(t *testing.T) {
	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = "http://127.0.0.1:1/mcp"
	cfg.MCPToken = "token"

	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	srv := httptest.NewServer(bridge.Handler())
	defer srv.Close()

	// 1 MiB max; this body exceeds that limit.
	oversized := strings.Repeat("a", (1<<20)+64)
	body := fmt.Sprintf("{\"question\":\"%s\"}", oversized)
	resp, err := http.Post(srv.URL+"/ask", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusRequestEntityTooLarge, raw)
	}
}
