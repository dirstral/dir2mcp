package test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	coreconfig "dir2mcp/internal/config"
	"dir2mcp/internal/dirstral/host"
	"dir2mcp/internal/protocol"
)

func TestComputeMCPURLDeterministic(t *testing.T) {
	url, deterministic := host.ComputeMCPURL("0.0.0.0:8087", "mcp")
	if !deterministic {
		t.Fatalf("expected deterministic url")
	}
	if url != "http://127.0.0.1:8087/mcp" {
		t.Fatalf("unexpected url: %s", url)
	}

	url, deterministic = host.ComputeMCPURL("127.0.0.1:0", "/mcp")
	if deterministic {
		t.Fatalf("expected non-deterministic for ephemeral port, got %s", url)
	}
}

func TestStatusReturnsNilWhenMCPReady(t *testing.T) {
	setTestConfigDir(t)

	server := newMockMCPServer(t, true)
	defer server.Close()

	s := host.State{PID: os.Getpid(), StartedAt: "now", MCPURL: server.URL}
	if err := host.SaveState(s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	if err := host.Status(); err != nil {
		t.Fatalf("expected ready status, got error: %v", err)
	}
}

func TestStatusReturnsErrorWhenMCPUnready(t *testing.T) {
	setTestConfigDir(t)

	server := newMockMCPServer(t, false)
	defer server.Close()

	s := host.State{PID: os.Getpid(), StartedAt: "now", MCPURL: server.URL}
	if err := host.SaveState(s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	err := host.Status()
	if err == nil {
		t.Fatalf("expected non-nil status error when MCP is unready")
	}
	if !strings.Contains(err.Error(), "mcp server not ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStatusIncludesConnectionContractDetails(t *testing.T) {
	setTestConfigDir(t)

	server := newMockMCPServer(t, true)
	defer server.Close()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".dir2mcp"), 0o755); err != nil {
		t.Fatalf("mkdir .dir2mcp: %v", err)
	}
	writeConnectionFixture(t, root, map[string]any{
		"url": server.URL,
		"headers": map[string]any{
			protocol.MCPProtocolVersionHeader: coreconfig.DefaultProtocolVersion,
			"Authorization":                   "Bearer <token-from-secret.token>",
		},
		"session": map[string]any{
			"uses_mcp_session_id":    true,
			"header_name":            protocol.MCPSessionHeader,
			"assigned_on_initialize": true,
		},
		"token_source": "secret.token",
		"token_file":   filepath.Join(root, ".dir2mcp", "secret.token"),
	})

	s := host.State{PID: os.Getpid(), StartedAt: "now", MCPURL: server.URL, RootDir: root}
	if err := host.SaveState(s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	out, err := captureStdout(func() error {
		return host.Status()
	})
	if err != nil {
		t.Fatalf("status returned error: %v", err)
	}

	clean := stripANSI(out)
	if !strings.Contains(clean, "protocol="+coreconfig.DefaultProtocolVersion) {
		t.Fatalf("expected protocol details in output, got: %q", clean)
	}
	if !strings.Contains(clean, "session_header="+protocol.MCPSessionHeader) {
		t.Fatalf("expected session header details in output, got: %q", clean)
	}
	if !strings.Contains(clean, "auth_source=secret.token") {
		t.Fatalf("expected auth source details in output, got: %q", clean)
	}
}

func TestStatusConnectionContractFallbacksToUnknown(t *testing.T) {
	setTestConfigDir(t)

	server := newMockMCPServer(t, true)
	defer server.Close()

	s := host.State{PID: os.Getpid(), StartedAt: "now", MCPURL: server.URL, RootDir: t.TempDir()}
	if err := host.SaveState(s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	out, err := captureStdout(func() error {
		return host.Status()
	})
	if err != nil {
		t.Fatalf("status returned error: %v", err)
	}

	clean := stripANSI(out)
	if !strings.Contains(clean, "protocol=unknown") {
		t.Fatalf("expected protocol fallback in output, got: %q", clean)
	}
	if !strings.Contains(clean, "session_header=unknown") {
		t.Fatalf("expected session header fallback in output, got: %q", clean)
	}
	if !strings.Contains(clean, "auth_source=unknown") {
		t.Fatalf("expected auth source fallback in output, got: %q", clean)
	}
}

func TestStatusDerivesAuthSourceFromTokenFileIndicator(t *testing.T) {
	setTestConfigDir(t)

	server := newMockMCPServer(t, true)
	defer server.Close()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".dir2mcp"), 0o755); err != nil {
		t.Fatalf("mkdir .dir2mcp: %v", err)
	}
	writeConnectionFixture(t, root, map[string]any{
		"url": server.URL,
		"headers": map[string]any{
			protocol.MCPProtocolVersionHeader: coreconfig.DefaultProtocolVersion,
		},
		"session": map[string]any{
			"uses_mcp_session_id": false,
			"header_name":         protocol.MCPSessionHeader,
		},
		"token_file": filepath.Join(root, ".dir2mcp", "secret.token"),
	})

	s := host.State{PID: os.Getpid(), StartedAt: "now", MCPURL: server.URL, RootDir: root}
	if err := host.SaveState(s); err != nil {
		t.Fatalf("save state: %v", err)
	}
	t.Cleanup(func() {
		_ = host.ClearState()
	})

	out, err := captureStdout(func() error {
		return host.Status()
	})
	if err != nil {
		t.Fatalf("status returned error: %v", err)
	}

	clean := stripANSI(out)
	if !strings.Contains(clean, "auth_source=file") {
		t.Fatalf("expected auth source fallback to file indicator, got: %q", clean)
	}
	if !strings.Contains(clean, "session_header=unknown") {
		t.Fatalf("expected session header to stay unknown when disabled, got: %q", clean)
	}
}

func TestStatusRemoteReturnsNilWhenReady(t *testing.T) {
	server := newMockMCPServer(t, true)
	defer server.Close()

	if err := host.StatusRemote(context.Background(), server.URL); err != nil {
		t.Fatalf("expected ready remote status, got error: %v", err)
	}
}

func TestStatusRemoteReturnsErrorForEmptyEndpoint(t *testing.T) {
	err := host.StatusRemote(context.Background(), " ")
	if err == nil {
		t.Fatalf("expected error for empty endpoint")
	}
	if !strings.Contains(err.Error(), "mcp server not ready") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func captureStdout(fn func() error) (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer func() {
		os.Stdout = old
	}()
	defer func() {
		_ = r.Close()
	}()
	defer func() {
		_ = w.Close()
	}()
	os.Stdout = w

	var buf bytes.Buffer
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&buf, r)
		copyDone <- copyErr
	}()

	fnErr := fn()
	closeErr := w.Close()
	copyErr := <-copyDone

	if joinedErr := errors.Join(fnErr, closeErr, copyErr); joinedErr != nil {
		return buf.String(), joinedErr
	}

	return buf.String(), nil
}

