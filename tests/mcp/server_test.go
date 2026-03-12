package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dir2mcp/internal/buildinfo"
	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/protocol"
)

func postToolsListWithSession(serverURL, mcpPath, sessionID string) (*http.Response, error) {
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	req, err := http.NewRequest(http.MethodPost, serverURL+mcpPath, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Session-Id", sessionID)
	return http.DefaultClient.Do(req)
}

func waitForSessionExpiry(t *testing.T, serverURL, mcpPath, sessionID, expectedReason string) *http.Response {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	var lastStatus int
	var lastReason string

	for time.Now().Before(deadline) {
		resp, err := postToolsListWithSession(serverURL, mcpPath, sessionID)
		if err != nil {
			t.Fatalf("tools/list request failed while waiting for expiry: %v", err)
		}

		reason := strings.TrimSpace(resp.Header.Get(protocol.MCPSessionExpiredHeader))
		if resp.StatusCode == http.StatusNotFound && reason == expectedReason {
			return resp
		}

		lastStatus = resp.StatusCode
		lastReason = reason
		_ = resp.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("session did not expire as expected; last status=%d reason=%q expected reason=%q", lastStatus, lastReason, expectedReason)
	return nil
}

func TestMCPInitialize_AllowsOriginWithPortWhenAllowlistOmitsPort(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"http://localhost"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}
	if strings.TrimSpace(resp.Header.Get("MCP-Session-Id")) == "" {
		t.Fatal("expected MCP-Session-Id header on initialize response")
	}
}

func TestSessionExpiration_InactivityHeader(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	// Inactivity expiry cannot be polled with repeated requests because each
	// successful request refreshes lastSeen. Use a conservative one-shot wait.
	cfg.SessionInactivityTimeout = 50 * time.Millisecond
	cfg.SessionMaxLifetime = 0

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	// initialize to obtain a session id
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do initialize: %v", err)
	}
	sessionID := resp.Header.Get("MCP-Session-Id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize failed: status=%d, sessionID=%q", resp.StatusCode, sessionID)
	}
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("initialize failed: status=%d, missing MCP-Session-Id", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Wait well beyond inactivity timeout, then issue a single check request.
	time.Sleep(500 * time.Millisecond)
	resp2, err := postToolsListWithSession(server.URL, cfg.MCPPath, sessionID)
	if err != nil {
		t.Fatalf("tools/list request failed after inactivity wait: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on expired session, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get(protocol.MCPSessionExpiredHeader) != "inactivity" {
		t.Fatalf("expected inactivity header, got %q", resp2.Header.Get(protocol.MCPSessionExpiredHeader))
	}
}

func TestSessionExpiration_MaxLifetimeHeader(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	// keep values small but with enough buffer to avoid timer-granularity
	// flakes in CI environments.
	cfg.SessionInactivityTimeout = 1 * time.Hour
	cfg.SessionMaxLifetime = 20 * time.Millisecond

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do initialize: %v", err)
	}
	sessionID := resp.Header.Get("MCP-Session-Id")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize failed: status=%d", resp.StatusCode)
	}
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("initialize failed: missing MCP-Session-Id")
	}
	_ = resp.Body.Close()

	resp2 := waitForSessionExpiry(t, server.URL, cfg.MCPPath, sessionID, "max-lifetime")
	defer func() { _ = resp2.Body.Close() }()

	// waitForSessionExpiry already asserts a 404 response and the
	// "max-lifetime" X-MCP-Session-Expired header, so additional
	// checks here would be redundant. Keep the call above for its
	// built-in validation.
}

func TestMCPInitialize_RejectsMissingJSONRPCVersion(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusBadRequest {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusBadRequest, string(payload))
	}

	var envelope struct {
		Error struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Error.Data.Code != "INVALID_FIELD" {
		t.Fatalf("canonical code=%q want=%q", envelope.Error.Data.Code, "INVALID_FIELD")
	}
}

func TestMCPInitialize_UsesBuildInfoVersion(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.ServerInfo.Version != buildinfo.Version {
		t.Fatalf("server version=%q want=%q", envelope.Result.ServerInfo.Version, buildinfo.Version)
	}
}
