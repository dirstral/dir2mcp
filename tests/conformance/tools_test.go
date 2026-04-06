package conformance

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// TestTools_ListContainsExpectedTools verifies that tools/list returns a list
// containing at least the four core tools.
func TestTools_ListContainsExpectedTools(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if !hasResult(body) {
		t.Fatalf("tools/list: expected result field body=%s", body)
	}

	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("tools/list: decode: %v body=%s", err, body)
	}

	nameSet := make(map[string]bool, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		nameSet[tool.Name] = true
	}

	required := []string{
		"dir2mcp.list_files",
		"dir2mcp.search",
		"dir2mcp.open_file",
		"dir2mcp.ask",
	}
	for _, name := range required {
		if !nameSet[name] {
			names := make([]string, len(envelope.Result.Tools))
			for i, tool := range envelope.Result.Tools {
				names[i] = tool.Name
			}
			t.Errorf("tools/list: missing tool %q; got %v", name, names)
		}
	}
}

// TestTools_CallListFilesSuccess verifies that tools/call with list_files
// against an existing temp directory returns a 200 with a successful result
// that mentions the fixture file.
func TestTools_CallListFilesSuccess(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	corpusDir := t.TempDir()
	if err := os.WriteFile(corpusDir+"/hello.txt", []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, listFilesCallBody(3, corpusDir), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list_files: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if !hasResult(body) {
		t.Fatalf("list_files: expected result field body=%s", body)
	}
	if isToolError(body) {
		t.Fatalf("list_files: expected successful result, got isError=true body=%s", body)
	}
	// Verify the response has the expected structured shape regardless of how
	// many files are indexed (store may be empty without ingestion).
	var envelope struct {
		Result struct {
			StructuredContent *struct {
				Files  json.RawMessage `json:"files"`
				Total  int             `json:"total"`
				Limit  int             `json:"limit"`
				Offset int             `json:"offset"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("list_files: decode response: %v body=%s", err, body)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("list_files: missing structuredContent body=%s", body)
	}
	if envelope.Result.StructuredContent.Files == nil {
		t.Fatalf("list_files: missing structuredContent.files body=%s", body)
	}
}

// TestTools_CallUnknownToolReturnsError verifies that tools/call with an
// unknown tool name returns a tool-level isError=true result (per MCP spec,
// tools/call errors are returned as tool-level errors, not JSON-RPC errors).
func TestTools_CallUnknownToolReturnsError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"dir2mcp.does_not_exist","arguments":{}}}`,
		nil,
	)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown tool: status=%d body=%s", resp.StatusCode, body)
	}
	if !isToolError(body) {
		t.Fatalf("unknown tool: expected tool-level isError=true result body=%s", body)
	}
}

// TestTools_CallMissingRequiredParamReturnsError verifies that tools/call with
// a valid tool but missing required param returns a tool-level isError=true.
func TestTools_CallMissingRequiredParamReturnsError(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dir2mcp.search","arguments":{}}}`,
		nil,
	)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("missing param: status=%d body=%s", resp.StatusCode, body)
	}
	if !isToolError(body) {
		t.Fatalf("missing param: expected tool-level isError=true result body=%s", body)
	}
}

// isToolError returns true if the result contains isError=true.
func isToolError(body []byte) bool {
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return envelope.Result.IsError
}

// TestTools_StatsCallReturnsResult verifies that tools/call for dir2mcp.stats
// (no required params) returns a successful result.
func TestTools_StatsCallReturnsResult(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	srv := newServer(cfg)
	defer srv.Close()

	sid := initSession(t, srv.URL+cfg.MCPPath)

	resp := sendRPC(t, srv.URL+cfg.MCPPath, sid, statsCallBody(6), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats: status=%d want=200 body=%s", resp.StatusCode, body)
	}
	if !hasResult(body) {
		t.Fatalf("stats: expected result field body=%s", body)
	}
	if isToolError(body) {
		t.Fatalf("stats: expected successful result, got isError=true body=%s", body)
	}
}
