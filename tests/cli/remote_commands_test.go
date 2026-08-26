package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/protocol"
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
	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, "http://127.0.0.1:0/mcp", "")

	commands := [][]string{{"ask", "q"}, {"search", "q"}, {"open-file", "docs/a.md"}, {"list-files"}}
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

func TestAskCommandUsesConnectionMetadataAndPrintsJSON(t *testing.T) {
	ts := newMCPTestServer(t, func(name string, args map[string]interface{}) map[string]interface{} {
		if name != protocol.ToolNameAsk {
			t.Fatalf("unexpected tool name: %s", name)
		}
		if question, _ := args["question"].(string); question != "what is indexed?" {
			t.Fatalf("unexpected question: %#v", args["question"])
		}
		return map[string]interface{}{
			"question":          "what is indexed?",
			"answer":            "Indexed docs are available.",
			"citations":         []interface{}{},
			"hits":              []interface{}{},
			"indexing_complete": true,
		}
	})
	defer ts.Close()

	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, ts.URL, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "ask", "what is indexed?"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ask output: %v raw=%s", err, stdout.String())
	}
	if payload["answer"] != "Indexed docs are available." {
		t.Fatalf("unexpected answer in payload: %#v", payload["answer"])
	}
}

// TestRemoteClientCompletesHandshakeBeforeToolTraffic pins the bundled
// client's bs-005-conformant wire sequence: initialize, then
// notifications/initialized carrying the session id and no id field, then the
// tool call. Every request must carry the pinned MCP-Protocol-Version header.
func TestRemoteClientCompletesHandshakeBeforeToolTraffic(t *testing.T) {
	type recorded struct {
		method  string
		hasID   bool
		session string
		proto   string
	}
	var (
		mu       sync.Mutex
		sequence []recorded
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		var method string
		_ = json.Unmarshal(req["method"], &method)
		_, hasID := req["id"]
		mu.Lock()
		sequence = append(sequence, recorded{
			method:  method,
			hasID:   hasID,
			session: r.Header.Get(protocol.MCPSessionHeader),
			proto:   r.Header.Get(protocol.MCPProtocolVersionHeader),
		})
		mu.Unlock()

		switch method {
		case protocol.RPCMethodInitialize:
			w.Header().Set(protocol.MCPSessionHeader, "session-656")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{}})
		case protocol.RPCMethodNotificationsInitialized:
			w.WriteHeader(http.StatusAccepted)
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 2,
				"result": map[string]interface{}{
					"isError":           false,
					"structuredContent": map[string]interface{}{"limit": 200, "offset": 0, "total": 0, "files": []interface{}{}},
				},
			})
		}
	}))
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

	mu.Lock()
	defer mu.Unlock()
	if len(sequence) != 3 {
		t.Fatalf("unexpected request count: %#v", sequence)
	}
	want := []string{protocol.RPCMethodInitialize, protocol.RPCMethodNotificationsInitialized, protocol.RPCMethodToolsCall}
	for i, method := range want {
		if sequence[i].method != method {
			t.Fatalf("request %d method=%q want=%q sequence=%#v", i, sequence[i].method, method, sequence)
		}
		if sequence[i].proto != protocol.ProtocolDefaultVersion {
			t.Fatalf("request %d %s=%q want=%q", i, protocol.MCPProtocolVersionHeader, sequence[i].proto, protocol.ProtocolDefaultVersion)
		}
	}
	if sequence[1].hasID {
		t.Fatalf("notifications/initialized must carry no id field: %#v", sequence[1])
	}
	if sequence[1].session != "session-656" || sequence[2].session != "session-656" {
		t.Fatalf("expected the assigned session id on post-initialize requests: %#v", sequence)
	}
}

// TestRemoteClientFailsWhenInitializedIsNotAccepted verifies the bundled
// client checks the HTTP 202 on notifications/initialized: a server that
// refuses the notification must fail the command instead of silently starting
// tool traffic on a half-open session.
func TestRemoteClientFailsWhenInitializedIsNotAccepted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var req struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case protocol.RPCMethodInitialize:
			w.Header().Set(protocol.MCPSessionHeader, "session-656")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": map[string]interface{}{}})
		case protocol.RPCMethodNotificationsInitialized:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "error": map[string]interface{}{"code": -32603, "message": "refused"}})
		default:
			t.Fatalf("tool traffic must not start after a refused handshake: %s", req.Method)
		}
	}))
	defer ts.Close()

	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, ts.URL, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"list-files"})
		if code == 0 {
			t.Fatalf("expected non-zero exit code, stdout=%s", stdout.String())
		}
	})
	if !strings.Contains(stderr.String(), "notifications/initialized") {
		t.Fatalf("expected the error to name the failed handshake step, stderr=%s", stderr.String())
	}
}

func newMCPTestServer(t *testing.T, onToolCall func(name string, args map[string]interface{}) map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		defer func() { _ = r.Body.Close() }()

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

		// bs-005: the bundled client must complete the handshake right after
		// initialize and carry the session id on the notification.
		if req.Method == protocol.RPCMethodNotificationsInitialized {
			if got := r.Header.Get(protocol.MCPSessionHeader); got == "" {
				t.Fatal("expected MCP session header on notifications/initialized")
			}
			w.WriteHeader(http.StatusAccepted)
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
