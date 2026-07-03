package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
)

// startConformanceServer boots an SDKTransport on a loopback listener and
// returns its base /mcp URL plus a stop func. It mirrors the setup used by the
// other transport tests but is shared across the conformance cases below.
func startConformanceServer(t *testing.T) (baseURL string, stop func()) {
	t.Helper()
	srv := mcp.NewServer(config.Config{MCPPath: "/mcp", AuthMode: "none"}, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	tr := mcp.NewSDKTransport(srv, ln, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, srv.Handler())
	}()

	stop = func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for Serve to exit")
		}
		_ = ln.Close()
	}
	return "http://" + ln.Addr().String() + "/mcp", stop
}

// initSession performs an initialize handshake and returns the session id.
func initSession(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("build initialize: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("initialize status: %d", resp.StatusCode)
	}
	sessionID := strings.TrimSpace(resp.Header.Get("MCP-Session-Id"))
	if sessionID == "" {
		t.Fatal("expected MCP-Session-Id from initialize")
	}
	return sessionID
}

// TestSDKTransport_PartialAcceptNegotiated verifies that a client sending only
// "Accept: application/json" (non-empty but partial) is accepted rather than
// 400'd by the SDK's dual-type requirement (#404, gap 2).
func TestSDKTransport_PartialAcceptNegotiated(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json") // partial: missing text/event-stream
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial Accept should be negotiated to 200, got %d: %s", resp.StatusCode, string(body))
	}
	if strings.TrimSpace(resp.Header.Get("MCP-Session-Id")) == "" {
		t.Fatal("expected session id despite partial Accept")
	}
}

// TestSDKTransport_DeleteTerminatesSession verifies DELETE terminates the
// session (204) and that the id is no longer usable afterward (#404, gap 3).
func TestSDKTransport_DeleteTerminatesSession(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}
	sessionID := initSession(t, client, url)

	delReq, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("build DELETE: %v", err)
	}
	delReq.Header.Set("MCP-Session-Id", sessionID)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 from DELETE, got %d", delResp.StatusCode)
	}

	// The terminated session id must no longer be accepted.
	listReq, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("build tools/list: %v", err)
	}
	listReq.Header.Set("Content-Type", "application/json")
	listReq.Header.Set("MCP-Session-Id", sessionID)
	listResp, err := client.Do(listReq)
	if err != nil {
		t.Fatalf("tools/list after DELETE: %v", err)
	}
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for terminated session, got %d", listResp.StatusCode)
	}
}

// TestSDKTransport_DeleteUnknownSession verifies DELETE with a missing/unknown
// session returns our canonical 404 SESSION_NOT_FOUND contract.
func TestSDKTransport_DeleteUnknownSession(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}

	// No session header at all.
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("build DELETE: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for DELETE without session, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "SESSION_NOT_FOUND") {
		t.Fatalf("expected SESSION_NOT_FOUND contract, got %s", string(body))
	}
}

// TestSDKTransport_GetUnsupported verifies GET is 405'd with an accurate Allow
// header advertising the supported verbs (#404, gap 3).
func TestSDKTransport_GetUnsupported(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", resp.StatusCode)
	}
	allow := resp.Header.Get("Allow")
	if !strings.Contains(allow, "POST") || !strings.Contains(allow, "DELETE") {
		t.Fatalf("expected Allow to advertise POST and DELETE, got %q", allow)
	}
}

// TestSDKTransport_CancelledNotificationForwarded verifies notifications/cancelled
// is accepted (202) rather than swallowed as an unknown method (#404, gap 1).
func TestSDKTransport_CancelledNotificationForwarded(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}
	sessionID := initSession(t, client, url)

	cancelReq, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"42","reason":"user aborted"}}`))
	if err != nil {
		t.Fatalf("build cancelled: %v", err)
	}
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelReq.Header.Set("MCP-Session-Id", sessionID)
	resp, err := client.Do(cancelReq)
	if err != nil {
		t.Fatalf("POST notifications/cancelled: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 for cancellation notification, got %d", resp.StatusCode)
	}
}

// TestSDKTransport_CancelledWithIDRejected verifies a malformed cancellation
// carrying an id is rejected via the JSON-RPC error contract rather than
// forwarded as a request.
func TestSDKTransport_CancelledWithIDRejected(t *testing.T) {
	url, stop := startConformanceServer(t)
	defer stop()

	client := &http.Client{Timeout: 3 * time.Second}
	sessionID := initSession(t, client, url)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"notifications/cancelled","params":{"requestId":"42"}}`))
	if err != nil {
		t.Fatalf("build cancelled+id: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Session-Id", sessionID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST cancelled+id: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 JSON-RPC error envelope, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "INVALID_FIELD") {
		t.Fatalf("expected INVALID_FIELD error, got %s", string(body))
	}
}
