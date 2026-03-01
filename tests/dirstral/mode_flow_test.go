package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"dir2mcp/internal/dirstral/app"
	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/dirstral/config"
	"dir2mcp/internal/dirstral/host"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/protocol"
)

func TestModeFlowSmokeServerToChatToVoiceHandoff(t *testing.T) {
	setTestConfigDir(t)
	_ = host.ClearState()
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	server, calls := newModeFlowMCPServer(t)
	defer server.Close()

	activeEndpoint := server.URL
	staleConfigEndpoint := "http://127.0.0.1:1/mcp"

	requireNoErrSegment(t, "host", host.SaveState(host.State{
		PID:       os.Getpid(),
		StartedAt: "now",
		MCPURL:    activeEndpoint,
	}))

	health := host.CheckHealth()
	if !health.Ready {
		t.Fatalf("host segment: expected active server endpoint to be ready, got ready=%v reachable=%v mcp_ready=%v detail=%q", health.Ready, health.Reachable, health.MCPReady, health.LastError)
	}

	chatURL := app.ResolveMCPURL(staleConfigEndpoint, "", false, "streamable-http")
	if chatURL != activeEndpoint {
		t.Fatalf("chat segment: expected handoff endpoint %q from server, got %q", activeEndpoint, chatURL)
	}

	client := mcp.NewWithTransport(chatURL, "streamable-http", false)
	defer func() {
		_ = client.Close()
	}()
	requireNoErrSegment(t, "chat", client.Initialize(context.Background()))

	in := bytes.NewBufferString("/list src\n/quit\n")
	out := &bytes.Buffer{}
	chatOpts := chat.Options{
		MCPURL:    chatURL,
		Transport: "streamable-http",
		Model:     "mistral-small-latest",
		JSON:      true,
	}
	requireNoErrSegment(t, "chat", chat.RunJSONLoopWithIO(context.Background(), client, chatOpts, in, out))

	events := decodeModeFlowEvents(t, "chat", out.Bytes())
	if len(events) < 3 {
		t.Fatalf("chat segment: expected at least 3 events (session/tool_result/exit), got %d", len(events))
	}

	var sawToolResult bool
	for _, evt := range events {
		typ, _ := evt["type"].(string)
		if typ != "tool_result" {
			continue
		}
		data, _ := evt["data"].(map[string]any)
		if data["tool"] == protocol.ToolNameListFiles {
			sawToolResult = true
			break
		}
	}
	if !sawToolResult {
		t.Fatalf("chat segment: expected tool_result for %s", protocol.ToolNameListFiles)
	}

	if !calls.HasToolCall(protocol.ToolNameListFiles) {
		t.Fatalf("chat segment: expected MCP tools/call for %s, got calls=%v", protocol.ToolNameListFiles, calls.ToolCalls())
	}

	cfg := config.Default()
	cfg.MCP.URL = staleConfigEndpoint

	voiceURL := app.ResolveMCPURL(cfg.MCP.URL, "", false, cfg.MCP.Transport)
	if voiceURL != activeEndpoint {
		t.Fatalf("voice segment: expected handoff endpoint %q from server, got %q", activeEndpoint, voiceURL)
	}

	voiceOpts := app.BuildVoiceOptions(cfg, voiceURL, "", "", true, false, "")
	if voiceOpts.MCPURL != activeEndpoint {
		t.Fatalf("voice segment: expected options MCPURL %q, got %q", activeEndpoint, voiceOpts.MCPURL)
	}
	if voiceOpts.Transport != cfg.MCP.Transport {
		t.Fatalf("voice segment: expected transport %q, got %q", cfg.MCP.Transport, voiceOpts.Transport)
	}
}

func TestModeFlowPrefersActiveEndpointOverStaleConfig(t *testing.T) {
	setTestConfigDir(t)
	_ = host.ClearState()
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	server, _ := newModeFlowMCPServer(t)
	defer server.Close()

	activeEndpoint := server.URL
	staleConfigEndpoint := "http://127.0.0.1:2/mcp"

	requireNoErrSegment(t, "host", host.SaveState(host.State{
		PID:       os.Getpid(),
		StartedAt: "now",
		MCPURL:    activeEndpoint,
	}))

	health := host.CheckHealth()
	if !health.Ready {
		t.Fatalf("host segment: expected server ready for runtime endpoint preference check, got ready=%v detail=%q", health.Ready, health.LastError)
	}

	resolved := app.ResolveMCPURL(staleConfigEndpoint, "", false, "streamable-http")
	if resolved != activeEndpoint {
		t.Fatalf("chat segment: expected runtime endpoint %q to override stale config %q, got %q", activeEndpoint, staleConfigEndpoint, resolved)
	}

	cfg := config.Default()
	cfg.MCP.URL = staleConfigEndpoint
	voiceOpts := app.BuildVoiceOptions(cfg, resolved, "", "", true, false, "")
	if voiceOpts.MCPURL != activeEndpoint {
		t.Fatalf("voice segment: expected runtime endpoint %q to override stale config %q, got %q", activeEndpoint, staleConfigEndpoint, voiceOpts.MCPURL)
	}
}

func requireNoErrSegment(t *testing.T, segment string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s segment: unexpected error: %v", segment, err)
	}
}

func decodeModeFlowEvents(t *testing.T, segment string, payload []byte) []map[string]any {
	t.Helper()

	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	events := make([]map[string]any, 0)
	for scanner.Scan() {
		evt := map[string]any{}
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			t.Fatalf("%s segment: invalid json event: %v", segment, err)
		}
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s segment: failed to scan events: %v", segment, err)
	}
	return events
}

type modeFlowCalls struct {
	mu        sync.Mutex
	toolCalls []string
}

func (m *modeFlowCalls) recordToolCall(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolCalls = append(m.toolCalls, name)
}

func (m *modeFlowCalls) HasToolCall(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, got := range m.toolCalls {
		if got == name {
			return true
		}
	}
	return false
}

func (m *modeFlowCalls) ToolCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	copyCalls := make([]string, len(m.toolCalls))
	copy(copyCalls, m.toolCalls)
	return copyCalls
}

func newModeFlowMCPServer(t *testing.T) (*httptest.Server, *modeFlowCalls) {
	t.Helper()

	const sessionID = "sess-mode-flow"
	calls := &modeFlowCalls{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		req := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			w.Header().Set(protocol.MCPSessionHeader, sessionID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]any{"capabilities": map[string]any{"tools": map[string]any{}}},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get(protocol.MCPSessionHeader) != sessionID {
				http.Error(w, "unknown session", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": protocol.ToolNameListFiles, "description": "list files"},
						{"name": protocol.ToolNameSearch, "description": "search"},
						{"name": protocol.ToolNameOpenFile, "description": "open file"},
						{"name": protocol.ToolNameStats, "description": "stats"},
						{"name": protocol.ToolNameAsk, "description": "ask"},
					},
				},
			})
		case "tools/call":
			if r.Header.Get(protocol.MCPSessionHeader) != sessionID {
				http.Error(w, "unknown session", http.StatusNotFound)
				return
			}
			params, _ := req["params"].(map[string]any)
			name, _ := params["name"].(string)
			calls.recordToolCall(name)

			result := map[string]any{"isError": false, "content": []map[string]any{{"type": "text", "text": "ok"}}}
			if name == protocol.ToolNameListFiles {
				result["structuredContent"] = map[string]any{
					"files": []map[string]any{{"rel_path": "src/main.go", "kind": "text"}},
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  result,
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	return server, calls
}
