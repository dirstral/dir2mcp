package elevenlabsbridge_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dir2mcp/internal/elevenlabsbridge"
	"dir2mcp/tests/testutil"
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
	t.Run("explicit env token wins", func(t *testing.T) {
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
	})

	t.Run("state dir fallback uses cwd not script path", func(t *testing.T) {
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
	})

	t.Run("missing token falls back to none", func(t *testing.T) {
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
	})
}

func TestAskEndpointCallsDir2McpAsk(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []recordedMCPRequest
	)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			JSONRPC string                 `json:"jsonrpc"`
			ID      int64                  `json:"id"`
			Method  string                 `json:"method"`
			Params  map[string]interface{} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
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
		case "tools/call":
			rec.ToolName, _ = payload.Params["name"].(string)
			args, _ := payload.Params["arguments"].(map[string]interface{})
			if q, _ := args["question"].(string); q != "" {
				rec.Question = q
			}
			if k, ok := args["k"].(float64); ok {
				rec.K = int(k)
			}
			if rec.ToolName != "dir2mcp.ask" {
				t.Fatalf("tool=%q want dir2mcp.ask", rec.ToolName)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":false,"content":[{"type":"text","text":"bridge answer"}],"structuredContent":{"question":"what is alpha?","answer":"bridge answer","citations":[{"chunk_id":1,"rel_path":"docs/a.md","span":{"kind":"lines","start_line":1,"end_line":2}}],"hits":[],"indexing_complete":true}}}`))
		default:
			t.Fatalf("unexpected MCP method %q", payload.Method)
		}

		mu.Lock()
		requests = append(requests, rec)
		mu.Unlock()
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
	if len(requests) != 2 {
		t.Fatalf("unexpected request count: %#v", requests)
	}
	if requests[0].Method != "initialize" || requests[1].Method != "tools/call" {
		t.Fatalf("unexpected method sequence: %#v", requests)
	}
	if requests[0].Authorization != "Bearer bridge-token" || requests[1].Authorization != "Bearer bridge-token" {
		t.Fatalf("expected authorization header to be forwarded: %#v", requests)
	}
	if requests[1].SessionID != "session-123" {
		t.Fatalf("expected session id to be forwarded, got %#v", requests[1].SessionID)
	}
	if requests[1].ToolName != "dir2mcp.ask" {
		t.Fatalf("tool=%q", requests[1].ToolName)
	}
	if requests[1].Question != "what is alpha?" {
		t.Fatalf("question=%q", requests[1].Question)
	}
	if requests[1].K != 7 {
		t.Fatalf("k=%d", requests[1].K)
	}
}
