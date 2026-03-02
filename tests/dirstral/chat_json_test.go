package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/protocol"
)

func decodeEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	events := make([]map[string]any, 0)
	for s.Scan() {
		var evt map[string]any
		if err := json.Unmarshal(s.Bytes(), &evt); err != nil {
			t.Fatalf("invalid json line: %v", err)
		}
		events = append(events, evt)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	return events
}

func assertEventEnvelope(t *testing.T, evt map[string]any) {
	t.Helper()
	if evt["version"] != "v1" {
		t.Fatalf("unexpected version in event: %#v", evt)
	}
	if _, ok := evt["type"].(string); !ok {
		t.Fatalf("event type must be string: %#v", evt)
	}
	if _, ok := evt["data"].(map[string]any); !ok {
		t.Fatalf("event data must be object: %#v", evt)
	}
}

func reportHandlerError(errCh chan error, err error) {
	select {
	case errCh <- err:
	default:
	}
}

func assertNoHandlerError(t *testing.T, errCh chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		t.Fatalf("test server handler error: %v", err)
	default:
	}
}

func TestAskTopKForModel(t *testing.T) {
	if got := chat.AskTopKForModel("mistral-large-latest"); got != 12 {
		t.Fatalf("unexpected top-k for large model: %d", got)
	}
	if got := chat.AskTopKForModel("mistral-small-latest"); got != 6 {
		t.Fatalf("unexpected top-k for small model: %d", got)
	}
	if got := chat.AskTopKForModel("custom-model"); got != 8 {
		t.Fatalf("unexpected top-k default: %d", got)
	}
}

func TestParseInputUsesModelProfileForAsk(t *testing.T) {
	parsed := chat.ParseInput("how does this work?", "mistral-small-latest")
	if parsed.Tool != protocol.ToolNameAsk {
		t.Fatalf("expected %s, got %q", protocol.ToolNameAsk, parsed.Tool)
	}
	if got := parsed.Args["k"]; got != 6 {
		t.Fatalf("expected k=6 for small model, got %v", got)
	}
}

func TestChatJSONOutputStructureAndModelPropagation(t *testing.T) {
	var askK any
	calledTools := make([]string, 0)
	handlerErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reportHandlerError(handlerErrCh, fmt.Errorf("decode request: %w", err))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set(protocol.MCPSessionHeader, "sess-test")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case protocol.RPCMethodToolsCall:
			params, _ := req["params"].(map[string]any)
			name, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]any)
			calledTools = append(calledTools, name)
			switch name {
			case protocol.ToolNameSearch:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result": map[string]any{
						"isError": false,
						"content": []map[string]any{{"type": "text", "text": "search ok"}},
						"structuredContent": map[string]any{
							"hits": []map[string]any{{
								"rel_path": "src/main.go",
								"snippet":  "search snippet",
								"score":    0.88,
								"span":     map[string]any{"kind": "lines", "start_line": 1, "end_line": 4},
							}},
						},
					},
				})
			case protocol.ToolNameAsk:
				askK = args["k"]
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result": map[string]any{
						"isError": false,
						"content": []map[string]any{{"type": "text", "text": "answer"}},
						"structuredContent": map[string]any{
							"answer": "answer",
							"citations": []map[string]any{{
								"rel_path": "src/main.go",
								"span":     map[string]any{"kind": "lines", "start_line": 3, "end_line": 9},
							}},
						},
					},
				})
			default:
				reportHandlerError(handlerErrCh, fmt.Errorf("unexpected tools/call name: %s", name))
				http.Error(w, "unexpected tool", http.StatusBadRequest)
				return
			}
		default:
			reportHandlerError(handlerErrCh, fmt.Errorf("unexpected method: %s", method))
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
	}))
	defer server.Close()

	client := mcp.New(server.URL, false)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	in := bytes.NewBufferString("what changed\n/quit\n")
	out := &bytes.Buffer{}
	opts := chat.Options{MCPURL: server.URL, Transport: "streamable-http", Model: "mistral-large-latest", JSON: true}
	if err := chat.RunJSONLoopWithIO(context.Background(), client, opts, in, out); err != nil {
		t.Fatalf("json loop: %v", err)
	}
	assertNoHandlerError(t, handlerErrCh)

	if askK != float64(12) {
		t.Fatalf("expected ask k=12 from model profile, got %v", askK)
	}
	if len(calledTools) != 2 || calledTools[0] != protocol.ToolNameSearch || calledTools[1] != protocol.ToolNameAsk {
		t.Fatalf("expected planner path search -> ask, got %v", calledTools)
	}

	events := decodeEvents(t, out.Bytes())
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(events))
	}
	for _, evt := range events {
		assertEventEnvelope(t, evt)
	}
	if events[0]["version"] != "v1" || events[0]["type"] != "session" {
		t.Fatalf("unexpected first event: %#v", events[0])
	}

	var toolEvent map[string]any
	for _, evt := range events {
		if evt["type"] == "tool_result" {
			toolEvent = evt
			break
		}
	}
	if toolEvent == nil {
		t.Fatalf("missing tool_result event")
	}
	data, _ := toolEvent["data"].(map[string]any)
	if data["tool"] != protocol.ToolNameAsk {
		t.Fatalf("unexpected tool_result tool: %v", data["tool"])
	}
	tools, _ := data["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected planner tool list in response, got %v", data["tools"])
	}
	if output, _ := data["output"].(string); !strings.HasPrefix(output, "Planner path") {
		t.Fatalf("expected analytical synthesis output to include planner path, got %q", output)
	}
	citations, _ := data["citations"].([]any)
	if len(citations) == 0 {
		t.Fatalf("expected citations in tool_result")
	}
	assertNoHandlerError(t, handlerErrCh)
}