func writeConnectionFixture(t *testing.T, root string, payload map[string]any) {
	t.Helper()

	connectionJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal connection payload: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, ".dir2mcp", "connection.json"), connectionJSON, 0o644); err != nil {
		t.Fatalf("write connection.json: %v", err)
	}
}

func stripANSI(value string) string {
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return ansi.ReplaceAllString(value, "")
}

func newMockMCPServer(t *testing.T, ready bool) *httptest.Server {
	t.Helper()

	const sessionID = "sess-test"

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		method, _ := req["method"].(string)
		id, hasID := req["id"]

		switch method {
		case protocol.RPCMethodInitialize:
			w.Header().Set(protocol.MCPSessionHeader, sessionID)
			w.Header().Set("Content-Type", "application/json")
			if !hasID {
				http.Error(w, "missing id", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"capabilities": map[string]any{"tools": map[string]any{}},
				},
			})
		case protocol.RPCMethodNotificationsInitialized:
			w.WriteHeader(http.StatusAccepted)
		case protocol.RPCMethodToolsList:
			if r.Header.Get(protocol.MCPSessionHeader) != sessionID {
				http.Error(w, "unknown session", http.StatusNotFound)
				return
			}
			if !ready {
				http.Error(w, "warming up", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"tools": []map[string]any{},
				},
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}
