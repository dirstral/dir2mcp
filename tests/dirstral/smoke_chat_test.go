package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"dir2mcp/internal/dirstral/chat"
	"dir2mcp/internal/dirstral/mcp"
	"dir2mcp/internal/protocol"
)

func TestSmokeChatJSONOverStdio(t *testing.T) {
	envPath, err := exec.LookPath("env")
	if err != nil {
		envPath = "env"
	}
	endpoint := fmt.Sprintf("%s GO_WANT_HELPER_PROCESS=1 %s -test.run=TestHelperProcessChatSmokeMCPStdio --", envPath, os.Args[0])

	client := mcp.NewWithTransport(endpoint, "stdio", false)
	defer func() {
		_ = client.Close()
	}()
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize stdio mcp client: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("list tools: %v", err)
	}

	in := bytes.NewBufferString("/list src\n/quit\n")
	out := &bytes.Buffer{}
	opts := chat.Options{
		MCPURL:    endpoint,
		Transport: "stdio",
		Model:     "mistral-small-latest",
		JSON:      true,
	}
	if err := chat.RunJSONLoopWithIO(context.Background(), client, opts, in, out); err != nil {
		t.Fatalf("chat run failed: %v", err)
	}

	events := decodeEvents(t, out.Bytes())
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(events))
	}
	if events[0]["type"] != "session" {
		t.Fatalf("first event must be session, got %v", events[0]["type"])
	}

	var toolEvent map[string]any
	var exitEvent map[string]any
	for _, evt := range events {
		if evt["type"] == "tool_result" {
			toolEvent = evt
		}
		if evt["type"] == "exit" {
			exitEvent = evt
		}
	}
	if toolEvent == nil {
		t.Fatalf("missing tool_result event")
	}
	if exitEvent == nil {
		t.Fatalf("missing exit event")
	}
	data, _ := toolEvent["data"].(map[string]any)
	if data["tool"] != protocol.ToolNameListFiles {
		t.Fatalf("expected %s call, got %v", protocol.ToolNameListFiles, data["tool"])
	}
}

func TestHelperProcessChatSmokeMCPStdio(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)

	for {
		payload, err := helperReadMessage(reader)
		if err != nil {
			os.Exit(0)
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			os.Exit(1)
		}
		method, _ := req["method"].(string)
		id := req["id"]

		switch method {
		case "initialize":
			helperWriteJSON(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"ok": true}})
		case "notifications/initialized":
			continue
		case "tools/list":
			helperWriteJSON(map[string]any{
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
			helperWriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"isError": false,
					"content": []map[string]any{{"type": "text", "text": "src/main.go"}},
					"structuredContent": map[string]any{
						"files": []map[string]any{{"rel_path": "src/main.go", "kind": "text"}},
					},
				},
			})
		default:
			helperWriteJSON(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}
