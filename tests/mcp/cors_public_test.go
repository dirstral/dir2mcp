package tests

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dir2mcp/internal/cli"
	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/protocol"
)

// TestCORS_PreflightReturns204 verifies allowed-origin preflight requests return 204 with CORS headers.
func TestCORS_PreflightReturns204(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"https://elevenlabs.io"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Origin", "https://elevenlabs.io")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", strings.Join([]string{"Content-Type", "Authorization", protocol.MCPProtocolVersionHeader}, ", "))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusNoContent)
	}

	acao := resp.Header.Get("Access-Control-Allow-Origin")
	if acao != "https://elevenlabs.io" {
		t.Fatalf("Access-Control-Allow-Origin=%q want=%q", acao, "https://elevenlabs.io")
	}

	acam := resp.Header.Get("Access-Control-Allow-Methods")
	if !strings.Contains(acam, "POST") {
		t.Fatalf("Access-Control-Allow-Methods=%q must contain POST", acam)
	}

	acah := resp.Header.Get("Access-Control-Allow-Headers")
	for _, expected := range []string{"Authorization", protocol.MCPProtocolVersionHeader, protocol.MCPSessionHeader} {
		if !strings.Contains(acah, expected) {
			t.Fatalf("Access-Control-Allow-Headers=%q must contain %q", acah, expected)
		}
	}

	aceh := resp.Header.Get("Access-Control-Expose-Headers")
	if !strings.Contains(aceh, protocol.MCPSessionHeader) {
		t.Fatalf("Access-Control-Expose-Headers=%q must contain %s", aceh, protocol.MCPSessionHeader)
	}
	if !strings.Contains(aceh, protocol.MCPSessionExpiredHeader) {
		t.Fatalf("Access-Control-Expose-Headers=%q must contain %s", aceh, protocol.MCPSessionExpiredHeader)
	}
}

// TestCORS_OptionsWithoutPreflightHeadersFallsThrough verifies non-preflight OPTIONS requests fall through to method handling.
func TestCORS_OptionsWithoutPreflightHeadersFallsThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"https://elevenlabs.io"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Origin", "https://elevenlabs.io")
	// Intentionally omit Access-Control-Request-* headers so this is not
	// treated as a CORS preflight request.

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

// TestCORS_PreflightDisallowedOriginNoCORSHeaders verifies disallowed-origin preflights do not receive allow-origin headers.
func TestCORS_PreflightDisallowedOriginNoCORSHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"https://elevenlabs.io"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+cfg.MCPPath, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", strings.Join([]string{"Content-Type", "Authorization", protocol.MCPProtocolVersionHeader}, ", "))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusNoContent)
	}

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin=%q want empty", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "" {
		t.Fatalf("Access-Control-Allow-Methods=%q want empty", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "" {
		t.Fatalf("Access-Control-Allow-Headers=%q want empty", got)
	}
}

// TestCORS_DisallowedOriginNoHeaders verifies disallowed origins are rejected without CORS allow-origin headers.
func TestCORS_DisallowedOriginNoHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"https://elevenlabs.io"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Should be rejected by origin check (403).
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusForbidden)
	}

	// No CORS headers should be set.
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	if acao != "" {
		t.Fatalf("Access-Control-Allow-Origin=%q want empty", acao)
	}
}

// TestCORS_AllowedOriginSetsHeaders verifies allowed origins on POST requests receive CORS allow-origin headers.
func TestCORS_AllowedOriginSetsHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.AllowedOrigins = []string{"https://elevenlabs.io"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req, err := http.NewRequest(http.MethodPost, server.URL+cfg.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://elevenlabs.io")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusOK)
	}

	acao := resp.Header.Get("Access-Control-Allow-Origin")
	if acao != "https://elevenlabs.io" {
		t.Fatalf("Access-Control-Allow-Origin=%q want=%q", acao, "https://elevenlabs.io")
	}
}

// TestPublicFlag_RejectsAuthNone verifies CLI guardrails reject --public when auth is explicitly disabled.
func TestPublicFlag_RejectsAuthNone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	code := app.RunWithContext(context.Background(), []string{"up", "--public", "--auth", "none"})

	if code == 0 {
		t.Fatal("expected non-zero exit code for --public --auth none")
	}
	if !strings.Contains(stderr.String(), "--public requires auth") {
		t.Fatalf("expected auth error message, got stderr=%q", stderr.String())
	}
}
