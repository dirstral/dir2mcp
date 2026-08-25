package elevenlabsbridge_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/elevenlabsbridge"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// oversizedFiller is the padding byte that an oversized upstream body repeats.
// The tests check that no run of it reaches the downstream response.
const oversizedFiller = "A"

// writeOversizedBody streams more than protocol.MaxResponseBytes to the client.
// It stops as soon as a write fails, because a correct client closes the
// connection at the cap. declare selects the two shapes that issue #711 names:
// a declared Content-Length, and a chunked body with no declared length.
func writeOversizedBody(w http.ResponseWriter, declare bool) {
	const chunkSize = 64 << 10
	total := protocol.MaxResponseBytes + (1 << 20)
	if declare {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
	}
	chunk := bytes.Repeat([]byte(oversizedFiller), chunkSize)
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

// oversizedUpstream serves a valid initialize response and then an oversized
// tools/call response with the supplied status code.
func oversizedUpstream(t *testing.T, toolStatus int, declare bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Method == protocol.RPCMethodInitialize {
			w.Header().Set(protocol.MCPSessionHeader, "session-711")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		if payload.Method == protocol.RPCMethodNotificationsInitialized {
			// bs-004: an accepted notification is answered with HTTP 202.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(toolStatus)
		writeOversizedBody(w, declare)
	}))
}

func newBridgeForUpstream(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	cfg := elevenlabsbridge.DefaultConfig()
	cfg.MCPURL = upstreamURL
	cfg.MCPToken = "bridge-token"
	cfg.StateDir = t.TempDir()
	bridge, err := elevenlabsbridge.New(cfg)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	srv := httptest.NewServer(bridge.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// postAskLimited sends one bridge /ask request and returns the status and the body.
func postAskLimited(t *testing.T, bridgeURL string) (int, string) {
	t.Helper()
	resp, err := http.Post(bridgeURL+"/ask", "application/json", strings.NewReader(`{"question":"what is alpha?"}`))
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		t.Fatalf("read bridge response: %v", err)
	}
	return resp.StatusCode, string(body)
}

// assertLimitError checks that the bridge reported the cap and echoed nothing.
func assertLimitError(t *testing.T, status int, body string) {
	t.Helper()
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d want %d body=%q", status, http.StatusBadGateway, truncateForLog(body))
	}
	wantLimit := fmt.Sprintf("%d-byte limit", protocol.MaxResponseBytes)
	if !strings.Contains(body, wantLimit) {
		t.Fatalf("error must name the limit %q, got %q", wantLimit, truncateForLog(body))
	}
	if !strings.Contains(body, "too large") {
		t.Fatalf("error must separate an oversized body from a decode failure, got %q", truncateForLog(body))
	}
	if strings.Contains(body, strings.Repeat(oversizedFiller, 64)) {
		t.Fatalf("bridge must not proxy upstream content, got %q", truncateForLog(body))
	}
	if len(body) > 1<<16 {
		t.Fatalf("bridge error body is %d bytes, want a short error", len(body))
	}
}

func truncateForLog(body string) string {
	if len(body) > 200 {
		return body[:200] + "..."
	}
	return body
}

func TestBridgeRejectsOversizedToolResponseWithDeclaredLength(t *testing.T) {
	upstream := oversizedUpstream(t, http.StatusOK, true)
	defer upstream.Close()
	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	assertLimitError(t, status, body)
}

func TestBridgeRejectsOversizedToolResponseWithChunkedBody(t *testing.T) {
	upstream := oversizedUpstream(t, http.StatusOK, false)
	defer upstream.Close()
	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	assertLimitError(t, status, body)
}

func TestBridgeDoesNotProxyOversizedUpstreamErrorBody(t *testing.T) {
	upstream := oversizedUpstream(t, http.StatusInternalServerError, false)
	defer upstream.Close()
	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	assertLimitError(t, status, body)
}

func TestBridgeRejectsOversizedInitializeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set(protocol.MCPSessionHeader, "session-711")
		writeOversizedBody(w, false)
	}))
	defer upstream.Close()
	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	assertLimitError(t, status, body)
}

// TestBridgeClearsSessionOnOversizedExpiredSessionResponse checks the session
// state after an oversized body hides the JSON-RPC payload. The upstream still
// says that the session is gone through the header, so the next call must run
// initialize again.
func TestBridgeClearsSessionOnOversizedExpiredSessionResponse(t *testing.T) {
	var mu sync.Mutex
	initializeCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Method == protocol.RPCMethodInitialize {
			mu.Lock()
			initializeCount++
			mu.Unlock()
			w.Header().Set(protocol.MCPSessionHeader, "session-711")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		if payload.Method == protocol.RPCMethodNotificationsInitialized {
			// bs-004: an accepted notification is answered with HTTP 202.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set(protocol.MCPSessionExpiredHeader, "true")
		w.WriteHeader(http.StatusNotFound)
		writeOversizedBody(w, false)
	}))
	defer upstream.Close()

	bridgeURL := newBridgeForUpstream(t, upstream.URL).URL
	for i := 0; i < 2; i++ {
		status, body := postAskLimited(t, bridgeURL)
		assertLimitError(t, status, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 2 {
		t.Fatalf("initialize count=%d, want 2 (the dead session must be dropped)", initializeCount)
	}
}

func TestBridgeRefusesUpstreamRedirect(t *testing.T) {
	var (
		mu            sync.Mutex
		redirectCount int
		redirectAuth  string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		redirectCount++
		redirectAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set(protocol.MCPSessionHeader, "session-711")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
	}))
	defer target.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	if status == http.StatusOK {
		t.Fatalf("bridge followed the redirect: body=%q", truncateForLog(body))
	}
	mu.Lock()
	defer mu.Unlock()
	if redirectCount != 0 {
		t.Fatalf("bridge sent %d requests to the redirect target, want 0", redirectCount)
	}
	if redirectAuth != "" {
		t.Fatalf("bridge sent credentials to the redirect target: %q", redirectAuth)
	}
}

func TestBridgePassesResponseUnderTheLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload.Method == protocol.RPCMethodInitialize {
			w.Header().Set(protocol.MCPSessionHeader, "session-711")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`))
			return
		}
		if payload.Method == protocol.RPCMethodNotificationsInitialized {
			// bs-004: an accepted notification is answered with HTTP 202.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		big := strings.Repeat("b", 1<<20)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"isError":false,"content":[{"type":"text","text":"` + big + `"}],"structuredContent":{"answer":"ok"}}}`))
	}))
	defer upstream.Close()

	status, body := postAskLimited(t, newBridgeForUpstream(t, upstream.URL).URL)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%q", status, truncateForLog(body))
	}
	var payload struct {
		Structured map[string]interface{} `json:"structured"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode bridge response: %v", err)
	}
	if payload.Structured["answer"] != "ok" {
		t.Fatalf("structured answer=%#v", payload.Structured["answer"])
	}
}
