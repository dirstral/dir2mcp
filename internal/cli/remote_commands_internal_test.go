package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/protocol"
)

// fakeMCPServer answers initialize immediately and delays tools/call by
// toolDelay. The delay aborts early when the request context is canceled, so
// cancellation tests finish fast.
//
// It must also answer notifications/initialized with 202: since #656 the
// client completes the bs-005 handshake before any tool call and treats a
// failed notification as fatal, so a fake that rejects it fails every test
// here for a reason that has nothing to do with timeouts.
func fakeMCPServer(t *testing.T, toolDelay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch req.Method {
		case protocol.RPCMethodInitialize:
			w.Header().Set(protocol.MCPSessionHeader, "test-session")
			w.Header().Set("Content-Type", "application/json")
			writeRPCResult(w, req.ID, map[string]interface{}{})
		case protocol.RPCMethodNotificationsInitialized:
			w.WriteHeader(http.StatusAccepted)
		case protocol.RPCMethodToolsCall:
			select {
			case <-time.After(toolDelay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			writeRPCResult(w, req.ID, map[string]interface{}{
				"isError":           false,
				"structuredContent": map[string]interface{}{"ok": true},
			})
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
}

func writeRPCResult(w http.ResponseWriter, id int64, result interface{}) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// TestRemoteMCPClient_NoFlatClientTimeout pins the transport shape fixed by
// issue #662: no request-wide http.Client.Timeout, distinct initialize and
// tool-call deadlines, and a tool-call default sized for long LLM/OCR tools.
func TestRemoteMCPClient_NoFlatClientTimeout(t *testing.T) {
	client := newRemoteMCPClient("http://127.0.0.1:1/mcp", "", remoteConnection{})
	if client.httpClient.Timeout != 0 {
		t.Fatalf("httpClient.Timeout = %v, want 0: a flat client timeout aborts long tool calls (issue #662)", client.httpClient.Timeout)
	}
	if client.initializeTimeout != remoteInitializeTimeout {
		t.Fatalf("initializeTimeout = %v, want %v", client.initializeTimeout, remoteInitializeTimeout)
	}
	if client.toolCallTimeout != remoteToolCallTimeout {
		t.Fatalf("toolCallTimeout = %v, want %v", client.toolCallTimeout, remoteToolCallTimeout)
	}
	if client.toolCallTimeout <= client.initializeTimeout {
		t.Fatalf("toolCallTimeout (%v) must exceed initializeTimeout (%v): tool execution runs long, the handshake does not", client.toolCallTimeout, client.initializeTimeout)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport is %T, want *http.Transport with connect-level timeouts", client.httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("ResponseHeaderTimeout = %v, want 0: JSON-RPC sends headers only after the tool finishes", transport.ResponseHeaderTimeout)
	}
	if transport.TLSHandshakeTimeout != remoteTLSHandshakeTimeout {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", transport.TLSHandshakeTimeout, remoteTLSHandshakeTimeout)
	}
}

// TestRemoteMCPClient_ToolCallOutlivesInitializeTimeout verifies the core
// #662 fix: a tools/call that runs longer than the short initialize deadline
// still completes, because the initialize deadline no longer applies to tool
// execution.
func TestRemoteMCPClient_ToolCallOutlivesInitializeTimeout(t *testing.T) {
	server := fakeMCPServer(t, 300*time.Millisecond)
	defer server.Close()

	client := newRemoteMCPClient(server.URL, "", remoteConnection{})
	client.initializeTimeout = 100 * time.Millisecond

	result, err := client.CallTool(context.Background(), "ask", map[string]interface{}{})
	if err != nil {
		t.Fatalf("CallTool failed: %v (a short handshake deadline must not abort a long tool call)", err)
	}
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("CallTool result = %v, want ok=true", result)
	}
}

// TestRemoteMCPClient_CallerContextCancelsToolCall verifies the caller
// context stays the primary cancellation mechanism: the call detaches
// promptly, well before the default tool-call backstop.
func TestRemoteMCPClient_CallerContextCancelsToolCall(t *testing.T) {
	server := fakeMCPServer(t, 30*time.Second)
	defer server.Close()

	client := newRemoteMCPClient(server.URL, "", remoteConnection{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.CallTool(ctx, "ask", map[string]interface{}{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("CallTool succeeded, want caller-context cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool error = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("CallTool took %v to observe cancellation, want prompt return", elapsed)
	}
}

// TestRemoteMCPClient_InitializeTimeoutStillApplies verifies the handshake
// keeps its own short deadline even though tool calls do not.
func TestRemoteMCPClient_InitializeTimeoutStillApplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server notices a client disconnect.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	client := newRemoteMCPClient(server.URL, "", remoteConnection{})
	client.initializeTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := client.CallTool(context.Background(), "ask", map[string]interface{}{})
	if err == nil {
		t.Fatal("CallTool succeeded, want initialize timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallTool error = %v, want context.DeadlineExceeded from initialize", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("initialize took %v to time out, want ~100ms", elapsed)
	}
}

// TestRemoteMCPClient_ToolCallTimeoutConfigurable verifies the tool-call
// backstop is honored when configured.
func TestRemoteMCPClient_ToolCallTimeoutConfigurable(t *testing.T) {
	server := fakeMCPServer(t, 30*time.Second)
	defer server.Close()

	client := newRemoteMCPClient(server.URL, "", remoteConnection{})
	client.toolCallTimeout = 100 * time.Millisecond

	start := time.Now()
	_, err := client.CallTool(context.Background(), "ask", map[string]interface{}{})
	if err == nil {
		t.Fatal("CallTool succeeded, want tool-call timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallTool error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("tool call took %v to time out, want ~100ms", elapsed)
	}
}

// TestWithCallTimeout_NonPositiveDisablesDeadline verifies a non-positive
// timeout leaves the caller context unbounded.
func TestWithCallTimeout_NonPositiveDisablesDeadline(t *testing.T) {
	ctx, cancel := withCallTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("withCallTimeout(ctx, 0) set a deadline, want none")
	}

	ctx2, cancel2 := withCallTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, ok := ctx2.Deadline(); !ok {
		t.Fatal("withCallTimeout(ctx, 1s) set no deadline, want one")
	}
}
