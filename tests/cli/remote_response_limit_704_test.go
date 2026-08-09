package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// remoteOversizedFiller is the padding byte that an oversized upstream body
// repeats. The tests check that no run of it reaches the CLI output.
const remoteOversizedFiller = "A"

// writeOversizedRemoteBody streams more than protocol.MaxResponseBytes to the
// client. It stops at the first write error, because a correct client closes
// the connection at the cap. declare selects a declared Content-Length or a
// chunked body with no declared length.
func writeOversizedRemoteBody(w http.ResponseWriter, declare bool) {
	const chunkSize = 64 << 10
	total := protocol.MaxResponseBytes + (1 << 20)
	if declare {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
	}
	chunk := bytes.Repeat([]byte(remoteOversizedFiller), chunkSize)
	flusher, _ := w.(http.Flusher)
	for sent := int64(0); sent < total; sent += chunkSize {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if flusher != nil && !declare {
			flusher.Flush()
		}
	}
}

// newOversizedMCPServer answers initialize normally and then sends an
// oversized tools/call response with the supplied status code.
func newOversizedMCPServer(t *testing.T, toolStatus int, declare bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string `json:"method"`
			ID     int64  `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Method == protocol.RPCMethodInitialize {
			w.Header().Set(protocol.MCPSessionHeader, "session-704")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      payload.ID,
				"result":  map[string]interface{}{"protocolVersion": protocol.ProtocolDefaultVersion},
			})
			return
		}
		w.WriteHeader(toolStatus)
		writeOversizedRemoteBody(w, declare)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runRemoteCommand runs one shim command against url and returns the exit code
// with the combined output.
func runRemoteCommand(t *testing.T, url string, args ...string) (int, string, string) {
	t.Helper()
	tmp := t.TempDir()
	writeConnectionMetadata(t, tmp, url, "")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := 0
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), args)
	})
	return code, stdout.String(), stderr.String()
}

// assertRemoteLimitError checks that the shim failed, named the cap, and kept
// upstream content out of the message.
func assertRemoteLimitError(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code == 0 {
		t.Fatalf("command must fail on an oversized response: stdout=%q", truncateOutput(stdout))
	}
	wantLimit := fmt.Sprintf("%d-byte limit", protocol.MaxResponseBytes)
	if !strings.Contains(stderr, wantLimit) {
		t.Fatalf("error must name the limit %q, got %q", wantLimit, truncateOutput(stderr))
	}
	if !strings.Contains(stderr, "too large") {
		t.Fatalf("error must separate an oversized body from a decode failure, got %q", truncateOutput(stderr))
	}
	if strings.Contains(stderr, strings.Repeat(remoteOversizedFiller, 64)) {
		t.Fatalf("error must not echo upstream content, got %q", truncateOutput(stderr))
	}
	if len(stderr) > 1<<16 {
		t.Fatalf("stderr is %d bytes, want a short error", len(stderr))
	}
}

func truncateOutput(text string) string {
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

func TestSearchShimRejectsOversizedToolResponseWithDeclaredLength(t *testing.T) {
	srv := newOversizedMCPServer(t, http.StatusOK, true)
	code, stdout, stderr := runRemoteCommand(t, srv.URL, "search", "auth flow")
	assertRemoteLimitError(t, code, stdout, stderr)
}

func TestListFilesShimRejectsOversizedToolResponseWithChunkedBody(t *testing.T) {
	srv := newOversizedMCPServer(t, http.StatusOK, false)
	code, stdout, stderr := runRemoteCommand(t, srv.URL, "list-files")
	assertRemoteLimitError(t, code, stdout, stderr)
}

func TestOpenFileShimRejectsOversizedErrorBody(t *testing.T) {
	srv := newOversizedMCPServer(t, http.StatusInternalServerError, false)
	code, stdout, stderr := runRemoteCommand(t, srv.URL, "open-file", "docs/spec.md")
	assertRemoteLimitError(t, code, stdout, stderr)
}

func TestSearchShimRejectsOversizedInitializeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(protocol.MCPSessionHeader, "session-704")
		writeOversizedRemoteBody(w, false)
	}))
	defer srv.Close()

	code, stdout, stderr := runRemoteCommand(t, srv.URL, "search", "auth flow")
	assertRemoteLimitError(t, code, stdout, stderr)
}

// TestRemoteShimRefusesRedirect covers the credential half of issue #704:
// connection.json headers must never reach a redirect target.
func TestRemoteShimRefusesRedirect(t *testing.T) {
	var (
		mu            sync.Mutex
		redirectCount int
		gotHeader     string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		redirectCount++
		gotHeader = r.Header.Get(protocol.MCPProtocolVersionHeader)
		mu.Unlock()
		w.Header().Set(protocol.MCPSessionHeader, "session-704")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"protocolVersion": protocol.ProtocolDefaultVersion},
		})
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	code, stdout, _ := runRemoteCommand(t, srv.URL, "search", "auth flow")
	if code == 0 {
		t.Fatalf("command must fail on a redirect: stdout=%q", truncateOutput(stdout))
	}
	mu.Lock()
	defer mu.Unlock()
	if redirectCount != 0 {
		t.Fatalf("the shim sent %d requests to the redirect target, want 0", redirectCount)
	}
	if gotHeader != "" {
		t.Fatalf("connection headers reached the redirect target: %q", gotHeader)
	}
}
