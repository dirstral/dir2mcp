package elevenlabsbridge_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	t.Run("MCP_URL auto-discovers from connection.json", func(t *testing.T) {
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
	})

	t.Run("token_file in connection.json takes precedence", func(t *testing.T) {
		tmp := t.TempDir()
		stateDir := filepath.Join(tmp, ".dir2mcp")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatalf("mkdir state dir: %v", err)
		}

		customTokenFile := filepath.Join(tmp, "custom.token")
		if err := os.WriteFile(customTokenFile, []byte("custom-token\n"), 0o600); err != nil {
			t.Fatalf("write custom token: %v", err)
		}
		// Also create secret.token to verify connection token_file wins.
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
	})

	t.Run("token_file in connection.json errors when file missing", func(t *testing.T) {
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
	})
}

func TestAskEndpointCallsDir2McpAsk(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []recordedMCPRequest
	)
	handlerErrCh := make(chan error, 4)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			JSONRPC string                 `json:"jsonrpc"`
			ID      int64                  `json:"id"`
			Method  string                 `json:"method"`
			Params  map[string]interface{} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			handlerErrCh <- fmt.Errorf("decode request: %w", err)
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
				handlerErrCh <- fmt.Errorf("tool=%q want dir2mcp.ask", rec.ToolName)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":false,"content":[{"type":"text","text":"bridge answer"}],"structuredContent":{"question":"what is alpha?","answer":"bridge answer","citations":[{"chunk_id":1,"rel_path":"docs/a.md","span":{"kind":"lines","start_line":1,"end_line":2}}],"hits":[],"indexing_complete":true}}}`))
		default:
			handlerErrCh <- fmt.Errorf("unexpected MCP method %q", payload.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
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
	close(handlerErrCh)
	for err := range handlerErrCh {
		if err != nil {
			t.Fatal(err)
		}
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
