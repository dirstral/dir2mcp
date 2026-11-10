package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// fakeRechecker records the last request and answers with a canned result, so
// the tool layer (SPEC §15.13) is tested without a watcher.
type fakeRechecker struct {
	lastRel       string
	lastRecursive bool
	calls         int
	result        ingest.RecheckResult
	err           error
}

func (f *fakeRechecker) RecheckPath(_ context.Context, relPath string, recursive bool) (ingest.RecheckResult, error) {
	f.calls++
	f.lastRel = relPath
	f.lastRecursive = recursive
	if f.err != nil {
		return ingest.RecheckResult{}, f.err
	}
	return f.result, nil
}

func listToolNames(t *testing.T, serverURL string, cfg config.Config) map[string]bool {
	t.Helper()
	sessionID := initializeSession(t, serverURL+cfg.MCPPath)
	resp := postRPC(t, serverURL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("tools/list status=%d body=%s", resp.StatusCode, string(payload))
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	names := make(map[string]bool, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

// TestMCPToolsList_RecheckPathOnlyWithRechecker: the tool is listed when, and
// only when, the server holds a rechecker (§15.13 Availability).
func TestMCPToolsList_RecheckPathOnlyWithRechecker(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	without := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer without.Close()
	if listToolNames(t, without.URL, cfg)[protocol.ToolNameRecheckPath] {
		t.Fatalf("%s listed by a server with no rechecker", protocol.ToolNameRecheckPath)
	}

	with := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithPathRechecker(&fakeRechecker{})).Handler())
	defer with.Close()
	if !listToolNames(t, with.URL, cfg)[protocol.ToolNameRecheckPath] {
		t.Fatalf("%s not listed by a server with a rechecker", protocol.ToolNameRecheckPath)
	}
}

// TestMCPToolsCallRecheckPath_UnavailableWithoutRechecker: a call that names
// the tool on a server that does not serve it is TOOL_UNAVAILABLE (§14.3), not
// the unknown-tool error.
func TestMCPToolsCallRecheckPath_UnavailableWithoutRechecker(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"a.txt"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCodeAndMessage(t, resp, protocol.ErrorCodeToolUnavailable, []string{"current mode"}, nil)

	// An unknown name keeps its own contract.
	resp2 := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"dir2mcp_no_such_tool","arguments":{}}}`)
	defer func() { _ = resp2.Body.Close() }()
	assertToolCallErrorCode(t, resp2, "METHOD_NOT_FOUND")
}

// TestMCPToolsCallRecheckPath_Success: the call reaches the rechecker with the
// parsed arguments and returns the four schema fields, nothing else.
func TestMCPToolsCallRecheckPath_Success(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	fake := &fakeRechecker{result: ingest.RecheckResult{RelPath: "docs/a.md", Kind: ingest.RecheckKindFile, Queued: true, Pending: 1}}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithPathRechecker(fake)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"docs/a.md","recursive":false}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if fake.calls != 1 || fake.lastRel != "docs/a.md" || fake.lastRecursive {
		t.Fatalf("rechecker saw calls=%d rel=%q recursive=%v; want 1, docs/a.md, false", fake.calls, fake.lastRel, fake.lastRecursive)
	}
	want := map[string]interface{}{"rel_path": "docs/a.md", "kind": "file", "queued": true, "pending": float64(1)}
	got := envelope.Result.StructuredContent
	if len(got) != len(want) {
		t.Fatalf("structuredContent has %d fields, want %d: %#v", len(got), len(want), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("structuredContent[%q] = %#v, want %#v", key, got[key], value)
		}
	}
	if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "text" || envelope.Result.Content[0].Text == "" {
		t.Fatalf("expected one text content item, got %#v", envelope.Result.Content)
	}
}

// TestMCPToolsCallRecheckPath_RecursiveDefaultsTrue: an omitted recursive is
// true (recheck_path.json default).
func TestMCPToolsCallRecheckPath_RecursiveDefaultsTrue(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	fake := &fakeRechecker{result: ingest.RecheckResult{RelPath: ".", Kind: ingest.RecheckKindDirectory, Queued: true, Pending: 1}}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithPathRechecker(fake)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"."}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if fake.calls != 1 || !fake.lastRecursive {
		t.Fatalf("rechecker saw calls=%d recursive=%v; want 1, true", fake.calls, fake.lastRecursive)
	}
}

// TestMCPToolsCallRecheckPath_ValidationErrors covers the §15.13 error codes
// the tool layer produces: MISSING_FIELD for an absent or empty rel_path,
// INVALID_FIELD for a bad type or an unknown argument, PATH_OUTSIDE_ROOT when
// the rechecker refuses the path.
func TestMCPToolsCallRecheckPath_ValidationErrors(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	fake := &fakeRechecker{}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithPathRechecker(fake)).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	cases := []struct {
		name string
		args string
		err  error
		code string
	}{
		{name: "absent rel_path", args: `{}`, code: "MISSING_FIELD"},
		{name: "empty rel_path", args: `{"rel_path":"   "}`, code: "MISSING_FIELD"},
		{name: "rel_path not a string", args: `{"rel_path":7}`, code: "INVALID_FIELD"},
		{name: "recursive not a boolean", args: `{"rel_path":"a.txt","recursive":"yes"}`, code: "INVALID_FIELD"},
		{name: "unknown argument", args: `{"rel_path":"a.txt","depth":2}`, code: "INVALID_FIELD"},
		{name: "outside root", args: `{"rel_path":"../etc/passwd"}`, err: model.ErrPathOutsideRoot, code: "PATH_OUTSIDE_ROOT"},
		{name: "watcher not running yet", args: `{"rel_path":"a.txt"}`, err: ingest.ErrWatchNotRunning, code: protocol.ErrorCodeIndexNotReady},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake.err = tc.err
			fake.calls = 0
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":%s}}`, 50+i, tc.args)
			resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, body)
			defer func() { _ = resp.Body.Close() }()
			assertToolCallErrorCode(t, resp, tc.code)
			if tc.err == nil && fake.calls != 0 {
				t.Fatalf("an invalid request reached the rechecker")
			}
		})
	}
}

// TestMCPToolsCallRecheckPath_RetryableOnlyWhileWatcherStarts: the not-running
// error is retryable, the outside-root error is not.
func TestMCPToolsCallRecheckPath_RetryableFlags(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	fake := &fakeRechecker{err: ingest.ErrWatchNotRunning}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithPathRechecker(fake)).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	read := func(id string) bool {
		resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
			`{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"dir2mcp_recheck_path","arguments":{"rel_path":"a.txt"}}}`)
		defer func() { _ = resp.Body.Close() }()
		var envelope struct {
			Result struct {
				StructuredContent struct {
					Error struct {
						Retryable bool `json:"retryable"`
					} `json:"error"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return envelope.Result.StructuredContent.Error.Retryable
	}
	if !read("70") {
		t.Fatal("ErrWatchNotRunning must map to a retryable error")
	}
	fake.err = errors.New("disk on fire")
	if read("71") {
		t.Fatal("an unexpected rechecker failure must not be reported as retryable")
	}
}
