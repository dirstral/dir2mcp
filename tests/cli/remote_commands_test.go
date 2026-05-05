package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dir2mcp/internal/cli"
	"dir2mcp/internal/protocol"
)

func TestSearchCommandUsesConnectionMetadataAndPrintsJSON(t *testing.T) {
	ts := newMCPTestServer(t, func(name string, args map[string]interface{}) map[string]interface{} {
		if name != protocol.ToolNameSearch {
			t.Fatalf("unexpected tool name: %s", name)
		}
		if query, _ := args["query"].(string); query != "auth flow" {
			t.Fatalf("unexpected query: %#v", args["query"])
		}
		return map[string]interface{}{
			"query":             "auth flow",
			"k":                 10,
			"index_used":        "text",
			"indexing_complete": true,
			"hits": []interface{}{
				map[string]interface{}{"rel_path": "docs/a.md", "score": 0.7},
			},
		}
	})
	defer ts.Close()

	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, ts.URL, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"search", "auth flow"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal search output: %v raw=%s", err, stdout.String())
	}
	if payload["query"] != "auth flow" {
		t.Fatalf("unexpected query in payload: %#v", payload["query"])
	}
}

func TestOpenFileCommandUsesConnectionMetadataAndPrintsJSON(t *testing.T) {
	ts := newMCPTestServer(t, func(name string, args map[string]interface{}) map[string]interface{} {
		if name != protocol.ToolNameOpenFile {
			t.Fatalf("unexpected tool name: %s", name)
		}
		if relPath, _ := args["rel_path"].(string); relPath != "docs/spec.md" {
			t.Fatalf("unexpected rel_path: %#v", args["rel_path"])
		}
		return map[string]interface{}{
			"rel_path":  "docs/spec.md",
			"doc_type":  "md",
			"content":   "hello",
			"truncated": false,
		}
	})
	defer ts.Close()

	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, ts.URL, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"open-file", "docs/spec.md"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal open-file output: %v raw=%s", err, stdout.String())
	}
	if payload["rel_path"] != "docs/spec.md" {
		t.Fatalf("unexpected rel_path in payload: %#v", payload["rel_path"])
	}
}

func TestListFilesCommandPrintsOneJSONLinePerFile(t *testing.T) {
	ts := newMCPTestServer(t, func(name string, args map[string]interface{}) map[string]interface{} {
		if name != protocol.ToolNameListFiles {
			t.Fatalf("unexpected tool name: %s", name)
		}
		return map[string]interface{}{
			"limit":  200,
			"offset": 0,
			"total":  2,
			"files": []interface{}{
				map[string]interface{}{"rel_path": "a.md", "doc_type": "md", "size_bytes": 1, "mtime_unix": 1, "status": "ok", "deleted": false},
				map[string]interface{}{"rel_path": "b.go", "doc_type": "code", "size_bytes": 2, "mtime_unix": 2, "status": "ok", "deleted": false},
			},
		}
	})
	defer ts.Close()

	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, ts.URL, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"list-files"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 ndjson lines, got %d raw=%q", len(lines), stdout.String())
	}
	for i, line := range lines {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid json: %v line=%q", i, err, line)
		}
		if _, ok := obj["rel_path"]; !ok {
			t.Fatalf("line %d missing rel_path: %#v", i, obj)
		}
	}
}

func TestRemoteCommandsReturnNonZeroWhenServerIsUnavailable(t *testing.T) {
	unused := mustUnusedPort(t)
	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, fmt.Sprintf("http://127.0.0.1:%d/mcp", unused), "")

	commands := [][]string{{"search", "q"}, {"open-file", "docs/a.md"}, {"list-files"}}
	for _, cmdArgs := range commands {
		t.Run(strings.Join(cmdArgs, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIO(&stdout, &stderr)
			withWorkingDir(t, tmp, func() {
				code := app.RunWithContext(context.Background(), cmdArgs)
				if code == 0 {
					t.Fatalf("expected non-zero exit code for %v", cmdArgs)
				}
			})
			if strings.TrimSpace(stderr.String()) == "" {
				t.Fatalf("expected error output for %v", cmdArgs)
			}
		})
	}
}

func newMCPTestServer(t *testing.T, onToolCall func(name string, args map[string]interface{}) map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		defer r.Body.Close()

		var req struct {
			Method string `json:"method"`
			ID     int64  `json:"id"`
			Params struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.Method == protocol.RPCMethodInitialize {
			w.Header().Set(protocol.MCPSessionHeader, "session-1")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"protocolVersion": protocol.ProtocolDefaultVersion,
				},
			})
			return
		}

		if req.Method != protocol.RPCMethodToolsCall {
			t.Fatalf("unexpected method: %s", req.Method)
		}
		if got := r.Header.Get(protocol.MCPSessionHeader); got == "" {
			t.Fatal("expected MCP session header on tools/call")
		}

		structured := onToolCall(req.Params.Name, req.Params.Arguments)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"isError":           false,
				"structuredContent": structured,
				"content":           []interface{}{map[string]interface{}{"type": "text", "text": "ok"}},
			},
		})
	}))
}

func writeConnectionMetadata(t *testing.T, root, url, tokenFile string) {
	t.Helper()
	stateDir := filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	payload := map[string]interface{}{
		"transport": "mcp_streamable_http",
		"url":       url,
		"headers": map[string]string{
			protocol.MCPProtocolVersionHeader: protocol.ProtocolDefaultVersion,
		},
		"session": map[string]interface{}{
			"uses_mcp_session_id":    true,
			"header_name":            protocol.MCPSessionHeader,
			"assigned_on_initialize": true,
		},
		"public":       false,
		"token_source": "none",
	}
	if tokenFile != "" {
		payload["token_file"] = tokenFile
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal connection payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}
}

func mustUnusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	return p
}