func TestChatJSONHelpErrorAndExitEvents(t *testing.T) {
	handlerErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reportHandlerError(handlerErrCh, fmt.Errorf("decode request: %w", err))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set(protocol.MCPSessionHeader, "sess-test")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			reportHandlerError(handlerErrCh, fmt.Errorf("unexpected method: %s", method))
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
	}))
	defer server.Close()

	client := mcp.New(server.URL, false)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	in := bytes.NewBufferString("/help\n/open\n/quit\n")
	out := &bytes.Buffer{}
	opts := chat.Options{MCPURL: server.URL, Transport: "streamable-http", Model: "mistral-small-latest", JSON: true}
	if err := chat.RunJSONLoopWithIO(context.Background(), client, opts, in, out); err != nil {
		t.Fatalf("json loop: %v", err)
	}
	assertNoHandlerError(t, handlerErrCh)

	events := decodeEvents(t, out.Bytes())
	if len(events) != 4 {
		t.Fatalf("expected exactly 4 events (session/help/error/exit), got %d", len(events))
	}
	for _, evt := range events {
		assertEventEnvelope(t, evt)
	}

	if events[0]["type"] != "session" {
		t.Fatalf("expected first event type=session, got %v", events[0]["type"])
	}
	if events[1]["type"] != "help" {
		t.Fatalf("expected second event type=help, got %v", events[1]["type"])
	}
	if events[2]["type"] != "error" {
		t.Fatalf("expected third event type=error, got %v", events[2]["type"])
	}
	if events[3]["type"] != "exit" {
		t.Fatalf("expected final event type=exit, got %v", events[3]["type"])
	}

	helpData, _ := events[1]["data"].(map[string]any)
	if _, ok := helpData["text"].(string); !ok {
		t.Fatalf("help event missing text field: %#v", events[1])
	}
	errData, _ := events[2]["data"].(map[string]any)
	if _, ok := errData["message"].(string); !ok {
		t.Fatalf("error event missing message field: %#v", events[2])
	}
	exitData, _ := events[3]["data"].(map[string]any)
	if exitData["reason"] != "user" {
		t.Fatalf("expected exit reason=user, got %#v", events[3])
	}
	assertNoHandlerError(t, handlerErrCh)
}

func TestChatJSONClearEvent(t *testing.T) {
	handlerErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reportHandlerError(handlerErrCh, fmt.Errorf("decode request: %w", err))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		switch method {
		case "initialize":
			w.Header().Set(protocol.MCPSessionHeader, "sess-test")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			reportHandlerError(handlerErrCh, fmt.Errorf("unexpected method: %s", method))
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
	}))
	defer server.Close()

	client := mcp.New(server.URL, false)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	in := bytes.NewBufferString("/clear\n/quit\n")
	out := &bytes.Buffer{}
	opts := chat.Options{MCPURL: server.URL, Transport: "streamable-http", Model: "mistral-small-latest", JSON: true}
	if err := chat.RunJSONLoopWithIO(context.Background(), client, opts, in, out); err != nil {
		t.Fatalf("json loop: %v", err)
	}
	assertNoHandlerError(t, handlerErrCh)

	events := decodeEvents(t, out.Bytes())
	if len(events) != 3 {
		t.Fatalf("expected exactly 3 events (session/cleared/exit), got %d", len(events))
	}
	if events[1]["type"] != "cleared" {
		t.Fatalf("expected second event type=cleared, got %v", events[1]["type"])
	}
	assertNoHandlerError(t, handlerErrCh)
}
